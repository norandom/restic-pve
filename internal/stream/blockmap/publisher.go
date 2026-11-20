package blockmap

import (
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

// WriteFileFunc writes a whole file; tests replace it to simulate a full or a slow disk.
type WriteFileFunc func(path string, data []byte, perm os.FileMode) error

// Publisher writes the snapshots of a stream to <dir>/live, from its own goroutine. The stream
// goroutine only stores pointers (Set) and counts the bytes it writes to stdout (Wrap); it
// never waits for the file system.
//
// On the first error (the directory cannot be made, the disk is full, ...) the publisher logs
// one line through Logf and switches itself off for the rest of the run.
type Publisher struct {
	dir       string
	vmid      uint32
	logf      func(string)
	writeFile WriteFileFunc
	now       func() time.Time

	snap    atomic.Pointer[State]
	final   atomic.Pointer[State]
	written atomic.Uint64
	busy    atomic.Int32
	off     atomic.Bool

	wake      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	lastSeen  uint64 // written at the previous file write (publisher goroutine only)
}

// Options configure a publisher.
type Options struct {
	Dir       string
	VMID      uint32
	Logf      func(string)     // one line when the publisher switches itself off
	WriteFile WriteFileFunc    // default os.WriteFile
	Now       func() time.Time // default time.Now
	Interval  time.Duration    // default Interval
}

// Start starts the publisher's goroutine.
func Start(o Options) *Publisher {
	p := &Publisher{dir: o.Dir, vmid: o.VMID, logf: o.Logf, writeFile: o.WriteFile, now: o.Now,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	if p.writeFile == nil {
		p.writeFile = os.WriteFile
	}
	if p.now == nil {
		p.now = time.Now
	}
	if p.logf == nil {
		p.logf = func(string) {}
	}
	every := o.Interval
	if every <= 0 {
		every = Interval
	}
	go p.loop(every)
	return p
}

// Set hands a snapshot to the publisher. It never blocks.
func (p *Publisher) Set(s *State) { p.snap.Store(s) }

// Off reports whether the publisher switched itself off after an error.
func (p *Publisher) Off() bool { return p.off.Load() }

// Wrap returns w with the bytes written to it counted for the "written" and "writing" fields.
func (p *Publisher) Wrap(w io.Writer) io.Writer { return &countWriter{w: w, p: p} }

type countWriter struct {
	w io.Writer
	p *Publisher
}

func (c *countWriter) Write(b []byte) (int, error) {
	c.p.busy.Add(1)
	n, err := c.w.Write(b)
	c.p.written.Add(uint64(n))
	c.p.busy.Add(-1)
	return n, err
}

// Close hands over the final snapshot and waits at most wait for it to be written (and, for
// a finished stream, kept as the guest's last map). It returns whether the final write ended
// in time; a slow file system never holds the caller longer than wait.
func (p *Publisher) Close(final *State, wait time.Duration) bool {
	p.closeOnce.Do(func() {
		if final != nil {
			p.final.Store(final)
		}
		close(p.wake)
	})
	select {
	case <-p.done:
		return true
	case <-time.After(wait):
		return false
	}
}

func (p *Publisher) loop(every time.Duration) {
	defer close(p.done)
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if !p.write(p.snap.Load(), false) {
				return
			}
		case <-p.wake: // closed: the final state
			if f := p.final.Load(); f != nil {
				p.write(f, true)
			}
			return
		}
	}
}

// write writes s with the current stdout counters as <dir>/live; a finished stream is also
// kept as the guest's last map. It returns false once the publisher is off.
func (p *Publisher) write(s *State, final bool) bool {
	if p.off.Load() {
		return false
	}
	if s == nil {
		return true
	}
	c := *s
	if !final {
		c.Updated = p.now()
	}
	n := p.written.Load()
	c.Written = n
	c.Writing = !final && (p.busy.Load() > 0 || n != p.lastSeen)
	p.lastSeen = n
	b := c.Encode()
	err := os.MkdirAll(p.dir, 0o755)
	if err == nil {
		err = p.atomicWrite(LiveFile, b)
	}
	if err == nil && final && c.Status == Done && c.Kind != KindProbe { // a probe is no layout of the disk
		err = p.atomicWrite(LastFile(p.vmid), b)
	}
	if err != nil {
		p.off.Store(true)
		p.logf("block view off for this run: " + err.Error())
		return false
	}
	return true
}

// atomicWrite writes name through a temporary file and a rename, so a reader sees the old or
// the new file, never a part of one.
func (p *Publisher) atomicWrite(name string, b []byte) error {
	tmp := filepath.Join(p.dir, "."+name+".tmp")
	if err := p.writeFile(tmp, b, 0o644); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, filepath.Join(p.dir, name)); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}
