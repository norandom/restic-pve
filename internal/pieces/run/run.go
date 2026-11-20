package run

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/changes"
	"github.com/norandom/restic-pve/internal/stream/cli"
	"github.com/norandom/restic-pve/internal/stream/source"
	"github.com/norandom/restic-pve/internal/stream/vma"
)

// codeErr is a failure with its result code.
type codeErr struct {
	code int
	err  error
}

func (c *codeErr) Error() string { return c.err.Error() }

func withCode(code int, err error) error { return &codeErr{code: code, err: err} }

var errDifference = errors.New("full check: a piece differs from the base")

// runner is one stored run of a guest (job mode auto or full-check).
type runner struct {
	ctx                    context.Context
	e                      Env
	j                      *Job
	ev                     *events
	v                      *view
	rec                    *pieces.RecordState
	recPath, root, staging string
	batch                  uint64
	partial                []string       // the tags of this run's batch snapshots
	parent                 *pieces.Parent // the newest backup when it is a usable pieces parent
	started                time.Time
	res                    Result
}

// Run is one stored run of a guest with changed reads (job mode auto or full-check): it
// chooses the run (design F2) and runs it to an accepted snapshot or a failure: the
// changed-blocks run (F1), the full read into pieces (after a provider job that starts
// Proxmox's record when needed), the full check, and the fall backs. It writes the result
// line on stderr.
func Run(ctx context.Context, e Env, j *Job) Result {
	ctx, cancel := context.WithDeadline(ctx, j.DeadlineTime())
	defer cancel()
	dir := filepath.Join(e.State, "pieces", j.Alias)
	r := &runner{ctx: ctx, e: e, j: j, ev: &events{w: e.Stderr}, recPath: pieces.RecordFile(e.State, j.Alias),
		root: filepath.Join(dir, "vm-"+strconv.FormatUint(uint64(j.VMID), 10)), staging: filepath.Join(dir, ".staging"),
		partial: []string{pieces.PartialTag}, started: e.now(), res: Result{Type: "result", Kind: "full", Check: "none", Disks: []Disk{}}}
	for _, t := range j.Tags {
		if strings.HasPrefix(t, "unit=") || strings.HasPrefix(t, "vmid=") {
			r.partial = append(r.partial, t)
		}
	}
	info, err := ParseInfo(j.Info)
	if err != nil {
		return r.end(CodeUsage, false, err.Error())
	}
	free := e.FreeSpace
	if free == nil {
		free = pieces.FreeSpace
	}
	err = os.MkdirAll(dir, 0o700)
	var n uint64
	if err == nil {
		n, err = free(dir)
	}
	batch, ok := pieces.BatchSize(n)
	if err == nil && !ok {
		return r.singleStream(n)
	}
	r.v = newView(e, j, r.ev, blockmap.KindChanges)
	if err != nil {
		return r.fail(err)
	}
	r.batch = batch
	if e.BatchBytes > 0 {
		r.batch = e.BatchBytes
	}
	r.rec = pieces.LoadRecord(r.recPath, j.VMID)
	in := Inputs{Job: j, Info: info, Record: r.rec, Root: r.root}
	if in.Newest, err = r.look(); err != nil {
		return r.fail(err)
	}
	if r.parent != nil {
		in.Parent, in.ParentID = r.parent.Manifest, r.parent.ID
	}
	switch d := Decide(in); d.Run {
	case RunChanges:
		return r.changes(in)
	case RunArmFull:
		return r.armFull(d, nil, r.parent)
	case RunCheck:
		return r.check(d, in)
	default:
		return r.full(d, r.parent)
	}
}

// look forgets the batch snapshots a crashed run left, finds the newest backup of the unit
// and loads it as the parent when it is a usable pieces snapshot of this guest.
func (r *runner) look() (*pieces.Snapshot, error) {
	R := r.e.Restic
	if left, err := R.Snapshots(r.ctx, r.j.ResticHost, r.partial); err == nil && len(left) > 0 {
		var ids []string
		for _, s := range left {
			ids = append(ids, s.ID)
		}
		R.Forget(r.ctx, ids...) // what stays is maintenance's
	}
	snaps, err := R.Snapshots(r.ctx, r.j.ResticHost, r.j.Tags)
	if err != nil || len(snaps) == 0 {
		return nil, err
	}
	n := &snaps[len(snaps)-1]
	if len(n.Paths) == 1 && n.Paths[0] == r.root {
		p, err := pieces.LoadParent(r.ctx, R, n.ID, r.want(""))
		if err != nil && !errors.Is(err, pieces.ErrParent) {
			return nil, err
		}
		r.parent = p
	}
	return n, nil
}

func (r *runner) want(base string) pieces.ParentWant {
	return pieces.ParentWant{Root: r.root, Host: r.j.ResticHost, Tags: r.j.Tags, VMID: r.j.VMID, Alias: r.j.Alias, Base: base}
}

func (r *runner) end(code int, transient bool, msg string) Result {
	r.res.Code, r.res.Transient, r.res.Message = code, transient, msg
	return finish(r.e, r.v, r.res)
}

// fail ends the run with the code of err.
func (r *runner) fail(err error) Result {
	var ce *codeErr
	var se *source.Error
	var ck *pieces.CheckError
	switch {
	case errors.As(err, &ce):
		return r.end(ce.code, false, err.Error())
	case errors.As(err, &se):
		code, tr, msg := fromSource(err)
		r.ev.mu.Lock()
		if code == CodeNotReady && r.ev.notReady != "" {
			msg = "the Proxmox plug-in is not ready: " + r.ev.notReady
		}
		r.ev.mu.Unlock()
		return r.end(code, tr, msg)
	case errors.As(err, &ck):
		return r.end(CodeStream, false, err.Error())
	}
	return r.end(CodeLocalIO, false, err.Error())
}

// done ends an accepted run; msg "" is the line of a changed-blocks run.
func (r *runner) done(s *stored, msg string) Result {
	r.res.Snapshot = &s.id
	r.res.ReadBytes += s.stats.ReadBytes
	r.res.StoredBytes += s.stats.KeptBytes
	r.res.SkippedBytes = s.disk - s.stats.ReadBytes
	if msg == "" {
		msg = fmt.Sprintf("read %s, skipped %s (unchanged), %d min", size(s.stats.ReadBytes), size(r.res.SkippedBytes),
			max(1, int(math.Ceil(r.e.now().Sub(r.started).Minutes()))))
	}
	return r.end(CodeOK, false, msg)
}

func (r *runner) saveRecord() error {
	if err := r.rec.Save(r.recPath); err != nil {
		return withCode(CodeLocalIO, fmt.Errorf("the record state could not be written: %w", err))
	}
	return nil
}

// --- provider jobs ---

// job is one provider job: ssh ... changes <vmid> <mode>, its stream and its ack.
type job struct {
	p    *source.Process
	in   io.WriteCloser
	dec  *changes.Decoder
	h    *changes.Header
	cfgs []vma.ConfigBlob
	lost chan int // after the end frame: bytes the host wrote after it, sent when its output ends
}

// startJob starts a provider job and reads its header.
func (r *runner) startJob(mode string) (*job, error) {
	r.ev.skip = mode != changes.ModeProbe // a stored run's consistency is its snapshot source's
	rd, w, err := source.Open(r.ctx, source.Options{SSHConfig: r.j.SSHConfig, Host: r.j.SSHHost, VMID: r.j.VMID, SSHProgram: r.e.SSHProgram,
		Request: []string{"changes", strconv.FormatUint(uint64(r.j.VMID), 10), mode}, Ack: true, Env: SafeEnv(os.Environ())}, r.ev)
	if err != nil {
		return nil, err
	}
	jb := &job{p: w.(*source.Process), dec: changes.NewDecoder(rd)}
	jb.in = jb.p.Stdin()
	f, err := jb.dec.Next()
	if err != nil {
		return nil, jb.broken(err)
	}
	if jb.h = f.Header; jb.h.VMID != r.j.VMID || jb.h.Mode != mode {
		return nil, jb.stop(withCode(CodeStream, fmt.Errorf("the host answered for VM %d in mode %s", jb.h.VMID, jb.h.Mode)))
	}
	return jb, nil
}

func (jb *job) sendFail(reason string) {
	if jb.h != nil {
		if line, err := (changes.Ack{Token: jb.h.Token, Verb: changes.VerbFail, Reason: reason}).Line(); err == nil {
			io.WriteString(jb.in, line)
		}
	}
	jb.in.Close()
}

// broken ends a job whose stream broke off or broke a rule: the host hears fail, and its own
// reason (ssh lost, plug-in not ready, vzdump failed) wins over the cut stream.
func (jb *job) broken(err error) error {
	jb.sendFail("the changes stream was not complete")
	t := time.AfterFunc(10*time.Second, jb.p.Abort)
	defer t.Stop()
	if werr := jb.p.Wait(); werr != nil && !strings.Contains(werr.Error(), "source aborted") {
		return werr
	}
	return withCode(CodeStream, fmt.Errorf("changes stream: %w", err))
}

// stop ends a job this side gives up: the host hears fail, then the session is cut. Proxmox
// keeps its record.
func (jb *job) stop(err error) error {
	reason := []byte(err.Error())
	for i, c := range reason {
		if c < 0x20 || c > 0x7e {
			reason[i] = ' '
		}
	}
	jb.sendFail(string(reason[:min(len(reason), changes.MaxReason)]))
	if jb.lost == nil { // pieces may still be on their way: cut the session
		jb.p.Abort()
	}
	t := time.AfterFunc(10*time.Second, jb.p.Abort)
	defer t.Stop()
	jb.p.Wait()
	return err
}

// frames reads the job's frames to the end frame: config frames are kept, maps and pieces go
// to the callbacks (an error of theirs stops the job).
func (jb *job) frames(onMap func(changes.Map) error, onPiece func(changes.Piece) error) error {
	for {
		f, err := jb.dec.Next()
		if err != nil {
			return jb.broken(err)
		}
		switch {
		case f.Config != nil:
			jb.cfgs = append(jb.cfgs, vma.ConfigBlob{Name: f.Config.Name, Data: bytes.Clone(f.Config.Data)})
		case f.Map != nil && onMap != nil:
			err = onMap(*f.Map)
		case f.Piece != nil:
			err = onPiece(*f.Piece)
		case f.End != nil:
			jb.lost = make(chan int, 1)
			go func() { // the host keeps its output open until it has the ack
				var b [1]byte
				n, _ := jb.p.Read(b[:])
				jb.lost <- n
			}()
			return nil
		}
		if err != nil {
			return jb.stop(err)
		}
	}
}

// ready says whether the host still waits for the ack.
func (jb *job) ready() error {
	select {
	case n := <-jb.lost:
		jb.lost <- n
		jb.in.Close()
		if err := jb.p.Wait(); err != nil {
			return err
		}
		return withCode(CodeSource, errors.New("the Proxmox side ended the job before the ack"))
	default:
		return nil
	}
}

// commit writes the record state, then sends the ack, and waits for the job's end.
func (r *runner) commit(jb *job, a changes.Ack) error {
	line, err := a.Line()
	if err == nil {
		err = r.saveRecord()
	}
	if err != nil {
		return jb.stop(err)
	}
	io.WriteString(jb.in, line) // a lost session shows in Wait
	jb.in.Close()
	n := <-jb.lost
	if err := jb.p.Wait(); err != nil {
		return err
	}
	if n > 0 {
		return withCode(CodeStream, errors.New("changes stream: data after the end frame"))
	}
	return nil
}

// noJob is the full read after the host answered exit 79; any other error ends the run.
func (r *runner) noJob(err error) Result {
	var se *source.Error
	if !errors.As(err, &se) || se.ExitCode != CodeNotReady {
		return r.fail(err)
	}
	reason, _, _ := strings.Cut(r.ev.notReady, "; run restic-pve install")
	if reason == "" {
		reason = "exit 79"
	}
	return r.full(full(RunFull, ReasonNotReady, "full read: the Proxmox plug-in is not ready ("+reason+"); run restic-pve install on the Proxmox host", false), r.parent)
}

// --- the runs ---

// changes is the changed-blocks run (F1). Proxmox's header is decided on again: a disk
// without record, or a changed size, disk set or version, turns it into ArmFull; failed
// checks into ArmFull without parent, once.
func (r *runner) changes(in Inputs) Result {
	jb, err := r.startJob(changes.ModeUse)
	if err != nil {
		return r.noJob(err)
	}
	in.Header = jb.h
	if d := Decide(in); d.Run != RunChanges {
		return r.armFull(d, jb, r.parent)
	}
	s, err := r.changesStore(jb, r.parent, nil)
	if err == nil {
		if err = r.rec.ChangesAccepted(jb.h.Token, s.id); err == nil {
			err = r.commit(jb, changes.Ack{Token: jb.h.Token, Verb: changes.VerbOK, Snapshot: s.id})
		}
	}
	if err != nil {
		jb.stop(err) // the host waits for the ack: it hears fail
	}
	var ck *pieces.CheckError
	if errors.As(err, &ck) {
		return r.armFull(full(RunArmFull, ReasonChecks, "full read: the changed-blocks run did not pass its checks ("+ck.Check+"); this run reads the whole disk", false), nil, nil)
	}
	if err != nil {
		return r.fail(err)
	}
	r.ev.skip = false
	r.ev.Consistency(jb.p.Classifier().Consistency())
	return r.done(s, "")
}

// changesStore stores the pieces of a provider job in mode use on parent; cmp sees each map
// with the parent's disk (the full check): after its first error the later pieces are read
// and dropped to the end of the stream, and nothing is stored.
func (r *runner) changesStore(jb *job, parent *pieces.Parent, cmp func(changes.Map, *pieces.Device) error) (*stored, error) {
	r.res.Kind, r.res.Disks = "changes", []Disk{}
	kind := blockmap.KindChanges
	if cmp != nil {
		kind = blockmap.KindCheck
	}
	devs := make([]vma.DevInfo, 0, len(jb.h.Devices))
	names := map[uint8]vma.DevInfo{}
	for _, d := range jb.h.Devices {
		devs = append(devs, vma.DevInfo{ID: d.ID, Name: d.Name, Size: d.Size})
		names[d.ID] = devs[len(devs)-1]
	}
	r.v.begin(kind, devs, blockmap.CellUnread)
	m := &pieces.Manifest{Kind: "changes", Parent: &parent.ID, QmdumpMap: parent.Manifest.QmdumpMap, Record: &pieces.Record{Job: jb.h.Token}}
	var cmpErr error
	return r.store(parent, m, devs, func(w *pieces.Writer) ([]vma.ConfigBlob, error) {
		err := jb.frames(func(mp changes.Map) error {
			d := names[mp.Dev]
			var dirty, changed uint64
			for i := range pieces.Pieces(d.Size) {
				if n := pieces.PieceLen(d.Size, i); mp.Dirty(i) {
					dirty, changed = dirty+n, changed+1
				} else {
					r.v.mark(d.Name, i, blockmap.CellKept, 0, n)
				}
			}
			m.Record.Disks = append(m.Record.Disks, pieces.RecordDisk{Name: d.Name, State: changes.BitmapReuse, DirtyBytes: dirty})
			r.res.Disks = append(r.res.Disks, Disk{Name: d.Name, Record: changes.BitmapReuse, DirtyBytes: dirty})
			r.v.update(func(s *blockmap.State) {
				s.Records = append(s.Records, blockmap.Record{Disk: d.Name, State: changes.BitmapReuse, Changed: changed, Pieces: pieces.Pieces(d.Size)})
			})
			if pd, _ := parent.Device(d.Name); cmp != nil {
				if err := cmp(mp, pd); cmpErr == nil {
					cmpErr = err
				}
			}
			return nil
		}, func(p changes.Piece) error {
			if cmpErr != nil {
				return nil // the job is acked fail at its end
			}
			d := names[p.Dev]
			r.v.mark(d.Name, p.Offset/pieces.PieceSize, blockmap.CellData, uint64(len(p.Data)), 0)
			return w.Put(r.ctx, d.Name, p.Offset/pieces.PieceSize, p.Data)
		})
		m.Consistency.Value, m.Consistency.Detail = jb.p.Classifier().Consistency()
		return jb.cfgs, errors.Join(err, cmpErr)
	}, jb.ready)
}

// armFull starts Proxmox's record with a provider job acked "arm" (jb when this run's job
// can be acked so, else a new job in mode new), then reads in full on parent.
func (r *runner) armFull(d Decision, jb *job, parent *pieces.Parent) Result {
	r.res.Fallback = d.Fallback()
	r.v.update(func(s *blockmap.State) { s.Kind, s.Mode, s.Message = blockmap.KindFull, blockmap.KindFull, d.Line })
	var err error
	if jb != nil && jb.h.Mode == changes.ModeUse && allReuse(jb.h) { // pieces would follow: it cannot arm
		jb.stop(errors.New(d.Line))
		jb = nil
	}
	if jb == nil {
		if jb, err = r.startJob(changes.ModeNew); err != nil {
			return r.noJob(err)
		}
	}
	if err = jb.frames(nil, nil); err != nil {
		return r.noJob(err)
	}
	if jb.dec.Expected() != changes.VerbArm {
		return r.fail(jb.stop(withCode(CodeStream, errors.New("changes stream: the job cannot be acked arm"))))
	}
	disks := []pieces.RecordSize{}
	for _, dv := range jb.h.Devices {
		disks = append(disks, pieces.RecordSize{Name: dv.Name, Size: dv.Size})
	}
	r.rec.Arm(jb.h.Token, jb.h.PVE, disks)
	if err = r.commit(jb, changes.Ack{Token: jb.h.Token, Verb: changes.VerbArm}); err != nil {
		return r.fail(err)
	}
	s, err := r.fullRead("full", parent, jb.h)
	if err == nil {
		if err = r.rec.FullAccepted(s.id, false); err == nil {
			err = r.saveRecord()
		}
	}
	if err != nil {
		return r.fail(err)
	}
	return r.done(s, d.Line)
}

func allReuse(h *changes.Header) bool {
	for _, d := range h.Devices {
		if d.Bitmap != changes.BitmapReuse {
			return false
		}
	}
	return true
}

// full reads in full without a provider job (a stopped guest, a fault, the plug-in not
// ready). Proxmox's record no longer belongs to the newest backup: the record state starts
// over.
func (r *runner) full(d Decision, parent *pieces.Parent) Result {
	r.res.Fallback = d.Fallback()
	s, err := r.fullRead("full", parent, nil)
	if err == nil {
		r.rec.State, r.rec.Job, r.rec.Base = pieces.RecordNone, "", ""
		err = r.saveRecord()
	}
	if err != nil {
		return r.fail(err)
	}
	return r.done(s, d.Line)
}

// check is the full check: a full read on the base S0 is accepted as S_B and becomes the
// base; then a provider job in mode use: every piece its map leaves out must have the same
// digest in S0 and S_B, disk by disk. Equal: the changed pieces are stored on S_B ("full
// check: ok"). Different: the job is acked fail, the fault is set, S_B stays the newest backup.
func (r *runner) check(d Decision, in Inputs) Result {
	r.res.Fallback = d.Fallback()
	s0 := r.parent
	sb, err := r.fullRead("check", s0, nil)
	if err == nil {
		if err = r.rec.FullAccepted(sb.id, true); err == nil {
			err = r.saveRecord()
		}
	}
	if err != nil {
		return r.fail(err)
	}
	r.res.Kind, r.res.Snapshot, r.res.ReadBytes, r.res.StoredBytes = "check", &sb.id, sb.stats.ReadBytes, sb.stats.KeptBytes
	pb, err := pieces.LoadParent(r.ctx, r.e.Restic, sb.id, r.want(sb.id))
	var jb *job
	if err == nil {
		jb, err = r.startJob(changes.ModeUse)
	}
	if err != nil {
		return r.fail(err)
	}
	in.Header, in.Parent, in.ParentID, in.Newest = jb.h, pb.Manifest, pb.ID, &pieces.Snapshot{ID: sb.id, Paths: []string{r.root}}
	if Decide(in).Run != RunChanges {
		jb.stop(errors.New("full check: Proxmox has no record to compare"))
		return r.end(CodeOK, false, "full check: Proxmox has no record of changes to compare (guest restarted, stopped or a disk changed); the full read is stored")
	}
	var diff []string
	var n uint64
	s, err := r.changesStore(jb, pb, func(mp changes.Map, db *pieces.Device) error {
		d0, ok := s0.Device(db.Name)
		k := uint64(0)
		for i := range db.Pieces {
			if !mp.Dirty(i) && (!ok || i >= d0.Pieces || d0.Digests[i] != db.Digests[i]) {
				k++
			}
		}
		if k == 0 {
			return nil
		}
		diff, n = append(diff, db.Name), n+k
		return errDifference
	})
	r.res.Kind = "check"
	today := r.e.now().UTC().Format("2006-01-02")
	if err != nil {
		jb.stop(err)
	}
	if errors.Is(err, errDifference) {
		r.rec.CheckDifference(today, diff, n)
		if err := r.saveRecord(); err != nil {
			return r.fail(err)
		}
		r.res.Check, r.res.Fault = "difference", r.rec.Fault
		return r.end(CodeOK, false, fmt.Sprintf("full check: %d pieces of %s differ from the last backup although Proxmox's record calls them unchanged; the full read is stored and the next runs read in full; run ./backup.sh %s --clear-fault %s after looking at it",
			n, strings.Join(diff, ", "), r.j.Source, r.j.Alias))
	}
	if err == nil {
		if err = r.rec.ChangesAccepted(jb.h.Token, s.id); err == nil {
			r.rec.CheckOK(today)
			err = r.commit(jb, changes.Ack{Token: jb.h.Token, Verb: changes.VerbOK, Snapshot: s.id})
		}
	}
	if err != nil {
		return r.fail(err)
	}
	r.res.Check = "ok"
	return r.done(s, "full check: ok")
}

// --- storing ---

type stored struct {
	id    string
	disk  uint64
	stats pieces.WriterStats
}

// store writes one snapshot: the tree is rebuilt from parent (nil: a run without parent),
// feed puts the pieces and returns the config files, the manifest m is completed with the
// digests, and the batches, the checks and the acceptance run; ready is asked just before the
// acceptance. On any failure every snapshot of the run is forgotten.
func (r *runner) store(parent *pieces.Parent, m *pieces.Manifest, devs []vma.DevInfo, feed func(*pieces.Writer) ([]vma.ConfigBlob, error), ready func() error) (*stored, error) {
	st := &pieces.Store{R: r.e.Restic, Root: r.root, Host: r.j.ResticHost, GuestTags: r.j.Tags, PartialTags: r.partial}
	resized := map[string]bool{}
	sizes := map[string]uint64{}
	disks := []pieces.DiskSize{}
	s := &stored{}
	for _, d := range devs {
		disks, s.disk, sizes[d.Name] = append(disks, pieces.DiskSize{Name: d.Name, Size: d.Size}), s.disk+d.Size, d.Size
	}
	if parent != nil {
		st.Original = parent.ID
		for _, d := range devs {
			pd, ok := parent.Device(d.Name)
			resized[d.Name] = !ok || pd.Size != d.Size
		}
		for _, d := range parent.Manifest.Devices {
			resized[d.Name] = resized[d.Name] || sizes[d.Name] != d.Size
		}
		if _, err := parent.Listing.Rebuild(r.root, pieces.PiecesOnly); err != nil {
			return nil, err
		}
	}
	w, err := pieces.NewWriter(pieces.WriterOptions{Root: r.root, Staging: r.staging, Disks: disks, Parent: parent, BatchBytes: r.batch, Now: r.e.Now,
		Batch: func(ctx context.Context, b *pieces.Batch) error {
			if r.e.beforeBatch != nil {
				r.e.beforeBatch(r.root)
			}
			if err := st.RunBatch(ctx, b); err != nil {
				return err
			}
			for _, p := range b.Pieces {
				r.v.mark(p.Device, p.Index, blockmap.CellHanded, 0, 0)
			}
			return nil
		}})
	if err != nil {
		return nil, err
	}
	cfgs, err := feed(w)
	var files []pieces.File
	if err == nil {
		files, err = r.manifest(w, m, devs, cfgs)
	}
	if cerr := w.Close(r.ctx, files); err == nil {
		err = cerr
	}
	if err == nil && ready != nil {
		err = ready()
	}
	s.stats = w.Stats()
	if err == nil {
		s.id, err = st.Accept(r.ctx, s.stats.Kept, resized)
	}
	if err != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		st.Discard(ctx)
		return nil, err
	}
	r.v.mu.Lock()
	for i, c := range r.v.marks { // read in full and equal to the parent: kept
		if c == blockmap.CellData {
			r.v.marks[i] = blockmap.CellKept
		}
	}
	r.v.mu.Unlock()
	return s, nil
}

// manifest completes m with the digests of every disk and the config files, and returns the
// files of the final batch.
func (r *runner) manifest(w *pieces.Writer, m *pieces.Manifest, devs []vma.DevInfo, cfgs []vma.ConfigBlob) ([]pieces.File, error) {
	m.Schema, m.Layout, m.Producer, m.VMID, m.Alias, m.PieceSize = pieces.Schema, pieces.Layout, r.e.Producer, r.j.VMID, r.j.Alias, pieces.PieceSize
	if m.Producer == "" {
		m.Producer = "restic-pve dev"
	}
	m.Created = r.e.now().UTC().Format(time.RFC3339)
	for _, d := range devs {
		dg, err := w.Digests(d.Name)
		if err != nil {
			return nil, err
		}
		pt, err := pieces.SHA256PT(d.Size, dg)
		if err != nil {
			return nil, err
		}
		m.Devices = append(m.Devices, pieces.Device{ID: d.ID, Name: d.Name, Size: d.Size, Pieces: pieces.Pieces(d.Size), SHA256PT: pt, Digests: dg})
	}
	files := []pieces.File{{Rel: pieces.ManifestName}}
	for _, c := range cfgs {
		m.Config = append(m.Config, pieces.Config{Name: c.Name, SHA256: pieces.PieceDigest(c.Data)})
		files = append(files, pieces.File{Rel: pieces.ConfigPath(c.Name), Data: c.Data})
	}
	b, err := m.Encode()
	files[0].Data = b
	return files, err
}

// fullRead reads the guest in full with today's dump request and stores it in pieces on
// parent (nil: every piece is stored). arm is the provider job that started the record, nil
// for a full read without one.
func (r *runner) fullRead(kind string, parent *pieces.Parent, arm *changes.Header) (*stored, error) {
	r.ev.skip = false
	rd, w, err := source.Open(r.ctx, source.Options{SSHConfig: r.j.SSHConfig, Host: r.j.SSHHost, VMID: r.j.VMID, SSHProgram: r.e.SSHProgram,
		Env: SafeEnv(os.Environ())}, r.ev)
	if err != nil {
		return nil, err
	}
	p := w.(*source.Process)
	vr, err := vma.NewReader(bufio.NewReaderSize(rd, 1<<20))
	if err != nil {
		return nil, cut(p, err)
	}
	h := vr.Header()
	r.res.Kind = kind
	r.v.begin(kind, h.Devices, blockmap.CellUnread)
	m := &pieces.Manifest{Kind: kind}
	if parent != nil {
		m.Parent = &parent.ID
	}
	bitmaps := map[string]string{}
	if arm != nil {
		m.Record = &pieces.Record{Job: arm.Token}
		for _, d := range arm.Devices {
			bitmaps[d.Name] = d.Bitmap
			m.Record.Disks = append(m.Record.Disks, pieces.RecordDisk{Name: d.Name, State: d.Bitmap, DirtyBytes: d.Size})
		}
	}
	r.res.Disks = []Disk{}
	for _, d := range h.Devices {
		b := bitmaps[d.Name]
		if b == "" {
			b = changes.BitmapNone
		}
		r.res.Disks = append(r.res.Disks, Disk{Name: d.Name, Record: b, DirtyBytes: d.Size})
	}
	for _, c := range h.Configs {
		for _, l := range strings.Split(string(c.Data), "\n") {
			if c.Name == "qemu-server.conf" && strings.HasPrefix(l, "#qmdump#map:") {
				m.QmdumpMap = append(m.QmdumpMap, l)
			}
		}
	}
	return r.store(parent, m, h.Devices, func(w *pieces.Writer) ([]vma.ConfigBlob, error) {
		if err := r.readVMA(vr, h, w); err != nil {
			return nil, cut(p, err)
		}
		if err := p.Wait(); err != nil {
			return nil, err
		}
		m.Consistency.Value, m.Consistency.Detail = p.Classifier().Consistency()
		return h.Configs, nil
	}, nil)
}

// cut ends a dump the reading side gave up: a stream that broke waits a little for the
// host's own reason, anything else stops ssh at once.
func cut(p *source.Process, err error) error {
	var fe *vma.FormatError
	if errors.As(err, &fe) {
		t := time.AfterFunc(10*time.Second, p.Abort)
		defer t.Stop()
		if werr := p.Wait(); werr != nil && !strings.Contains(werr.Error(), "source aborted") {
			return werr
		}
		return withCode(CodeStream, err)
	}
	p.Abort()
	p.Wait()
	return err
}

// maxOpen bounds the pieces whose clusters are still arriving (1 GiB of RAM).
const maxOpen = 256

const clustersPerPiece = pieces.PieceSize / vma.ClusterSize

// readVMA turns the VMA clusters, in any order, into whole pieces: a piece is put when all
// its clusters arrived; zero clusters stay zero.
func (r *runner) readVMA(vr *vma.Reader, h *vma.Header, w *pieces.Writer) error {
	type key struct {
		dev   uint8
		piece uint64
	}
	type part struct {
		data []byte
		left int
	}
	devs := map[uint8]vma.DevInfo{}
	for _, d := range h.Devices {
		devs[d.ID] = d
	}
	open := map[key]*part{}
	for {
		c, err := vr.Next()
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
		d := devs[c.Dev]
		k := key{c.Dev, uint64(c.Index) / clustersPerPiece}
		pc := open[k]
		if pc == nil {
			if len(open) >= maxOpen {
				return withCode(CodeLocalIO, fmt.Errorf("more than %d pieces of 4 MiB arrive at the same time", maxOpen))
			}
			n := pieces.PieceLen(d.Size, k.piece)
			pc = &part{data: make([]byte, n), left: int((n + vma.ClusterSize - 1) / vma.ClusterSize)}
			open[k] = pc
		}
		if !c.Zero {
			copy(pc.data[(uint64(c.Index)%clustersPerPiece)*vma.ClusterSize:], c.Data)
		}
		if pc.left--; pc.left == 0 {
			delete(open, k)
			r.v.mark(d.Name, k.piece, blockmap.CellData, uint64(len(pc.data)), 0)
			if err := w.Put(r.ctx, d.Name, k.piece, pc.data); err != nil {
				return err
			}
		}
	}
}

// --- the single stream ---

// resultFilter passes the stream command's JSON lines on, except its result line, which it
// keeps.
type resultFilter struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte
	res Result
	got bool
}

func (f *resultFilter) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.buf = append(f.buf, p...)
	for {
		i := bytes.IndexByte(f.buf, '\n')
		if i < 0 {
			return len(p), nil
		}
		if line := f.buf[:i+1]; bytes.HasPrefix(line, []byte(`{"type":"result"`)) {
			f.got = json.Unmarshal(line, &f.res) == nil
		} else {
			f.w.Write(line)
		}
		f.buf = f.buf[i+1:]
	}
}

// singleStream is today's single stream (RVS1: `stream backup` into `restic backup --stdin`),
// run from inside this command when the working space for pieces does not fit. Its manifest
// is <state>/pieces/<alias>.rvs-manifest.json. The next run finds no pieces parent and arms.
func (r *runner) singleStream(free uint64) Result {
	line := fmt.Sprintf("full read as a single stream: %s has %s free, pieces need %s", r.e.State, size(free), size(pieces.MinWorkingSpace))
	r.res.Fallback = &Fallback{Reason: ReasonNoSpace, Line: line}
	rd, wr, err := os.Pipe()
	if err != nil {
		return r.fail(err)
	}
	ctx, cancel := context.WithCancel(r.ctx)
	defer cancel()
	var id string
	done := make(chan error, 1)
	go func() {
		out, err := r.e.Restic.RawIn(ctx, rd, func(rd io.Reader) error {
			sc := bufio.NewScanner(rd)
			sc.Buffer(nil, 1<<20)
			for sc.Scan() {
				var m struct {
					Type string `json:"message_type"`
					ID   string `json:"snapshot_id"`
				}
				if json.Unmarshal(sc.Bytes(), &m) == nil && m.Type == "summary" {
					id = m.ID
				}
			}
			return sc.Err()
		}, "backup", "--host", r.j.ResticHost, "--tag", strings.Join(r.j.Tags, ","), "--stdin", "--stdin-filename",
			fmt.Sprintf("%s/vm-%d.rvs", r.j.Alias, r.j.VMID))
		if err == nil && out.Exit != 0 {
			err = &pieces.ResticError{Command: "backup", Outcome: out}
		}
		done <- err
	}()
	f := &resultFilter{w: r.e.Stderr}
	code := cli.Main([]string{"backup", "--ssh-config", r.j.SSHConfig, "--ssh-host", r.j.SSHHost, "--vmid", strconv.FormatUint(uint64(r.j.VMID), 10),
		"--manifest-out", filepath.Join(r.e.State, "pieces", r.j.Alias+".rvs-manifest.json"), "--progress-interval", strconv.Itoa(r.j.ProgressInterval) + "s",
		"--view-dir", r.j.ViewDir, "--view-name", r.j.Alias}, nil, wr, f)
	var rerr error
	if code != 0 {
		cancel() // restic stops before it sees the end of its input
		rerr = <-done
		wr.Close()
		if !f.got {
			f.res.Message = fmt.Sprintf("stream backup ended with exit %d", code)
		}
	} else if wr.Close(); true {
		if rerr = <-done; rerr != nil || id == "" {
			code, f.res = CodeLocalIO, Result{Message: fmt.Sprintf("restic backup of the single stream: %v", rerr)}
		}
	}
	if code != 0 {
		if id != "" {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
			defer cancel()
			r.e.Restic.Forget(ctx, id)
		}
		return r.end(code, f.res.Transient, f.res.Message)
	}
	r.res.Snapshot = &id
	return r.end(CodeOK, false, line)
}
