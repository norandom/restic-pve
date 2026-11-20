package run

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/changes"
)

// The kinds of run a guest with changed reads can get (design F2).
const (
	RunChanges = "changes"  // read only the changed pieces through a provider job
	RunArmFull = "arm-full" // start Proxmox's record (provider job "new"), then read in full
	RunFull    = "full"     // read in full; the record is not started or not trusted
	RunCheck   = "check"    // full check: read in full, then compare with the record
)

// Reasons of a run that is not a changed-blocks run; each has its plain line.
const (
	ReasonFault      = "fault"
	ReasonStopped    = "stopped"
	ReasonFirstRun   = "first-run"
	ReasonNotPieces  = "not-pieces"
	ReasonDisks      = "disks-changed"
	ReasonSize       = "size-changed"
	ReasonPVE        = "pve-changed"
	ReasonRecordGone = "record-gone"
	ReasonCheckDue   = "check-due"
	ReasonCleared    = "fault-cleared" // the first run after --clear-fault
	ReasonProbed     = "probed"        // a probe from this backup machine moved Proxmox's record on
	ReasonJobMoved   = "job-moved"     // another provider job moved Proxmox's record on
	ReasonHostOld    = "host-old"      // the host does not say which job the record counts from
	ReasonChecks     = "checks-failed" // the changed-blocks run failed C1-C3 (Run)
	ReasonNotReady   = "not-ready"     // the host answered exit 79 (Run)
	ReasonNoSpace    = "no-space"      // too little working space: the single stream (Run)
)

const nextReadsChanges = "; the next run reads only changes"

// Decision is the run a guest gets and the one line that says why.
type Decision struct {
	Run      string
	Reason   string // "" for a changed-blocks run
	Line     string // the plain line of the design's fall back table; "" for a changed-blocks run
	Expected bool   // an expected event, not a warning (2.6)
}

// Fallback is the result's fallback field, nil for a changed-blocks run.
func (d Decision) Fallback() *Fallback {
	if d.Run == RunChanges {
		return nil
	}
	return &Fallback{Reason: d.Reason, Expected: d.Expected, Line: d.Line}
}

// Info is what the decision needs of the request info JSON in the job file.
type Info struct {
	Status string   `json:"status"`
	Disks  []string `json:"disks"` // config keys: scsi0, efidisk0, ...
	PVE    string   `json:"pve_version"`
}

var diskKeyRe = regexp.MustCompile(`^(scsi|virtio|sata|ide|efidisk|tpmstate)[0-9]+$`)

// ParseInfo reads the fields of the request info JSON the decision uses.
func ParseInfo(raw json.RawMessage) (Info, error) {
	var i Info
	if err := json.Unmarshal(raw, &i); err != nil {
		return Info{}, fmt.Errorf("%w: info: %v", ErrJob, err)
	}
	if i.Status == "" || len(i.Disks) == 0 || i.PVE == "" {
		return Info{}, fmt.Errorf("%w: info needs status, disks and pve_version", ErrJob)
	}
	for _, d := range i.Disks {
		if !diskKeyRe.MatchString(d) {
			return Info{}, fmt.Errorf("%w: info disk %q", ErrJob, d)
		}
	}
	return i, nil
}

// Devices returns the device names vzdump gives the guest's disks (drive-<key>), sorted.
func (i Info) Devices() []string {
	out := make([]string, 0, len(i.Disks))
	for _, d := range i.Disks {
		out = append(out, "drive-"+d)
	}
	sort.Strings(out)
	return out
}

// Inputs are what one decision looks at.
type Inputs struct {
	Job    *Job
	Info   Info
	Record *pieces.RecordState
	// Newest is the newest snapshot of the unit's tags (either layout), nil when there is none.
	Newest *pieces.Snapshot
	Root   string // the snapshot path of the piece layout
	// Parent is the manifest of the newest pieces snapshot when it passed LoadParent, and
	// ParentID its id.
	Parent   *pieces.Manifest
	ParentID string
	// Header is the provider job's header once it arrived (the decision is asked again
	// then); nil before the job.
	Header *changes.Header
}

func full(run, reason, line string, expected bool) Decision {
	return Decision{Run: run, Reason: reason, Line: line, Expected: expected}
}

// Decide chooses the run (design F2). In order: a fault, a stopped guest, a probe since the
// last run, no record base, a newest backup that is not a usable pieces parent of the record,
// a changed disk set, a changed Proxmox version, a changed disk size, a disk without record, a
// record that counts from another job than the record state's, a full check due; otherwise a
// changed-blocks run.
func Decide(in Inputs) Decision {
	r := in.Record
	clear := fmt.Sprintf("run ./backup.sh %s --clear-fault %s after looking at it", in.Job.Source, in.Job.Alias)
	if f := r.Fault; f != nil && f.Reason == pieces.FaultUnreadable {
		return full(RunFull, ReasonFault, fmt.Sprintf("full read: the record state could not be read on %s and was set aside (%s.record.json.bad-*), so a fault of a full check may be lost; %s",
			f.Date, in.Job.Alias, clear), false)
	}
	if r.Fault != nil {
		return full(RunFull, ReasonFault, fmt.Sprintf("full read: full check found a difference on %s; %s", r.Fault.Date, clear), false)
	}
	if in.Info.Status != "running" {
		return full(RunFull, ReasonStopped, "full read: the guest is stopped", true)
	}
	gone := full(RunArmFull, ReasonRecordGone, "full read: Proxmox has no record of changes (guest restarted, stopped or a disk changed)"+nextReadsChanges, true)
	pieceLayout := in.Newest != nil && len(in.Newest.Paths) == 1 && in.Newest.Paths[0] == in.Root
	if r.State == pieces.RecordNone && r.Job == "" && len(r.Disks) > 0 && pieceLayout {
		return gone // a full read without a provider job (stopped guest) started the record over
	}
	if r.State == pieces.RecordNone && r.Job == "" && len(r.Disks) == 0 && r.LastCheck != nil && r.LastCheck.Result == "difference" {
		return full(RunArmFull, ReasonCleared, "full read: the fault was cleared; this run starts a new record of changes"+nextReadsChanges, true)
	}
	if r.State == pieces.RecordNone && r.Job != "" {
		return full(RunArmFull, ReasonProbed, "full read: a probe of this guest came before this run"+nextReadsChanges, true)
	}
	if r.State != pieces.RecordBased || in.Newest == nil {
		return full(RunArmFull, ReasonFirstRun, "full read: first run with changed reads"+nextReadsChanges, true)
	}
	notPieces := full(RunArmFull, ReasonNotPieces, "full read: the last backup is not in the piece layout", true)
	if !pieceLayout || in.Newest.ID != r.Base || in.Parent == nil || in.ParentID != r.Base {
		return notPieces
	}
	changed := func(reason, what string) Decision {
		return full(RunArmFull, reason, "full read: "+what+" changed since the last backup"+nextReadsChanges, true)
	}
	sizes := map[string]uint64{}
	var stored []string
	for _, d := range in.Parent.Devices {
		sizes[d.Name] = d.Size
		stored = append(stored, d.Name)
	}
	sort.Strings(stored)
	if !equal(in.Info.Devices(), stored) {
		return changed(ReasonDisks, "the set of disks")
	}
	if r.PVE != "" && in.Info.PVE != r.PVE || in.Header != nil && in.Header.PVE != "" && r.PVE != "" && in.Header.PVE != r.PVE {
		return changed(ReasonPVE, "the Proxmox version")
	}
	if h := in.Header; h != nil {
		var names []string
		for _, d := range h.Devices {
			names = append(names, d.Name)
		}
		sort.Strings(names)
		if !equal(names, stored) {
			return changed(ReasonDisks, "the set of disks")
		}
		for _, d := range h.Devices {
			if d.Size != sizes[d.Name] {
				return changed(ReasonSize, "the size of "+d.Name)
			}
		}
		for _, d := range h.Devices {
			if d.Bitmap != "reuse" {
				return gone
			}
		}
		// Proxmox's record counts from the guest's last successful provider job on the host;
		// it must be the one the record state names, or the record misses what came before
		switch {
		case h.Prev == nil:
			return full(RunArmFull, ReasonHostOld, "full read: the Proxmox host does not report which job its record of changes counts from; run restic-pve update on the Proxmox host", false)
		case *h.Prev != r.Job:
			return full(RunArmFull, ReasonJobMoved, "full read: Proxmox's record of changes moved on since the last backup (a probe, another unit or backup machine for this guest, or an older state folder)"+nextReadsChanges, false)
		}
		return Decision{Run: RunChanges, Expected: true}
	}
	if in.Job.Mode == ModeFullCheck || r.RunsSinceCheck+1 >= in.Job.FullCheckEvery {
		return full(RunCheck, ReasonCheckDue, "full check: reading the whole disk to compare", true)
	}
	return Decision{Run: RunChanges, Expected: true}
}

func equal(a, b []string) bool {
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
