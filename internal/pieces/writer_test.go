package pieces

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// pieceData returns piece index of a disk filled with the byte fill.
func pieceData(size, index uint64, fill byte) []byte {
	return bytes.Repeat([]byte{fill}, int(PieceLen(size, index)))
}

// fakeParent builds a parent of one disk without restic: a manifest whose pieces hold the
// byte fill, a listing with the mtimes given by mtime(i), and the tree rebuilt from it.
func fakeParent(t *testing.T, root, dev string, size uint64, fill byte, mtime func(i uint64) time.Time) *Parent {
	t.Helper()
	d := Device{ID: 1, Name: dev, Size: size, Pieces: Pieces(size)}
	entries := []Entry{{Rel: "", Dir: true}, {Rel: DisksDir, Dir: true}, {Rel: DisksDir + "/" + dev, Dir: true}}
	for i := range d.Pieces {
		d.Digests = append(d.Digests, PieceDigest(pieceData(size, i, fill)))
		rel := PiecePath(dev, i)
		if i%GroupSize == 0 {
			entries = append(entries, Entry{Rel: parentRel(rel), Dir: true, MTime: mtime(0)})
		}
		entries = append(entries, Entry{Rel: rel, Size: PieceLen(size, i), Mode: 0o644, MTime: mtime(i), ATime: mtime(i)})
	}
	d.SHA256PT, _ = SHA256PT(size, d.Digests)
	m := testManifest(t, map[string]uint64{dev: size})
	m.Devices = []Device{d}
	l := newListing(Snapshot{ID: strings.Repeat("a", 64), Paths: []string{root}}, root, entries)
	if _, err := l.Rebuild(root, PiecesOnly); err != nil {
		t.Fatal(err)
	}
	return newParent(l.Snapshot.ID, l, m)
}

func blocks(t *testing.T, p string) int64 {
	t.Helper()
	var s unix.Stat_t
	if err := unix.Stat(p, &s); err != nil {
		t.Fatal(err)
	}
	return s.Blocks
}

func dirEmpty(t *testing.T, dir string) bool {
	t.Helper()
	ents, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	return len(ents) == 0
}

func TestWriterEqualPieceNeverReachesRestic(t *testing.T) {
	dir := t.TempDir()
	root, staging := filepath.Join(dir, "web", "vm-100"), filepath.Join(dir, "web", ".staging")
	const size = 4*PieceSize + 1000
	pm := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	parent := fakeParent(t, root, "drive-scsi0", size, 'p', func(uint64) time.Time { return pm.Add(123456789) })
	now := time.Date(2026, 11, 20, 3, 0, 0, 500_000_000, time.UTC)
	var batches []*Batch
	var sawTree []bool
	w, err := NewWriter(WriterOptions{Root: root, Staging: staging, Disks: []DiskSize{{"drive-scsi0", size}}, Parent: parent,
		BatchBytes: 1 << 30, Now: func() time.Time { return now },
		Batch: func(_ context.Context, b *Batch) error {
			batches = append(batches, b)
			sawTree = append(sawTree, b.InTree(PiecePath("drive-scsi0", 0)), b.InTree(ManifestName))
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	// piece 0 comes back with the parent's content: it is never staged
	if err := w.Put(ctx, "drive-scsi0", 0, pieceData(size, 0, 'p')); err != nil {
		t.Fatal(err)
	}
	if !dirEmpty(t, staging) {
		t.Fatal("a piece equal to the parent was staged")
	}
	// piece 1 changed; piece 4 is the short tail and changed
	if err := w.Put(ctx, "drive-scsi0", 1, pieceData(size, 1, 'n')); err != nil {
		t.Fatal(err)
	}
	if err := w.Put(ctx, "drive-scsi0", 4, pieceData(size, 4, 'n')); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(ctx, []File{{Rel: ManifestName, Data: []byte("{}")}}); err != nil {
		t.Fatal(err)
	}
	if len(batches) != 1 || !batches[0].Final || len(batches[0].Pieces) != 2 ||
		batches[0].Pieces[0].Index != 1 || batches[0].Pieces[1].Index != 4 || batches[0].TreeFiles != 6 {
		t.Fatalf("batches %+v", batches)
	}
	if !sawTree[0] || !sawTree[1] || len(batches[0].Extra) != 1 {
		t.Fatalf("tree at the batch: piece 0 %v, manifest %v, extra %v", sawTree[0], sawTree[1], batches[0].Extra)
	}
	// the placeholder of piece 0 stayed as built: the parent's size and mtime, no data
	p0 := filepath.Join(root, PiecePath("drive-scsi0", 0))
	fi, _ := os.Stat(p0)
	if !fi.ModTime().Equal(pm.Add(123456789)) || fi.Size() != PieceSize || blocks(t, p0) != 0 {
		t.Fatalf("placeholder 0: %v %d %d blocks", fi.ModTime(), fi.Size(), blocks(t, p0))
	}
	// the kept pieces were punched after the batch, mtime the run's
	p1 := filepath.Join(root, PiecePath("drive-scsi0", 1))
	fi, _ = os.Stat(p1)
	if !fi.ModTime().Equal(now.Truncate(time.Second)) || fi.Size() != PieceSize || blocks(t, p1) != 0 {
		t.Fatalf("piece 1 after the batch: %v %d %d blocks", fi.ModTime(), fi.Size(), blocks(t, p1))
	}
	dg, err := w.Digests("drive-scsi0")
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []string{parent.Manifest.Devices[0].Digests[0], PieceDigest(pieceData(size, 1, 'n')),
		parent.Manifest.Devices[0].Digests[2], parent.Manifest.Devices[0].Digests[3], PieceDigest(pieceData(size, 4, 'n'))} {
		if dg[i] != want {
			t.Fatalf("digest %d %s, want %s", i, dg[i], want)
		}
	}
	st := w.Stats()
	if st.ReadBytes != 2*PieceSize+1000 || st.KeptBytes != PieceSize+1000 || st.EqualPieces != 1 || len(st.Kept) != 2 {
		t.Fatalf("stats %+v", st)
	}
	if _, err := os.Stat(staging); !os.IsNotExist(err) {
		t.Fatal("staging left behind")
	}
}

func TestWriterBackpressure(t *testing.T) {
	dir := t.TempDir()
	root, staging := filepath.Join(dir, "vm-100"), filepath.Join(dir, ".staging")
	const size = 10 * PieceSize
	release := make(chan struct{})
	var started atomic.Int32
	var mu sync.Mutex
	var order []int
	w, err := NewWriter(WriterOptions{Root: root, Staging: staging, Disks: []DiskSize{{"drive-scsi0", size}},
		BatchBytes: 2 * PieceSize,
		Batch: func(_ context.Context, b *Batch) error {
			mu.Lock()
			order = append(order, b.Seq)
			mu.Unlock()
			if started.Add(1) == 1 {
				<-release // restic is slow on the first batch
			}
			return nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var put atomic.Int32
	done := make(chan error, 1)
	go func() {
		for i := range uint64(10) {
			if err := w.Put(ctx, "drive-scsi0", i, pieceData(size, i, byte(i))); err != nil {
				done <- err
				return
			}
			put.Add(1)
		}
		done <- nil
	}()
	// batch 1 (pieces 0, 1) runs; batch 2 (2, 3) is staged: two batches, the reader stops
	deadline := time.Now().Add(5 * time.Second)
	for put.Load() < 4 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(300 * time.Millisecond)
	if got := put.Load(); got != 4 {
		t.Fatalf("%d pieces taken while restic runs the first batch, want 4 (two batches)", got)
	}
	if w.Stats().InFlight != 4*PieceSize {
		t.Fatalf("in flight %d", w.Stats().InFlight)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if err := w.Close(ctx, nil); err != nil {
		t.Fatal(err)
	}
	if !equalInts(order, []int{1, 2, 3, 4, 5, 6}) { // five full batches and the final one
		t.Fatalf("batches %v", order)
	}
}

func equalInts(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestWriterMTimeClockSetBack(t *testing.T) {
	for _, c := range []struct {
		name       string
		parent     time.Time
		now        time.Time
		want       time.Time
		equalTrunc bool
	}{
		// the clock runs normally: the run's mtime is now, whole seconds
		{"normal", time.Date(2026, 11, 19, 3, 0, 0, 999, time.UTC), time.Date(2026, 11, 20, 3, 0, 0, 7e8, time.UTC), time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC), false},
		// the clock was set back below the parent's newest mtime: parent + 1 s
		{"clock set back", time.Date(2026, 11, 21, 3, 0, 0, 5e8, time.UTC), time.Date(2026, 11, 20, 3, 0, 0, 0, time.UTC), time.Date(2026, 11, 21, 3, 0, 1, 0, time.UTC), false},
		// now within the same second as the parent's mtime
		{"same second", time.Date(2026, 11, 20, 3, 0, 0, 1e8, time.UTC), time.Date(2026, 11, 20, 3, 0, 0, 9e8, time.UTC), time.Date(2026, 11, 20, 3, 0, 1, 0, time.UTC), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			root := filepath.Join(dir, "vm-100")
			const size = 2 * PieceSize
			parent := fakeParent(t, root, "drive-scsi0", size, 'p', func(i uint64) time.Time { return c.parent.Add(-time.Duration(i) * time.Hour) })
			w, err := NewWriter(WriterOptions{Root: root, Staging: filepath.Join(dir, ".staging"), Disks: []DiskSize{{"drive-scsi0", size}},
				Parent: parent, BatchBytes: 1 << 30, Now: func() time.Time { return c.now },
				Batch: func(context.Context, *Batch) error { return nil }})
			if err != nil {
				t.Fatal(err)
			}
			if !w.MTime().Equal(c.want) {
				t.Fatalf("mtime %v, want %v", w.MTime(), c.want)
			}
			if err := w.Put(context.Background(), "drive-scsi0", 0, pieceData(size, 0, 'n')); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(context.Background(), nil); err != nil {
				t.Fatal(err)
			}
			fi, _ := os.Stat(filepath.Join(root, PiecePath("drive-scsi0", 0)))
			if !fi.ModTime().Equal(c.want) || fi.ModTime().Equal(c.parent) {
				t.Fatalf("piece mtime %v, parent %v", fi.ModTime(), c.parent)
			}
		})
	}
}

func TestWriterRefuses(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vm-100")
	ok := func(context.Context, *Batch) error { return nil }
	if _, err := NewWriter(WriterOptions{Root: root, Staging: filepath.Join(root, ".staging"), Disks: []DiskSize{{"drive-scsi0", PieceSize}}, BatchBytes: 1, Batch: ok}); err == nil {
		t.Fatal("staging inside the tree accepted")
	}
	const size = 3*PieceSize + 1
	w, err := NewWriter(WriterOptions{Root: root, Staging: filepath.Join(dir, ".staging"), Disks: []DiskSize{{"drive-scsi0", size}}, BatchBytes: 1 << 30, Batch: ok})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for name, f := range map[string]func() error{
		"unknown disk":    func() error { return w.Put(ctx, "drive-scsi1", 0, pieceData(size, 0, 1)) },
		"index too large": func() error { return w.Put(ctx, "drive-scsi0", 4, make([]byte, 1)) },
		"wrong length":    func() error { return w.Put(ctx, "drive-scsi0", 3, make([]byte, 2)) },
	} {
		if f() == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if err := w.Put(ctx, "drive-scsi0", 0, pieceData(size, 0, 1)); err != nil {
		t.Fatal(err)
	}
	if w.Put(ctx, "drive-scsi0", 0, pieceData(size, 0, 1)) == nil {
		t.Error("a piece twice accepted")
	}
	// pieces 1-3 were neither read nor kept from a parent: the run cannot be complete
	if err := w.Close(ctx, nil); err == nil || !strings.Contains(err.Error(), "piece 1 of drive-scsi0") {
		t.Fatalf("close with missing pieces: %v", err)
	}
}

func TestWriterSizeChangedDropsPlaceholders(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "vm-100")
	mt := time.Date(2026, 11, 19, 3, 0, 0, 0, time.UTC)
	parent := fakeParent(t, root, "drive-scsi0", 2*PieceSize+7, 'p', func(uint64) time.Time { return mt })
	var b0 *Batch
	w, err := NewWriter(WriterOptions{Root: root, Staging: filepath.Join(dir, ".staging"), Disks: []DiskSize{{"drive-scsi0", 3 * PieceSize}},
		Parent: parent, BatchBytes: 1 << 30, Batch: func(_ context.Context, b *Batch) error { b0 = b; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, PiecePath("drive-scsi0", 2))); !os.IsNotExist(err) {
		t.Fatal("the old tail placeholder stayed although the disk grew")
	}
	if err := w.Close(context.Background(), nil); err == nil {
		t.Fatal("closed without the grown tail")
	}
	w2, _ := NewWriter(WriterOptions{Root: root, Staging: filepath.Join(dir, ".staging"), Disks: []DiskSize{{"drive-scsi0", 3 * PieceSize}},
		Parent: fakeParent(t, root, "drive-scsi0", 2*PieceSize+7, 'p', func(uint64) time.Time { return mt }), BatchBytes: 1 << 30,
		Batch: func(_ context.Context, b *Batch) error { b0 = b; return nil }})
	if err := w2.Put(context.Background(), "drive-scsi0", 2, pieceData(3*PieceSize, 2, 'p')); err != nil {
		t.Fatal(err)
	}
	if err := w2.Close(context.Background(), nil); err != nil || len(b0.Pieces) != 1 || b0.TreeFiles != 3 {
		t.Fatalf("grown disk: %v %+v", err, b0)
	}
}

func TestWriterBatchErrorStops(t *testing.T) {
	dir := t.TempDir()
	boom := errors.New("C1 failed")
	w, err := NewWriter(WriterOptions{Root: filepath.Join(dir, "vm-100"), Staging: filepath.Join(dir, ".staging"),
		Disks: []DiskSize{{"drive-scsi0", 4 * PieceSize}}, BatchBytes: PieceSize,
		Batch: func(context.Context, *Batch) error { return boom }})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	var perr error
	for i := uint64(0); i < 4 && perr == nil; i++ {
		perr = w.Put(ctx, "drive-scsi0", i, pieceData(4*PieceSize, i, 1))
		time.Sleep(20 * time.Millisecond)
	}
	if !errors.Is(perr, boom) {
		t.Fatalf("put after a failed batch: %v", perr)
	}
	if err := w.Close(ctx, nil); !errors.Is(err, boom) {
		t.Fatalf("close: %v", err)
	}
}

func TestBatchSize(t *testing.T) {
	for _, c := range []struct {
		free, want uint64
		ok         bool
	}{
		{2*256<<20 + 64<<20 - 1, 0, false},
		{2*256<<20 + 64<<20, (2*256<<20 + 64<<20) / 4, true},
		{8 << 30, 1 << 30, true},
		{51 << 30, 1 << 30, true},
	} {
		got, ok := BatchSize(c.free)
		if got != c.want || ok != c.ok {
			t.Errorf("BatchSize(%d) = %d %v, want %d %v", c.free, got, ok, c.want, c.ok)
		}
	}
	if free, err := FreeSpace(t.TempDir()); err != nil || free == 0 {
		t.Fatalf("free space: %d %v", free, err)
	}
}
