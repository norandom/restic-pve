package pieces

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Record states of a guest, as the backup machine knows Proxmox's change record.
const (
	RecordNone  = "none"  // no record to build on: the next run starts one (arm) and reads in full
	RecordArmed = "armed" // a provider job with "new" was acked: the record counts from its job
	RecordBased = "based" // a stored backup the record builds on (base)
)

// RecordFile is the record state file of a guest below the state root.
func RecordFile(state, alias string) string {
	return filepath.Join(state, "pieces", alias+".record.json")
}

// RecordState is /state/pieces/<alias>.record.json: what the backup machine knows about the
// change record of one guest. It is written with temp file and rename, always before the ack
// line goes to the host (Commit): if the ack never arrives, Proxmox did not advance the
// record, which then still covers everything since an older job; the state file at worst
// names a newer base than the host advanced to, and the next run's checks catch that.
type RecordState struct {
	Schema         int          `json:"schema"`
	VMID           uint32       `json:"vmid"`
	State          string       `json:"state"` // none | armed | based
	Job            string       `json:"job"`   // token of the last acked provider job, or ""
	Base           string       `json:"base"`  // snapshot id the record builds on, or ""
	PVE            string       `json:"pve"`
	Disks          []RecordSize `json:"disks"`
	RunsSinceCheck int          `json:"runs_since_check"`
	LastCheck      *Check       `json:"last_check"`
	Fault          *Fault       `json:"fault"`
}

// RecordSize is one disk at the time of the record's job.
type RecordSize struct {
	Name string `json:"name"`
	Size uint64 `json:"size"`
}

// Check is the last full check.
type Check struct {
	Date   string `json:"date"`   // YYYY-MM-DD
	Result string `json:"result"` // ok | difference
}

// Fault is a difference a full check found; every run of the guest reads in full until the
// admin clears it.
type Fault struct {
	Date   string   `json:"date"`
	Disks  []string `json:"disks"`
	Pieces uint64   `json:"pieces"` // pieces that differ
}

// ErrRecord: a transition that the state does not allow.
var ErrRecord = errors.New("pieces: record state")

// NewRecord is the state "none" of a guest.
func NewRecord(vmid uint32) *RecordState {
	return &RecordState{Schema: 1, VMID: vmid, State: RecordNone, Disks: []RecordSize{}}
}

func (r *RecordState) valid(vmid uint32) bool {
	if r.Schema != 1 || r.VMID != vmid || len(r.Disks) > 255 || r.RunsSinceCheck < 0 {
		return false
	}
	switch r.State {
	case RecordNone, RecordArmed:
		if r.Base != "" {
			return false
		}
	case RecordBased:
		if !isHex(r.Base, 64) {
			return false
		}
	default:
		return false
	}
	if r.Job != "" && !isHex(r.Job, 32) || r.State != RecordNone && r.Job == "" {
		return false
	}
	for _, d := range r.Disks {
		if !ValidDeviceName(d.Name) || d.Size == 0 {
			return false
		}
	}
	return printable(r.PVE, 64) || r.PVE == ""
}

// LoadRecord reads the state file of guest vmid. A missing, unreadable or broken file, or one
// of another guest, means "none": the next run reads in full.
func LoadRecord(path string, vmid uint32) *RecordState {
	f, err := os.Open(path)
	if err != nil {
		return NewRecord(vmid)
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 1<<20+1))
	if err != nil || len(b) > 1<<20 {
		return NewRecord(vmid)
	}
	var r RecordState
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil || !r.valid(vmid) {
		return NewRecord(vmid)
	}
	if r.Disks == nil {
		r.Disks = []RecordSize{}
	}
	return &r
}

// Save writes the state with a temp file in the same folder, fsync and rename.
func (r *RecordState) Save(path string) error {
	if !r.valid(r.VMID) {
		return fmt.Errorf("%w: refusing to write an invalid state %+v", ErrRecord, *r)
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// Commit writes the state, then the ack line to w (the host's stdin). Nothing is sent when
// the state cannot be written.
func Commit(path string, r *RecordState, ackLine string, w io.Writer) error {
	if err := r.Save(path); err != nil {
		return err
	}
	_, err := io.WriteString(w, ackLine)
	return err
}

// Arm: a provider job with "new" (or with a disk without a record) was acked "arm". The record
// counts from that job; there is no base yet.
func (r *RecordState) Arm(job, pve string, disks []RecordSize) {
	r.State, r.Job, r.Base, r.PVE = RecordArmed, job, "", pve
	r.Disks = append([]RecordSize{}, disks...)
}

// FullAccepted: a full read was stored and accepted after an arm (or as the full read of a
// full check, which keeps the record's job): the record builds on it from now on.
func (r *RecordState) FullAccepted(snapshot string, check bool) error {
	if r.State != RecordArmed && !(check && r.State == RecordBased) {
		return fmt.Errorf("%w: a full read builds a base only after an arm or in a full check, not in state %s", ErrRecord, r.State)
	}
	if !isHex(snapshot, 64) {
		return fmt.Errorf("%w: snapshot id %q", ErrRecord, snapshot)
	}
	r.State, r.Base = RecordBased, snapshot
	return nil
}

// ChangesAccepted: a changed-blocks run of job was stored and accepted: it is the new base.
func (r *RecordState) ChangesAccepted(job, snapshot string) error {
	if r.State != RecordBased {
		return fmt.Errorf("%w: a changed-blocks run needs a base, state is %s", ErrRecord, r.State)
	}
	if !isHex(job, 32) || !isHex(snapshot, 64) {
		return fmt.Errorf("%w: job %q snapshot %q", ErrRecord, job, snapshot)
	}
	r.Job, r.Base = job, snapshot
	r.RunsSinceCheck++
	return nil
}

// Probe: a probe was acked, so Proxmox advanced the record past the base; nothing was stored.
// The next run starts over.
func (r *RecordState) Probe(job string) {
	r.State, r.Job, r.Base = RecordNone, job, ""
}

// CheckOK: a full check found no difference.
func (r *RecordState) CheckOK(date string) {
	r.LastCheck = &Check{Date: date, Result: "ok"}
	r.RunsSinceCheck = 0
}

// CheckDifference: a full check found pieces that differ; the fault stays until cleared.
func (r *RecordState) CheckDifference(date string, disks []string, pieces uint64) {
	r.LastCheck = &Check{Date: date, Result: "difference"}
	r.Fault = &Fault{Date: date, Disks: append([]string{}, disks...), Pieces: pieces}
	r.RunsSinceCheck = 0
}

// ClearFault is the admin's --clear-fault.
func (r *RecordState) ClearFault() { r.Fault = nil }
