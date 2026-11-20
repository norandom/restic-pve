package run

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/pieces"
	"github.com/norandom/restic-pve/internal/stream/changes"
)

func TestDecideTable(t *testing.T) {
	const root = "/state/pieces/web/vm-100"
	base := strings.Repeat("b", 64)
	job := &Job{Mode: ModeAuto, VMID: 100, Alias: "web", Source: "pve", FullCheckEvery: 7}
	info := func(status, pve string, disks ...string) Info {
		raw, _ := json.Marshal(map[string]any{"status": status, "pve_version": pve, "disks": disks})
		i, err := ParseInfo(raw)
		if err != nil {
			t.Fatal(err)
		}
		return i
	}
	running := info("running", "9.2.24", "scsi0", "efidisk0")
	based := func() *pieces.RecordState {
		r := pieces.NewRecord(100)
		r.Arm(strings.Repeat("1", 32), "9.2.24", []pieces.RecordSize{{Name: "drive-scsi0", Size: 64 << 30}, {Name: "drive-efidisk0", Size: 528 << 10}})
		r.FullAccepted(base, false)
		r.RunsSinceCheck = 2
		return r
	}
	parent := &pieces.Manifest{Devices: []pieces.Device{{Name: "drive-efidisk0", Size: 528 << 10}, {Name: "drive-scsi0", Size: 64 << 30}}}
	newest := &pieces.Snapshot{ID: base, Paths: []string{root}}
	prev := strings.Repeat("1", 32) // the job of based()
	header := func(scsi0 uint64, bitmap string) *changes.Header {
		return &changes.Header{Prev: &prev, Devices: []changes.Device{{ID: 1, Name: "drive-efidisk0", Size: 528 << 10, Bitmap: "reuse"}, {ID: 2, Name: "drive-scsi0", Size: scsi0, Bitmap: bitmap}}}
	}
	ok := func() Inputs {
		return Inputs{Job: job, Info: running, Record: based(), Newest: newest, Root: root, Parent: parent, ParentID: base}
	}
	with := func(f func(*Inputs)) Inputs { in := ok(); f(&in); return in }

	for _, c := range []struct {
		name     string
		in       Inputs
		run      string
		reason   string
		line     string
		expected bool
	}{
		{"changes", ok(), RunChanges, "", "", true},
		{"changes after the header", with(func(i *Inputs) { i.Header = header(64<<30, "reuse") }), RunChanges, "", "", true},
		{"fault set", with(func(i *Inputs) {
			i.Record.Fault = &pieces.Fault{Date: "2026-11-27", Disks: []string{"drive-scsi0"}, Pieces: 3}
		}), RunFull, ReasonFault,
			"full read: full check found a difference on 2026-11-27; run ./backup.sh pve --clear-fault web after looking at it", false},
		{"fault on a stopped guest", with(func(i *Inputs) {
			i.Info = info("stopped", "9.2.24", "scsi0", "efidisk0")
			i.Record.Fault = &pieces.Fault{Date: "2026-11-27"}
		}), RunFull, ReasonFault,
			"full read: full check found a difference on 2026-11-27; run ./backup.sh pve --clear-fault web after looking at it", false},
		{"guest stopped", with(func(i *Inputs) { i.Info = info("stopped", "9.2.24", "scsi0", "efidisk0") }), RunFull, ReasonStopped, "full read: the guest is stopped", true},
		{"guest paused", with(func(i *Inputs) { i.Info = info("paused", "9.2.24", "scsi0", "efidisk0") }), RunFull, ReasonStopped, "full read: the guest is stopped", true},
		{"first run: no record", with(func(i *Inputs) {
			i.Record = pieces.NewRecord(100)
			i.Newest = &pieces.Snapshot{ID: base, Paths: []string{"/web/vm-100.rvs"}}
		}), RunArmFull, ReasonFirstRun,
			"full read: first run with changed reads; the next run reads only changes", true},
		{"first run: armed, full read not stored yet", with(func(i *Inputs) {
			i.Record = pieces.NewRecord(100)
			i.Record.Arm(strings.Repeat("1", 32), "9.2.24", nil)
		}), RunArmFull, ReasonFirstRun, "full read: first run with changed reads; the next run reads only changes", true},
		{"record started over by a full read without provider job", with(func(i *Inputs) { i.Record.State, i.Record.Job, i.Record.Base = pieces.RecordNone, "", "" }), RunArmFull, ReasonRecordGone,
			"full read: Proxmox has no record of changes (guest restarted, stopped or a disk changed); the next run reads only changes", true},
		{"first run: no backup at all", with(func(i *Inputs) { i.Newest = nil; i.Parent = nil }), RunArmFull, ReasonFirstRun,
			"full read: first run with changed reads; the next run reads only changes", true},
		{"newest backup is a single stream", with(func(i *Inputs) {
			i.Newest = &pieces.Snapshot{ID: strings.Repeat("c", 64), Paths: []string{"/web/vm-100.rvs"}}
			i.Parent = nil
		}), RunArmFull, ReasonNotPieces,
			"full read: the last backup is not in the piece layout", true},
		{"newest pieces backup is not the base", with(func(i *Inputs) { i.Newest = &pieces.Snapshot{ID: strings.Repeat("c", 64), Paths: []string{root}} }), RunArmFull, ReasonNotPieces,
			"full read: the last backup is not in the piece layout", true},
		{"parent unusable", with(func(i *Inputs) { i.Parent = nil }), RunArmFull, ReasonNotPieces,
			"full read: the last backup is not in the piece layout", true},
		{"disk added", with(func(i *Inputs) { i.Info = info("running", "9.2.24", "scsi0", "efidisk0", "scsi1") }), RunArmFull, ReasonDisks,
			"full read: the set of disks changed since the last backup; the next run reads only changes", true},
		{"disk removed (header)", with(func(i *Inputs) { i.Header = header(64<<30, "reuse"); i.Header.Devices = i.Header.Devices[1:] }), RunArmFull, ReasonDisks,
			"full read: the set of disks changed since the last backup; the next run reads only changes", true},
		{"disk grown", with(func(i *Inputs) { i.Header = header(65<<30, "reuse") }), RunArmFull, ReasonSize,
			"full read: the size of drive-scsi0 changed since the last backup; the next run reads only changes", true},
		{"Proxmox upgraded", with(func(i *Inputs) { i.Info = info("running", "9.3.1", "scsi0", "efidisk0") }), RunArmFull, ReasonPVE,
			"full read: the Proxmox version changed since the last backup; the next run reads only changes", true},
		{"record gone (new)", with(func(i *Inputs) { i.Header = header(64<<30, "new") }), RunArmFull, ReasonRecordGone,
			"full read: Proxmox has no record of changes (guest restarted, stopped or a disk changed); the next run reads only changes", true},
		{"record gone (none)", with(func(i *Inputs) { i.Header = header(64<<30, "none") }), RunArmFull, ReasonRecordGone,
			"full read: Proxmox has no record of changes (guest restarted, stopped or a disk changed); the next run reads only changes", true},
		{"another job moved the record on", with(func(i *Inputs) {
			i.Header = header(64<<30, "reuse")
			other := strings.Repeat("2", 32)
			i.Header.Prev = &other
		}), RunArmFull, ReasonJobMoved,
			"full read: Proxmox's record of changes moved on since the last backup (a probe, another unit or backup machine for this guest, or an older state folder); the next run reads only changes", false},
		{"no job on the host since its start", with(func(i *Inputs) {
			i.Header = header(64<<30, "reuse")
			none := ""
			i.Header.Prev = &none
		}), RunArmFull, ReasonJobMoved,
			"full read: Proxmox's record of changes moved on since the last backup (a probe, another unit or backup machine for this guest, or an older state folder); the next run reads only changes", false},
		{"a host that does not report the job", with(func(i *Inputs) { i.Header = header(64<<30, "reuse"); i.Header.Prev = nil }), RunArmFull, ReasonHostOld,
			"full read: the Proxmox host does not report which job its record of changes counts from; run restic-pve update on the Proxmox host", false},
		{"the first run after a cleared fault", with(func(i *Inputs) {
			i.Record.CheckDifference("2026-11-27", []string{"drive-scsi0"}, 3)
			i.Record.ClearFault()
		}), RunArmFull, ReasonCleared,
			"full read: the fault was cleared; this run starts a new record of changes; the next run reads only changes", true},
		{"record state set aside", with(func(i *Inputs) {
			i.Record = pieces.NewRecord(100)
			i.Record.Fault = &pieces.Fault{Date: "2026-11-27", Disks: []string{}, Reason: pieces.FaultUnreadable}
		}), RunFull, ReasonFault,
			"full read: the record state could not be read on 2026-11-27 and was set aside (web.record.json.bad-*), so a fault of a full check may be lost; run ./backup.sh pve --clear-fault web after looking at it", false},
		{"full check due (seventh run)", with(func(i *Inputs) { i.Record.RunsSinceCheck = 6 }), RunCheck, ReasonCheckDue, "full check: reading the whole disk to compare", true},
		{"full check not yet due (sixth run)", with(func(i *Inputs) { i.Record.RunsSinceCheck = 5 }), RunChanges, "", "", true},
		{"full check on demand", with(func(i *Inputs) { j := *job; j.Mode = ModeFullCheck; i.Job = &j }), RunCheck, ReasonCheckDue, "full check: reading the whole disk to compare", true},
		{"full check on demand without a base", with(func(i *Inputs) { j := *job; j.Mode = ModeFullCheck; i.Job = &j; i.Record = pieces.NewRecord(100) }), RunArmFull, ReasonFirstRun,
			"full read: first run with changed reads; the next run reads only changes", true},
	} {
		d := Decide(c.in)
		if d.Run != c.run || d.Reason != c.reason || d.Line != c.line || d.Expected != c.expected {
			t.Errorf("%s:\n got  %s %q %q expected=%v\n want %s %q %q expected=%v", c.name, d.Run, d.Reason, d.Line, d.Expected, c.run, c.reason, c.line, c.expected)
		}
		if fb := d.Fallback(); (fb == nil) != (c.run == RunChanges) || fb != nil && (fb.Reason != c.reason || fb.Expected != c.expected) {
			t.Errorf("%s: fallback %+v", c.name, fb)
		}
	}
}

func TestParseInfo(t *testing.T) {
	i, err := ParseInfo(json.RawMessage(`{"vmid":100,"status":"running","disks":["scsi0","efidisk0"],"pve_version":"9.2.24","provider":{"plugin":"current"}}`))
	if err != nil || i.Status != "running" || len(i.Disks) != 2 || i.PVE != "9.2.24" {
		t.Fatalf("%+v %v", i, err)
	}
	if d := i.Devices(); d[0] != "drive-efidisk0" || d[1] != "drive-scsi0" {
		t.Fatalf("devices %q", d)
	}
	for _, bad := range []string{``, `[]`, `{"status":"running"}`, `{"status":"running","disks":["scsi0 x"],"pve_version":"9"}`} {
		if _, err := ParseInfo(json.RawMessage(bad)); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
