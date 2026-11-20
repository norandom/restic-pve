// Package changes is the changes stream: the frames `restic-pve request changes` writes on
// stdout for one provider job, and the ack line the backup machine sends back on stdin.
//
// Frames use the RVS1 frame header (64 bytes, magic "RVSF", CRC-32C over the header; see
// package rvs) with their own type bytes:
//
//	'C' header  JSON {"format":"RVC1","token","vmid","mode","pve","devices":[...]}
//	'G' config  u16le name length, name, data (qemu-server.conf, then qemu-server.fw)
//	'M' map     frame dev = device id; payload: state byte (0 no record, 1 record), then for
//	            state 1 a bitset of 1 bit per 4 MiB piece, bit i in byte i/8, LSB first
//	'P' piece   frame dev = device id, offset = piece start (multiple of 4 MiB), payload =
//	            the piece (4 MiB, or the disk's tail)
//	'E' end     JSON {"pieces_sent","bytes_sent"}
//
// Order: header, config frames, then for each device in ascending id its map and, in mode
// use, every piece its map marks dirty in ascending offset, then the end frame. The encoder
// and the decoder enforce the same rules (seq), so a stream the encoder accepted is one the
// decoder accepts, and anything truncated, reordered or altered in its structure is refused.
// The header CRC covers frame headers only; piece contents are checked by the backup
// machine's own digests.
//
// The ack line: "ack <token> ok <snapshot-id>" | "ack <token> arm" | "ack <token> probe" |
// "ack <token> fail <reason>", ending in a newline.
package changes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// Format is the header's "format" value.
const Format = "RVC1"

// Frame types.
const (
	TypeHeader rvs.FrameType = 'C'
	TypeConfig rvs.FrameType = 'G'
	TypeMap    rvs.FrameType = 'M'
	TypePiece  rvs.FrameType = 'P'
	TypeEnd    rvs.FrameType = 'E'
)

// Payload bounds; the decoder checks a frame's length before it reads the payload.
const (
	MaxHeaderBytes = 64 << 10
	MaxConfigBytes = 2 + 255 + vma.MaxBlobSize // a config must fit a VMA blob
	MaxMapBytes    = 1 + (pieces.MaxPieces+7)/8
	MaxEndBytes    = 4 << 10
)

// Modes of a provider job.
const (
	ModeProbe = "probe"
	ModeUse   = "use"
	ModeNew   = "new"
)

// Bitmap modes Proxmox reports per disk.
const (
	BitmapNone  = "none"
	BitmapNew   = "new"
	BitmapReuse = "reuse"
)

// Header is the first frame.
type Header struct {
	Format  string   `json:"format"`
	Token   string   `json:"token"` // 128-bit job token, 32 lowercase hex
	VMID    uint32   `json:"vmid"`
	Mode    string   `json:"mode"`
	PVE     string   `json:"pve"`
	Devices []Device `json:"devices"` // ascending id
}

// Device is one disk of the job.
type Device struct {
	ID         uint8  `json:"id"`
	Name       string `json:"name"`
	Size       uint64 `json:"size"`
	Bitmap     string `json:"bitmap"` // none | new | reuse
	BitmapName string `json:"bitmap_name"`
}

// Config is one config frame.
type Config struct {
	Name string
	Data []byte
}

// Map is one device's map frame.
type Map struct {
	Dev      uint8
	Recorded bool   // state 1: Proxmox has a record; Bits is valid
	Bits     []byte // 1 bit per piece, bit i in byte i/8, LSB first; nil when not recorded
}

// Dirty reports whether piece i is marked.
func (m Map) Dirty(i uint64) bool {
	return m.Recorded && i/8 < uint64(len(m.Bits)) && m.Bits[i/8]&(1<<(i%8)) != 0
}

// NewMap returns a recorded map for a disk of size bytes with the given pieces marked.
func NewMap(dev uint8, size uint64, dirty []uint64) Map {
	m := Map{Dev: dev, Recorded: true, Bits: make([]byte, (pieces.Pieces(size)+7)/8)}
	for _, i := range dirty {
		m.Bits[i/8] |= 1 << (i % 8)
	}
	return m
}

// Piece is one piece frame. Data is valid until the decoder's next call.
type Piece struct {
	Dev    uint8
	Offset uint64
	Data   []byte
}

// End is the last frame.
type End struct {
	PiecesSent uint64 `json:"pieces_sent"`
	BytesSent  uint64 `json:"bytes_sent"`
}

// Frame is one decoded frame; exactly one field is set.
type Frame struct {
	Header *Header
	Config *Config
	Map    *Map
	Piece  *Piece
	End    *End
}

// Error is a structural defect of a changes stream. errors.Is(err, rvs.ErrIntegrity) holds
// (CLI exit 3).
type Error struct {
	Kind   rvs.ErrorKind
	Offset int64
	Detail string
}

func (e *Error) Error() string {
	return fmt.Sprintf("changes: %s at offset %d: %s", e.Kind, e.Offset, e.Detail)
}

// Unwrap places every stream error in the integrity class.
func (e *Error) Unwrap() error { return rvs.ErrIntegrity }

// ruleErr is a broken rule found by seq; the decoder turns it into *Error, the encoder into
// an rvs.ErrSequence error.
type ruleErr struct {
	kind rvs.ErrorKind
	msg  string
}

func (e *ruleErr) Error() string { return e.msg }

func rule(kind rvs.ErrorKind, format string, args ...any) error {
	return &ruleErr{kind, fmt.Sprintf(format, args...)}
}

// --- the stream rules, shared by encoder and decoder ---

type stage int

const (
	stHeader stage = iota
	stConfig       // after the header: config frames, or the first map
	stBody         // maps and pieces
	stDone         // after the end frame
)

type seq struct {
	st       stage
	h        Header
	allReuse bool
	configs  int
	dev      int    // index of the device whose map came last; -1 before the first map
	m        Map    // that map
	next     uint64 // first piece index not yet passed on the current device
	left     uint64 // dirty pieces of the current device not yet sent
	sent     End
}

func validHeader(h *Header) error {
	if h.Format != Format {
		return fmt.Errorf("format %q", h.Format)
	}
	if !isHex(h.Token, 32) {
		return fmt.Errorf("token %q is not 32 lowercase hex characters", h.Token)
	}
	if h.VMID < 100 || h.VMID > 999999999 {
		return fmt.Errorf("vmid %d", h.VMID)
	}
	switch h.Mode {
	case ModeProbe, ModeUse, ModeNew:
	default:
		return fmt.Errorf("mode %q", h.Mode)
	}
	if !printable(h.PVE, 64) {
		return fmt.Errorf("pve %q", h.PVE)
	}
	if len(h.Devices) == 0 || len(h.Devices) > vma.MaxDevID {
		return fmt.Errorf("%d devices", len(h.Devices))
	}
	names := map[string]bool{}
	for i, d := range h.Devices {
		if d.ID < vma.MinDevID || d.ID > vma.MaxDevID || i > 0 && d.ID <= h.Devices[i-1].ID {
			return fmt.Errorf("device %d: id %d out of range or not ascending", i, d.ID)
		}
		if !pieces.ValidDeviceName(d.Name) || names[d.Name] {
			return fmt.Errorf("device %d: name %q invalid or twice", d.ID, d.Name)
		}
		names[d.Name] = true
		if d.Size == 0 || pieces.Pieces(d.Size) > pieces.MaxPieces {
			return fmt.Errorf("device %s: size %d", d.Name, d.Size)
		}
		switch d.Bitmap {
		case BitmapNone, BitmapNew, BitmapReuse:
		default:
			return fmt.Errorf("device %s: bitmap %q", d.Name, d.Bitmap)
		}
		if d.BitmapName != "" && !printable(d.BitmapName, 255) || d.Bitmap == BitmapReuse && d.BitmapName == "" {
			return fmt.Errorf("device %s: bitmap name %q", d.Name, d.BitmapName)
		}
	}
	return nil
}

func (s *seq) header(h *Header) error {
	if s.st != stHeader {
		return rule(rvs.ErrOrder, "a second header frame")
	}
	if err := validHeader(h); err != nil {
		return rule(rvs.ErrPayload, "header: %v", err)
	}
	s.h = *h
	s.h.Devices = append([]Device(nil), h.Devices...)
	s.allReuse = true
	for _, d := range h.Devices {
		s.allReuse = s.allReuse && d.Bitmap == BitmapReuse
	}
	s.st, s.dev = stConfig, -1
	return nil
}

var configOrder = []string{"qemu-server.conf", "qemu-server.fw"}

func (s *seq) config(c *Config) error {
	if s.st != stConfig {
		return rule(rvs.ErrOrder, "config frame %q outside the config part", c.Name)
	}
	if s.configs >= len(configOrder) || c.Name != configOrder[s.configs] {
		return rule(rvs.ErrPayload, "config %q where %s is expected", c.Name, configOrder[min(s.configs, len(configOrder)-1)])
	}
	if len(c.Data) > vma.MaxBlobSize {
		return rule(rvs.ErrLength, "config %s is %d bytes", c.Name, len(c.Data))
	}
	s.configs++
	return nil
}

// recordAllowed tells whether a device may (and then must) have a recorded map.
func (s *seq) recordWanted(d Device) bool {
	switch s.h.Mode {
	case ModeUse:
		return s.allReuse
	case ModeProbe:
		return d.Bitmap == BitmapReuse
	}
	return false
}

// closeDevice checks that every dirty piece of the current device was sent (mode use).
func (s *seq) closeDevice() error {
	if s.dev >= 0 && s.left > 0 {
		return rule(rvs.ErrOrder, "device %s: %d dirty pieces not sent", s.h.Devices[s.dev].Name, s.left)
	}
	return nil
}

func (s *seq) mapFrame(m *Map) error {
	if s.st == stConfig {
		if s.configs == 0 {
			return rule(rvs.ErrOrder, "map frame before the qemu-server.conf config frame")
		}
		s.st = stBody
	}
	if s.st != stBody {
		return rule(rvs.ErrOrder, "map frame out of place")
	}
	if err := s.closeDevice(); err != nil {
		return err
	}
	if s.dev+1 >= len(s.h.Devices) {
		return rule(rvs.ErrOrder, "map frame for device %d after the last device", m.Dev)
	}
	d := s.h.Devices[s.dev+1]
	if m.Dev != d.ID {
		return rule(rvs.ErrOrder, "map frame for device %d where device %d is next", m.Dev, d.ID)
	}
	if m.Recorded != s.recordWanted(d) {
		return rule(rvs.ErrPayload, "device %s: map state %v with bitmap %s in mode %s", d.Name, m.Recorded, d.Bitmap, s.h.Mode)
	}
	n := pieces.Pieces(d.Size)
	var dirty uint64
	if m.Recorded {
		if uint64(len(m.Bits)) != (n+7)/8 {
			return rule(rvs.ErrLength, "device %s: map of %d bytes for %d pieces", d.Name, len(m.Bits), n)
		}
		if n%8 != 0 && m.Bits[len(m.Bits)-1]>>(n%8) != 0 {
			return rule(rvs.ErrPayload, "device %s: bits set past the last piece", d.Name)
		}
		for _, b := range m.Bits {
			dirty += uint64(bits.OnesCount8(b))
		}
	} else if len(m.Bits) != 0 {
		return rule(rvs.ErrLength, "device %s: a map without record carries %d bytes", d.Name, len(m.Bits))
	}
	s.dev, s.m, s.next, s.left = s.dev+1, *m, 0, 0
	s.m.Bits = append([]byte(nil), m.Bits...)
	if s.h.Mode == ModeUse {
		s.left = dirty
	}
	return nil
}

func (s *seq) piece(dev uint8, offset, length uint64) error {
	if s.st != stBody || s.dev < 0 {
		return rule(rvs.ErrOrder, "piece frame before its map")
	}
	d := s.h.Devices[s.dev]
	if dev != d.ID {
		return rule(rvs.ErrOrder, "piece of device %d while device %d is open", dev, d.ID)
	}
	if offset%pieces.PieceSize != 0 {
		return rule(rvs.ErrOffset, "device %s: piece offset %d is not a multiple of 4 MiB", d.Name, offset)
	}
	if offset >= d.Size {
		return rule(rvs.ErrOffset, "device %s: piece offset %d beyond the size %d", d.Name, offset, d.Size)
	}
	i := offset / pieces.PieceSize
	if s.h.Mode != ModeUse || !s.m.Dirty(i) || i < s.next {
		return rule(rvs.ErrOrder, "device %s: piece %d not expected", d.Name, i)
	}
	for j := s.next; j < i; j++ {
		if s.m.Dirty(j) {
			return rule(rvs.ErrOrder, "device %s: piece %d sent before dirty piece %d", d.Name, i, j)
		}
	}
	if want := pieces.PieceLen(d.Size, i); length != want {
		return rule(rvs.ErrLength, "device %s: piece %d is %d bytes, want %d", d.Name, i, length, want)
	}
	s.next, s.left = i+1, s.left-1
	s.sent.PiecesSent++
	s.sent.BytesSent += length
	return nil
}

func (s *seq) end(e *End) error {
	if s.st != stBody || s.dev != len(s.h.Devices)-1 {
		return rule(rvs.ErrOrder, "end frame before the map of every device")
	}
	if err := s.closeDevice(); err != nil {
		return err
	}
	if *e != s.sent {
		return rule(rvs.ErrPayload, "end frame says %d pieces and %d bytes, stream had %d and %d",
			e.PiecesSent, e.BytesSent, s.sent.PiecesSent, s.sent.BytesSent)
	}
	s.st = stDone
	return nil
}

// expected is the success verb of the ack once the stream is complete: probe in mode probe,
// ok in mode use when every disk had a record, arm otherwise.
func (s *seq) expected() Verb {
	switch {
	case s.st != stDone:
		return ""
	case s.h.Mode == ModeProbe:
		return VerbProbe
	case s.h.Mode == ModeUse && s.allReuse:
		return VerbOK
	}
	return VerbArm
}

// --- encoder ---

// Encoder writes a changes stream. It refuses (rvs.ErrSequence) any frame the decoder would
// refuse and keeps the first error.
type Encoder struct {
	w   io.Writer
	s   seq
	err error
}

// NewEncoder returns an encoder writing to w.
func NewEncoder(w io.Writer) *Encoder { return &Encoder{w: w} }

func (e *Encoder) check(err error) error {
	if err != nil && e.err == nil {
		e.err = fmt.Errorf("%w: changes: %v", rvs.ErrSequence, err)
	}
	return e.err
}

func (e *Encoder) emit(h rvs.FrameHeader, payload []byte) error {
	h.Length = uint64(len(payload))
	hdr := h.Encode()
	if _, err := e.w.Write(hdr[:]); err != nil {
		e.err = fmt.Errorf("changes: write: %w", err)
		return e.err
	}
	if _, err := e.w.Write(payload); err != nil {
		e.err = fmt.Errorf("changes: write: %w", err)
		return e.err
	}
	return nil
}

// Header writes the header frame. h.Format is set to RVC1.
func (e *Encoder) Header(h Header) error {
	if e.err != nil {
		return e.err
	}
	h.Format = Format
	if err := e.check(e.s.header(&h)); err != nil {
		return err
	}
	b, err := json.Marshal(h)
	if err != nil || len(b) > MaxHeaderBytes {
		return e.check(fmt.Errorf("header payload: %d bytes, %v", len(b), err))
	}
	return e.emit(rvs.FrameHeader{Type: TypeHeader}, b)
}

// Config writes a config frame.
func (e *Encoder) Config(name string, data []byte) error {
	if e.err != nil {
		return e.err
	}
	if err := e.check(e.s.config(&Config{name, data})); err != nil {
		return err
	}
	b := make([]byte, 0, 2+len(name)+len(data))
	b = append(append(append(b, byte(len(name)), byte(len(name)>>8)), name...), data...)
	return e.emit(rvs.FrameHeader{Type: TypeConfig}, b)
}

// Map writes a device's map frame.
func (e *Encoder) Map(m Map) error {
	if e.err != nil {
		return e.err
	}
	if err := e.check(e.s.mapFrame(&m)); err != nil {
		return err
	}
	b := []byte{0}
	if m.Recorded {
		b[0] = 1
		b = append(b, m.Bits...)
	}
	return e.emit(rvs.FrameHeader{Type: TypeMap, Dev: uint32(m.Dev)}, b)
}

// Piece writes a piece frame.
func (e *Encoder) Piece(dev uint8, offset uint64, data []byte) error {
	if e.err != nil {
		return e.err
	}
	if err := e.check(e.s.piece(dev, offset, uint64(len(data)))); err != nil {
		return err
	}
	return e.emit(rvs.FrameHeader{Type: TypePiece, Dev: uint32(dev), Offset: offset}, data)
}

// End writes the end frame with the counts of the pieces written.
func (e *Encoder) End() error {
	if e.err != nil {
		return e.err
	}
	end := e.s.sent
	if err := e.check(e.s.end(&end)); err != nil {
		return err
	}
	b, _ := json.Marshal(end)
	return e.emit(rvs.FrameHeader{Type: TypeEnd}, b)
}

// Expected is the success verb the ack must carry, once End was written.
func (e *Encoder) Expected() Verb { return e.s.expected() }

// --- decoder ---

// Decoder reads a changes stream frame by frame.
type Decoder struct {
	r   io.Reader
	off int64
	s   seq
	buf []byte
	err error
}

// NewDecoder returns a decoder reading from r.
func NewDecoder(r io.Reader) *Decoder { return &Decoder{r: r} }

func (d *Decoder) fail(kind rvs.ErrorKind, at int64, format string, args ...any) error {
	if d.err == nil {
		d.err = &Error{Kind: kind, Offset: at, Detail: fmt.Sprintf(format, args...)}
	}
	return d.err
}

// Next returns the next frame. After the end frame it returns io.EOF without reading, since
// the host side keeps stdout open until it has the ack; CheckEOF checks the rest later. A
// stream that ends before the end frame, and every broken rule, is an *Error.
func (d *Decoder) Next() (Frame, error) {
	if d.err != nil {
		return Frame{}, d.err
	}
	if d.s.st == stDone {
		return Frame{}, io.EOF
	}
	at := d.off
	var hb [rvs.FrameHeaderSize]byte
	if n, err := io.ReadFull(d.r, hb[:]); err != nil {
		if err == io.EOF || err == io.ErrUnexpectedEOF {
			return Frame{}, d.fail(rvs.ErrTruncated, at+int64(n), "stream ends before the end frame")
		}
		return Frame{}, d.fail(rvs.ErrTruncated, at+int64(n), "read: %v", err)
	}
	d.off += rvs.FrameHeaderSize
	fh, err := rvs.DecodeFrameHeader(hb[:])
	if err != nil {
		var fe *rvs.FormatError
		if errors.As(err, &fe) {
			return Frame{}, d.fail(fe.Kind, at, "%s", fe.Detail)
		}
		return Frame{}, d.fail(rvs.ErrPayload, at, "%v", err)
	}
	var limit uint64
	switch fh.Type {
	case TypeHeader:
		limit = MaxHeaderBytes
	case TypeConfig:
		limit = MaxConfigBytes
	case TypeMap:
		limit = MaxMapBytes
	case TypePiece:
		limit = pieces.PieceSize
	case TypeEnd:
		limit = MaxEndBytes
	default:
		return Frame{}, d.fail(rvs.ErrFrameType, at, "frame type %d", uint8(fh.Type))
	}
	if fh.Length > limit {
		return Frame{}, d.fail(rvs.ErrLength, at, "%s frame of %d bytes, at most %d", typeName(fh.Type), fh.Length, limit)
	}
	if fh.Type != TypePiece && fh.Offset != 0 || (fh.Type != TypePiece && fh.Type != TypeMap) && fh.Dev != 0 || fh.Dev > vma.MaxDevID {
		return Frame{}, d.fail(rvs.ErrOffset, at, "%s frame with dev %d offset %d", typeName(fh.Type), fh.Dev, fh.Offset)
	}
	if uint64(cap(d.buf)) < fh.Length {
		d.buf = make([]byte, fh.Length)
	}
	p := d.buf[:fh.Length]
	if n, err := io.ReadFull(d.r, p); err != nil {
		return Frame{}, d.fail(rvs.ErrTruncated, d.off+int64(n), "%s frame payload cut after %d of %d bytes", typeName(fh.Type), n, fh.Length)
	}
	d.off += int64(fh.Length)

	f, err := d.frame(fh, p)
	if err != nil {
		var re *ruleErr
		if errors.As(err, &re) {
			return Frame{}, d.fail(re.kind, at, "%s", re.msg)
		}
		return Frame{}, d.fail(rvs.ErrPayload, at, "%v", err)
	}
	return f, nil
}

func strictJSON(p []byte, v any) error {
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("data after the JSON object")
	}
	return nil
}

func (d *Decoder) frame(fh rvs.FrameHeader, p []byte) (Frame, error) {
	switch fh.Type {
	case TypeHeader:
		var h Header
		if err := strictJSON(p, &h); err != nil {
			if d.s.st != stHeader {
				return Frame{}, rule(rvs.ErrOrder, "a second header frame")
			}
			return Frame{}, rule(rvs.ErrPayload, "header: %v", err)
		}
		return Frame{Header: &h}, d.s.header(&h)
	case TypeConfig:
		if len(p) < 2 {
			return Frame{}, rule(rvs.ErrPayload, "config frame name cut")
		}
		nl := int(p[0]) | int(p[1])<<8
		if len(p) < 2+nl {
			return Frame{}, rule(rvs.ErrPayload, "config frame name cut")
		}
		c := &Config{Name: string(p[2 : 2+nl]), Data: append([]byte(nil), p[2+nl:]...)}
		return Frame{Config: c}, d.s.config(c)
	case TypeMap:
		if len(p) < 1 || p[0] > 1 {
			return Frame{}, rule(rvs.ErrPayload, "map state byte missing or not 0/1")
		}
		m := &Map{Dev: uint8(fh.Dev), Recorded: p[0] == 1}
		if len(p) > 1 {
			m.Bits = append([]byte(nil), p[1:]...)
		}
		return Frame{Map: m}, d.s.mapFrame(m)
	case TypePiece:
		pc := &Piece{Dev: uint8(fh.Dev), Offset: fh.Offset, Data: p}
		return Frame{Piece: pc}, d.s.piece(pc.Dev, pc.Offset, uint64(len(p)))
	default: // TypeEnd
		var e End
		if err := strictJSON(p, &e); err != nil {
			return Frame{}, rule(rvs.ErrPayload, "end: %v", err)
		}
		return Frame{End: &e}, d.s.end(&e)
	}
}

// Header returns the stream's header once it was read.
func (d *Decoder) Header() *Header {
	if d.s.st == stHeader {
		return nil
	}
	h := d.s.h
	return &h
}

// Expected is the success verb the ack must carry, once the end frame was read.
func (d *Decoder) Expected() Verb { return d.s.expected() }

// CheckEOF checks that nothing follows the end frame. Call it only after the ack was sent:
// the host side closes stdout then.
func (d *Decoder) CheckEOF() error {
	if d.s.st != stDone {
		return d.fail(rvs.ErrOrder, d.off, "CheckEOF before the end frame")
	}
	var b [1]byte
	if n, err := io.ReadFull(d.r, b[:]); n > 0 || err != io.EOF {
		return d.fail(rvs.ErrTrailingData, d.off, "data after the end frame")
	}
	return nil
}

func typeName(t rvs.FrameType) string {
	switch t {
	case TypeHeader:
		return "header"
	case TypeConfig:
		return "config"
	case TypeMap:
		return "map"
	case TypePiece:
		return "piece"
	case TypeEnd:
		return "end"
	}
	return fmt.Sprintf("type %d", uint8(t))
}

func isHex(s string, n int) bool {
	if len(s) != n {
		return false
	}
	for i := 0; i < len(s); i++ {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func printable(s string, maxLen int) bool {
	if len(s) == 0 || len(s) > maxLen {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			return false
		}
	}
	return true
}
