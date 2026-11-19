package convert

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/stream/reorder"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
	"github.com/norandom/restic-pve/internal/stream/vma"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// --- fixtures ---

func testDevices() []vmatest.Device {
	return []vmatest.Device{
		vmatest.EFIDisk(1, 256*1024, 31),
		vmatest.DataDisk(2, "scsi0", 6<<20+1000, 32), // short final cluster
		vmatest.TPMState(3, 64*1024, 33),
	}
}

func testConfigs() []vma.ConfigBlob {
	return []vma.ConfigBlob{
		vmatest.ExampleConfig(),
		{Name: "qemu-server.fw", Data: []byte("[OPTIONS]\nenable: 0\n")},
	}
}

func content(d vmatest.Device) []byte {
	out := make([]byte, d.Size)
	copy(out, d.Data)
	return out
}

func clusterOf(c []byte, index uint32) []byte {
	lo := uint64(index) * vma.ClusterSize
	data := make([]byte, vma.ClusterSize)
	copy(data, c[lo:min(lo+vma.ClusterSize, uint64(len(c)))])
	if bytes.Equal(data, make([]byte, vma.ClusterSize)) {
		return nil
	}
	return data
}

type teeSink struct {
	fp *verify.Fingerprint
	w  *rvs.Writer
}

func (s teeSink) InOrder(dev uint8, index uint32, data []byte) error {
	if err := s.fp.InOrder(dev, index, data); err != nil {
		return err
	}
	return s.w.InOrder(dev, index, data)
}

func (s teeSink) Patch(dev uint8, index uint32, data []byte) error {
	if err := s.fp.Patch(dev, index, data); err != nil {
		return err
	}
	return s.w.Patch(dev, index, data)
}

// encode produces a valid RVS1 stream (with correct sha256ct digests) for the
// devices, fed in the given order through a reorder buffer with the caps.
func encode(t *testing.T, devs []vmatest.Device, order []vmatest.ClusterRef, caps reorder.Caps) ([]byte, *rvs.Trailer) {
	t.Helper()
	h := vma.Header{CTime: 1760000000, Configs: testConfigs()}
	h.UUID[0] = 7
	for _, d := range devs {
		h.Devices = append(h.Devices, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
	}
	sort.Slice(h.Devices, func(i, j int) bool { return h.Devices[i].ID < h.Devices[j].ID })
	var out bytes.Buffer
	w, err := rvs.NewWriter(&out, h)
	if err != nil {
		t.Fatal(err)
	}
	l := reorder.Layout{Devices: h.Devices}
	fp := verify.NewFingerprint(l)
	b, err := reorder.New(caps, l, teeSink{fp, w}, fp)
	if err != nil {
		t.Fatal(err)
	}
	contents := map[uint8][]byte{}
	for _, d := range devs {
		contents[d.ID] = content(d)
	}
	if order == nil {
		order = vmatest.SequentialOrder(devs)
	}
	for _, r := range order {
		data := clusterOf(contents[r.Dev], r.Index)
		if err := b.Put(vma.Cluster{Dev: r.Dev, Index: r.Index, Zero: data == nil, Data: data}); err != nil {
			t.Fatal(err)
		}
	}
	if err := b.Finish(); err != nil {
		t.Fatal(err)
	}
	tds, err := fp.Devices()
	if err != nil {
		t.Fatal(err)
	}
	st := b.Stats()
	tr := rvs.Trailer{VMAUUID: "07000000-0000-0000-0000-000000000000", VMACTime: h.CTime, Devices: tds,
		PatchCount: st.PatchCount, Consistency: "crash", Tool: "vmastream test"}
	if err := w.Close(tr); err != nil {
		t.Fatal(err)
	}
	return out.Bytes(), &tr
}

// patchedStream returns a stream with patch frames (far jumps, tiny caps).
func patchedStream(t *testing.T) ([]byte, *rvs.Trailer, []vmatest.Device) {
	t.Helper()
	devs := testDevices()
	seq := vmatest.SequentialOrder(devs)
	order := append(append([]vmatest.ClusterRef{}, seq[len(seq)/2:]...), seq[:len(seq)/2]...)
	s, tr := encode(t, devs, order, reorder.Caps{RAMBytes: 8 * vma.ClusterSize})
	if tr.PatchCount == 0 {
		t.Fatal("fixture must contain patches")
	}
	return s, tr, devs
}

type frame struct {
	h   rvs.FrameHeader
	raw []byte
}

func split(t *testing.T, s []byte) []frame {
	t.Helper()
	var out []frame
	for len(s) > 0 {
		h, err := rvs.DecodeFrameHeader(s[:rvs.FrameHeaderSize])
		if err != nil {
			t.Fatal(err)
		}
		n := rvs.FrameHeaderSize + int(h.Length)
		out = append(out, frame{h, s[:n:n]})
		s = s[n:]
	}
	return out
}

func join(fs []frame) []byte {
	var b []byte
	for _, f := range fs {
		b = append(b, f.raw...)
	}
	return b
}

func manifestFor(tr *rvs.Trailer) *verify.Manifest {
	return &verify.Manifest{Schema: verify.ManifestSchema, Devices: append([]rvs.TrailerDevice(nil), tr.Devices...)}
}

// flipDataByte flips one byte of the data disk's first non-zero cluster in its data frame.
func flipDataByte(t *testing.T, s []byte) []byte {
	t.Helper()
	fs := split(t, s)
	for i, f := range fs {
		if f.h.Type == rvs.TypeData && f.h.Dev == 2 {
			raw := append([]byte(nil), f.raw...)
			raw[rvs.FrameHeaderSize+100] ^= 0x01 // cluster 0 of PatternData is random data
			fs[i].raw = raw
			return join(fs)
		}
	}
	t.Fatal("no data frame for device 2")
	return nil
}

func wantClass(t *testing.T, err, class error) {
	t.Helper()
	if err == nil {
		t.Fatalf("want %v, got nil", class)
	}
	if !errors.Is(err, class) {
		t.Fatalf("error %q is not of class %v", err, class)
	}
}

// --- verify ---

func TestVerifyCleanStream(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) ([]byte, *rvs.Trailer){
		"sequential": func(t *testing.T) ([]byte, *rvs.Trailer) {
			return encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
		},
		"patched": func(t *testing.T) ([]byte, *rvs.Trailer) { s, tr, _ := patchedStream(t); return s, tr },
	} {
		t.Run(name, func(t *testing.T) {
			s, tr := mk(t)
			res, err := Verify(bytes.NewReader(s), Options{Manifest: manifestFor(tr)})
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			if len(res.Trailer.Devices) != 3 || res.Trailer.Devices[1].SHA256CT != tr.Devices[1].SHA256CT {
				t.Fatalf("result trailer %+v", res.Trailer)
			}
			if name == "patched" && res.PatchedClusters == 0 {
				t.Fatal("patched clusters not counted")
			}
			if _, err := Verify(bytes.NewReader(s), Options{}); err != nil {
				t.Fatalf("Verify without manifest: %v", err)
			}
		})
	}
}

func TestVerifyFlippedDataByteIsIntegrity(t *testing.T) {
	s, _ := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	_, err := Verify(bytes.NewReader(flipDataByte(t, s)), Options{})
	wantClass(t, err, rvs.ErrIntegrity)
	if errors.Is(err, verify.ErrManifest) {
		t.Fatal("digest mismatch must not be a manifest error")
	}
}

func TestVerifyWrongManifestIsManifestClass(t *testing.T) {
	s, tr := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	cases := map[string]func(m *verify.Manifest){
		"digest":         func(m *verify.Manifest) { m.Devices[1].SHA256CT = strings.Repeat("0", 64) },
		"size":           func(m *verify.Manifest) { m.Devices[0].Size++ },
		"missing device": func(m *verify.Manifest) { m.Devices = m.Devices[:2] },
	}
	for name, mod := range cases {
		t.Run(name, func(t *testing.T) {
			m := manifestFor(tr)
			mod(m)
			_, err := Verify(bytes.NewReader(s), Options{Manifest: m})
			wantClass(t, err, verify.ErrManifest)
			if errors.Is(err, rvs.ErrIntegrity) {
				t.Fatal("manifest mismatch must not be an integrity error")
			}
		})
	}
}

func TestVerifyMalformedAndTruncatedStreams(t *testing.T) {
	s, _ := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	cases := map[string][]byte{
		"truncated":        s[:len(s)-200],
		"cut in a frame":   s[:len(s)/2],
		"header crc":       append(append([]byte(nil), s[:10]...), append([]byte{s[10] ^ 0xff}, s[11:]...)...),
		"empty":            nil,
		"trailing garbage": append(append([]byte(nil), s...), 0),
	}
	for name, b := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Verify(bytes.NewReader(b), Options{})
			wantClass(t, err, rvs.ErrIntegrity)
		})
	}
}

func TestDuplicatePatchIsRejected(t *testing.T) {
	s, _, _ := patchedStream(t)
	fs := split(t, s)
	for i, f := range fs {
		if f.h.Type == rvs.TypePatch {
			dup := append(append(append([]frame{}, fs[:i+1]...), f), fs[i+1:]...)
			_, err := Verify(bytes.NewReader(join(dup)), Options{})
			wantClass(t, err, rvs.ErrIntegrity)
			if !strings.Contains(err.Error(), "twice") {
				t.Fatalf("error should name the doubly patched position: %v", err)
			}
			return
		}
	}
	t.Fatal("no patch frame")
}

func TestNonZeroPlaceholderIsRejected(t *testing.T) {
	s, _, _ := patchedStream(t)
	fs := split(t, s)
	var p *rvs.FrameHeader
	for i := range fs {
		f := fs[i]
		if f.h.Type == rvs.TypePatch && p == nil {
			h := f.h
			p = &h
		}
		if p != nil && f.h.Type == rvs.TypeData && f.h.Dev == p.Dev && f.h.Offset <= p.Offset && p.Offset < f.h.Offset+f.h.Length {
			raw := append([]byte(nil), f.raw...)
			raw[rvs.FrameHeaderSize+int(p.Offset-f.h.Offset)] = 0x5a
			fs[i].raw = raw
			_, err := Verify(bytes.NewReader(join(fs)), Options{})
			wantClass(t, err, rvs.ErrIntegrity)
			if !strings.Contains(err.Error(), "placeholder") {
				t.Fatalf("error should name the placeholder: %v", err)
			}
			return
		}
	}
	t.Fatal("no patched data frame")
}

type failReader struct{}

func (failReader) Read([]byte) (int, error) { return 0, errors.New("device gone") }

func TestInputReadErrorIsLocalIO(t *testing.T) {
	_, err := Verify(failReader{}, Options{})
	wantClass(t, err, ErrLocalIO)
	if errors.Is(err, rvs.ErrIntegrity) {
		t.Fatal("a read error is not an integrity error")
	}
}

// --- to-vma ---

// readVMA parses a VMA stream with the 2.2 reader and rebuilds each device.
func readVMA(t *testing.T, b []byte) (*vma.Header, map[uint8][]byte, error) {
	t.Helper()
	r, err := vma.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, nil, err
	}
	h := r.Header()
	devs := map[uint8][]byte{}
	for _, d := range h.Devices {
		devs[d.ID] = make([]byte, vma.Clusters(d.Size)*vma.ClusterSize)
	}
	for {
		c, err := r.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return h, nil, err
		}
		if !c.Zero {
			copy(devs[c.Dev][uint64(c.Index)*vma.ClusterSize:], c.Data)
		}
	}
	for _, d := range h.Devices {
		devs[d.ID] = devs[d.ID][:d.Size]
	}
	return h, devs, nil
}

func TestToVMARoundTrip(t *testing.T) {
	for name, mk := range map[string]func(*testing.T) ([]byte, *rvs.Trailer, []vmatest.Device){
		"sequential": func(t *testing.T) ([]byte, *rvs.Trailer, []vmatest.Device) {
			devs := testDevices()
			s, tr := encode(t, devs, vmatest.InterleavedOrder(devs), reorder.Caps{RAMBytes: 16 << 20})
			return s, tr, devs
		},
		"patched": patchedStream,
	} {
		t.Run(name, func(t *testing.T) {
			s, tr, devs := mk(t)
			var out bytes.Buffer
			if _, err := ToVMA(bytes.NewReader(s), &out, Options{Manifest: manifestFor(tr)}); err != nil {
				t.Fatalf("ToVMA: %v", err)
			}
			h, got, err := readVMA(t, out.Bytes())
			if err != nil {
				t.Fatalf("2.2 reader rejects to-vma output: %v", err)
			}
			if len(h.Devices) != len(devs) {
				t.Fatalf("devices %+v", h.Devices)
			}
			for _, d := range devs {
				if !bytes.Equal(got[d.ID], content(d)) {
					t.Fatalf("device %d differs after to-vma", d.ID)
				}
			}
			if len(h.Configs) != 2 {
				t.Fatalf("configs %+v", h.Configs)
			}
			for i, c := range testConfigs() {
				if h.Configs[i].Name != c.Name || !bytes.Equal(h.Configs[i].Data, c.Data) {
					t.Fatalf("config %d not verbatim: %q", i, h.Configs[i].Data)
				}
			}
			if h.UUID == ([16]byte{}) || h.CTime == 0 {
				t.Fatal("to-vma must write a fresh uuid and the current time")
			}
		})
	}
}

// assertTruncatedVMA checks that b is a VMA stream (header intact) that the
// 2.2 reader rejects because clusters are missing.
func assertTruncatedVMA(t *testing.T, b []byte) {
	t.Helper()
	if len(b) < vma.HeaderFixedSize {
		return // not even a header: no restore tool accepts it
	}
	_, _, err := readVMA(t, b)
	if err == nil {
		t.Fatal("to-vma output is a complete VMA stream after a failure")
	}
	var fe *vma.FormatError
	if !errors.As(err, &fe) || (fe.Kind != vma.ErrMissingCluster && fe.Kind != vma.ErrTruncated) {
		t.Fatalf("output rejected for an unexpected reason: %v", err)
	}
}

func TestToVMAWithholdsFinalExtentOnMismatch(t *testing.T) {
	s, tr := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	bad := manifestFor(tr)
	bad.Devices[0].SHA256CT = strings.Repeat("f", 64)
	cases := map[string]struct {
		in    []byte
		m     *verify.Manifest
		class error
	}{
		"flipped data byte": {flipDataByte(t, s), nil, rvs.ErrIntegrity},
		"wrong manifest":    {s, bad, verify.ErrManifest},
		"truncated stream":  {s[:len(s)-300], nil, rvs.ErrIntegrity},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			var out bytes.Buffer
			_, err := ToVMA(bytes.NewReader(c.in), &out, Options{Manifest: c.m})
			wantClass(t, err, c.class)
			if out.Len() == 0 {
				t.Fatal("expected the partial VMA stream on the output")
			}
			assertTruncatedVMA(t, out.Bytes())
		})
	}
}

type failWriter struct{ after int }

func (w *failWriter) Write(p []byte) (int, error) {
	if w.after < len(p) {
		return 0, errors.New("pipe closed")
	}
	w.after -= len(p)
	return len(p), nil
}

func TestToVMAOutputErrorIsLocalIO(t *testing.T) {
	s, _ := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	for _, after := range []int{0, 1 << 20} {
		_, err := ToVMA(bytes.NewReader(s), &failWriter{after: after}, Options{})
		wantClass(t, err, ErrLocalIO)
	}
}

// --- to-raw ---

func readSums(t *testing.T, dir string) map[string]string {
	t.Helper()
	f, err := os.Open(filepath.Join(dir, SumsFile))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	out := map[string]string{}
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		sum, name, ok := strings.Cut(sc.Text(), "  ")
		if !ok || len(sum) != 64 || strings.Contains(name, "/") {
			t.Fatalf("SHA256SUMS line %q is not in sha256sum format", sc.Text())
		}
		out[name] = sum
	}
	return out
}

func TestToRawWritesDevicesAndSums(t *testing.T) {
	s, tr, devs := patchedStream(t)
	dir := filepath.Join(t.TempDir(), "out")
	res, err := ToRaw(bytes.NewReader(s), dir, Options{Manifest: manifestFor(tr)})
	if err != nil {
		t.Fatalf("ToRaw: %v", err)
	}
	if res.PatchedClusters == 0 {
		t.Fatal("fixture should exercise patches")
	}
	sums := readSums(t, dir)
	if len(sums) != len(devs) {
		t.Fatalf("SHA256SUMS lists %d files, want %d", len(sums), len(devs))
	}
	for _, d := range devs {
		name := d.Name + ".raw"
		got, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			t.Fatal(err)
		}
		want := content(d)
		if !bytes.Equal(got, want) {
			t.Fatalf("%s differs from the device content", name)
		}
		s := sha256.Sum256(want)
		if sums[name] != hex.EncodeToString(s[:]) {
			t.Fatalf("SHA256SUMS %s = %s, want plain sha256 %x", name, sums[name], s)
		}
	}
	ents, _ := os.ReadDir(dir)
	if len(ents) != len(devs)+1 {
		t.Fatalf("unexpected files in the output dir: %d entries", len(ents))
	}
}

func TestToRawFailureLeavesOnlyInvalidFiles(t *testing.T) {
	s, tr, devs := patchedStream(t)
	bad := manifestFor(tr)
	bad.Devices[2].SHA256CT = strings.Repeat("e", 64)
	cases := map[string]struct {
		in    []byte
		m     *verify.Manifest
		class error
	}{
		"flipped data byte": {flipDataByte(t, s), nil, rvs.ErrIntegrity},
		"wrong manifest":    {s, bad, verify.ErrManifest},
		"truncated stream":  {s[:len(s)/2], nil, rvs.ErrIntegrity},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			_, err := ToRaw(bytes.NewReader(c.in), dir, Options{Manifest: c.m})
			wantClass(t, err, c.class)
			ents, _ := os.ReadDir(dir)
			if len(ents) != len(devs) {
				t.Fatalf("want %d *.raw.invalid files, got %d entries", len(devs), len(ents))
			}
			for _, e := range ents {
				if !strings.HasSuffix(e.Name(), ".raw"+InvalidSuffix) {
					t.Fatalf("unexpected file %q after a failed to-raw", e.Name())
				}
			}
		})
	}
}

func TestToRawOutputDirErrorsAreLocalIO(t *testing.T) {
	s, _ := encode(t, testDevices(), nil, reorder.Caps{RAMBytes: 1 << 20})
	base := t.TempDir()
	file := filepath.Join(base, "plain-file")
	if err := os.WriteFile(file, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	existing := filepath.Join(base, "existing")
	if err := os.Mkdir(existing, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(existing, "drive-scsi0.raw"), []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	for name, dir := range map[string]string{
		"dir is a file":      file,
		"below a file":       filepath.Join(file, "sub"),
		"target file exists": existing,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ToRaw(bytes.NewReader(s), dir, Options{})
			wantClass(t, err, ErrLocalIO)
		})
	}
	if b, _ := os.ReadFile(filepath.Join(existing, "drive-scsi0.raw")); string(b) != "keep" {
		t.Fatal("an existing raw file was overwritten")
	}
}

func TestToRawRejectsUnsafeDeviceNames(t *testing.T) {
	devs := []vmatest.Device{{ID: 1, Name: "../escape", Size: vma.ClusterSize, Data: []byte{1}}}
	s, _ := encode(t, devs, nil, reorder.Caps{RAMBytes: 1 << 20})
	dir := filepath.Join(t.TempDir(), "out")
	_, err := ToRaw(bytes.NewReader(s), dir, Options{})
	wantClass(t, err, rvs.ErrIntegrity)
	if _, err := os.Stat(filepath.Join(filepath.Dir(dir), "escape.raw")); !os.IsNotExist(err) {
		t.Fatal("a file was written outside the output directory")
	}
}
