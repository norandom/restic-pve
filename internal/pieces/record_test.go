package pieces

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var (
	job1  = strings.Repeat("1", 32)
	job2  = strings.Repeat("2", 32)
	snapA = strings.Repeat("a", 64)
	snapB = strings.Repeat("b", 64)
	disks = []RecordSize{{"drive-scsi0", 64 << 20}}
)

func TestRecordTransitions(t *testing.T) {
	path := RecordFile(t.TempDir(), "web")
	r := LoadRecord(path, 100)
	if r.State != RecordNone || r.Base != "" {
		t.Fatalf("missing file: %+v", r)
	}
	// changes needs a base; a full read builds one only after an arm
	if r.ChangesAccepted(job1, snapA) == nil || r.FullAccepted(snapA, false) == nil {
		t.Fatal("none accepted a run")
	}
	r.Arm(job1, "9.2.24", disks)
	if r.State != RecordArmed || r.Job != job1 || r.Base != "" {
		t.Fatalf("arm: %+v", r)
	}
	if err := r.FullAccepted(snapA, false); err != nil || r.State != RecordBased || r.Base != snapA {
		t.Fatalf("full after arm: %+v %v", r, err)
	}
	if err := r.ChangesAccepted(job2, snapB); err != nil || r.Base != snapB || r.Job != job2 || r.RunsSinceCheck != 1 {
		t.Fatalf("changes: %+v %v", r, err)
	}
	// full check: the full read becomes the base, the record's job stays
	if err := r.FullAccepted(snapA, true); err != nil || r.Base != snapA || r.Job != job2 {
		t.Fatalf("full in a check: %+v %v", r, err)
	}
	r.CheckOK("2026-11-27")
	if r.LastCheck.Result != "ok" || r.RunsSinceCheck != 0 {
		t.Fatalf("check ok: %+v", r)
	}
	// a difference: the record that missed a write is dropped, the next run after the clear arms
	r.CheckDifference("2026-11-28", []string{"drive-scsi0"}, 3)
	if r.Fault == nil || r.Fault.Pieces != 3 || r.LastCheck.Result != "difference" || r.State != RecordNone || r.Job != "" || r.Base != "" || len(r.Disks) != 0 {
		t.Fatalf("fault: %+v", r)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	got := LoadRecord(path, 100)
	if got.Fault == nil || got.State != RecordNone || got.PVE != "9.2.24" {
		t.Fatalf("reload: %+v", got)
	}
	got.ClearFault()
	if got.Fault != nil {
		t.Fatal("clear")
	}
	// probe: the record advanced past the base
	got.Probe(job1)
	if got.State != RecordNone || got.Base != "" || got.Job != job1 {
		t.Fatalf("probe: %+v", got)
	}
	if err := got.Save(path); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Dir(path)); len(ents) != 1 {
		t.Fatalf("temp file left: %v", ents)
	}
}

func TestRecordBrokenFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web.record.json")
	good := `{"schema":1,"vmid":100,"state":"based","job":"` + job1 + `","base":"` + snapA + `","pve":"9.2.24","disks":[],"runs_since_check":0,"last_check":null,"fault":null}`
	os.WriteFile(path, []byte(good), 0o600)
	if r, err := ReadRecord(path, 100); err != nil || r.State != RecordBased {
		t.Fatalf("good file: %+v %v", r, err)
	}
	for _, bad := range []string{"", "{", "null", strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		strings.Replace(good, `"based"`, `"maybe"`, 1), strings.Replace(good, snapA, "abc", 1),
		strings.Replace(good, `"vmid":100`, `"vmid":101`, 1), strings.Replace(good, `"fault":null`, `"fault":null,"extra":1`, 1),
		strings.Replace(good, `"based"`, `"armed"`, 1), strings.Replace(good, job1, "", 1), good + "{}", good[:len(good)/2],
		strings.Replace(good, `"fault":null`, `"fault":{"date":"2026-11-27","disks":[],"pieces":0,"reason":"later"}`, 1)} {
		os.WriteFile(path, []byte(bad), 0o600)
		if r, err := ReadRecord(path, 100); !errors.Is(err, ErrRecordFile) {
			t.Errorf("%q: %+v %v", bad, r, err)
		}
		// the screens read it as none and change nothing
		if r := LoadRecord(path, 100); r.State != RecordNone || r.Base != "" || r.Fault != nil {
			t.Errorf("%q: %+v", bad, r)
		}
	}
	// a run sets it aside: a fault (it may have held one), saved at once
	now := time.Date(2026, 11, 27, 5, 4, 3, 0, time.UTC)
	r, moved, err := SetAside(path, 100, now)
	if err != nil || moved != path+".bad-20261127T050403Z" || r.Fault == nil || r.Fault.Reason != FaultUnreadable || r.Fault.Date != "2026-11-27" {
		t.Fatalf("%+v %q %v", r, moved, err)
	}
	if got, err := ReadRecord(path, 100); err != nil || got.Fault == nil || got.Fault.Reason != FaultUnreadable || got.State != RecordNone {
		t.Fatalf("saved: %+v %v", got, err)
	}
	if b, _ := os.ReadFile(moved); !strings.Contains(string(b), `"later"`) {
		t.Fatalf("the bad file was not kept: %q", b)
	}
}

func TestRecordSaveRefusesAnInvalidState(t *testing.T) {
	path := RecordFile(t.TempDir(), "web")
	bad := NewRecord(100)
	bad.State = RecordBased // no base
	if err := bad.Save(path); err == nil {
		t.Fatal("an invalid state was written")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("file: %v", err)
	}
}
