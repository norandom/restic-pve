package pieces

import (
	"bytes"
	"context"
	"errors"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/testfix"
)

// harness runs whole pieces runs (writer, restic batches, checks, acceptance) against a
// throwaway repository.
type harness struct {
	t                *testing.T
	repo             *testfix.Repo
	r                *Restic
	root, staging    string
	guest, partial   []string
	batch            uint64
	size             uint64
	conf             []byte
	acceptedManifest *Manifest
}

const hostName = "bk"

func newHarness(t *testing.T, size uint64) *harness {
	repo := testfix.NewRepo(t)
	dir := t.TempDir()
	return &harness{t: t, repo: repo, r: &Restic{Bin: repo.Bin, Env: repo.Env()},
		root: filepath.Join(dir, "web", "vm-100"), staging: filepath.Join(dir, "web", ".staging"),
		guest:   []string{"source=pve", "instance=site", "unit=guest-web", "kind=guest-image", "vmid=100"},
		partial: []string{PartialTag, "unit=guest-web", "vmid=100"},
		batch:   1 << 30, size: size, conf: []byte("name: web\n")}
}

// inject is called with the batch and the store just before restic runs the batch.
type inject func(b *Batch, s *Store)

// run stores img through the writer: parent nil is a full read without parent; put lists the
// pieces that arrive (nil: all of them). It returns the accepted id or the rejection, after
// the run's snapshots were forgotten.
func (h *harness) run(parent *Parent, img []byte, put []uint64, in inject) (string, *Store, error) {
	h.t.Helper()
	ctx := context.Background()
	s := &Store{R: h.r, Root: h.root, Host: h.hostname(), GuestTags: h.guest, PartialTags: h.partial}
	kind := "full"
	var parentID *string
	if parent != nil {
		s.Original = parent.ID
		kind = "changes"
		parentID = &parent.ID
		if _, err := parent.Listing.Rebuild(h.root, PiecesOnly); err != nil {
			h.t.Fatal(err)
		}
	}
	w, err := NewWriter(WriterOptions{Root: h.root, Staging: h.staging, Disks: []DiskSize{{"drive-scsi0", h.size}}, Parent: parent,
		BatchBytes: h.batch, Batch: func(ctx context.Context, b *Batch) error {
			if in != nil {
				in(b, s)
			}
			return s.RunBatch(ctx, b)
		}})
	if err != nil {
		h.t.Fatal(err)
	}
	if put == nil {
		for i := range Pieces(h.size) {
			put = append(put, i)
		}
	}
	for _, i := range put {
		off := i * PieceSize
		if err := w.Put(ctx, "drive-scsi0", i, img[off:off+PieceLen(h.size, i)]); err != nil {
			break // the writer's error comes back from Close
		}
	}
	m := &Manifest{Schema: Schema, Layout: Layout, Producer: "restic-pve test", VMID: 100, Alias: "web",
		Created: time.Now().UTC().Format(time.RFC3339), Kind: kind, Parent: parentID, PieceSize: PieceSize,
		Config:      []Config{{Name: "qemu-server.conf", SHA256: PieceDigest(h.conf)}},
		Consistency: Consistency{Value: "application"}}
	if kind == "changes" {
		m.Record = &Record{Job: strings.Repeat("1", 32), Disks: []RecordDisk{{Name: "drive-scsi0", State: "reuse"}}}
	}
	var files []File
	if dg, err := w.Digests("drive-scsi0"); err == nil {
		pt, _ := SHA256PT(h.size, dg)
		m.Devices = []Device{{ID: 1, Name: "drive-scsi0", Size: h.size, Pieces: Pieces(h.size), SHA256PT: pt, Digests: dg}}
		mb, err := m.Encode()
		if err != nil {
			h.t.Fatal(err)
		}
		files = []File{{Rel: ManifestName, Data: mb}, {Rel: ConfigPath("qemu-server.conf"), Data: h.conf}}
	}
	if err := w.Close(ctx, files); err != nil {
		s.Discard(ctx)
		return "", s, err
	}
	resized := map[string]bool{}
	if parent != nil {
		if d, ok := parent.Device("drive-scsi0"); !ok || d.Size != h.size {
			resized["drive-scsi0"] = true
		}
	}
	id, err := s.Accept(ctx, w.Stats().Kept, resized)
	if err != nil {
		s.Discard(ctx)
		return "", s, err
	}
	h.acceptedManifest = m
	return id, s, nil
}

func (h *harness) hostname() string { return hostName }

func (h *harness) parent(id string) *Parent {
	h.t.Helper()
	p, err := LoadParent(context.Background(), h.r, id, ParentWant{Root: h.root, Host: hostName, Tags: h.guest, VMID: 100, Alias: "web", Base: id})
	if err != nil {
		h.t.Fatal(err)
	}
	return p
}

func (h *harness) snapshots() []Snapshot {
	h.t.Helper()
	s, err := h.r.Snapshots(context.Background(), hostName, nil)
	if err != nil {
		h.t.Fatal(err)
	}
	return s
}

func (h *harness) dump(id string, rel string) []byte {
	h.t.Helper()
	b, err := h.r.DumpFile(context.Background(), id, h.root+"/"+rel, 8<<20)
	if err != nil {
		h.t.Fatal(err)
	}
	return b
}

// restored returns the image of snapshot id from its pieces.
func (h *harness) restored(id string) []byte {
	var img []byte
	for i := range Pieces(h.size) {
		img = append(img, h.dump(id, PiecePath("drive-scsi0", i))...)
	}
	return img
}

func (h *harness) wantCheck(err error, check string) {
	h.t.Helper()
	var ce *CheckError
	if !errors.As(err, &ce) || ce.Check != check {
		h.t.Fatalf("rejection %v, want check %s", err, check)
	}
}

func TestStoreAcceptsAndRejectsB3(t *testing.T) {
	const size = 5*PieceSize + 4096 // six pieces, a short tail
	h := newHarness(t, size)
	rng := rand.New(rand.NewSource(1))
	img := make([]byte, size)
	rng.Read(img)
	ctx := context.Background()

	// a first run: no parent, three batches of two pieces, all pieces real
	h.batch = 2 * PieceSize
	sa, s, err := h.run(nil, img, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Runs() != 3 {
		t.Fatalf("first run: %d batches, want 3", s.Runs())
	}
	snaps := h.snapshots()
	if len(snaps) != 1 || snaps[0].ID != sa {
		t.Fatalf("after the first run: %d snapshots (partials left?)", len(snaps))
	}
	got := slices.Clone(snaps[0].Tags)
	sort.Strings(got)
	want := slices.Clone(h.guest)
	sort.Strings(want)
	if !slices.Equal(got, want) {
		t.Fatalf("accepted tags %q, want the guest tags %q", got, want)
	}
	if !bytes.Equal(h.restored(sa), img) {
		t.Fatal("first run does not restore to the image")
	}

	// a changed-blocks run: pieces 1 and 5 changed, piece 0 sent but equal
	img2 := bytes.Clone(img)
	rng.Read(img2[PieceSize : PieceSize+100])
	rng.Read(img2[5*PieceSize:])
	h.batch = 1 << 30
	sb, _, err := h.run(h.parent(sa), img2, []uint64{0, 1, 5}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(h.restored(sb), img2) {
		t.Fatal("changed-blocks run does not restore to the changed image: a placeholder was stored")
	}
	if len(h.snapshots()) != 2 {
		t.Fatalf("%d snapshots after the changed-blocks run", len(h.snapshots()))
	}
	m, err := ReadManifest(bytes.NewReader(h.dump(sb, ManifestName)))
	if err != nil || *m.Parent != sa {
		t.Fatalf("manifest of the changed-blocks run: %v", err)
	}
	full := NewPT(size)
	for i := range Pieces(size) {
		full.AddData(img2[i*PieceSize : i*PieceSize+PieceLen(size, i)])
	}
	if pt, _ := full.Sum(); pt != m.Devices[0].SHA256PT {
		t.Fatal("sha256pt of the changed-blocks run differs from a full read of the same content")
	}

	pb := h.parent(sb)
	before := len(h.snapshots())
	img3 := bytes.Clone(img2)
	rng.Read(img3[4*PieceSize : 4*PieceSize+10]) // the run reads piece 4
	c3 := func(s *Store) error {                 // C3 on the rejected batch snapshot, before it is forgotten
		ch, err := h.r.Diff(ctx, sb, s.Batches()[len(s.Batches())-1], h.root)
		if err != nil {
			t.Fatal(err)
		}
		return CheckC3(ch, map[string]bool{PiecePath("drive-scsi0", 4): true}, nil)
	}
	type tc struct {
		name, check string
		in          inject
		c3          bool // C3 also catches it on the batch snapshot
	}
	cases := []tc{
		{"force", "C1", func(b *Batch, s *Store) { s.force = true }, true},
		{"wrong parent", "C1", func(b *Batch, s *Store) { s.prev = sa }, true},
		{"touched placeholder", "C1", func(b *Batch, s *Store) {
			p := filepath.Join(h.root, PiecePath("drive-scsi0", 2))
			os.Chtimes(p, time.Now(), time.Now())
		}, true},
		{"same mtime (writer bug)", "C1", func(b *Batch, s *Store) {
			e, _ := pb.Listing.File(PiecePath("drive-scsi0", 4))
			os.Chtimes(filepath.Join(h.root, PiecePath("drive-scsi0", 4)), e.ATime, e.MTime)
		}, false},
		{"missing placeholder", "C1", func(b *Batch, s *Store) {
			os.Remove(filepath.Join(h.root, PiecePath("drive-scsi0", 3)))
		}, true},
		{"parent that does not exist", "C2", func(b *Batch, s *Store) { s.prev = strings.Repeat("0", 64) }, false},
	}
	for _, c := range cases {
		_, _, err := h.run(pb, img3, []uint64{4}, c.in)
		h.wantCheck(err, c.check)
		if n := len(h.snapshots()); n != before {
			t.Fatalf("%s: %d snapshots after the rejection, want %d (the run's own forgotten)", c.name, n, before)
		}
		t.Logf("%s: rejected: %v", c.name, err)
	}
	// the same run without an injection is accepted: the cases above failed for their cause
	if _, _, err := h.run(pb, img3, []uint64{4}, nil); err != nil {
		t.Fatalf("control run: %v", err)
	}
	before = len(h.snapshots())
	// C3 on its own: the same injections, checked by the tree diff before the snapshot is
	// forgotten (C1 and C2 are left out by calling the batch run without its checks)
	for _, c := range cases {
		if c.name == "parent that does not exist" {
			continue
		}
		var err3 error
		h.run(pb, img3, []uint64{4}, func(b *Batch, s *Store) {
			c.in(b, s)
			s.skipChecks = true
			s.after = func() { err3 = c3(s) }
		})
		if c.c3 && err3 == nil {
			t.Errorf("%s: C3 did not catch it", c.name)
		}
		if !c.c3 && err3 != nil {
			t.Errorf("%s: C3 caught it (%v); research B3 says only C1 does", c.name, err3)
		}
		t.Logf("%s: C3 alone: %v", c.name, err3)
	}
	if n := len(h.snapshots()); n != before {
		t.Fatalf("%d snapshots after the C3 runs, want %d", n, before)
	}

	// a lost pack: the index is repaired, restic reads the placeholders whose blobs are gone
	packs, _ := filepath.Glob(filepath.Join(h.repo.Dir, "data", "*", "*"))
	sort.Slice(packs, func(i, j int) bool { return fileSize(packs[i]) > fileSize(packs[j]) })
	os.Remove(packs[0])
	if out, err := h.repo.Run("repair", "index"); err != nil {
		t.Fatalf("repair index: %v %s", err, out)
	}
	_, _, err = h.run(pb, img3, []uint64{4}, nil)
	var ce *CheckError
	if !errors.As(err, &ce) {
		t.Fatalf("lost pack: %v, want a rejection", err)
	}
	t.Logf("lost pack: rejected: %v", err)
	if n := len(h.snapshots()); n != before {
		t.Fatalf("lost pack: %d snapshots after the rejection, want %d", n, before)
	}
}

func fileSize(p string) int64 {
	fi, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return fi.Size()
}

// TestCheckC3Rules covers the tree diff rules on recorded restic lines.
func TestCheckC3Rules(t *testing.T) {
	kept := map[string]bool{PiecePath("drive-scsi0", 4): true}
	p := func(mod, rel string) DiffChange { return DiffChange{Rel: rel, Modifier: mod} }
	for _, c := range []struct {
		name    string
		changes []DiffChange
		resized map[string]bool
		ok      bool
	}{
		{"kept piece and the manifest", []DiffChange{p("M", PiecePath("drive-scsi0", 4)), p("M", ManifestName), p("M", ConfigPath("qemu-server.conf"))}, nil, true},
		{"a piece not kept", []DiffChange{p("M", PiecePath("drive-scsi0", 3))}, nil, false},
		{"a piece removed", []DiffChange{p("-", PiecePath("drive-scsi0", 3))}, nil, false},
		{"a piece added without a resize", []DiffChange{p("+", PiecePath("drive-scsi0", 9))}, nil, false},
		{"a piece added on a grown disk", []DiffChange{p("+", PiecePath("drive-scsi0", 9)), p("+", "disks/drive-scsi0/0001/")}, map[string]bool{"drive-scsi0": true}, true},
		{"a type change", []DiffChange{p("T", PiecePath("drive-scsi0", 4))}, nil, false},
		{"another path", []DiffChange{p("+", "notes.txt")}, nil, false},
		{"a config added", []DiffChange{p("+", ConfigPath("qemu-server.fw"))}, nil, true},
	} {
		err := CheckC3(c.changes, kept, c.resized)
		if (err == nil) != c.ok {
			t.Errorf("%s: %v", c.name, err)
		}
		if err != nil && !strings.Contains(err.Error(), "C3") {
			t.Errorf("%s: %v does not name C3", c.name, err)
		}
	}
}
