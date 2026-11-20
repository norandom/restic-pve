// Package nbd is a small NBD client for the export QEMU opens for one Proxmox backup provider
// job: fixed newstyle handshake over a Unix socket, structured replies, the meta context
// qemu:dirty-bitmap:<name>, and the commands BLOCK_STATUS, READ, TRIM and DISC. The protocol
// is https://github.com/NetworkBlockDevice/nbd/blob/master/doc/proto.md.
//
// The client never writes: it has no WRITE or WRITE_ZEROES command, and send refuses every
// command but the four above. Proxmox opens the export writable only so that pieces can be
// discarded (TRIM) after they were read (Base.pm, backup_vm). Any reply the client does not
// know, a reply that does not fit the request, and every length beyond its bound end the
// session with ErrProtocol.
package nbd

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// Protocol numbers (proto.md).
const (
	magicInit     = 0x4e42444d41474943 // "NBDMAGIC"
	magicOpt      = 0x49484156454F5054 // "IHAVEOPT"
	magicRep      = 0x0003e889045565a9
	magicRequest  = 0x25609513
	magicSimple   = 0x67446698
	magicStruct   = 0x668e33ef
	flagFixed     = 1 << 0 // handshake: fixed newstyle
	flagNoZeroes  = 1 << 1
	optStructured = 8
	optGo         = 7
	optSetMeta    = 10
	repAck        = 1
	repInfo       = 3
	repMeta       = 4
	repErrBit     = 1 << 31
	infoExport    = 0
	infoBlockSize = 3
	tHasFlags     = 1 << 0
	tSendTrim     = 1 << 5

	cmdRead        = 0
	cmdDisc        = 2
	cmdTrim        = 4
	cmdBlockStatus = 7

	replyFlagDone   = 1
	replyNone       = 0
	replyOffsetData = 1
	replyOffsetHole = 2
	replyBlockStat  = 5
	replyError      = 1<<15 + 1
	replyErrorOff   = 1<<15 + 2
)

// Bounds.
const (
	// MaxStatusRange is the largest range of one BLOCK_STATUS request.
	MaxStatusRange = 1 << 30
	// MaxRead is the largest READ (the server's maximum block size may lower it).
	MaxRead = 32 << 20
	// maxOptReply bounds an option reply's data, maxStatusReply a block status chunk, and
	// maxErrorMsg the message of an error reply.
	maxOptReply    = 64 << 10
	maxStatusReply = 4 + 8*(1<<17)
	maxErrorMsg    = 4096
	// DefaultTimeout bounds each handshake step and command.
	DefaultTimeout = 5 * time.Minute
)

// MetaPrefix is the meta context namespace of QEMU's dirty bitmaps; bit 0 of a status means
// dirty.
const MetaPrefix = "qemu:dirty-bitmap:"

// ErrProtocol is the class of a server that does not keep to the protocol or to the bounds.
var ErrProtocol = errors.New("nbd: protocol error")

// ErrMisaligned: a dirty extent does not start and end on a piece boundary or the disk end.
var ErrMisaligned = errors.New("nbd: change record not on piece boundaries")

// ErrNoTrim: the export does not accept TRIM.
var ErrNoTrim = errors.New("nbd: export does not accept trim")

func perr(format string, args ...any) error {
	return fmt.Errorf("%w: "+format, append([]any{ErrProtocol}, args...)...)
}

// ServerError is an error the server reported for one command.
type ServerError struct {
	Command string
	Errno   uint32
	Message string
}

func (e *ServerError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nbd: %s failed: error %d: %s", e.Command, e.Errno, e.Message)
	}
	return fmt.Sprintf("nbd: %s failed: error %d", e.Command, e.Errno)
}

// Extent is a range of the export with its status.
type Extent struct {
	Offset, Length uint64
	Dirty          bool
}

// Client is one NBD session. It is not safe for concurrent use; requests are sent one at a
// time. After any error but a *ServerError the session is unusable.
type Client struct {
	conn     net.Conn
	Timeout  time.Duration
	size     uint64
	tflags   uint16
	ctxID    uint32
	hasCtx   bool
	minBlock uint32
	maxBlock uint32
	cookie   uint64
	err      error
}

// Dial connects to the Unix socket at path and negotiates export; with a bitmap name it also
// sets the meta context qemu:dirty-bitmap:<bitmap>.
func Dial(path, export, bitmap string) (*Client, error) {
	conn, err := net.DialTimeout("unix", path, 30*time.Second)
	if err != nil {
		return nil, fmt.Errorf("nbd: connect %s: %w", path, err)
	}
	c, err := NewClient(conn, export, bitmap)
	if err != nil {
		conn.Close()
		return nil, err
	}
	return c, nil
}

// NewClient runs the handshake on conn.
func NewClient(conn net.Conn, export, bitmap string) (*Client, error) {
	c := &Client{conn: conn, Timeout: DefaultTimeout, minBlock: 1, maxBlock: MaxRead}
	if len(export) == 0 || len(export) > 4096 || len(bitmap) > 1024 {
		return nil, fmt.Errorf("nbd: export or bitmap name length")
	}
	if err := c.handshake(export, bitmap); err != nil {
		return nil, err
	}
	return c, nil
}

func (c *Client) deadline() {
	if c.Timeout > 0 {
		c.conn.SetDeadline(time.Now().Add(c.Timeout))
	}
}

func (c *Client) readFull(b []byte) error {
	if _, err := io.ReadFull(c.conn, b); err != nil {
		return perr("connection: %v", err)
	}
	return nil
}

func (c *Client) write(b []byte) error {
	if _, err := c.conn.Write(b); err != nil {
		return perr("connection: %v", err)
	}
	return nil
}

func (c *Client) handshake(export, bitmap string) error {
	c.deadline()
	var b [18]byte
	if err := c.readFull(b[:]); err != nil {
		return err
	}
	if be.Uint64(b[0:]) != magicInit || be.Uint64(b[8:]) != magicOpt {
		return perr("not a newstyle NBD server")
	}
	hs := be.Uint16(b[16:])
	if hs&flagFixed == 0 {
		return perr("server does not offer fixed newstyle")
	}
	var cf uint32 = flagFixed
	if hs&flagNoZeroes != 0 {
		cf |= flagNoZeroes
	}
	if err := c.write(be.AppendUint32(nil, cf)); err != nil {
		return err
	}

	// structured replies are required: block status only exists with them
	if err := c.sendOpt(optStructured, nil); err != nil {
		return err
	}
	if typ, _, err := c.optReply(optStructured); err != nil {
		return err
	} else if typ != repAck {
		return perr("structured reply option answered with reply type %d", typ)
	}

	if bitmap != "" {
		ctx := MetaPrefix + bitmap
		d := be.AppendUint32(nil, uint32(len(export)))
		d = append(d, export...)
		d = be.AppendUint32(d, 1)
		d = be.AppendUint32(d, uint32(len(ctx)))
		d = append(d, ctx...)
		if err := c.sendOpt(optSetMeta, d); err != nil {
			return err
		}
		for {
			typ, data, err := c.optReply(optSetMeta)
			if err != nil {
				return err
			}
			if typ == repAck {
				break
			}
			if typ != repMeta || len(data) < 4 || string(data[4:]) != ctx || c.hasCtx {
				return perr("unexpected meta context reply (type %d, %d bytes)", typ, len(data))
			}
			c.ctxID, c.hasCtx = be.Uint32(data), true
		}
		if !c.hasCtx {
			return fmt.Errorf("nbd: the export has no bitmap %q", bitmap)
		}
	}

	d := be.AppendUint32(nil, uint32(len(export)))
	d = append(d, export...)
	d = be.AppendUint16(d, 1)
	d = be.AppendUint16(d, infoBlockSize)
	if err := c.sendOpt(optGo, d); err != nil {
		return err
	}
	gotExport := false
	for {
		typ, data, err := c.optReply(optGo)
		if err != nil {
			return err
		}
		if typ == repAck {
			break
		}
		if typ != repInfo || len(data) < 2 {
			return perr("unexpected reply type %d to GO", typ)
		}
		switch be.Uint16(data) {
		case infoExport:
			if len(data) != 12 {
				return perr("export info of %d bytes", len(data))
			}
			c.size, c.tflags, gotExport = be.Uint64(data[2:]), be.Uint16(data[10:]), true
		case infoBlockSize:
			if len(data) != 14 {
				return perr("block size info of %d bytes", len(data))
			}
			min, max := be.Uint32(data[2:]), be.Uint32(data[10:])
			if min == 0 || min > 64<<10 || min&(min-1) != 0 || max < min {
				return perr("block sizes min %d max %d", min, max)
			}
			c.minBlock, c.maxBlock = min, max
		default: // name, description: allowed and ignored
		}
	}
	if !gotExport {
		return perr("GO finished without export info")
	}
	if c.tflags&tHasFlags == 0 {
		return perr("transmission flags without HAS_FLAGS")
	}
	if c.size == 0 || c.size > 1<<50 {
		return perr("export size %d", c.size)
	}
	return nil
}

func (c *Client) sendOpt(opt uint32, data []byte) error {
	b := be.AppendUint64(nil, magicOpt)
	b = be.AppendUint32(b, opt)
	b = be.AppendUint32(b, uint32(len(data)))
	return c.write(append(b, data...))
}

// optReply reads one option reply; an error reply becomes an error with its message.
func (c *Client) optReply(opt uint32) (uint32, []byte, error) {
	var h [20]byte
	if err := c.readFull(h[:]); err != nil {
		return 0, nil, err
	}
	if be.Uint64(h[0:]) != magicRep || be.Uint32(h[8:]) != opt {
		return 0, nil, perr("option reply magic or option wrong")
	}
	typ, n := be.Uint32(h[12:]), be.Uint32(h[16:])
	if n > maxOptReply {
		return 0, nil, perr("option reply of %d bytes", n)
	}
	data := make([]byte, n)
	if err := c.readFull(data); err != nil {
		return 0, nil, err
	}
	if typ&repErrBit != 0 {
		return 0, nil, fmt.Errorf("nbd: option %d refused (error %d): %s", opt, typ&^repErrBit, printable(data))
	}
	return typ, data, nil
}

// Size is the export's size in bytes.
func (c *Client) Size() uint64 { return c.size }

var cmdNames = map[uint16]string{cmdRead: "read", cmdDisc: "disc", cmdTrim: "trim", cmdBlockStatus: "block status"}

// send writes one request. Only READ, DISC, TRIM and BLOCK_STATUS exist here.
func (c *Client) send(cmd uint16, offset uint64, length uint32) (uint64, error) {
	if c.err != nil {
		return 0, c.err
	}
	if _, ok := cmdNames[cmd]; !ok {
		return 0, fmt.Errorf("nbd: command %d is not one this client sends", cmd)
	}
	c.cookie++
	b := be.AppendUint32(nil, magicRequest)
	b = be.AppendUint16(b, 0) // no command flags
	b = be.AppendUint16(b, cmd)
	b = be.AppendUint64(b, c.cookie)
	b = be.AppendUint64(b, offset)
	b = be.AppendUint32(b, length)
	c.deadline()
	if err := c.write(b); err != nil {
		c.err = err
		return 0, err
	}
	return c.cookie, nil
}

func (c *Client) broken(err error) error {
	if c.err == nil {
		c.err = err
	}
	return err
}

func (c *Client) checkRange(offset, length uint64, max uint64) error {
	if length == 0 || length > max || offset > c.size || length > c.size-offset {
		return fmt.Errorf("nbd: range %d+%d outside the export (%d bytes) or above %d", offset, length, c.size, max)
	}
	return nil
}

// reply handles the replies to one request and tells whether it was a simple reply (which
// filled simpleData). onChunk gets each structured data chunk; it returns an error for a chunk
// that does not fit the request.
func (c *Client) reply(cookie uint64, cmd uint16, simpleData []byte, onChunk func(typ uint16, p []byte) error) (bool, error) {
	var serr *ServerError
	for {
		var m [4]byte
		if err := c.readFull(m[:]); err != nil {
			return false, c.broken(err)
		}
		switch be.Uint32(m[:]) {
		case magicSimple:
			var h [12]byte
			if err := c.readFull(h[:]); err != nil {
				return false, c.broken(err)
			}
			if be.Uint64(h[4:]) != cookie {
				return false, c.broken(perr("reply for another request"))
			}
			if e := be.Uint32(h[:]); e != 0 {
				return true, &ServerError{Command: cmdNames[cmd], Errno: e}
			}
			if cmd == cmdBlockStatus {
				return false, c.broken(perr("simple reply to block status"))
			}
			if simpleData != nil {
				if err := c.readFull(simpleData); err != nil {
					return false, c.broken(err)
				}
			}
			return true, nil
		case magicStruct:
		default:
			return false, c.broken(perr("reply magic %08x", be.Uint32(m[:])))
		}
		var h [16]byte
		if err := c.readFull(h[:]); err != nil {
			return false, c.broken(err)
		}
		flags, typ, n := be.Uint16(h[0:]), be.Uint16(h[2:]), be.Uint32(h[12:])
		if be.Uint64(h[4:]) != cookie {
			return false, c.broken(perr("reply for another request"))
		}
		if flags&^replyFlagDone != 0 {
			return false, c.broken(perr("reply flags %04x", flags))
		}
		var limit uint32
		switch typ {
		case replyNone:
			limit = 0
		case replyOffsetData:
			limit = 8 + MaxRead
		case replyOffsetHole:
			limit = 12
		case replyBlockStat:
			limit = maxStatusReply
		case replyError, replyErrorOff:
			limit = 6 + maxErrorMsg + 8
		default:
			return false, c.broken(perr("unknown reply type %d", typ))
		}
		if n > limit {
			return false, c.broken(perr("reply type %d of %d bytes", typ, n))
		}
		if typ == replyOffsetData && n < 8 {
			return false, c.broken(perr("data chunk of %d bytes", n))
		}
		p := make([]byte, n)
		if err := c.readFull(p); err != nil {
			return false, c.broken(err)
		}
		switch typ {
		case replyNone:
			if flags&replyFlagDone == 0 {
				return false, c.broken(perr("none reply without done"))
			}
		case replyError, replyErrorOff:
			if n < 6 {
				return false, c.broken(perr("error reply of %d bytes", n))
			}
			ml := int(be.Uint16(p[4:]))
			want := 6 + ml
			if typ == replyErrorOff {
				want += 8
			}
			if want != int(n) {
				return false, c.broken(perr("error reply of %d bytes with a %d-byte message", n, ml))
			}
			if serr == nil {
				serr = &ServerError{Command: cmdNames[cmd], Errno: be.Uint32(p), Message: printable(p[6 : 6+ml])}
			}
		default:
			if err := onChunk(typ, p); err != nil {
				return false, c.broken(err)
			}
		}
		if flags&replyFlagDone != 0 {
			if serr != nil {
				return false, serr
			}
			return false, nil
		}
	}
}

// Read reads len(p) bytes at offset. The chunks of a structured reply must cover the range
// exactly once; holes read as zeros.
func (c *Client) Read(offset uint64, p []byte) error {
	if err := c.checkRange(offset, uint64(len(p)), min(MaxRead, uint64(c.maxBlock))); err != nil {
		return err
	}
	if m := uint64(c.minBlock); offset%m != 0 || (uint64(len(p))%m != 0 && offset+uint64(len(p)) != c.size) {
		return fmt.Errorf("nbd: read %d+%d not on the export's block size %d", offset, len(p), m)
	}
	cookie, err := c.send(cmdRead, offset, uint32(len(p)))
	if err != nil {
		return err
	}
	var covered []Extent
	simple, err := c.reply(cookie, cmdRead, p, func(typ uint16, b []byte) error {
		var start, n uint64
		switch typ {
		case replyOffsetData:
			start, n = be.Uint64(b), uint64(len(b)-8)
		case replyOffsetHole:
			if len(b) != 12 {
				return perr("hole chunk of %d bytes", len(b))
			}
			start, n = be.Uint64(b), uint64(be.Uint32(b[8:]))
		default:
			return perr("reply type %d to a read", typ)
		}
		if n == 0 || start < offset || start-offset > uint64(len(p)) || n > uint64(len(p))-(start-offset) {
			return perr("read chunk %d+%d outside the request %d+%d", start, n, offset, len(p))
		}
		for _, e := range covered {
			if start < e.Offset+e.Length && e.Offset < start+n {
				return perr("read chunks overlap at %d", start)
			}
		}
		covered = append(covered, Extent{Offset: start, Length: n})
		dst := p[start-offset : start-offset+n]
		if typ == replyOffsetData {
			copy(dst, b[8:])
		} else {
			clear(dst)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !simple {
		var total uint64
		for _, e := range covered {
			total += e.Length
		}
		if total != uint64(len(p)) {
			return c.broken(perr("read reply covers %d of %d bytes", total, len(p)))
		}
	}
	return nil
}

// Trim discards length bytes at offset.
func (c *Client) Trim(offset, length uint64) error {
	if c.tflags&tSendTrim == 0 {
		return ErrNoTrim
	}
	if err := c.checkRange(offset, length, 1<<31); err != nil {
		return err
	}
	cookie, err := c.send(cmdTrim, offset, uint32(length))
	if err != nil {
		return err
	}
	_, err = c.reply(cookie, cmdTrim, nil, func(typ uint16, _ []byte) error {
		return perr("reply type %d to a trim", typ)
	})
	return err
}

// BlockStatus queries the bitmap context for offset+length (at most MaxStatusRange). The
// extents start at offset, are contiguous, and end at most at offset+length; the server may
// describe less than asked for.
func (c *Client) BlockStatus(offset, length uint64) ([]Extent, error) {
	if !c.hasCtx {
		return nil, errors.New("nbd: no bitmap context negotiated")
	}
	if err := c.checkRange(offset, length, MaxStatusRange); err != nil {
		return nil, err
	}
	cookie, err := c.send(cmdBlockStatus, offset, uint32(length))
	if err != nil {
		return nil, err
	}
	var out []Extent
	got := false
	_, err = c.reply(cookie, cmdBlockStatus, nil, func(typ uint16, b []byte) error {
		if typ != replyBlockStat {
			return perr("reply type %d to block status", typ)
		}
		if got {
			return perr("more than one block status chunk")
		}
		got = true
		if len(b) < 12 || (len(b)-4)%8 != 0 {
			return perr("block status chunk of %d bytes", len(b))
		}
		if be.Uint32(b) != c.ctxID {
			return perr("block status for context %d", be.Uint32(b))
		}
		pos, end := offset, offset+length
		for d := b[4:]; len(d) > 0; d = d[8:] {
			n, st := uint64(be.Uint32(d)), be.Uint32(d[4:])
			if n == 0 {
				return perr("block status descriptor of length 0")
			}
			if st&^1 != 0 {
				return perr("block status flags %08x", st)
			}
			if pos >= end {
				return perr("block status descriptor after the requested range")
			}
			n = min(n, end-pos) // the last descriptor may reach beyond the request
			out = append(out, Extent{Offset: pos, Length: n, Dirty: st&1 != 0})
			pos += n
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !got {
		return nil, c.broken(perr("block status without a status chunk"))
	}
	return out, nil
}

// Dirty returns the dirty extents of the whole export in ascending order, adjacent ones
// merged, querying at most MaxStatusRange per request.
func (c *Client) Dirty() ([]Extent, error) {
	var out []Extent
	for off := uint64(0); off < c.size; {
		ext, err := c.BlockStatus(off, min(MaxStatusRange, c.size-off))
		if err != nil {
			return nil, err
		}
		for _, e := range ext {
			if !e.Dirty {
				continue
			}
			if k := len(out) - 1; k >= 0 && out[k].Offset+out[k].Length == e.Offset {
				out[k].Length += e.Length
			} else {
				out = append(out, e)
			}
		}
		last := ext[len(ext)-1]
		off = last.Offset + last.Length
	}
	return out, nil
}

// Close sends DISC and closes the connection.
func (c *Client) Close() error {
	if c.err == nil {
		c.send(cmdDisc, 0, 0)
	}
	return c.conn.Close()
}

// PieceIndexes turns dirty extents of a disk of size bytes into piece indexes. Every extent
// must start on a piece boundary and end on one or at the disk end, else ErrMisaligned:
// Proxmox's record has a granularity of 4 MiB, and a piece with an unchanged part could not
// be read from the export.
func PieceIndexes(dirty []Extent, size, piece uint64) ([]uint64, error) {
	var out []uint64
	var prev uint64
	for _, e := range dirty {
		end := e.Offset + e.Length
		if e.Length == 0 || end > size || end < e.Offset || e.Offset < prev {
			return nil, fmt.Errorf("nbd: dirty extent %d+%d out of order or beyond %d", e.Offset, e.Length, size)
		}
		if e.Offset%piece != 0 || end%piece != 0 && end != size {
			return nil, fmt.Errorf("%w: extent %d+%d", ErrMisaligned, e.Offset, e.Length)
		}
		for i := e.Offset / piece; i*piece < end; i++ {
			out = append(out, i)
		}
		prev = end
	}
	return out, nil
}

var be = binary.BigEndian

func printable(b []byte) string {
	if len(b) > 200 {
		b = b[:200]
	}
	out := make([]byte, len(b))
	for i, c := range b {
		if c < 0x20 || c > 0x7e {
			c = '?'
		}
		out[i] = c
	}
	return string(out)
}
