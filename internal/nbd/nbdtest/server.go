// Package nbdtest is a fake NBD server for tests: one export with a dirty bitmap, served the
// way QEMU serves a Proxmox backup provider's export (fixed newstyle, structured replies, the
// meta context qemu:dirty-bitmap:<name>). Faults can be injected. It shares no code or
// constants with package nbd, so a misreading of the protocol does not hide in both.
package nbdtest

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"path/filepath"
	"sync"
	"testing"
)

// Fault makes the server break the protocol in one way.
type Fault int

const (
	FaultNone                 Fault = iota
	FaultDropAfterGo                // close the connection right after the handshake
	FaultDropInRead                 // send half a data chunk of the first read, then close
	FaultDropInStatus               // close the connection instead of answering block status
	FaultUnknownReply               // answer block status with reply type 99
	FaultStatusFlags                // set an unknown status bit in block status
	FaultWrongCookie                // answer the first read with another cookie
	FaultReadShort                  // a read reply that covers only half the range
	FaultReadOverlap                // two data chunks over the same bytes
	FaultNoMetaContext              // the bitmap is not exported
	FaultNoStructured               // refuse structured replies
	FaultHugeOptionReply            // a GO info reply claiming 1 GiB
	FaultBadMagic                   // a reply magic that is no NBD magic
	FaultStatusPastSize             // a non-final descriptor reaching past the requested range
	FaultServerErrorOnRead          // an NBD error chunk (EIO) for every read; the session stays usable
	FaultStatusZeroDescriptor       // a descriptor of length 0
)

// Range is a byte range of the export.
type Range struct{ Off, Len uint64 }

// Command is one request the server received.
type Command struct {
	Type   uint16
	Offset uint64
	Length uint32
}

// Server serves one export.
type Server struct {
	Export string
	Bitmap string  // meta context qemu:dirty-bitmap:<Bitmap>
	Data   []byte  // the start of the disk
	Size   uint64  // the disk's size; 0 means len(Data). Bytes past Data read as zeros.
	Dirty  []Range // dirty ranges, ascending, not overlapping
	// MaxDescriptors limits the descriptors per block status reply (0: no limit), so the
	// client must ask again for the rest.
	MaxDescriptors int
	// SplitReads answers each read with two chunks in reverse order, all-zero halves as holes.
	SplitReads bool
	Fault      Fault

	mu       sync.Mutex
	commands []Command
	trimmed  []Range
}

func (s *Server) size() uint64 {
	if s.Size > 0 {
		return s.Size
	}
	return uint64(len(s.Data))
}

// bytes returns off+n of the disk.
func (s *Server) bytes(off, n uint64) []byte {
	b := make([]byte, n)
	if off < uint64(len(s.Data)) {
		copy(b, s.Data[off:])
	}
	return b
}

// Commands returns every request received so far.
func (s *Server) Commands() []Command {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Command(nil), s.commands...)
}

// Trimmed returns every trimmed range.
func (s *Server) Trimmed() []Range {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Range(nil), s.trimmed...)
}

// Start serves s on a Unix socket in a temporary directory of t and returns its path. Each
// connection is one session.
func Start(t testing.TB, s *Server) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "nbd.sock")
	l, err := net.Listen("unix", p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				s.Serve(c)
			}()
		}
	}()
	return p
}

var be = binary.BigEndian

const (
	repMagic    = 0x3e889045565a9
	simpleMagic = 0x67446698
	chunkMagic  = 0x668e33ef
)

func put(w io.Writer, parts ...any) error {
	for _, p := range parts {
		if err := binary.Write(w, binary.BigEndian, p); err != nil {
			return err
		}
	}
	return nil
}

func optReply(c net.Conn, opt, typ uint32, data []byte) error {
	return put(c, uint64(repMagic), opt, typ, uint32(len(data)), data)
}

// Serve runs one session on c until the client disconnects or a fault ends it.
func (s *Server) Serve(c net.Conn) error {
	return serve(c, s.Fault == FaultNoStructured, func(name string) *Server {
		if name == s.Export {
			return s
		}
		return nil
	})
}

// Exports serves several exports on one socket, chosen by name, as QEMU serves the devices
// of one backup job.
type Exports []*Server

// Serve runs one session on c.
func (x Exports) Serve(c net.Conn) error {
	noStructured := false
	for _, s := range x {
		noStructured = noStructured || s.Fault == FaultNoStructured
	}
	return serve(c, noStructured, func(name string) *Server {
		for _, s := range x {
			if s.Export == name {
				return s
			}
		}
		return nil
	})
}

// serve runs the handshake (the option phase knows no export yet, so noStructured stands for
// all of them), then the transmission phase of the export the client chose.
func serve(c net.Conn, noStructured bool, pick func(string) *Server) error {
	if err := put(c, []byte("NBDMAGIC"), []byte("IHAVEOPT"), uint16(3)); err != nil {
		return err
	}
	var cf uint32
	if err := binary.Read(c, be, &cf); err != nil {
		return err
	}
	if cf&1 == 0 {
		return errors.New("client is not fixed newstyle")
	}
	structured := false
	for {
		var h struct {
			Magic    uint64
			Opt, Len uint32
		}
		if err := binary.Read(c, be, &h); err != nil {
			return err
		}
		if h.Len > 1<<16 {
			return errors.New("option too long")
		}
		data := make([]byte, h.Len)
		if _, err := io.ReadFull(c, data); err != nil {
			return err
		}
		switch h.Opt {
		case 8: // structured reply
			if noStructured {
				optReply(c, h.Opt, 1<<31+1, []byte("no"))
				continue
			}
			structured = true
			optReply(c, h.Opt, 1, nil)
		case 10: // set meta context
			nl := be.Uint32(data)
			name := string(data[4 : 4+nl])
			q := data[4+nl+4:]
			ql := be.Uint32(q)
			query := string(q[4 : 4+ql])
			if s := pick(name); s != nil && query == "qemu:dirty-bitmap:"+s.Bitmap && s.Fault != FaultNoMetaContext {
				optReply(c, h.Opt, 4, append(be.AppendUint32(nil, 7), query...))
			}
			optReply(c, h.Opt, 1, nil)
		case 7: // go
			nl := be.Uint32(data)
			s := pick(string(data[4 : 4+nl]))
			if s == nil {
				optReply(c, h.Opt, 1<<31+6, []byte("unknown export"))
				continue
			}
			if s.Fault == FaultHugeOptionReply {
				put(c, uint64(repMagic), h.Opt, uint32(3), uint32(1<<30))
				return nil
			}
			// export info: size, flags HAS_FLAGS|SEND_FLUSH|SEND_TRIM|SEND_WRITE_ZEROES (writable, as QEMU's)
			optReply(c, h.Opt, 3, be.AppendUint16(be.AppendUint64(be.AppendUint16(nil, 0), s.size()), 1|4|32|64))
			optReply(c, h.Opt, 3, be.AppendUint32(be.AppendUint32(be.AppendUint32(be.AppendUint16(nil, 3), 512), 4096), 32<<20))
			optReply(c, h.Opt, 1, nil)
			if s.Fault == FaultDropAfterGo {
				return nil
			}
			return s.transmission(c, structured)
		default:
			optReply(c, h.Opt, 1<<31+1, nil)
		}
	}
}

func (s *Server) transmission(c net.Conn, structured bool) error {
	reads := 0
	for {
		var r struct {
			Magic       uint32
			Flags, Type uint16
			Cookie      uint64
			Offset      uint64
			Length      uint32
		}
		if err := binary.Read(c, be, &r); err != nil {
			return err
		}
		s.mu.Lock()
		s.commands = append(s.commands, Command{r.Type, r.Offset, r.Length})
		s.mu.Unlock()
		switch r.Type {
		case 0: // read
			reads++
			if err := s.read(c, r.Cookie, r.Offset, r.Length, reads == 1, structured); err != nil {
				return err
			}
		case 2: // disc
			return nil
		case 4: // trim
			s.mu.Lock()
			s.trimmed = append(s.trimmed, Range{r.Offset, uint64(r.Length)})
			s.mu.Unlock()
			put(c, uint32(simpleMagic), uint32(0), r.Cookie)
		case 7: // block status
			if err := s.status(c, r.Cookie, r.Offset, r.Length); err != nil {
				return err
			}
		default: // anything else, writes included, is refused
			put(c, uint32(simpleMagic), uint32(1), r.Cookie) // EPERM
		}
	}
}

func chunk(c net.Conn, flags, typ uint16, cookie uint64, payload []byte) error {
	return put(c, uint32(chunkMagic), flags, typ, cookie, uint32(len(payload)), payload)
}

func zero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func (s *Server) read(c net.Conn, cookie, off uint64, n uint32, first, structured bool) error {
	data := s.bytes(off, uint64(n))
	switch {
	case s.Fault == FaultServerErrorOnRead:
		msg := []byte("Input/output error")
		return chunk(c, 1, 1<<15+1, cookie, append(be.AppendUint16(be.AppendUint32(nil, 5), uint16(len(msg))), msg...))
	case s.Fault == FaultWrongCookie && first:
		cookie++
	case s.Fault == FaultBadMagic && first:
		return put(c, uint32(0xdeadbeef))
	case s.Fault == FaultDropInRead && first:
		half := append(be.AppendUint64(nil, off), data[:n/2]...)
		put(c, uint32(chunkMagic), uint16(1), uint16(1), cookie, uint32(8+len(data)))
		c.Write(half)
		return errors.New("dropped")
	case s.Fault == FaultReadShort:
		return chunk(c, 1, 1, cookie, append(be.AppendUint64(nil, off), data[:n/2]...))
	case s.Fault == FaultReadOverlap:
		chunk(c, 0, 1, cookie, append(be.AppendUint64(nil, off), data...))
		return chunk(c, 1, 1, cookie, append(be.AppendUint64(nil, off), data[:n/2]...))
	}
	if !structured {
		return put(c, uint32(simpleMagic), uint32(0), cookie, data)
	}
	if !s.SplitReads || n < 2 {
		return chunk(c, 1, 1, cookie, append(be.AppendUint64(nil, off), data...))
	}
	h := uint64(n / 2)
	parts := []struct {
		o uint64
		b []byte
	}{{off + h, data[h:]}, {off, data[:h]}} // second half first
	for i, p := range parts {
		flags := uint16(0)
		if i == len(parts)-1 {
			flags = 1
		}
		var err error
		if zero(p.b) {
			err = chunk(c, flags, 2, cookie, be.AppendUint32(be.AppendUint64(nil, p.o), uint32(len(p.b))))
		} else {
			err = chunk(c, flags, 1, cookie, append(be.AppendUint64(nil, p.o), p.b...))
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// extents describes off+n as (length, dirty) descriptors from the dirty ranges.
func (s *Server) extents(off, n uint64) [][2]uint64 {
	var out [][2]uint64
	end := off + n
	pos := off
	add := func(l, d uint64) {
		if l == 0 {
			return
		}
		if k := len(out) - 1; k >= 0 && out[k][1] == d {
			out[k][0] += l
			return
		}
		out = append(out, [2]uint64{l, d})
	}
	for _, r := range s.Dirty {
		re := r.Off + r.Len
		if re <= pos || r.Off >= end {
			continue
		}
		if r.Off > pos {
			add(r.Off-pos, 0)
			pos = r.Off
		}
		e := min(re, end)
		add(e-pos, 1)
		pos = e
	}
	add(end-pos, 0)
	return out
}

func (s *Server) status(c net.Conn, cookie, off uint64, n uint32) error {
	switch s.Fault {
	case FaultDropInStatus:
		return errors.New("dropped")
	case FaultUnknownReply:
		return chunk(c, 1, 99, cookie, nil)
	}
	ext := s.extents(off, uint64(n))
	if s.MaxDescriptors > 0 && len(ext) > s.MaxDescriptors {
		ext = ext[:s.MaxDescriptors]
	}
	p := be.AppendUint32(nil, 7)
	for i, e := range ext {
		flags := uint32(e[1])
		if s.Fault == FaultStatusFlags {
			flags |= 4
		}
		l := uint32(e[0])
		if s.Fault == FaultStatusPastSize && i == 0 && len(ext) > 1 {
			l = n + 4096
		}
		if s.Fault == FaultStatusZeroDescriptor && i == 0 {
			l = 0
		}
		p = be.AppendUint32(be.AppendUint32(p, l), flags)
	}
	return chunk(c, 1, 5, cookie, p)
}
