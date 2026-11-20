package nbd_test

import (
	"bytes"
	"errors"
	"math/rand"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/nbd"
	"github.com/norandom/restic-pve/internal/nbd/nbdtest"
)

const mib = 1 << 20

func disk(size int, seed int64) []byte {
	b := make([]byte, size)
	rand.New(rand.NewSource(seed)).Read(b)
	for i := 4 * mib; i < 6*mib && i < size; i++ { // an all-zero region, read back as holes
		b[i] = 0
	}
	return b
}

func dial(t *testing.T, s *nbdtest.Server) *nbd.Client {
	t.Helper()
	c, err := nbd.Dial(nbdtest.Start(t, s), s.Export, s.Bitmap)
	if err != nil {
		t.Fatal(err)
	}
	c.Timeout = 10 * time.Second
	t.Cleanup(func() { c.Close() })
	return c
}

func newServer(size int, dirty ...nbdtest.Range) *nbdtest.Server {
	return &nbdtest.Server{Export: "drive-scsi0", Bitmap: "snapshot-access:restic-pve", Data: disk(size, int64(size)), Dirty: dirty}
}

// noWrites checks the server never saw a command but read, disc, trim and block status.
func noWrites(t *testing.T, s *nbdtest.Server) {
	t.Helper()
	for _, c := range s.Commands() {
		switch c.Type {
		case 0, 2, 4, 7:
		default:
			t.Fatalf("client sent command type %d", c.Type)
		}
	}
}

func TestCleanDisk(t *testing.T) {
	s := newServer(64 * mib)
	c := dial(t, s)
	if c.Size() != 64*mib {
		t.Fatalf("size %d", c.Size())
	}
	d, err := c.Dirty()
	if err != nil || len(d) != 0 {
		t.Fatalf("dirty %v %v", d, err)
	}
	c.Close()
	noWrites(t, s)
}

func TestScatteredDirtyExtentsAndData(t *testing.T) {
	size := 3<<30 + 10*mib // more than one 1 GiB status range, a short tail
	dirty := []nbdtest.Range{{Off: 0, Len: 4 * mib}, {Off: 8 * mib, Len: 8 * mib}, {Off: 1<<30 - 4*mib, Len: 8 * mib}, {Off: 3 << 30, Len: 10 * mib}}
	s := &nbdtest.Server{Export: "drive-scsi0", Bitmap: "b", Data: disk(16*mib, 1), Size: uint64(size), Dirty: dirty, MaxDescriptors: 2, SplitReads: true}
	c := dial(t, s)
	got, err := c.Dirty()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(dirty) {
		t.Fatalf("dirty %+v, want %+v", got, dirty)
	}
	for i, e := range got {
		if !e.Dirty || e.Offset != dirty[i].Off || e.Length != dirty[i].Len {
			t.Fatalf("extent %d: %+v, want %+v", i, e, dirty[i])
		}
	}
	for _, st := range s.Commands() {
		if st.Type == 7 && st.Length > nbd.MaxStatusRange {
			t.Fatalf("block status of %d bytes", st.Length)
		}
	}
	idx, err := nbd.PieceIndexes(got, uint64(size), 4*mib)
	if err != nil {
		t.Fatal(err)
	}
	if want := []uint64{0, 2, 3, 255, 256, 768, 769, 770}; len(idx) != len(want) {
		t.Fatalf("pieces %v, want %v", idx, want)
	}
	buf := make([]byte, 4*mib)
	for _, i := range idx {
		off := i * 4 * mib
		p := buf[:min(4*mib, uint64(size)-off)]
		if err := c.Read(off, p); err != nil {
			t.Fatalf("read piece %d: %v", i, err)
		}
		want := make([]byte, len(p))
		if off < uint64(len(s.Data)) {
			copy(want, s.Data[off:])
		}
		if !bytes.Equal(p, want) {
			t.Fatalf("piece %d differs", i)
		}
		if err := c.Trim(off, uint64(len(p))); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.Trimmed()) != len(idx) {
		t.Fatalf("%d trims", len(s.Trimmed()))
	}
	c.Close()
	noWrites(t, s)
}

func TestReadWithHolesAndSimpleReplies(t *testing.T) {
	s := newServer(8*mib, nbdtest.Range{Off: 0, Len: 8 * mib})
	s.SplitReads = true
	c := dial(t, s)
	p := make([]byte, 4*mib)
	for i := range p {
		p[i] = 0xee // the hole half must come back as zeros
	}
	if err := c.Read(4*mib, p); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(p, s.Data[4*mib:]) {
		t.Fatal("holes not read as zeros")
	}
}

func TestMisalignedExtent(t *testing.T) {
	size := uint64(64 * mib)
	s := newServer(int(size), nbdtest.Range{Off: 4 * mib, Len: 4*mib + 65536})
	c := dial(t, s)
	d, err := c.Dirty()
	if err != nil {
		t.Fatal(err)
	}
	if len(d) != 1 || d[0].Length != 4*mib+65536 {
		t.Fatalf("client changed the extent: %+v", d)
	}
	if _, err := nbd.PieceIndexes(d, size, 4*mib); !errors.Is(err, nbd.ErrMisaligned) {
		t.Fatalf("misaligned extent accepted: %v", err)
	}
	if _, err := nbd.PieceIndexes([]nbd.Extent{{Offset: 60 * mib, Length: 4 * mib, Dirty: true}}, 62*mib+1, 4*mib); err == nil {
		t.Fatal("extent past the disk accepted")
	}
	if idx, err := nbd.PieceIndexes([]nbd.Extent{{Offset: 60 * mib, Length: 2*mib + 1, Dirty: true}}, 62*mib+1, 4*mib); err != nil || len(idx) != 1 || idx[0] != 15 {
		t.Fatalf("tail extent: %v %v", idx, err)
	}
}

func TestBrokenServers(t *testing.T) {
	dirty := nbdtest.Range{Off: 0, Len: 8 * mib}
	for name, f := range map[string]nbdtest.Fault{
		"drops the connection in a read":   nbdtest.FaultDropInRead,
		"drops the connection in a status": nbdtest.FaultDropInStatus,
		"drops after the handshake":        nbdtest.FaultDropAfterGo,
		"unknown reply type":               nbdtest.FaultUnknownReply,
		"unknown status flag":              nbdtest.FaultStatusFlags,
		"wrong cookie":                     nbdtest.FaultWrongCookie,
		"read covers half":                 nbdtest.FaultReadShort,
		"read chunks overlap":              nbdtest.FaultReadOverlap,
		"bad reply magic":                  nbdtest.FaultBadMagic,
		"descriptor past the request":      nbdtest.FaultStatusPastSize,
		"descriptor of length 0":           nbdtest.FaultStatusZeroDescriptor,
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(16*mib, dirty)
			s.Fault = f
			c := dial(t, s)
			_, err1 := c.Dirty()
			err2 := c.Read(0, make([]byte, 4*mib))
			if err1 == nil && err2 == nil {
				t.Fatal("broken server accepted")
			}
			for _, err := range []error{err1, err2} {
				if err != nil && !errors.Is(err, nbd.ErrProtocol) {
					t.Fatalf("error %v is not a protocol error", err)
				}
			}
			// after a protocol error the session is unusable
			if err := c.Read(0, make([]byte, 4096)); err == nil {
				t.Fatal("session usable after a protocol error")
			}
			noWrites(t, s)
		})
	}
	for name, f := range map[string]nbdtest.Fault{
		"no structured replies": nbdtest.FaultNoStructured,
		"bitmap not exported":   nbdtest.FaultNoMetaContext,
		"huge option reply":     nbdtest.FaultHugeOptionReply,
	} {
		t.Run(name, func(t *testing.T) {
			s := newServer(16*mib, dirty)
			s.Fault = f
			if c, err := nbd.Dial(nbdtest.Start(t, s), s.Export, s.Bitmap); err == nil {
				c.Close()
				t.Fatal("handshake accepted")
			}
		})
	}
	s := newServer(16*mib, dirty)
	if _, err := nbd.Dial(nbdtest.Start(t, s), "drive-other", s.Bitmap); err == nil {
		t.Fatal("unknown export accepted")
	}
}

func TestServerErrorKeepsSession(t *testing.T) {
	s := newServer(16*mib, nbdtest.Range{Off: 0, Len: 8 * mib})
	s.Fault = nbdtest.FaultServerErrorOnRead
	c := dial(t, s)
	var se *nbd.ServerError
	if err := c.Read(0, make([]byte, 4*mib)); !errors.As(err, &se) || se.Errno != 5 || se.Message != "Input/output error" {
		t.Fatalf("read: %v", err)
	}
	if _, err := c.Dirty(); err != nil {
		t.Fatalf("session broken after a server error: %v", err)
	}
}

func TestBounds(t *testing.T) {
	s := newServer(16*mib, nbdtest.Range{Off: 0, Len: 16 * mib})
	c := dial(t, s)
	for name, err := range map[string]error{
		"read past the end":  c.Read(12*mib, make([]byte, 8*mib)),
		"read too large":     c.Read(0, make([]byte, nbd.MaxRead+1)),
		"empty read":         c.Read(0, nil),
		"unaligned read":     c.Read(1, make([]byte, 4096)),
		"trim past the end":  c.Trim(15*mib, 2*mib),
		"status beyond 1GiB": func() error { _, e := c.BlockStatus(0, nbd.MaxStatusRange+1); return e }(),
	} {
		if err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if n := len(s.Commands()); n != 0 {
		t.Fatalf("%d requests sent for refused calls", n)
	}
}
