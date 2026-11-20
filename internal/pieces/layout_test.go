package pieces

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/testfix"
)

func TestPiecePath(t *testing.T) {
	for _, c := range []struct {
		i    uint64
		want string
	}{
		{0, "disks/drive-scsi0/0000/00000000"},
		{255, "disks/drive-scsi0/0000/00000255"},
		{256, "disks/drive-scsi0/0001/00000256"},
		{31999, "disks/drive-scsi0/0124/00031999"},
		{MaxPieces - 1, "disks/drive-scsi0/9999/02559999"},
	} {
		if got := PiecePath("drive-scsi0", c.i); got != c.want {
			t.Errorf("PiecePath(%d) = %s, want %s", c.i, got, c.want)
		}
	}
	if Pieces(125<<30) != 32000 || PieceLen(PieceSize*2+10, 2) != 10 || PieceLen(PieceSize*2+10, 1) != PieceSize {
		t.Fatal("Pieces or PieceLen wrong")
	}
}

func TestLess(t *testing.T) {
	ordered := []string{
		"_manifest.json", // after upper case and digits, before lower case: first in the root
		"config/qemu-server.conf",
		"config/qemu-server.fw",
		"disks/drive-A/0000/00000000",
		"disks/drive-a/0000/00000000",
		"disks/drive-a/0001/00000256",
		"disks/drive-a-x/0000/00000000", // plain string order would put this before drive-a/
		"disks/drive-a.b/0000/00000000",
	}
	for i := range ordered {
		for j := range ordered {
			if got := Less(ordered[i], ordered[j]); got != (i < j) {
				t.Errorf("Less(%q, %q) = %v", ordered[i], ordered[j], got)
			}
		}
	}
	if !Less("disks", "disks/x") || Less("disks/x", "disks") {
		t.Error("a parent must come before its children")
	}
}

func TestValidNames(t *testing.T) {
	for _, n := range []string{"drive-scsi0", "drive-efidisk0", "drive-tpmstate0", "A_b.c-1"} {
		if !ValidDeviceName(n) {
			t.Errorf("%q refused", n)
		}
	}
	for _, n := range []string{"", ".hidden", "..", "a/b", "a b", "ä", strings.Repeat("x", 65)} {
		if ValidDeviceName(n) {
			t.Errorf("%q accepted", n)
		}
	}
}

func randomImage(t *testing.T, r *rand.Rand, size int) []byte {
	t.Helper()
	b := make([]byte, size)
	r.Read(b)
	return b
}

func ptOf(t *testing.T, img []byte) string {
	t.Helper()
	p := NewPT(uint64(len(img)))
	for off := 0; off < len(img); off += PieceSize {
		if err := p.AddData(img[off:min(off+PieceSize, len(img))]); err != nil {
			t.Fatal(err)
		}
	}
	s, err := p.Sum()
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func digestsOf(img []byte) []string {
	var out []string
	for off := 0; off < len(img); off += PieceSize {
		out = append(out, PieceDigest(img[off:min(off+PieceSize, len(img))]))
	}
	return out
}

// A full read and a changed-blocks assembly (parent digests for clean pieces, fresh bytes for
// the dirty ones) of the same content give the same sha256pt.
func TestSHA256PTFullEqualsChanges(t *testing.T) {
	r := rand.New(rand.NewSource(1))
	size := 6*PieceSize + 12345
	parent := randomImage(t, r, size)
	parentDigests := digestsOf(parent)

	now := append([]byte(nil), parent...)
	dirty := map[int]bool{1: true, 4: true, 6: true} // 6 is the short tail
	for i := range dirty {
		now[i*PieceSize+7] ^= 0xff
	}

	assembled := NewPT(uint64(size))
	for i := range parentDigests {
		if dirty[i] {
			if err := assembled.AddData(now[i*PieceSize : min((i+1)*PieceSize, size)]); err != nil {
				t.Fatal(err)
			}
		} else if err := assembled.Add(parentDigests[i]); err != nil {
			t.Fatal(err)
		}
	}
	got, err := assembled.Sum()
	if err != nil {
		t.Fatal(err)
	}
	if full := ptOf(t, now); got != full {
		t.Fatalf("changes assembly %s, full read %s", got, full)
	}
	if got == ptOf(t, parent) {
		t.Fatal("sha256pt did not change with the content")
	}
}

// The fingerprint detects a single changed byte in any piece, also the short last one.
func TestSHA256PTDetectsOneByte(t *testing.T) {
	r := rand.New(rand.NewSource(2))
	size := 4*PieceSize + 4097
	img := randomImage(t, r, size)
	base := ptOf(t, img)
	for i := 0; i <= size/PieceSize; i++ {
		pos := i*PieceSize + r.Intn(int(PieceLen(uint64(size), uint64(i))))
		img[pos] ^= 1
		if ptOf(t, img) == base {
			t.Fatalf("one byte changed in piece %d (offset %d), same sha256pt", i, pos)
		}
		img[pos] ^= 1
	}
	if ptOf(t, img) != base {
		t.Fatal("restored image gives another sha256pt")
	}
}

func TestSHA256PTBindsSizeAndCount(t *testing.T) {
	d := PieceDigest(make([]byte, 10))
	a, _ := SHA256PT(10, []string{d})
	b, _ := SHA256PT(11, []string{d}) // same digest list, other size
	if a == b {
		t.Fatal("size not bound")
	}
	if _, err := SHA256PT(PieceSize+1, []string{d}); err == nil {
		t.Fatal("one digest for two pieces accepted")
	}
	if _, err := SHA256PT(10, []string{d, d}); err == nil {
		t.Fatal("two digests for one piece accepted")
	}
	if err := NewPT(10).AddData(make([]byte, 9)); err == nil {
		t.Fatal("short piece accepted")
	}
	if err := NewPT(10).Add(strings.ToUpper(d)); err == nil {
		t.Fatal("upper-case digest accepted")
	}
	// the definition, written out
	h := sha256.New()
	h.Write([]byte("RVS2-PT\x00"))
	h.Write([]byte{10, 0, 0, 0, 0, 0, 0, 0})
	h.Write([]byte{0, 0, 0x40, 0, 0, 0, 0, 0})
	raw, _ := hex.DecodeString(d)
	h.Write(raw)
	if want := hex.EncodeToString(h.Sum(nil)); a != want {
		t.Fatalf("sha256pt %s, definition gives %s", a, want)
	}
}

var zeroDigests = map[uint64]string{}

// testManifest builds a valid manifest whose disks hold zeros.
func testManifest(t testing.TB, sizes map[string]uint64) *Manifest {
	t.Helper()
	names := make([]string, 0, len(sizes))
	for n := range sizes {
		names = append(names, n)
	}
	sort.Strings(names)
	parent := strings.Repeat("ab", 32)
	m := &Manifest{
		Schema: Schema, Layout: Layout, Producer: "restic-pve test", VMID: 100, Alias: "web",
		Created: "2026-11-20T10:00:00Z", Kind: "changes", Parent: &parent, PieceSize: PieceSize,
		Config:      []Config{{Name: "qemu-server.fw", SHA256: PieceDigest(nil)}, {Name: "qemu-server.conf", SHA256: PieceDigest([]byte("x"))}},
		QmdumpMap:   []string{"#qmdump#map:scsi0:drive-scsi0:local:raw:"},
		Consistency: Consistency{Value: "application", Detail: ""},
		Record:      &Record{Job: strings.Repeat("0f", 16)},
	}
	for i, n := range names {
		size := sizes[n]
		d := Device{ID: uint8(i + 1), Name: n, Size: size, Pieces: Pieces(size)}
		for p := uint64(0); p < d.Pieces; p++ {
			l := PieceLen(size, p)
			z, ok := zeroDigests[l]
			if !ok {
				z = PieceDigest(make([]byte, l))
				zeroDigests[l] = z
			}
			d.Digests = append(d.Digests, z)
		}
		var err error
		if d.SHA256PT, err = SHA256PT(size, d.Digests); err != nil {
			t.Fatal(err)
		}
		m.Devices = append(m.Devices, d)
		m.Record.Disks = append(m.Record.Disks, RecordDisk{Name: n, State: "reuse", DirtyBytes: min(PieceSize, size)})
	}
	return m
}

func TestFilesOrder(t *testing.T) {
	m := testManifest(t, map[string]uint64{"drive-scsi0": 300 * PieceSize, "drive-scsi0-x": 2*PieceSize + 1, "drive-efidisk0": 528 << 10})
	files := Files(m)
	if len(files) != 1+2+300+3+1 {
		t.Fatalf("%d files", len(files))
	}
	if files[0] != ManifestName || files[1] != "config/qemu-server.conf" || files[2] != "config/qemu-server.fw" {
		t.Fatalf("head %v", files[:3])
	}
	if !sort.SliceIsSorted(files, func(i, j int) bool { return Less(files[i], files[j]) }) {
		t.Fatal("Files is not in restic's name order")
	}
	for i := 1; i < len(files); i++ {
		if files[i] == files[i-1] {
			t.Fatalf("%s twice", files[i])
		}
	}
}

func TestManifestRoundTrip(t *testing.T) {
	m := testManifest(t, map[string]uint64{"drive-scsi0": 5*PieceSize + 3, "drive-efidisk0": 1 << 20})
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReadManifest(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	b2, err := got.Encode()
	if err != nil || !bytes.Equal(b, b2) {
		t.Fatalf("round trip differs: %v", err)
	}
	full := testManifest(t, map[string]uint64{"drive-scsi0": PieceSize})
	full.Kind, full.Parent, full.Record = "full", nil, nil
	if _, err := full.Encode(); err != nil {
		t.Fatalf("full read without parent and record refused: %v", err)
	}
}

func mustEncode(t *testing.T, m *Manifest) []byte {
	t.Helper()
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func TestManifestRejects(t *testing.T) {
	sizes := map[string]uint64{"drive-scsi0": 3*PieceSize + 5, "drive-virtio1": 2 * PieceSize}
	good := mustEncode(t, testManifest(t, sizes))

	// every truncation is rejected
	for n := 0; n < len(good); n += max(1, len(good)/97) {
		if _, err := ReadManifest(bytes.NewReader(good[:n])); !errors.Is(err, ErrManifest) {
			t.Fatalf("truncated to %d bytes: %v", n, err)
		}
	}
	// altered text: each replacement must be rejected
	d0 := testManifest(t, sizes).Devices[0]
	altered := map[string][2]string{
		"one digest char":  {`"` + d0.Digests[1] + `"`, `"` + d0.Digests[1][:63] + "0" + `"`},
		"sha256pt":         {d0.SHA256PT, strings.Repeat("0", 64)},
		"unknown field":    {`"schema":2`, `"schema":2,"extra":1`},
		"schema":           {`"schema":2`, `"schema":1`},
		"piece size":       {`"piece_size":4194304`, `"piece_size":1048576`},
		"size":             {`"size":12582917`, `"size":12582918`},
		"pieces count":     {`"pieces":4`, `"pieces":5`},
		"kind":             {`"kind":"changes"`, `"kind":"delta"`},
		"record state":     {`"state":"reuse"`, `"state":"new"`},
		"device name":      {`"name":"drive-scsi0"`, `"name":"../x"`},
		"producer control": {`"producer":"restic-pve test"`, `"producer":"a\u0007b"`},
	}
	for name, r := range altered {
		s := string(good)
		if !strings.Contains(s, r[0]) {
			t.Fatalf("%s: %q not in the manifest", name, r[0])
		}
		if _, err := ReadManifest(strings.NewReader(strings.Replace(s, r[0], r[1], 1))); !errors.Is(err, ErrManifest) {
			t.Errorf("%s: accepted (%v)", name, err)
		}
	}
	if _, err := ReadManifest(bytes.NewReader(append(append([]byte(nil), good...), []byte(" {}")...))); !errors.Is(err, ErrManifest) {
		t.Error("data after the object accepted")
	}

	// reordered: two digests swapped, devices swapped
	m := testManifest(t, sizes)
	m.Devices[0].Digests[0], m.Devices[0].Digests[3] = m.Devices[0].Digests[3], m.Devices[0].Digests[0]
	if _, err := m.Encode(); !errors.Is(err, ErrManifest) {
		t.Error("swapped digests accepted")
	}
	m = testManifest(t, sizes)
	m.Devices[0], m.Devices[1] = m.Devices[1], m.Devices[0]
	if _, err := m.Encode(); !errors.Is(err, ErrManifest) {
		t.Error("devices out of id order accepted")
	}
	m = testManifest(t, sizes)
	m.Parent = nil
	if _, err := m.Encode(); !errors.Is(err, ErrManifest) {
		t.Error("changes without parent accepted")
	}
	m = testManifest(t, sizes)
	m.Record = nil
	if _, err := m.Encode(); !errors.Is(err, ErrManifest) {
		t.Error("changes without record accepted")
	}
	m = testManifest(t, sizes)
	m.Config = m.Config[:1] // only the firewall file
	if _, err := m.Encode(); !errors.Is(err, ErrManifest) {
		t.Error("config without qemu-server.conf accepted")
	}

	// bounded input
	if _, err := ReadManifest(io.MultiReader(strings.NewReader(`{"schema":2,"producer":"`), &endless{}, strings.NewReader(`"}`))); !errors.Is(err, ErrManifest) {
		t.Errorf("oversized manifest: %v", err)
	}
}

// endless yields 'x' forever; ReadManifest must stop at its bound.
type endless struct{ n int64 }

func (e *endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	e.n += int64(len(p))
	return len(p), nil
}

// TestFilesEqualResticTarOrder stores a tree of the layout with the pinned restic and checks
// that `restic dump --archive tar` returns its files in exactly the order of Files.
func TestFilesEqualResticTarOrder(t *testing.T) {
	repo := testfix.NewRepo(t)
	m := testManifest(t, map[string]uint64{
		"drive-scsi0":    257*PieceSize + 1, // crosses a group boundary, short tail
		"drive-scsi0-x":  PieceSize,         // a name that plain string order puts first
		"drive-efidisk0": 528 << 10,
		"drive-B":        PieceSize,
	})
	root := filepath.Join(t.TempDir(), "vm-100")
	want := Files(m)
	for _, f := range want {
		p := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		// the content does not matter for the order; keep the files small
		if err := os.WriteFile(p, []byte(f), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if out, err := repo.Run("backup", "--quiet", "--host", "test", root); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	cmd := repo.Cmd("dump", "--archive", "tar", "latest", root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	prefix := strings.TrimPrefix(root, "/") + "/"
	var got []string
	tr := tar.NewReader(stdout)
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if h.Typeflag != tar.TypeReg {
			continue
		}
		if !strings.HasPrefix(h.Name, prefix) {
			t.Fatalf("entry %q outside %q", h.Name, prefix)
		}
		got = append(got, strings.TrimPrefix(h.Name, prefix))
	}
	if err := cmd.Wait(); err != nil {
		t.Fatalf("restic dump: %v: %s", err, stderr.String())
	}
	if len(got) != len(want) {
		t.Fatalf("tar has %d files, layout %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("entry %d: tar %s, layout %s", i, got[i], want[i])
		}
	}
}
