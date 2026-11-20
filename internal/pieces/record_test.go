package pieces

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	r.CheckDifference("2026-11-28", []string{"drive-scsi0"}, 3)
	if r.Fault == nil || r.Fault.Pieces != 3 || r.LastCheck.Result != "difference" {
		t.Fatalf("fault: %+v", r)
	}
	if err := r.Save(path); err != nil {
		t.Fatal(err)
	}
	got := LoadRecord(path, 100)
	if got.Fault == nil || got.Base != snapA || got.Disks[0] != disks[0] || got.PVE != "9.2.24" {
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

func TestRecordBrokenFileIsNone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "web.record.json")
	good := `{"schema":1,"vmid":100,"state":"based","job":"` + job1 + `","base":"` + snapA + `","pve":"9.2.24","disks":[],"runs_since_check":0,"last_check":null,"fault":null}`
	os.WriteFile(path, []byte(good), 0o600)
	if r := LoadRecord(path, 100); r.State != RecordBased {
		t.Fatalf("good file: %+v", r)
	}
	for _, bad := range []string{"", "{", "null", strings.Replace(good, `"schema":1`, `"schema":2`, 1),
		strings.Replace(good, `"based"`, `"maybe"`, 1), strings.Replace(good, snapA, "abc", 1),
		strings.Replace(good, `"vmid":100`, `"vmid":101`, 1), strings.Replace(good, `"fault":null`, `"fault":null,"extra":1`, 1),
		strings.Replace(good, `"based"`, `"armed"`, 1), strings.Replace(good, job1, "", 1)} {
		os.WriteFile(path, []byte(bad), 0o600)
		if r := LoadRecord(path, 100); r.State != RecordNone || r.Base != "" {
			t.Errorf("%q: %+v", bad, r)
		}
	}
}

// ackWriter checks, at the moment the ack is written, what the state file says.
type ackWriter struct {
	t     *testing.T
	path  string
	state string
	got   string
}

func (w *ackWriter) Write(b []byte) (int, error) {
	w.state = LoadRecord(w.path, 100).State
	w.got += string(b)
	return len(b), nil
}

func TestCommitWritesTheFileBeforeTheAck(t *testing.T) {
	path := RecordFile(t.TempDir(), "web")
	r := NewRecord(100)
	r.Arm(job1, "9.2.24", disks)
	w := &ackWriter{t: t, path: path}
	if err := Commit(path, r, "ack "+job1+" arm\n", w); err != nil {
		t.Fatal(err)
	}
	if w.state != RecordArmed || w.got != "ack "+job1+" arm\n" {
		t.Fatalf("state at the ack: %q, ack %q", w.state, w.got)
	}
	// a state that cannot be written: no ack
	bad := NewRecord(100)
	bad.State = "based" // no base
	w = &ackWriter{t: t, path: path}
	if err := Commit(path, bad, "ack x\n", w); err == nil || w.got != "" {
		t.Fatalf("acked after a failed write: %v %q", err, w.got)
	}
	os.Chmod(filepath.Dir(path), 0o500)
	defer os.Chmod(filepath.Dir(path), 0o700)
	if os.Geteuid() != 0 {
		if err := Commit(path, r, "ack y\n", w); err == nil || w.got != "" {
			t.Fatalf("acked although the file could not be written: %v %q", err, w.got)
		}
	}
}
