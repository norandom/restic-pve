package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/source"
)

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
}

// Result is the last stderr line of the pieces command.
type Result struct {
	Type         string    `json:"type"`
	Status       string    `json:"status"`
	Code         int       `json:"code"`
	Transient    bool      `json:"transient"`
	Message      string    `json:"message"`
	Snapshot     *string   `json:"snapshot"`
	Kind         string    `json:"kind"`
	ReadBytes    uint64    `json:"read_bytes"`
	SkippedBytes uint64    `json:"skipped_bytes"`
	StoredBytes  uint64    `json:"stored_bytes"`
	Disks        []Disk    `json:"disks"`
	Fallback     *Fallback `json:"fallback"`
	Check        string    `json:"check"`
}

// events writes the JSON lines of the vzdump log the way `stream backup` does: the
// consistency once, and the warn and error lines; info lines (the plug-in's and the provider
// job's among them) stay out.
type events struct {
	mu       sync.Mutex
	w        io.Writer
	once     sync.Once
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
		e.notReady = "the Proxmox plug-in is not ready: " + after
		e.mu.Unlock()
	}
	e.emit(struct {
		Type  string `json:"type"`
		Level string `json:"level"`
		Line  string `json:"line"`
	}{"vzdump", level, line})
}

// SafeEnv is env without the variables that hold or point to a secret (passwords,
// passphrases, tokens, keys, webhooks, credentials): ssh needs none of them.
func SafeEnv(env []string) []string {
	out := []string{}
	for _, kv := range env {
		k, _, _ := strings.Cut(kv, "=")
		u := strings.ToUpper(k)
		secret := strings.HasSuffix(u, "_KEY") || strings.HasSuffix(u, "_PASS")
		for _, s := range []string{"PASSWORD", "PASSPHRASE", "SECRET", "TOKEN", "WEBHOOK", "CREDENTIAL"} {
			secret = secret || strings.Contains(u, s)
		}
		if !secret {
			out = append(out, kv)
		}
	}
	return out
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

// viewMap is the probe's map: every 4 MiB piece of all disks one after the other, grouped to
// at most blockmap.MaxCells cells; a cell is "=" when all its pieces are unchanged, "?" when
// one of them would be read by the next run.
func viewMap(devs []changes.Device, maps map[uint8]*changes.Map) []byte {
	var marks []bool // true: the next run reads it
	for _, d := range devs {
		m := maps[d.ID]
		for i := uint64(0); i < pieces.Pieces(d.Size); i++ {
			marks = append(marks, m == nil || !m.Recorded || m.Dirty(i))
		}
	}
	n := len(marks)
	cells := min(n, blockmap.MaxCells)
	out := make([]byte, cells)
	for c := range cells {
		out[c] = blockmap.CellKept
		for i := c * n / cells; i < (c+1)*n/cells; i++ {
			if marks[i] {
				out[c] = blockmap.CellUnread
				break
			}
		}
	}
	return out
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

// Probe asks the Proxmox host for its change record of the guest (request changes <vmid>
// probe), shows it in the block view, sets the guest's record state to none (Proxmox's
// record now counts from this job, past any base), sends "ack <token> probe" and writes the
// result line. It reads no disk data and runs no restic command.
func Probe(ctx context.Context, e Env, j *Job) Result {
	ctx, cancel := context.WithDeadline(ctx, j.DeadlineTime())
	defer cancel()
	ev := &events{w: e.Stderr}
	started := e.now()
	base := blockmap.State{VMID: j.VMID, Name: j.Alias, Mode: "probe", Kind: blockmap.KindProbe, Status: blockmap.Running, Started: started}
	pub := blockmap.Start(blockmap.Options{Dir: j.ViewDir, VMID: j.VMID, Now: e.Now, Logf: func(msg string) {
		ev.emit(struct {
			Type    string `json:"type"`
			Level   string `json:"level"`
			Message string `json:"message"`
		}{"view", "debug", msg})
	}})
	first := base
	first.Updated = started
	pub.Set(&first)
	res := Result{Type: "result", Kind: "probe", Check: "none", Disks: []Disk{}}
	final := base
	end := func(code int, transient bool, msg string) Result {
		ev.mu.Lock()
		if code == CodeNotReady && ev.notReady != "" {
			msg = ev.notReady
		}
		ev.mu.Unlock()
		res.Code, res.Transient, res.Message = code, transient, msg
		res.Status = "ok"
		final.Status = blockmap.Done
		if code != CodeOK {
			res.Status, final.Status, final.Message = "error", blockmap.Failed, msg
		}
		final.Updated = e.now()
		pub.Close(&final, 5*time.Second)
		b, _ := json.Marshal(res)
		e.Stderr.Write(append(b, '\n'))
		return res
	}

	vmid := strconv.FormatUint(uint64(j.VMID), 10)
	rd, w, err := source.Open(ctx, source.Options{SSHConfig: j.SSHConfig, Host: j.SSHHost, VMID: j.VMID, SSHProgram: e.SSHProgram,
		Request: []string{"changes", vmid, changes.ModeProbe}, Ack: true, Env: SafeEnv(os.Environ())}, ev)
	if err != nil {
		code, tr, msg := fromSource(err)
		return end(code, tr, msg)
	}
	proc := w.(*source.Process)
	stdin := proc.Stdin()
	defer stdin.Close()
	dec := changes.NewDecoder(rd)
	var h *changes.Header
	maps := map[uint8]*changes.Map{}
	var derr error
	for {
		f, err := dec.Next()
		if err != nil {
			derr = err
			break
		}
		if f.Header != nil {
			h = f.Header
			if h.VMID != j.VMID || h.Mode != changes.ModeProbe {
				derr = fmt.Errorf("the host answered for VM %d in mode %s", h.VMID, h.Mode)
				break
			}
		}
		if f.Map != nil {
			m := *f.Map
			maps[m.Dev] = &m
		}
		if f.End != nil {
			break
		}
	}
	if derr != nil {
		if h != nil { // tell the host this job failed; Proxmox keeps the record
			if line, err := (changes.Ack{Token: h.Token, Verb: changes.VerbFail, Reason: "the probe stream was not complete"}).Line(); err == nil {
				io.WriteString(stdin, line)
			}
		}
		stdin.Close()
		// the host side ended first (ssh lost, plug-in not ready, vzdump failed): its reason
		// wins over the cut stream; otherwise the stream itself broke a rule
		if werr := w.Wait(); werr != nil && !strings.Contains(werr.Error(), "source aborted") {
			code, tr, msg := fromSource(werr)
			return end(code, tr, msg)
		}
		proc.Abort()
		return end(CodeStream, false, "changes stream: "+derr.Error())
	}

	// what Proxmox reported, per disk
	var skipped, disk uint64
	var parts []string
	for _, d := range h.Devices {
		m := maps[d.ID]
		disk += d.Size
		var dirty, changed uint64
		if m != nil && m.Recorded {
			for i := uint64(0); i < pieces.Pieces(d.Size); i++ {
				if m.Dirty(i) {
					changed++
					dirty += pieces.PieceLen(d.Size, i)
				}
			}
			skipped += d.Size - dirty
			parts = append(parts, fmt.Sprintf("%s record reuse, %s changed of %s", d.Name, size(dirty), size(d.Size)))
		} else {
			dirty = d.Size
			parts = append(parts, fmt.Sprintf("%s record %s (the next run reads it in full)", d.Name, d.Bitmap))
		}
		res.Disks = append(res.Disks, Disk{Name: d.Name, Record: d.Bitmap, DirtyBytes: dirty})
		final.Records = append(final.Records, blockmap.Record{Disk: d.Name, State: d.Bitmap, Changed: changed, Pieces: pieces.Pieces(d.Size)})
	}
	res.SkippedBytes = skipped
	final.Disk, final.Skipped, final.Map = disk, skipped, viewMap(h.Devices, maps)

	// the record state first, then the ack: nothing is stored, the record moved past any base
	rec := pieces.LoadRecord(pieces.RecordFile(e.State, j.Alias), j.VMID)
	rec.Probe(h.Token)
	line, _ := (changes.Ack{Token: h.Token, Verb: changes.VerbProbe}).Line()
	if err := pieces.Commit(pieces.RecordFile(e.State, j.Alias), rec, line, stdin); err != nil {
		if l, e2 := (changes.Ack{Token: h.Token, Verb: changes.VerbFail, Reason: "the record state could not be written"}).Line(); e2 == nil {
			io.WriteString(stdin, l)
		}
		stdin.Close()
		w.Wait()
		return end(CodeLocalIO, false, "record state: "+err.Error())
	}
	stdin.Close()
	eofErr := dec.CheckEOF()
	if werr := w.Wait(); werr != nil {
		code, tr, msg := fromSource(werr)
		return end(code, tr, msg)
	}
	if eofErr != nil {
		return end(CodeStream, false, "changes stream: "+eofErr.Error())
	}
	return end(CodeOK, false, "probe: "+strings.Join(parts, "; ")+"; nothing was read or stored")
}
