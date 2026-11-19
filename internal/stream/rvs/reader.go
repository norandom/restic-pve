package rvs

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Reader reads an RVS1 stream in one pass from a plain io.Reader. Memory use
// is the header, the configs, the trailer and one frame header; payloads are
// streamed to the caller.
//
// It validates every frame header (magic, hcrc, reserved fields, type) and
// the frame sequence: header first, configs directly after it, data frames
// per device in ascending id at offsets 0, FrameBytes, … with the exact
// length of their position, patch frames inside a device, cluster aligned,
// whole clusters (or ending at the device end) and before the data frame
// they patch, the trailer after the last data frame and nothing after it.
// Errors are *FormatError (integrity class) unless the underlying reader
// fails. Reader wraps and passes on that error.
//
// The Reader does not detect a position patched twice; converters record
// patched positions anyway and must reject duplicates.
type Reader struct {
	r   io.Reader
	pos int64 // bytes consumed from r
	err error // sticky

	h       Header
	pending *FrameHeader // first body frame, read while collecting configs
	pendAt  int64

	dev  int    // index into h.Devices of the next data frame
	next uint64 // offset of the next data frame

	payload *payloadReader
	trailer *Trailer
}

// NewReader reads and validates the header frame and the config frames.
func NewReader(r io.Reader) (*Reader, error) {
	rr := &Reader{r: r}
	if err := rr.readPreamble(); err != nil {
		return nil, err
	}
	return rr, nil
}

// Header returns the device table and the configs (verbatim).
func (r *Reader) Header() *Header { return &r.h }

func (r *Reader) readFull(b []byte) error {
	n, err := io.ReadFull(r.r, b)
	r.pos += int64(n)
	switch {
	case err == nil:
		return nil
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		return formatErr(ErrTruncated, r.pos, "stream ends early (%d bytes missing)", len(b)-n)
	default:
		return fmt.Errorf("rvs: read at offset %d: %w", r.pos, err)
	}
}

// readFrameHeader reads and decodes one frame header. It returns the frame
// start offset.
func (r *Reader) readFrameHeader() (FrameHeader, int64, error) {
	at := r.pos
	var b [FrameHeaderSize]byte
	if err := r.readFull(b[:]); err != nil {
		return FrameHeader{}, at, err
	}
	h, err := DecodeFrameHeader(b[:])
	if err != nil {
		var fe *FormatError
		if errors.As(err, &fe) {
			fe.Offset = at
		}
		return h, at, err
	}
	if h.Type < TypeHeader || h.Type > TypeTrailer {
		return h, at, formatErr(ErrFrameType, at, "%v", h.Type)
	}
	if h.Type != TypeData && h.Type != TypePatch && (h.Dev != 0 || h.Offset != 0) {
		return h, at, formatErr(ErrOffset, at, "%v frame with dev %d offset %d", h.Type, h.Dev, h.Offset)
	}
	return h, at, nil
}

// readSmallPayload reads a bounded header/config/trailer payload.
func (r *Reader) readSmallPayload(h FrameHeader, at int64, limit uint64) ([]byte, error) {
	if h.Length > limit {
		return nil, formatErr(ErrLength, at, "%v payload %d bytes exceeds %d", h.Type, h.Length, limit)
	}
	p := make([]byte, h.Length)
	if err := r.readFull(p); err != nil {
		return nil, err
	}
	return p, nil
}

func (r *Reader) readPreamble() error {
	h, at, err := r.readFrameHeader()
	if err != nil {
		return err
	}
	if h.Type != TypeHeader {
		return formatErr(ErrOrder, at, "first frame is %v, want header", h.Type)
	}
	p, err := r.readSmallPayload(h, at, maxHeaderPayload)
	if err != nil {
		return err
	}
	if len(p) >= 4 && !bytes.Equal(p[:4], PayloadMagic[:]) {
		return formatErr(ErrMagic, at, "header payload magic %q", p[:4])
	}
	devs, err := decodeHeaderPayload(p)
	if err != nil {
		return formatErr(ErrPayload, at, "header: %v", err)
	}
	r.h.Devices = devs
	r.skipEmpty()
	for {
		h, at, err := r.readFrameHeader()
		if err != nil {
			return err
		}
		if h.Type != TypeConfig {
			r.pending, r.pendAt = &h, at
			return nil
		}
		p, err := r.readSmallPayload(h, at, maxConfigPayload)
		if err != nil {
			return err
		}
		c, err := decodeConfigPayload(p)
		if err != nil {
			return formatErr(ErrPayload, at, "config: %v", err)
		}
		r.h.Configs = append(r.h.Configs, c)
	}
}

func (r *Reader) skipEmpty() {
	for r.dev < len(r.h.Devices) && r.next >= r.h.Devices[r.dev].Size {
		r.dev++
		r.next = 0
	}
}

func (r *Reader) fail(err error) error {
	if r.err == nil {
		r.err = err
	}
	return err
}

// Next returns the next data or patch frame and its payload (exactly Length
// bytes; an unread rest is skipped by the following call). After the trailer
// has been read and validated, Next returns io.EOF and Trailer is available.
func (r *Reader) Next() (FrameHeader, io.Reader, error) {
	if r.err != nil {
		return FrameHeader{}, nil, r.err
	}
	if r.payload != nil {
		if _, err := io.Copy(io.Discard, r.payload); err != nil {
			return FrameHeader{}, nil, r.fail(err)
		}
		if r.payload.err != nil {
			return FrameHeader{}, nil, r.fail(r.payload.err)
		}
		r.payload = nil
	}
	var (
		h   FrameHeader
		at  int64
		err error
	)
	if r.pending != nil {
		h, at = *r.pending, r.pendAt
		r.pending = nil
	} else if h, at, err = r.readFrameHeader(); err != nil {
		return FrameHeader{}, nil, r.fail(err)
	}
	switch h.Type {
	case TypeData:
		err = r.checkData(h, at)
	case TypePatch:
		err = r.checkPatch(h, at)
	case TypeTrailer:
		if err = r.readTrailer(h, at); err == nil {
			err = io.EOF
		}
	default:
		err = formatErr(ErrOrder, at, "%v frame inside the body", h.Type)
	}
	if err != nil {
		return FrameHeader{}, nil, r.fail(err)
	}
	r.payload = &payloadReader{r: r, left: h.Length}
	return h, r.payload, nil
}

func (r *Reader) checkData(h FrameHeader, at int64) error {
	if r.dev >= len(r.h.Devices) {
		return formatErr(ErrOrder, at, "data frame dev %d offset %d after the last device", h.Dev, h.Offset)
	}
	d := r.h.Devices[r.dev]
	if h.Dev != uint32(d.ID) || h.Offset != r.next {
		return formatErr(ErrOrder, at, "data frame dev %d offset %d, want dev %d offset %d", h.Dev, h.Offset, d.ID, r.next)
	}
	if want := dataFrameLength(d.Size, h.Offset); h.Length != want {
		return formatErr(ErrLength, at, "data frame dev %d offset %d length %d, want %d", h.Dev, h.Offset, h.Length, want)
	}
	r.next += h.Length
	r.skipEmpty()
	return nil
}

func (r *Reader) checkPatch(h FrameHeader, at int64) error {
	pos := -1
	for i, d := range r.h.Devices {
		if uint32(d.ID) == h.Dev {
			pos = i
			break
		}
	}
	if pos < 0 {
		return formatErr(ErrOffset, at, "patch for unknown device %d", h.Dev)
	}
	d := r.h.Devices[pos]
	if h.Offset%ClusterSize != 0 || h.Offset >= d.Size {
		return formatErr(ErrOffset, at, "patch dev %d offset %d (device %d bytes)", h.Dev, h.Offset, d.Size)
	}
	end := h.Offset + h.Length
	if h.Length == 0 || end > d.Size || end < h.Offset || (h.Length%ClusterSize != 0 && end != d.Size) {
		return formatErr(ErrLength, at, "patch dev %d offset %d length %d (device %d bytes)", h.Dev, h.Offset, h.Length, d.Size)
	}
	if pos < r.dev || (pos == r.dev && h.Offset < r.next) {
		return formatErr(ErrOrder, at, "patch dev %d offset %d after its data frame", h.Dev, h.Offset)
	}
	return nil
}

func (r *Reader) readTrailer(h FrameHeader, at int64) error {
	if r.dev < len(r.h.Devices) {
		return formatErr(ErrOrder, at, "trailer before data frame dev %d offset %d", r.h.Devices[r.dev].ID, r.next)
	}
	p, err := r.readSmallPayload(h, at, MaxTrailerBytes)
	if err != nil {
		return err
	}
	dec := json.NewDecoder(bytes.NewReader(p))
	var t Trailer
	if err := dec.Decode(&t); err != nil {
		return formatErr(ErrPayload, at, "trailer: %v", err)
	}
	if dec.More() {
		return formatErr(ErrPayload, at, "trailer: data after the JSON object")
	}
	if t.Format != Format {
		return formatErr(ErrPayload, at, "trailer format %q", t.Format)
	}
	if err := checkTrailerDevices(&t, r.h.Devices); err != nil {
		return formatErr(ErrPayload, at, "%v", err)
	}
	var one [1]byte
	n, err := io.ReadFull(r.r, one[:])
	if n > 0 {
		return formatErr(ErrTrailingData, r.pos, "data after the trailer")
	}
	if err != nil && err != io.EOF {
		return fmt.Errorf("rvs: read at offset %d: %w", r.pos, err)
	}
	r.trailer = &t
	return nil
}

// Trailer returns the validated trailer once Next has returned io.EOF.
func (r *Reader) Trailer() (*Trailer, error) {
	if r.trailer == nil {
		return nil, errors.New("rvs: trailer not reached")
	}
	return r.trailer, nil
}

// payloadReader yields exactly left bytes of the current frame; a short
// stream is reported as ErrTruncated.
type payloadReader struct {
	r    *Reader
	left uint64
	err  error
}

func (p *payloadReader) Read(b []byte) (int, error) {
	if p.err != nil {
		return 0, p.err
	}
	if p.left == 0 {
		return 0, io.EOF
	}
	if uint64(len(b)) > p.left {
		b = b[:p.left]
	}
	n, err := p.r.r.Read(b)
	p.r.pos += int64(n)
	p.left -= uint64(n)
	switch {
	case err == nil || (err == io.EOF && p.left == 0):
		return n, nil
	case err == io.EOF || err == io.ErrUnexpectedEOF:
		p.err = formatErr(ErrTruncated, p.r.pos, "stream ends inside a frame payload (%d bytes missing)", p.left)
	default:
		p.err = fmt.Errorf("rvs: read at offset %d: %w", p.r.pos, err)
	}
	p.r.fail(p.err)
	return n, p.err
}
