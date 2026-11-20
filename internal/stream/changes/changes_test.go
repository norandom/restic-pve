package changes

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"math/rand"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/rvs"
)

const testToken = "00112233445566778899aabbccddeeff"

func testHeader(mode string, bitmaps ...string) Header {
	h := Header{Token: testToken, VMID: 100, Mode: mode, PVE: "9.2.24"}
	sizes := []uint64{5*pieces.PieceSize + 4096, 2 * pieces.PieceSize, 528 << 10}
	names := []string{"drive-scsi0", "drive-scsi1", "drive-efidisk0"}
	for i, b := range bitmaps {
		h.Devices = append(h.Devices, Device{ID: uint8(i + 1), Name: names[i], Size: sizes[i], Bitmap: b, BitmapName: "snapshot-access:restic-pve"})
	}
	return h
}

type stream struct {
	h      Header
	maps   []Map
	pieces map[uint8][][]byte // per device, the data of each dirty piece in order
	bytes  []byte
}

// build writes a whole stream with the encoder; dirty lists the dirty pieces per device.
func build(t *testing.T, h Header, dirty map[uint8][]uint64) *stream {
	t.Helper()
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	s := &stream{h: h, pieces: map[uint8][][]byte{}}
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(e.Header(h))
	must(e.Config("qemu-server.conf", []byte("cores: 2\nscsi0: local:100/vm-100-disk-0.raw,size=20G\n")))
	must(e.Config("qemu-server.fw", []byte("[OPTIONS]\nenable: 1\n")))
	r := rand.New(rand.NewSource(int64(len(dirty))))
	for _, d := range h.Devices {
		m := Map{Dev: d.ID}
		s.sendsRecord(d, &m, dirty[d.ID])
		must(e.Map(m))
		s.maps = append(s.maps, m)
		if h.Mode != ModeUse || !m.Recorded {
			continue
		}
		for _, i := range dirty[d.ID] {
			data := make([]byte, pieces.PieceLen(d.Size, i))
			r.Read(data)
			s.pieces[d.ID] = append(s.pieces[d.ID], data)
			must(e.Piece(d.ID, i*pieces.PieceSize, data))
		}
	}
	must(e.End())
	s.bytes = buf.Bytes()
	return s
}

func (s *stream) sendsRecord(d Device, m *Map, dirty []uint64) {
	all := true
	for _, x := range s.h.Devices {
		all = all && x.Bitmap == BitmapReuse
	}
	if s.h.Mode == ModeUse && all || s.h.Mode == ModeProbe && d.Bitmap == BitmapReuse {
		*m = NewMap(d.ID, d.Size, dirty)
	}
}

func decodeAll(t *testing.T, b []byte) (*Decoder, []Frame, error) {
	t.Helper()
	d := NewDecoder(bytes.NewReader(b))
	var out []Frame
	for {
		f, err := d.Next()
		if err == io.EOF {
			return d, out, nil
		}
		if err != nil {
			return d, out, err
		}
		if f.Piece != nil {
			cp := *f.Piece
			cp.Data = append([]byte(nil), cp.Data...)
			f.Piece = &cp
		}
		out = append(out, f)
	}
}

func TestRoundTripUse(t *testing.T) {
	dirty := map[uint8][]uint64{1: {0, 3, 5}, 2: {}, 3: {0}} // 5 and device 3's piece are short tails
	s := build(t, testHeader(ModeUse, BitmapReuse, BitmapReuse, BitmapReuse), dirty)
	d, frames, err := decodeAll(t, s.bytes)
	if err != nil {
		t.Fatal(err)
	}
	if h := frames[0].Header; h == nil || h.Token != testToken || h.Format != Format || len(h.Devices) != 3 {
		t.Fatalf("header %+v", frames[0].Header)
	}
	if c := frames[2].Config; c == nil || c.Name != "qemu-server.fw" || string(c.Data) != "[OPTIONS]\nenable: 1\n" {
		t.Fatalf("config %+v", frames[2].Config)
	}
	var got [][]byte
	var bytesSent uint64
	for _, f := range frames {
		if f.Piece != nil {
			got = append(got, f.Piece.Data)
			bytesSent += uint64(len(f.Piece.Data))
		}
	}
	want := append(append(s.pieces[1], s.pieces[2]...), s.pieces[3]...)
	if len(got) != len(want) {
		t.Fatalf("%d pieces, want %d", len(got), len(want))
	}
	for i := range want {
		if !bytes.Equal(got[i], want[i]) {
			t.Fatalf("piece %d differs", i)
		}
	}
	end := frames[len(frames)-1].End
	if end == nil || end.PiecesSent != 4 || end.BytesSent != bytesSent {
		t.Fatalf("end %+v", end)
	}
	if d.Expected() != VerbOK {
		t.Fatalf("expected %q", d.Expected())
	}
	if err := d.CheckEOF(); err != nil {
		t.Fatal(err)
	}
	m := frames[3].Map
	if !m.Dirty(0) || m.Dirty(1) || !m.Dirty(3) || !m.Dirty(5) || m.Dirty(6) {
		t.Fatalf("map bits %08b", m.Bits)
	}
}

func TestModesAndExpectedAck(t *testing.T) {
	for _, c := range []struct {
		name    string
		h       Header
		want    Verb
		records int
	}{
		{"probe", testHeader(ModeProbe, BitmapReuse, BitmapNew), VerbProbe, 1},
		{"new", testHeader(ModeNew, BitmapNew, BitmapNew), VerbArm, 0},
		{"use with a disk without record", testHeader(ModeUse, BitmapReuse, BitmapNone), VerbArm, 0},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := build(t, c.h, map[uint8][]uint64{1: {1, 2}})
			d, frames, err := decodeAll(t, s.bytes)
			if err != nil {
				t.Fatal(err)
			}
			n := 0
			for _, f := range frames {
				if f.Piece != nil {
					t.Fatal("piece frame in a stream without reads")
				}
				if f.Map != nil && f.Map.Recorded {
					n++
				}
			}
			if n != c.records || d.Expected() != c.want {
				t.Fatalf("%d recorded maps, expected %q", n, d.Expected())
			}
		})
	}
}

func raw(typ rvs.FrameType, dev uint32, off uint64, payload []byte) []byte {
	h := rvs.FrameHeader{Type: typ, Dev: dev, Offset: off, Length: uint64(len(payload))}.Encode()
	return append(h[:], payload...)
}

// frameSpans splits a stream into its frames (raw bytes).
func frameSpans(t *testing.T, b []byte) [][]byte {
	t.Helper()
	var out [][]byte
	for len(b) > 0 {
		h, err := rvs.DecodeFrameHeader(b[:rvs.FrameHeaderSize])
		if err != nil {
			t.Fatal(err)
		}
		n := rvs.FrameHeaderSize + int(h.Length)
		out = append(out, b[:n])
		b = b[n:]
	}
	return out
}

func join(fs ...[]byte) []byte { return bytes.Join(fs, nil) }

func wantKind(t *testing.T, name string, b []byte, kind rvs.ErrorKind) {
	t.Helper()
	_, _, err := decodeAll(t, b)
	var e *Error
	if !errors.As(err, &e) || !errors.Is(err, rvs.ErrIntegrity) {
		t.Fatalf("%s: err %v, want a stream error", name, err)
	}
	if e.Kind != kind {
		t.Fatalf("%s: kind %v (%v), want %v", name, e.Kind, err, kind)
	}
}

func useStream(t *testing.T) (*stream, [][]byte) {
	s := build(t, testHeader(ModeUse, BitmapReuse, BitmapReuse), map[uint8][]uint64{1: {1, 4, 5}, 2: {0}})
	return s, frameSpans(t, s.bytes)
	// frames: 0 header, 1 conf, 2 fw, 3 map 1, 4-6 pieces of 1, 7 map 2, 8 piece of 2, 9 end
}

func TestCRCError(t *testing.T) {
	s, f := useStream(t)
	start := len(f[0])                       // the second frame's header
	for _, at := range []int{4, 9, 30, 55} { // type, dev, length, reserved area: all under the CRC
		b := append([]byte(nil), s.bytes...)
		b[start+at] ^= 0x01
		_, _, err := decodeAll(t, b)
		if !errors.Is(err, rvs.ErrIntegrity) {
			t.Fatalf("flip at %d: %v", at, err)
		}
	}
	b := append([]byte(nil), s.bytes...)
	b[60-4] ^= 0xff // the CRC itself, first frame
	wantKind(t, "crc", b, rvs.ErrCRC)
}

func TestPieceOffsetNotAligned(t *testing.T) {
	s, f := useStream(t)
	p := s.pieces[1][0]
	wantKind(t, "offset +4096", join(join(f[:4]...), raw(TypePiece, 1, 1*pieces.PieceSize+4096, p), join(f[5:]...)), rvs.ErrOffset)
	wantKind(t, "offset beyond the disk", join(join(f[:4]...), raw(TypePiece, 1, 100*pieces.PieceSize, p), join(f[5:]...)), rvs.ErrOffset)

	var buf bytes.Buffer
	e := NewEncoder(&buf)
	e.Header(s.h)
	e.Config("qemu-server.conf", nil)
	e.Map(s.maps[0])
	if err := e.Piece(1, pieces.PieceSize+512, p); !errors.Is(err, rvs.ErrSequence) {
		t.Fatalf("encoder wrote a misaligned piece: %v", err)
	}
}

func TestMapLengthWrong(t *testing.T) {
	_, f := useStream(t)
	m := f[3][rvs.FrameHeaderSize:] // state byte + 1 byte of bits (6 pieces)
	wantKind(t, "short map", join(join(f[:3]...), raw(TypeMap, 1, 0, m[:1]), join(f[4:]...)), rvs.ErrLength)
	wantKind(t, "long map", join(join(f[:3]...), raw(TypeMap, 1, 0, append(append([]byte(nil), m...), 0)), join(f[4:]...)), rvs.ErrLength)
	pad := append([]byte(nil), m...)
	pad[1] |= 0x80 // bit 7: piece 7 of a 6-piece disk
	wantKind(t, "bit past the last piece", join(join(f[:3]...), raw(TypeMap, 1, 0, pad), join(f[4:]...)), rvs.ErrPayload)
	wantKind(t, "state byte 2", join(join(f[:3]...), raw(TypeMap, 1, 0, []byte{2, m[1]}), join(f[4:]...)), rvs.ErrPayload)
	wantKind(t, "no record in an all-reuse use job", join(join(f[:3]...), raw(TypeMap, 1, 0, []byte{0}), join(f[4:]...)), rvs.ErrPayload)
}

func TestReorderedAndAltered(t *testing.T) {
	s, f := useStream(t)
	cases := map[string]struct {
		b    []byte
		kind rvs.ErrorKind
	}{
		"pieces swapped":          {join(join(f[:4]...), f[5], f[4], join(f[6:]...)), rvs.ErrOrder},
		"piece before its map":    {join(join(f[:3]...), f[4], join(f[3:]...)), rvs.ErrOrder},
		"device 2 map first":      {join(join(f[:3]...), f[7], join(f[3:7]...), join(f[8:]...)), rvs.ErrOrder},
		"dirty piece missing":     {join(join(f[:5]...), join(f[6:]...)), rvs.ErrOrder},
		"piece sent twice":        {join(join(f[:5]...), f[4], join(f[5:]...)), rvs.ErrOrder},
		"clean piece sent":        {join(join(f[:4]...), raw(TypePiece, 1, 0, s.pieces[1][0]), join(f[4:]...)), rvs.ErrOrder},
		"piece of wrong device":   {join(join(f[:4]...), raw(TypePiece, 2, pieces.PieceSize, s.pieces[1][0]), join(f[5:]...)), rvs.ErrOrder},
		"short piece":             {join(join(f[:4]...), raw(TypePiece, 1, pieces.PieceSize, s.pieces[1][0][:100]), join(f[5:]...)), rvs.ErrLength},
		"end before the last map": {join(join(f[:7]...), f[9]), rvs.ErrOrder},
		"config after a map":      {join(f[0], f[1], f[3], f[2], join(f[4:]...)), rvs.ErrOrder},
		"config fw first":         {join(f[0], f[2], f[1], join(f[3:]...)), rvs.ErrPayload},
		"map without config":      {join(f[0], join(f[3:]...)), rvs.ErrOrder},
		"two headers":             {join(f[0], f[0], join(f[1:]...)), rvs.ErrOrder},
		"no header":               {join(f[1:]...), rvs.ErrOrder},
		"end counts altered":      {join(join(f[:9]...), raw(TypeEnd, 0, 0, []byte(`{"pieces_sent":4,"bytes_sent":1}`))), rvs.ErrPayload},
		"end with unknown field":  {join(join(f[:9]...), raw(TypeEnd, 0, 0, []byte(`{"pieces_sent":4,"bytes_sent":16781312,"x":1}`))), rvs.ErrPayload},
		"unknown frame type":      {join(join(f[:3]...), raw('Z', 0, 0, nil), join(f[3:]...)), rvs.ErrFrameType},
		"header with offset":      {join(raw(TypeHeader, 0, 8, f[0][rvs.FrameHeaderSize:]), join(f[1:]...)), rvs.ErrOffset},
	}
	for name, c := range cases {
		wantKind(t, name, c.b, c.kind)
	}

	// header payload altered: each must be refused
	hb := f[0][rvs.FrameHeaderSize:]
	for _, r := range [][2]string{
		{testToken, strings.ToUpper(testToken)},
		{`"mode":"use"`, `"mode":"full"`},
		{`"format":"RVC1"`, `"format":"RVS1"`},
		{`"bitmap":"reuse"`, `"bitmap":"maybe"`},
		{`"vmid":100`, `"vmid":99`},
		{`"name":"drive-scsi0"`, `"name":"../etc"`},
		{`"vmid":100`, `"vmid":100,"extra":true`},
	} {
		alt := strings.Replace(string(hb), r[0], r[1], 1)
		if alt == string(hb) {
			t.Fatalf("%q not in the header %s", r[0], hb)
		}
		wantKind(t, "header "+r[1], join(raw(TypeHeader, 0, 0, []byte(alt)), join(f[1:]...)), rvs.ErrPayload)
	}
	var h Header
	if err := json.Unmarshal(hb, &h); err != nil || h.Devices[0].Size != s.h.Devices[0].Size {
		t.Fatalf("header payload is not the JSON it claims: %v", err)
	}
}

func TestTruncatedEverywhere(t *testing.T) {
	s, _ := useStream(t)
	r := rand.New(rand.NewSource(3))
	cuts := []int{0, 1, 63, 64, 65}
	for i := 0; i < 200; i++ {
		cuts = append(cuts, r.Intn(len(s.bytes)))
	}
	for _, n := range cuts {
		_, _, err := decodeAll(t, s.bytes[:n])
		if !errors.Is(err, rvs.ErrIntegrity) {
			t.Fatalf("cut at %d of %d: %v", n, len(s.bytes), err)
		}
	}
	d, _, err := decodeAll(t, append(append([]byte(nil), s.bytes...), 0))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.CheckEOF(); !errors.Is(err, rvs.ErrIntegrity) {
		t.Fatalf("trailing byte: %v", err)
	}
}

// oneHeader yields a frame header and then blocks the test if the decoder read on: a length
// above the bound must be refused before the payload is read or allocated.
type oneHeader struct{ b []byte }

func (o *oneHeader) Read(p []byte) (int, error) {
	if len(o.b) == 0 {
		panic("decoder read past a frame header whose length is out of bounds")
	}
	n := copy(p, o.b)
	o.b = o.b[n:]
	return n, nil
}

func TestLengthBoundsBeforeRead(t *testing.T) {
	for typ, n := range map[rvs.FrameType]uint64{
		TypeHeader: MaxHeaderBytes + 1, TypeConfig: MaxConfigBytes + 1, TypeMap: MaxMapBytes + 1,
		TypePiece: pieces.PieceSize + 1, TypeEnd: MaxEndBytes + 1,
	} {
		h := rvs.FrameHeader{Type: typ, Length: n}.Encode()
		d := NewDecoder(&oneHeader{b: h[:]})
		_, err := d.Next()
		var e *Error
		if !errors.As(err, &e) || e.Kind != rvs.ErrLength {
			t.Fatalf("type %c length %d: %v", typ, n, err)
		}
	}
}

func TestEncoderRefusesBadSequences(t *testing.T) {
	h := testHeader(ModeProbe, BitmapReuse)
	var buf bytes.Buffer
	e := NewEncoder(&buf)
	if err := e.Map(Map{Dev: 1}); !errors.Is(err, rvs.ErrSequence) {
		t.Fatalf("map before header: %v", err)
	}
	e = NewEncoder(&buf)
	e.Header(h)
	e.Config("qemu-server.conf", nil)
	e.Map(NewMap(1, h.Devices[0].Size, []uint64{2}))
	if err := e.Piece(1, 2*pieces.PieceSize, make([]byte, pieces.PieceSize)); !errors.Is(err, rvs.ErrSequence) {
		t.Fatalf("piece in a probe: %v", err)
	}
	if err := e.End(); err == nil {
		t.Fatal("encoder keeps going after an error")
	}
	bad := h
	bad.Token = "short"
	if err := NewEncoder(&buf).Header(bad); !errors.Is(err, rvs.ErrSequence) {
		t.Fatalf("bad token: %v", err)
	}
}

func TestAckLines(t *testing.T) {
	snap := strings.Repeat("a1", 32)
	for _, a := range []Ack{
		{Token: testToken, Verb: VerbOK, Snapshot: snap},
		{Token: testToken, Verb: VerbArm},
		{Token: testToken, Verb: VerbProbe},
		{Token: testToken, Verb: VerbFail, Reason: "check C2 failed: 3 pieces read"},
	} {
		line, err := a.Line()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseAck(strings.TrimSuffix(line, "\n"))
		if err != nil || got != a {
			t.Fatalf("%q: %+v %v", line, got, err)
		}
	}
	got, err := ReadAck(strings.NewReader("ack "+testToken+" ok "+snap+"\n"), testToken, VerbOK)
	if err != nil || got.Snapshot != snap {
		t.Fatalf("ok: %+v %v", got, err)
	}
	for name, c := range map[string]struct {
		in   string
		want Verb
	}{
		"wrong token":        {"ack ffffffffffffffffffffffffffffffff ok " + snap + "\n", VerbOK},
		"eof before the ack": {"", VerbOK},
		"eof inside the ack": {"ack " + testToken + " ok " + snap, VerbOK},
		"fail":               {"ack " + testToken + " fail restic exit 1\n", VerbOK},
		"arm where ok":       {"ack " + testToken + " arm\n", VerbOK},
		"ok where probe":     {"ack " + testToken + " ok " + snap + "\n", VerbProbe},
		"short snapshot id":  {"ack " + testToken + " ok " + snap[:8] + "\n", VerbOK},
		"arm with extra":     {"ack " + testToken + " arm now\n", VerbArm},
		"no ack word":        {"nak " + testToken + " arm\n", VerbArm},
		"two spaces":         {"ack  " + testToken + " arm\n", VerbArm},
		"control character":  {"ack " + testToken + " fail a\tb\n", VerbArm},
		"empty reason":       {"ack " + testToken + " fail \n", VerbArm},
		"crlf":               {"ack " + testToken + " arm\r\n", VerbArm},
		"overlong":           {"ack " + testToken + " fail " + strings.Repeat("x", 2000) + "\n", VerbArm},
		"before the end":     {"ack " + testToken + " arm\n", ""},
	} {
		_, err := ReadAck(strings.NewReader(c.in), testToken, c.want)
		if !errors.Is(err, ErrAck) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := ReadAck(strings.NewReader(""), testToken, VerbOK); !errors.Is(err, ErrAckEOF) {
		t.Fatalf("eof: %v", err)
	}
	a, err := ReadAck(strings.NewReader("ack "+testToken+" fail no space\n"), testToken, VerbOK)
	if err == nil || a.Reason != "no space" || !strings.Contains(err.Error(), "no space") {
		t.Fatalf("fail ack: %+v %v", a, err)
	}
	r := strings.NewReader("ack " + testToken + " arm\nrest")
	if _, err := ReadAck(r, testToken, VerbArm); err != nil {
		t.Fatal(err)
	}
	if rest, _ := io.ReadAll(r); string(rest) != "rest" {
		t.Fatalf("ReadAck consumed past the line: %q left", rest)
	}
}
