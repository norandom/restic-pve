package convert

import (
	"archive/tar"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// pdev is one disk of a pieces snapshot built in memory.
type pdev struct {
	id   uint8
	name string
	data []byte
}

// pdevs: ids not in name order (the tar has drive-efidisk0 first, the VMA keeps the ids), a
// disk with a short last piece and a cluster tail, zero runs that become holes.
func pdevs() []pdev {
	scsi := make([]byte, 2*pieces.PieceSize+70000)
	for i := range scsi {
		if i < pieces.PieceSize/2 || i > pieces.PieceSize+100 {
			scsi[i] = byte(i*7 + i/4099)
		}
	}
	efi := make([]byte, 128<<10)
	copy(efi, "EFI PART")
	return []pdev{{1, "drive-scsi0", scsi}, {2, "drive-efidisk0", efi}}
}

const plainConf = "name: web\nscsi0: local:vm-100-disk-0,size=8M\n"

var qmLines = []string{"#qmdump#map:efidisk0:drive-efidisk0:local:raw:", "#qmdump#map:scsi0:drive-scsi0:local:raw:"}

// manifestOf is the manifest of a full read of devs.
func manifestOf(t *testing.T, devs []pdev, cfgs []vma.ConfigBlob, qm []string) *pieces.Manifest {
	t.Helper()
	m := &pieces.Manifest{Schema: pieces.Schema, Layout: pieces.Layout, Producer: "restic-pve test", VMID: 100, Alias: "web",
		Created: time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC).Format(time.RFC3339), Kind: "full", PieceSize: pieces.PieceSize,
		QmdumpMap: qm, Consistency: pieces.Consistency{Value: "application"}}
	for _, c := range cfgs {
		m.Config = append(m.Config, pieces.Config{Name: c.Name, SHA256: pieces.PieceDigest(c.Data)})
	}
	for _, d := range devs {
		size := uint64(len(d.data))
		var dg []string
		for i := range pieces.Pieces(size) {
			dg = append(dg, pieces.PieceDigest(d.data[i*pieces.PieceSize:i*pieces.PieceSize+pieces.PieceLen(size, i)]))
		}
		pt, err := pieces.SHA256PT(size, dg)
		if err != nil {
			t.Fatal(err)
		}
		m.Devices = append(m.Devices, pieces.Device{ID: d.id, Name: d.name, Size: size, Pieces: pieces.Pieces(size), SHA256PT: pt, Digests: dg})
	}
	return m
}

// piecesTar writes the tar `restic dump --archive tar` gives for the snapshot: every file in
// pieces.Files order below prefix, each folder before its files.
func piecesTar(t *testing.T, devs []pdev, cfgs []vma.ConfigBlob, qm []string, prefix string) ([]byte, *pieces.Manifest) {
	t.Helper()
	m := manifestOf(t, devs, cfgs, qm)
	mb, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	content := map[string][]byte{pieces.ManifestName: mb}
	for _, c := range cfgs {
		content[pieces.ConfigPath(c.Name)] = c.Data
	}
	for _, d := range devs {
		size := uint64(len(d.data))
		for i := range pieces.Pieces(size) {
			content[pieces.PiecePath(d.name, i)] = d.data[i*pieces.PieceSize : i*pieces.PieceSize+pieces.PieceLen(size, i)]
		}
	}
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, rel := range pieces.Files(m) {
		name := prefix + rel
		for d := path.Dir(name); d != "." && d != "/" && !dirs[d]; d = path.Dir(d) {
			dirs[d] = true
		}
		if dir := path.Dir(rel); dir != "." && !dirs["w:"+dir] {
			dirs["w:"+dir] = true
			if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: prefix + dir + "/", Mode: 0o755}); err != nil {
				t.Fatal(err)
			}
		}
		b := content[rel]
		if err := tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: name, Mode: 0o644, Size: int64(len(b))}); err != nil {
			t.Fatal(err)
		}
		tw.Write(b)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes(), m
}

func confBlobs(conf string) []vma.ConfigBlob {
	return []vma.ConfigBlob{{Name: "qemu-server.conf", Data: []byte(conf)}, {Name: "qemu-server.fw", Data: []byte("[OPTIONS]\nenable: 0\n")}}
}

func TestPiecesVerify(t *testing.T) {
	for _, prefix := range []string{"", "/state/pieces/web/vm-100/", "state/pieces/web/vm-100/"} {
		b, _ := piecesTar(t, pdevs(), confBlobs(plainConf), qmLines, prefix)
		res, err := VerifyPieces(bytes.NewReader(b), Options{})
		if err != nil {
			t.Fatalf("prefix %q: %v", prefix, err)
		}
		if res.Bytes != uint64(len(pdevs()[0].data)+len(pdevs()[1].data)) || len(res.Header.Devices) != 2 || res.Header.Devices[0].ID != 1 {
			t.Fatalf("result %+v", res.Header)
		}
	}
}

func TestPiecesToVMAAddsTheMapLines(t *testing.T) {
	devs := pdevs()
	for _, c := range []struct {
		name, conf, want string
		qm               []string
	}{
		{"provider config without map lines", plainConf, strings.Join(qmLines, "\n") + "\n" + plainConf, qmLines},
		{"config with its own map lines", qmLines[1] + "\n" + plainConf, qmLines[1] + "\n" + plainConf, qmLines},
		{"no lines kept in the manifest", plainConf, plainConf, nil},
	} {
		b, _ := piecesTar(t, devs, confBlobs(c.conf), c.qm, "/state/pieces/web/vm-100/")
		var out bytes.Buffer
		if _, err := ToVMAPieces(bytes.NewReader(b), &out, Options{}); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		h, got, err := readVMA(t, out.Bytes())
		if err != nil {
			t.Fatalf("%s: read back: %v", c.name, err)
		}
		if len(h.Configs) != 2 || h.Configs[0].Name != "qemu-server.conf" || string(h.Configs[0].Data) != c.want ||
			string(h.Configs[1].Data) != "[OPTIONS]\nenable: 0\n" {
			t.Fatalf("%s: configs %q", c.name, h.Configs)
		}
		for _, d := range devs {
			if sha256.Sum256(got[d.id]) != sha256.Sum256(d.data) {
				t.Fatalf("%s: device %s differs after the round trip", c.name, d.name)
			}
		}
	}
}

func TestPiecesToRaw(t *testing.T) {
	b, _ := piecesTar(t, pdevs(), confBlobs(plainConf), nil, "")
	dir := t.TempDir()
	if _, err := ToRawPieces(bytes.NewReader(b), dir, Options{}); err != nil {
		t.Fatal(err)
	}
	sums := readSums(t, dir)
	for _, d := range pdevs() {
		got, err := os.ReadFile(filepath.Join(dir, d.name+RawSuffix))
		if err != nil || !bytes.Equal(got, d.data) {
			t.Fatalf("%s: %v, %d bytes", d.name, err, len(got))
		}
		s := sha256.Sum256(d.data)
		if sums[d.name+RawSuffix] != hex.EncodeToString(s[:]) {
			t.Fatalf("SHA256SUMS %v", sums)
		}
	}
	// a wrong piece leaves only invalid files
	bad := bytes.Clone(b)
	bad[bytes.Index(bad, []byte("EFI PART"))] ^= 1
	dir = t.TempDir()
	_, err := ToRawPieces(bytes.NewReader(bad), dir, Options{})
	wantClass(t, err, rvs.ErrIntegrity)
	if _, err := os.Stat(filepath.Join(dir, "drive-scsi0.raw"+InvalidSuffix)); err != nil {
		t.Fatalf("no invalid file: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, SumsFile)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SHA256SUMS written after a failure: %v", err)
	}
}

// rewrite builds a tar from the entries of b, changed by edit (nil: the entry is left out).
func rewrite(t *testing.T, b []byte, edit func(i int, h *tar.Header, data []byte) (*tar.Header, []byte)) []byte {
	t.Helper()
	tr := tar.NewReader(bytes.NewReader(b))
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	for i := 0; ; i++ {
		h, err := tr.Next()
		if err != nil {
			break
		}
		data := make([]byte, h.Size)
		if _, err := io.ReadFull(tr, data); err != nil {
			t.Fatal(err)
		}
		h2, d2 := edit(i, h, data)
		if h2 == nil {
			continue
		}
		h2.Size = int64(len(d2))
		tw.WriteHeader(h2)
		tw.Write(d2)
	}
	tw.Close()
	return out.Bytes()
}

func TestPiecesRejects(t *testing.T) {
	b, m := piecesTar(t, pdevs(), confBlobs(plainConf), qmLines, "")
	names := []string{}
	rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) { names = append(names, h.Name); return h, d })
	index := func(name string) int {
		for i, n := range names {
			if n == name {
				return i
			}
		}
		t.Fatalf("no entry %s in %q", name, names)
		return -1
	}
	piece1 := pieces.PiecePath("drive-scsi0", 1)
	cases := []struct {
		name  string
		in    []byte
		class error
		text  string
	}{
		{"cut inside a piece", b[:len(b)/2], rvs.ErrIntegrity, "pieces tar"},
		{"cut inside the manifest", b[:700], rvs.ErrIntegrity, "pieces tar"},
		{"cut between two pieces", rewrite(t, b, func(i int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if i > index(piece1) {
				return nil, nil
			}
			return h, d
		}), rvs.ErrIntegrity, "truncated"},
		{"wrong piece", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == piece1 {
				d[100] ^= 0x40
			}
			return h, d
		}), rvs.ErrIntegrity, "piece 1"},
		{"piece twice", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == piece1 {
				h.Name = pieces.PiecePath("drive-scsi0", 0)
			}
			return h, d
		}), rvs.ErrIntegrity, "should come"},
		{"piece left out", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == piece1 {
				return nil, nil
			}
			return h, d
		}), rvs.ErrIntegrity, "should come"},
		{"manifest not first", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == pieces.ManifestName {
				return nil, nil
			}
			return h, d
		}), rvs.ErrIntegrity, "not _manifest.json"},
		{"config changed", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == "config/qemu-server.conf" {
				return h, append(d, '#')
			}
			return h, d
		}), rvs.ErrIntegrity, "qemu-server.conf"},
		{"broken manifest", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == pieces.ManifestName {
				return h, bytes.Replace(d, []byte(`"schema":2`), []byte(`"schema":3`), 1)
			}
			return h, d
		}), pieces.ErrManifest, "schema"},
		{"a link among the pieces", rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) {
			if h.Name == piece1 {
				return &tar.Header{Typeflag: tar.TypeSymlink, Name: h.Name, Linkname: "00000000"}, nil
			}
			return h, d
		}), rvs.ErrIntegrity, "not a plain file"},
		{"not a tar", []byte("RVS1 is not a tar file at all, and it is long enough to fill a block? " + strings.Repeat("x", 600)), rvs.ErrIntegrity, "pieces tar"},
		{"empty input", nil, rvs.ErrIntegrity, "no manifest"},
	}
	// a file after the last piece
	extra := rewrite(t, b, func(_ int, h *tar.Header, d []byte) (*tar.Header, []byte) { return h, d })
	extra = extra[:len(extra)-1024] // without the end-of-archive blocks
	var more bytes.Buffer
	tw := tar.NewWriter(&more)
	tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: pieces.PiecePath("drive-scsi0", 99), Size: 1})
	tw.Write([]byte{1})
	tw.Close()
	extra = append(extra, more.Bytes()...)
	cases = append(cases, struct {
		name  string
		in    []byte
		class error
		text  string
	}{"extra file", extra, rvs.ErrIntegrity, "should come"})
	for _, c := range cases {
		_, err := VerifyPieces(bytes.NewReader(c.in), Options{})
		if err == nil || !errors.Is(err, c.class) || !strings.Contains(err.Error(), c.text) {
			t.Errorf("%s: %v, want %v with %q", c.name, err, c.class, c.text)
		}
	}
	// a manifest given to compare with (backupctl's copy) must name the same disks
	other := *m
	other.Devices = append([]pieces.Device(nil), m.Devices...)
	other.Devices[0].SHA256PT = strings.Repeat("0", 64)
	_, err := VerifyPieces(bytes.NewReader(b), Options{Pieces: &other})
	wantClass(t, err, pieces.ErrManifest)
	if _, err := VerifyPieces(bytes.NewReader(b), Options{Pieces: m}); err != nil {
		t.Fatalf("same manifest: %v", err)
	}
	// a read error of the input is local I/O, not a broken tar
	_, err = VerifyPieces(failReader{}, Options{})
	wantClass(t, err, ErrLocalIO)
}
