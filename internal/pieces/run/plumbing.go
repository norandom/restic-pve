package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/source"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// The plumbing the probe and the stored runs share: the environment, the result line, the
// vzdump log events and the block view.

// Exit codes and result codes of the pieces command (the stream contract's, plus 79).
const (
	CodeOK       = 0
	CodeStream   = 3  // the changes stream broke a rule
	CodeSource   = 6  // ssh or the Proxmox side failed
	CodeLocalIO  = 7  // the record state could not be written
	CodeUsage    = 64 // the job file cannot be used
	CodeNotReady = 79 // the Proxmox plug-in is not ready
)

// Env is what a run needs of the backup machine.
type Env struct {
	State      string    // RESTIC_PVE_STATE: record states live in <State>/pieces
	Stderr     io.Writer // the JSON lines and the result line
	Now        func() time.Time
	SSHProgram string // "" is ssh, found in PATH

	// The stored runs (Run) only.
	Restic     *pieces.Restic               // restic with backupctl's environment
	FreeSpace  func(string) (uint64, error) // nil: pieces.FreeSpace
	BatchBytes uint64                       // 0: pieces.BatchSize of the free space
	Producer   string                       // the manifest's producer; "" is restic-pve dev

	beforeBatch func(root string) // tests: called before each restic batch
}

func (e Env) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now()
}

// Disk is one disk in the result line.
type Disk struct {
	Name       string `json:"name"`
	Record     string `json:"record"`      // none | new | reuse
	DirtyBytes uint64 `json:"dirty_bytes"` // what the next run reads of this disk
}

// Fallback says why a run read in full (later releases).
type Fallback struct {
	Reason   string `json:"reason"`
	Expected bool   `json:"expected"`
	Line     string `json:"line"` // the plain line of the design's fall back table
}

// Result is the last stderr line of the pieces command.
type Result struct {
	Type         string        `json:"type"`
	Status       string        `json:"status"`
	Code         int           `json:"code"`
	Transient    bool          `json:"transient"`
	Message      string        `json:"message"`
	Snapshot     *string       `json:"snapshot"`
	Kind         string        `json:"kind"`
	ReadBytes    uint64        `json:"read_bytes"`
	SkippedBytes uint64        `json:"skipped_bytes"`
	StoredBytes  uint64        `json:"stored_bytes"`
	Disks        []Disk        `json:"disks"`
	Fallback     *Fallback     `json:"fallback"`
	Check        string        `json:"check"`
	Fault        *pieces.Fault `json:"fault"` // the difference a full check of this run found
}

// events writes the JSON lines of the vzdump log the way `stream backup` does: the
// consistency once, and the warn and error lines; info lines (the plug-in's and the provider
// job's among them) stay out.
type events struct {
	mu       sync.Mutex
	w        io.Writer
	once     sync.Once
	skip     bool   // a provider job of a stored run: its consistency is not the snapshot's
	notReady string // the host's "plug-in not ready: ..." line, which says what to do
}

func (e *events) emit(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.w.Write(append(b, '\n'))
}

func (e *events) Consistency(value, detail string) {
	if e.skip {
		return
	}
	e.once.Do(func() {
		e.emit(struct {
			Type   string `json:"type"`
			Value  string `json:"value"`
			Detail string `json:"detail"`
		}{"consistency", value, detail})
	})
}

func (e *events) VzdumpLine(level, line string) {
	if _, after, ok := strings.Cut(line, "plug-in not ready: "); ok {
		e.mu.Lock()
		e.notReady = after
		e.mu.Unlock()
	}
	e.emit(struct {
		Type  string `json:"type"`
		Level string `json:"level"`
		Line  string `json:"line"`
	}{"vzdump", level, line})
}

// fromSource turns a failed source into the result code: 79 stays 79, everything else is a
// source failure (transient as the source says).
func fromSource(err error) (int, bool, string) {
	var se *source.Error
	if errors.As(err, &se) {
		if se.ExitCode == 79 {
			return CodeNotReady, false, se.Detail
		}
		if se.ExitCode == 3 {
			return CodeStream, false, "the Proxmox side refused the change record: " + se.Detail
		}
		return CodeSource, se.Transient, se.Error()
	}
	return CodeSource, false, err.Error()
}

// cells groups one mark per piece (of all disks, one after the other) into at most
// blockmap.MaxCells cells; a cell shows the least advanced of its pieces: "?" unread, "o" read,
// "#" handed to restic, "=" unchanged.
func cells(marks []byte) []byte {
	n := len(marks)
	k := min(n, blockmap.MaxCells)
	out := make([]byte, k)
	for c := range k {
		out[c] = blockmap.CellKept
		for _, m := range marks[c*n/k : (c+1)*n/k] {
			if strings.IndexByte("=#o?", m) > strings.IndexByte("=#o?", out[c]) {
				out[c] = m
			}
		}
	}
	return out
}

// view is the block view of one run: one mark per piece, the publisher, progress lines.
type view struct {
	mu    sync.Mutex
	pub   *blockmap.Publisher
	ev    *events
	st    blockmap.State
	now   func() time.Time
	every time.Duration
	marks []byte
	first map[string]int // a disk's first piece in marks
	shown time.Time      // last map published
	prog  time.Time      // last progress line
}

func newView(e Env, j *Job, ev *events, kind string) *view {
	v := &view{ev: ev, now: e.now, every: time.Duration(j.ProgressInterval) * time.Second, first: map[string]int{}}
	v.st = blockmap.State{VMID: j.VMID, Name: j.Alias, Mode: kind, Kind: kind, Status: blockmap.Running, Started: e.now()}
	v.pub = blockmap.Start(blockmap.Options{Dir: j.ViewDir, VMID: j.VMID, Now: e.Now, Logf: func(msg string) {
		ev.emit(struct {
			Type    string `json:"type"`
			Level   string `json:"level"`
			Message string `json:"message"`
		}{"view", "debug", msg})
	}})
	v.prog = v.st.Started
	v.update(func(*blockmap.State) {})
	return v
}

// begin starts a new map of kind over the disks, every piece marked c (a fall back starts a
// full map).
func (v *view) begin(kind string, disks []vma.DevInfo, c byte) {
	v.update(func(s *blockmap.State) {
		s.Kind, s.Mode, s.Disk, s.Read, s.Skipped, s.Records, v.marks = kind, kind, 0, 0, 0, nil, nil
		for _, d := range disks {
			v.first[d.Name] = len(v.marks)
			v.marks = append(v.marks, bytes.Repeat([]byte{c}, int(pieces.Pieces(d.Size)))...)
			s.Disk += d.Size
		}
	})
}

// mark sets piece i of dev to c; read bytes came from the host, skipped were not read.
func (v *view) mark(dev string, i uint64, c byte, read, skipped uint64) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if p, ok := v.first[dev]; ok && p+int(i) < len(v.marks) {
		v.marks[p+int(i)] = c
	}
	v.st.Read += read
	v.st.Skipped += skipped
	if now := v.now(); now.Sub(v.shown) >= blockmap.Interval {
		v.publish(now)
	}
	if now := v.now(); now.Sub(v.prog) >= v.every {
		v.prog = now
		v.progress(dev)
	}
}

// update changes the state and publishes it.
func (v *view) update(f func(*blockmap.State)) {
	v.mu.Lock()
	defer v.mu.Unlock()
	f(&v.st)
	v.publish(v.now())
}

func (v *view) publish(now time.Time) { // with mu held
	v.shown = now
	s := v.st
	s.Updated, s.Map = now, cells(v.marks)
	v.pub.Set(&s)
}

// progress writes a progress line: pieces passed (read or skipped) of all disks.
func (v *view) progress(dev string) { // with mu held
	done, pct := v.st.Read+v.st.Skipped, 100.0
	if v.st.Disk > 0 {
		pct = math.Round(float64(done)*1000/float64(v.st.Disk)) / 10
	}
	v.ev.emit(struct {
		Type       string  `json:"type"`
		Dev        string  `json:"dev"`
		BytesDone  uint64  `json:"bytes_done"`
		TotalBytes uint64  `json:"total_bytes"`
		Percent    float64 `json:"percent"`
		RAMBytes   int64   `json:"ram_bytes"`
		Patches    uint64  `json:"patches"`
	}{"progress", dev, done, v.st.Disk, pct, 0, 0})
}

// finish ends the view and writes the result line.
func finish(e Env, v *view, res Result) Result {
	res.Status = "ok"
	if res.Code != CodeOK {
		res.Status = "error"
	}
	if v != nil { // nil: the single stream writes its own view
		v.mu.Lock()
		s := v.st
		s.Status, s.Message, s.Updated, s.Map = blockmap.Done, res.Message, e.now(), cells(v.marks)
		if res.Code != CodeOK {
			s.Status = blockmap.Failed
		}
		v.mu.Unlock()
		v.pub.Close(&s, 5*time.Second)
	}
	b, _ := json.Marshal(res)
	e.Stderr.Write(append(b, '\n'))
	return res
}

func size(b uint64) string {
	switch {
	case b >= 1<<30:
		return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30))
	case b >= 1<<20:
		return fmt.Sprintf("%.0f MiB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%.0f KiB", float64(b)/(1<<10))
}
