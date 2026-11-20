package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/testfix"
)

// piecesSnapshot stores two disks in the piece layout with the library code of a pieces run
// (writer, restic batches, checks, acceptance) and returns `restic dump --archive tar` of the
// accepted snapshot, the disks by device id and the guest config.
func piecesSnapshot(t *testing.T) ([]byte, map[uint8][]byte, []byte) {
	repo := testfix.NewRepo(t)
	ctx := context.Background()
	dir := t.TempDir()
	root := filepath.Join(dir, "web", "vm-100")
	r := &pieces.Restic{Bin: repo.Bin, Env: repo.Env(), Nice: []string{}}
	s := &pieces.Store{R: r, Root: root, Host: "bk",
		GuestTags:   []string{"source=pve", "instance=site", "unit=guest-web", "kind=guest-image", "vmid=100"},
		PartialTags: []string{pieces.PartialTag, "unit=guest-web", "vmid=100"}}
	scsi := make([]byte, 2*pieces.PieceSize+70000) // a short last piece with a cluster tail
	for i := range scsi {
		if i < pieces.PieceSize/2 || i > pieces.PieceSize+100 { // a zero run: holes in to-raw
			scsi[i] = byte(i*13 + i/4093)
		}
	}
	copy(scsi[pieces.PieceSize+4096:], "PIECE-ONE-MARKER")
	efi := make([]byte, 128<<10)
	copy(efi, "EFI PART")
	disks := []struct {
		id   uint8
		name string
		data []byte
	}{{1, "drive-scsi0", scsi}, {2, "drive-efidisk0", efi}}
	var sizes []pieces.DiskSize
	for _, d := range disks {
		sizes = append(sizes, pieces.DiskSize{Name: d.name, Size: uint64(len(d.data))})
	}
	runs := 0
	w, err := pieces.NewWriter(pieces.WriterOptions{Root: root, Staging: filepath.Join(dir, "web", ".staging"), Disks: sizes,
		BatchBytes: pieces.PieceSize, Batch: func(ctx context.Context, b *pieces.Batch) error { // a batch per piece: the final snapshot is built over partials
			runs++
			return s.RunBatch(ctx, b)
		}})
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range disks {
		size := uint64(len(d.data))
		for i := range pieces.Pieces(size) {
			if err := w.Put(ctx, d.name, i, d.data[i*pieces.PieceSize:i*pieces.PieceSize+pieces.PieceLen(size, i)]); err != nil {
				t.Fatal(err)
			}
		}
	}
	conf := []byte("name: web\nscsi0: local:vm-100-disk-0,size=8M\n") // a provider config: no map lines
	m := &pieces.Manifest{Schema: pieces.Schema, Layout: pieces.Layout, Producer: "restic-pve test", VMID: 100, Alias: "web",
		Created: time.Now().UTC().Format(time.RFC3339), Kind: "full", PieceSize: pieces.PieceSize,
		Config:      []pieces.Config{{Name: "qemu-server.conf", SHA256: pieces.PieceDigest(conf)}},
		QmdumpMap:   []string{"#qmdump#map:efidisk0:drive-efidisk0:local:raw:", "#qmdump#map:scsi0:drive-scsi0:local:raw:"},
		Consistency: pieces.Consistency{Value: "application"}}
	for _, d := range disks {
		dg, err := w.Digests(d.name)
		if err != nil {
			t.Fatal(err)
		}
		pt, _ := pieces.SHA256PT(uint64(len(d.data)), dg)
		m.Devices = append(m.Devices, pieces.Device{ID: d.id, Name: d.name, Size: uint64(len(d.data)),
			Pieces: pieces.Pieces(uint64(len(d.data))), SHA256PT: pt, Digests: dg})
	}
	mb, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Close(ctx, []pieces.File{{Rel: pieces.ManifestName, Data: mb}, {Rel: pieces.ConfigPath("qemu-server.conf"), Data: conf}}); err != nil {
		t.Fatal(err)
	}
	id, err := s.Accept(ctx, w.Stats().Kept, nil)
	if err != nil {
		t.Fatal(err)
	}
	s.Discard(ctx)
	if runs < 2 {
		t.Fatalf("%d batches, want several", runs)
	}
	tarb, err := repo.Run("dump", "--archive", "tar", id, root)
	if err != nil {
		t.Fatal(err)
	}
	return tarb, map[uint8][]byte{1: scsi, 2: efi}, conf
}

func TestPiecesSnapshotFromRestic(t *testing.T) {
	tarb, disks, conf := piecesSnapshot(t)

	runWith(t, tarb, "verify", "--pieces").expect(t, exitOK)

	r := runWith(t, tarb, "to-vma", "--pieces")
	r.expect(t, exitOK)
	h, got, err := decodeVMA(r.stdout)
	if err != nil {
		t.Fatalf("to-vma output: %v", err)
	}
	if len(h.Devices) != 2 {
		t.Fatalf("devices %+v", h.Devices)
	}
	for id, want := range disks {
		if sha256.Sum256(got[id]) != sha256.Sum256(want) {
			t.Fatalf("device %d: digest differs from the image", id)
		}
	}
	wantConf := "#qmdump#map:efidisk0:drive-efidisk0:local:raw:\n#qmdump#map:scsi0:drive-scsi0:local:raw:\n" + string(conf)
	if len(h.Configs) != 1 || string(h.Configs[0].Data) != wantConf {
		t.Fatalf("config %q", h.Configs)
	}

	out := t.TempDir()
	runWith(t, tarb, "to-raw", "--pieces", "--dir", out).expect(t, exitOK)
	for id, name := range map[uint8]string{1: "drive-scsi0", 2: "drive-efidisk0"} {
		if b, err := os.ReadFile(filepath.Join(out, name+".raw")); err != nil || !bytes.Equal(b, disks[id]) {
			t.Fatalf("%s.raw: %v", name, err)
		}
	}

	// a truncated tar and a wrong piece fail with exit 3, as an RVS1 stream does
	for _, n := range []int{len(tarb) / 2, len(tarb) - 1024 - 80000, 600} {
		if res := runWith(t, tarb[:n], "verify", "--pieces").expect(t, exitIntegrity); !strings.Contains(res.Message, "pieces tar") {
			t.Fatalf("cut at %d: %q", n, res.Message)
		}
	}
	bad := bytes.Clone(tarb)
	i := bytes.Index(bad, []byte("PIECE-ONE-MARKER"))
	if i < 0 {
		t.Fatal("marker not in the tar")
	}
	bad[i] ^= 1
	for _, args := range [][]string{{"verify", "--pieces"}, {"to-vma", "--pieces"}, {"to-raw", "--pieces", "--dir", t.TempDir()}} {
		if res := runWith(t, bad, args...).expect(t, exitIntegrity); !strings.Contains(res.Message, "drive-scsi0 piece 1") {
			t.Fatalf("%s: %q", args[0], res.Message)
		}
	}
	// the manifest copy the control code keeps must match the one inside the snapshot
	_, m := tarManifest(t, tarb)
	good := writeTemp(t, "m.json", mustEncode(t, m))
	runWith(t, tarb, "verify", "--pieces", "--manifest", good).expect(t, exitOK)
	m.Devices[1].Digests[0] = strings.Repeat("0", 64)
	m.Devices[1].SHA256PT, _ = pieces.SHA256PT(m.Devices[1].Size, m.Devices[1].Digests)
	runWith(t, tarb, "verify", "--pieces", "--manifest", writeTemp(t, "m2.json", mustEncode(t, m))).expect(t, exitManifestMismatch)
	// an RVS1 manifest is not a pieces manifest
	runWith(t, tarb, "verify", "--pieces", "--manifest", writeTemp(t, "v1.json", []byte(`{"schema":1}`))).expect(t, exitManifestMismatch)
	// RVS1 input given to --pieces is not a tar
	runWith(t, backupGuest(t).rvs, "verify", "--pieces").expect(t, exitIntegrity)
}

// tarManifest returns the manifest that the tar carries first.
func tarManifest(t *testing.T, tarb []byte) ([]byte, *pieces.Manifest) {
	t.Helper()
	i := bytes.Index(tarb, []byte(`{"alias"`))
	j := bytes.IndexByte(tarb[i:], 0)
	b := bytes.TrimSpace(tarb[i : i+j])
	m, err := pieces.ReadManifest(bytes.NewReader(b))
	if err != nil {
		t.Fatal(err)
	}
	return b, m
}

func mustEncode(t *testing.T, m *pieces.Manifest) []byte {
	t.Helper()
	b, err := m.Encode()
	if err != nil {
		t.Fatal(err)
	}
	return b
}
