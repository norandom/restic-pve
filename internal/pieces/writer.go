package pieces

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"
)

// DiskSize is one disk of a run.
type DiskSize struct {
	Name string
	Size uint64
}

// Piece is one piece a run hands to restic.
type Piece struct {
	Device string
	Index  uint64
	Rel    string // path below the tree root
	Size   uint64
	Digest string
}

// File is a file the final batch writes into the tree (the manifest, the config files).
type File struct {
	Rel  string
	Data []byte
}

// Batch is one restic run over the tree: the pieces renamed into the tree for it, the other
// files written for it, and the whole tree as it then stands (what C1 and C2 compare with).
type Batch struct {
	Seq       int // 1, 2, ...
	Final     bool
	Pieces    []Piece  // renamed into the tree for this batch
	Extra     []string // other files written into the tree for this batch
	TreeFiles int      // files in the tree during this batch
	Bytes     uint64   // bytes of Pieces

	inTree map[string]bool
}

// InTree reports whether the file rel is in the tree during this batch.
func (b *Batch) InTree(rel string) bool { return b.inTree[rel] }

// WriterOptions configure a Writer.
type WriterOptions struct {
	Root       string     // the tree root (the snapshot path); rebuilt from Parent when there is one
	Staging    string     // the staging folder, outside Root, on the same file system
	Disks      []DiskSize // the disks of this run
	Parent     *Parent    // nil: a run without parent (no placeholders, every piece real)
	BatchBytes uint64     // kept bytes that make a batch (BatchSize)
	Now        func() time.Time
	// Batch hands one batch to restic and checks it (C1, C2). Batches run one at a time, in
	// order, while the writer goes on taking pieces up to two batches.
	Batch func(context.Context, *Batch) error
}

// WriterStats are the figures of a run so far.
type WriterStats struct {
	ReadBytes   uint64  // bytes put
	KeptBytes   uint64  // bytes of pieces that differ from the parent (handed to restic)
	EqualPieces int     // pieces put whose digest equals the parent's (left as placeholders)
	InFlight    uint64  // kept bytes not yet punched (staging and the running batch)
	Kept        []Piece // every kept piece of the run, in put order
}

type pieceKey struct {
	dev   string
	index uint64
}

// Writer is the piece writer of one run. A piece that arrives whole (Put) gets its SHA-256;
// equal to the parent's digest, it is dropped and its placeholder stays; else it is written to
// staging with the run's mtime. When the kept pieces reach a batch, they are renamed into the
// tree and handed to restic; after the batch passed, they are punched back into placeholders
// (no data blocks, mtime set back to the run's) so the next batch takes them from the batch
// snapshot. While restic runs, Put takes pieces up to two batches and then waits, which slows
// the stream (and the host side behind it) instead of failing the run. Put is called from one
// goroutine.
type Writer struct {
	opt    WriterOptions
	mtime  time.Time
	disks  map[string]uint64
	limit  uint64
	queue  chan *Batch
	done   chan struct{}
	ctx    context.Context
	cancel context.CancelFunc

	mu       sync.Mutex
	wake     chan struct{}
	err      error
	closed   bool
	digests  map[string][]string
	seen     map[pieceKey]bool
	tree     map[string]bool // files in the tree; after the start only the worker changes it, under mu
	pending  []Piece
	pendingB uint64
	seq      int
	stats    WriterStats
}

// MinWorkingSpace is the free space a pieces run needs on the state file system: two batches
// of at least 256 MiB plus room for the manifest.
const MinWorkingSpace = 2*256<<20 + 64<<20

// BatchSize returns the batch size for free bytes on the state file system, min(1 GiB,
// free/4), and false when free is below MinWorkingSpace: the run then uses the single stream.
func BatchSize(free uint64) (uint64, bool) {
	if free < MinWorkingSpace {
		return 0, false
	}
	return min(1<<30, free/4), true
}

// FreeSpace returns the bytes available to this user on the file system of path.
func FreeSpace(path string) (uint64, error) {
	var s unix.Statfs_t
	if err := unix.Statfs(path, &s); err != nil {
		return 0, fmt.Errorf("pieces: free space of %s: %w", path, err)
	}
	return s.Bavail * uint64(s.Bsize), nil
}

// NewWriter starts the writer of a run. With a parent, the tree at Root must be the one
// Rebuild made from the parent's listing; placeholders that do not fit this run's disks (a
// disk gone, grown or shrunk) are removed. Without a parent, Root is emptied. The staging
// folder is emptied in both cases.
func NewWriter(opt WriterOptions) (*Writer, error) {
	if opt.Root == "" || opt.Staging == "" || opt.Batch == nil || opt.BatchBytes == 0 || len(opt.Disks) == 0 {
		return nil, errors.New("pieces: writer needs a root, a staging folder, disks, a batch size and a batch function")
	}
	root, staging := filepath.Clean(opt.Root), filepath.Clean(opt.Staging)
	if staging == root || strings.HasPrefix(staging, root+"/") || strings.HasPrefix(root, staging+"/") {
		return nil, fmt.Errorf("pieces: staging %s must be outside the tree %s", staging, root)
	}
	if opt.Now == nil {
		opt.Now = time.Now
	}
	w := &Writer{opt: opt, disks: map[string]uint64{}, limit: 2 * opt.BatchBytes, queue: make(chan *Batch, 64), done: make(chan struct{}),
		wake: make(chan struct{}), digests: map[string][]string{}, seen: map[pieceKey]bool{}, tree: map[string]bool{}}
	for _, d := range opt.Disks {
		if !ValidDeviceName(d.Name) || d.Size == 0 || Pieces(d.Size) > MaxPieces {
			return nil, fmt.Errorf("pieces: disk %q of %d bytes", d.Name, d.Size)
		}
		if _, dup := w.disks[d.Name]; dup {
			return nil, fmt.Errorf("pieces: disk %s twice", d.Name)
		}
		w.disks[d.Name] = d.Size
		w.digests[d.Name] = make([]string, Pieces(d.Size))
	}
	// the run's mtime: max(now, newest parent mtime + 1 s), whole seconds. Every kept piece
	// then differs in mtime from its placeholder, even with a clock set back.
	w.mtime = opt.Now().Truncate(time.Second)
	if opt.Parent != nil {
		if pm := opt.Parent.MaxMTime().Truncate(time.Second).Add(time.Second); pm.After(w.mtime) {
			w.mtime = pm
		}
	}
	if err := os.RemoveAll(staging); err != nil {
		return nil, fmt.Errorf("pieces: %w", err)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return nil, fmt.Errorf("pieces: %w", err)
	}
	if opt.Parent == nil {
		if err := os.RemoveAll(root); err != nil {
			return nil, fmt.Errorf("pieces: %w", err)
		}
	} else {
		for _, e := range opt.Parent.Listing.Entries {
			if e.Dir || !PiecesOnly(e.Rel) {
				continue
			}
			dev, idx, ok := parsePiece(e.Rel)
			size, known := w.disks[dev]
			digest, has := "", false
			if ok && known {
				digest, has = opt.Parent.Digest(dev, idx)
			}
			if !has || idx >= Pieces(size) || e.Size != PieceLen(size, idx) {
				if err := os.Remove(filepath.Join(root, e.Rel)); err != nil && !os.IsNotExist(err) {
					return nil, fmt.Errorf("pieces: %w", err)
				}
				continue
			}
			w.tree[e.Rel] = true
			w.digests[dev][idx] = digest
		}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("pieces: %w", err)
	}
	w.opt.Root, w.opt.Staging = root, staging
	w.ctx, w.cancel = context.WithCancel(context.Background())
	go w.work()
	return w, nil
}

// MTime is the mtime of every kept piece of this run.
func (w *Writer) MTime() time.Time { return w.mtime }

// Stats returns the figures so far.
func (w *Writer) Stats() WriterStats {
	w.mu.Lock()
	defer w.mu.Unlock()
	s := w.stats
	s.Kept = append([]Piece(nil), s.Kept...)
	return s
}

// signal wakes every waiter; called with mu held.
func (w *Writer) signal() {
	close(w.wake)
	w.wake = make(chan struct{})
}

func (w *Writer) fail(err error) {
	w.mu.Lock()
	if w.err == nil {
		w.err = err
	}
	w.signal()
	w.mu.Unlock()
}

// Put takes piece index of dev (its whole real length).
func (w *Writer) Put(ctx context.Context, dev string, index uint64, data []byte) error {
	size, ok := w.disks[dev]
	switch {
	case !ok:
		return fmt.Errorf("pieces: piece of an unknown disk %q", dev)
	case index >= Pieces(size):
		return fmt.Errorf("pieces: piece %d of %s, which has %d", index, dev, Pieces(size))
	case uint64(len(data)) != PieceLen(size, index):
		return fmt.Errorf("pieces: piece %d of %s is %d bytes, want %d", index, dev, len(data), PieceLen(size, index))
	}
	digest := PieceDigest(data)
	rel := PiecePath(dev, index)
	n := uint64(len(data))
	k := pieceKey{dev, index}
	w.mu.Lock()
	switch {
	case w.err != nil:
		w.mu.Unlock()
		return w.err
	case w.closed:
		w.mu.Unlock()
		return errors.New("pieces: put after close")
	case w.seen[k]:
		w.mu.Unlock()
		return fmt.Errorf("pieces: piece %d of %s twice", index, dev)
	}
	w.seen[k] = true
	w.stats.ReadBytes += n
	if w.tree[rel] && w.digests[dev][index] == digest {
		// the parent's content: the placeholder stays, restic never sees this piece
		w.stats.EqualPieces++
		w.mu.Unlock()
		return nil
	}
	for w.stats.InFlight > 0 && w.stats.InFlight+n > w.limit {
		ch := w.wake
		w.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			return ctx.Err()
		}
		w.mu.Lock()
		if w.err != nil {
			w.mu.Unlock()
			return w.err
		}
	}
	w.stats.InFlight += n
	w.digests[dev][index] = digest
	w.mu.Unlock()

	p := Piece{Device: dev, Index: index, Rel: rel, Size: n, Digest: digest}
	if err := w.stage(p, data); err != nil {
		w.fail(err)
		return err
	}
	w.mu.Lock()
	w.stats.KeptBytes += n
	w.stats.Kept = append(w.stats.Kept, p)
	w.pending = append(w.pending, p)
	w.pendingB += n
	var b *Batch
	if w.pendingB >= w.opt.BatchBytes {
		b = w.seal(false)
	}
	w.mu.Unlock()
	if b != nil {
		w.queue <- b
	}
	return nil
}

func (w *Writer) stagePath(p Piece) string {
	return filepath.Join(w.opt.Staging, fmt.Sprintf("%s.%08d", p.Device, p.Index))
}

var zeros [64 << 10]byte

// stage writes a kept piece to staging with the run's mtime; its zero 64 KiB clusters stay
// holes.
func (w *Writer) stage(p Piece, data []byte) error {
	path := w.stagePath(p)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o644)
	if err == nil {
		for off := 0; off < len(data) && err == nil; off += len(zeros) {
			if b := data[off:min(off+len(zeros), len(data))]; !bytes.Equal(b, zeros[:len(b)]) {
				_, err = f.WriteAt(b, int64(off))
			}
		}
		if err == nil {
			err = f.Truncate(int64(len(data)))
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}
	if err == nil {
		err = os.Chtimes(path, w.mtime, w.mtime)
	}
	if err != nil {
		return fmt.Errorf("pieces: stage %s: %w", p.Rel, err)
	}
	return nil
}

// seal makes a batch of the pending pieces; called with mu held.
func (w *Writer) seal(final bool) *Batch {
	w.seq++
	b := &Batch{Seq: w.seq, Final: final, Pieces: w.pending, Bytes: w.pendingB}
	w.pending, w.pendingB = nil, 0
	return b
}

// work runs the batches in order: rename into the tree, restic and the checks, punch.
func (w *Writer) work() {
	defer close(w.done)
	for b := range w.queue {
		w.mu.Lock()
		failed := w.err != nil
		w.mu.Unlock()
		if failed {
			continue // drain
		}
		if err := w.runBatch(b); err != nil {
			w.fail(err)
		}
	}
}

func (w *Writer) runBatch(b *Batch) error {
	for _, p := range b.Pieces {
		dst := filepath.Join(w.opt.Root, p.Rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("pieces: %w", err)
		}
		if err := os.Rename(w.stagePath(p), dst); err != nil {
			return fmt.Errorf("pieces: into the tree: %w", err)
		}
		w.addTree(p.Rel)
	}
	for _, rel := range b.Extra {
		dst := filepath.Join(w.opt.Root, rel)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return fmt.Errorf("pieces: %w", err)
		}
		if err := os.Rename(w.extraPath(rel), dst); err != nil {
			return fmt.Errorf("pieces: into the tree: %w", err)
		}
		w.addTree(rel)
	}
	// the batch reads the tree set without the lock: only this goroutine writes it, and not
	// while the batch runs
	b.inTree = w.tree
	b.TreeFiles = len(w.tree)
	if err := w.opt.Batch(w.ctx, b); err != nil {
		return err
	}
	for _, p := range b.Pieces {
		if err := punch(filepath.Join(w.opt.Root, p.Rel), p.Size, w.mtime); err != nil {
			return err
		}
	}
	w.mu.Lock()
	w.stats.InFlight -= b.Bytes
	w.signal()
	w.mu.Unlock()
	return nil
}

func (w *Writer) addTree(rel string) {
	w.mu.Lock()
	w.tree[rel] = true
	w.mu.Unlock()
}

// punch frees the data blocks of a piece that restic has stored, keeps its size and sets its
// mtime back to the run's: a placeholder again.
func punch(path string, size uint64, mtime time.Time) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("pieces: punch: %w", err)
	}
	err = unix.Fallocate(int(f.Fd()), unix.FALLOC_FL_PUNCH_HOLE|unix.FALLOC_FL_KEEP_SIZE, 0, int64(size))
	if errors.Is(err, unix.EOPNOTSUPP) || errors.Is(err, unix.ENOSYS) {
		// a file system without hole punching: the same result through truncate
		if err = f.Truncate(0); err == nil {
			err = f.Truncate(int64(size))
		}
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Chtimes(path, mtime, mtime)
	}
	if err != nil {
		return fmt.Errorf("pieces: punch %s: %w", path, err)
	}
	return nil
}

// Close writes files into the tree, runs the remaining pieces as the final batch and waits
// for every batch. It refuses to finish when a piece of a disk was neither put nor kept from
// the parent. The staging folder is removed.
func (w *Writer) Close(ctx context.Context, files []File) error {
	defer os.RemoveAll(w.opt.Staging)
	w.mu.Lock()
	if w.closed {
		w.mu.Unlock()
		return errors.New("pieces: writer closed twice")
	}
	w.closed = true
	var missing error
	for _, d := range w.opt.Disks {
		for i, dg := range w.digests[d.Name] {
			if dg == "" {
				missing = fmt.Errorf("pieces: piece %d of %s was neither read nor kept from the parent", i, d.Name)
				break
			}
		}
		if missing != nil {
			break
		}
	}
	if missing != nil && w.err == nil {
		w.err = missing
	}
	var b *Batch
	if w.err == nil {
		b = w.seal(true)
	}
	w.mu.Unlock()
	if b != nil {
		for _, f := range files {
			if err := w.writeFile(f); err != nil {
				w.fail(err)
				b = nil
				break
			}
			b.Extra = append(b.Extra, f.Rel)
		}
	}
	if b != nil {
		w.queue <- b
	}
	close(w.queue)
	select {
	case <-w.done:
	case <-ctx.Done():
		w.cancel()
		<-w.done
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.err
}

// writeFile stages a file of the final batch; the worker renames it into the tree when the
// final batch starts, so an earlier batch that still runs never sees it.
func (w *Writer) writeFile(f File) error {
	if f.Rel == "" || PiecesOnly(f.Rel) || strings.Contains(f.Rel, "..") {
		return fmt.Errorf("pieces: %q cannot be written into the tree", f.Rel)
	}
	if err := os.WriteFile(w.extraPath(f.Rel), f.Data, 0o644); err != nil {
		return fmt.Errorf("pieces: %w", err)
	}
	return nil
}

func (w *Writer) extraPath(rel string) string {
	return filepath.Join(w.opt.Staging, ".file."+strings.ReplaceAll(rel, "/", "_"))
}

// Digests returns the digests of every piece of dev, once each is known.
func (w *Writer) Digests(dev string) ([]string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	d, ok := w.digests[dev]
	if !ok {
		return nil, fmt.Errorf("pieces: unknown disk %q", dev)
	}
	for i, dg := range d {
		if dg == "" {
			return nil, fmt.Errorf("pieces: piece %d of %s has no digest", i, dev)
		}
	}
	return append([]string(nil), d...), nil
}
