package facts

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestReadsAndWorkSpace(t *testing.T) {
	m := newMachine(t)
	c, err := LoadConfig(m.vars[ConfigEnv], "")
	if err != nil {
		t.Fatal(err)
	}
	// nothing reads changes yet: no facts, no working space row
	if f := Local(m.env(), c); len(f.Reads) != 0 || f.Space.Work != nil {
		t.Fatalf("before: %+v %+v", f.Reads, f.Space.Work)
	}
	write(t, filepath.Join(m.state, GuestsDir, RecordsFile), `{"schema":1,"runs":[],"guest_runs":{"web":
		{"ended":"2026-11-27T05:04:00Z","kind":"changes","read_bytes":1288490188,"skipped_bytes":132929658470}}}`)
	pieces := filepath.Join(m.state, "pieces")
	write(t, filepath.Join(pieces, "web.record.json"), `{"schema":1,"vmid":100,"state":"based","job":"`+strings.Repeat("a", 32)+
		`","base":"`+strings.Repeat("b", 64)+`","pve":"9.2.24","disks":[{"name":"drive-scsi0","size":134217728000}],`+
		`"runs_since_check":0,"last_check":{"date":"2026-11-27","result":"ok"},"fault":null}`)
	write(t, filepath.Join(pieces, "db.record.json"), `{"schema":1,"vmid":101,"state":"none","job":"","base":"","pve":"","disks":[],`+
		`"runs_since_check":0,"last_check":{"date":"2026-11-26","result":"difference"},"fault":{"date":"2026-11-26","disks":["drive-scsi0"],"pieces":3}}`)
	// web: one real piece (1 MiB of data) and one placeholder (sparse), 64 KiB in staging
	write(t, filepath.Join(pieces, "web", "vm-100", "disks", "drive-scsi0", "0000", "00000000"), strings.Repeat("x", 1<<20))
	ph := filepath.Join(pieces, "web", "vm-100", "disks", "drive-scsi0", "0000", "00000001")
	write(t, ph, "")
	if err := os.Truncate(ph, 4<<20); err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(pieces, "web", ".staging", "drive-scsi0.00000002"), strings.Repeat("y", 64<<10))
	write(t, filepath.Join(pieces, "gone", "vm-102", "_manifest.json"), strings.Repeat("z", 8<<10)) // not in backup.toml any more

	f := Local(m.env(), c)
	web := f.Reads["web"]
	if web.Last == nil || web.Last.Kind != "changes" || web.Last.ReadBytes != 1288490188 || web.Last.SkippedBytes != 132929658470 ||
		web.Record == nil || web.Record.LastCheck == nil || web.Record.LastCheck.Date != "2026-11-27" || web.Record.Fault != nil {
		t.Fatalf("web: %+v %+v", web.Last, web.Record)
	}
	db := f.Reads["db"]
	if db.Last != nil || db.Record == nil || db.Record.Fault == nil || db.Record.Fault.Pieces != 3 {
		t.Fatalf("db: %+v %+v", db.Last, db.Record)
	}
	w := f.Space.Work
	if w == nil || w.Dir != pieces || len(w.Guests) != 1 || w.Guests[0].Alias != "web" || w.Free == nil {
		t.Fatalf("work %+v", w)
	}
	if b := w.Guests[0].Bytes; b < 1<<20+64<<10 || b >= 2<<20 {
		t.Fatalf("web working space %d bytes: the placeholder must not count", b)
	}
	if w.Total < w.Guests[0].Bytes+8<<10 {
		t.Fatalf("total %d leaves out the folder of a guest that is gone", w.Total)
	}
	// a broken record state file is the state "none" (as the run reads it), not a fault
	write(t, filepath.Join(pieces, "db.record.json"), "{")
	if db := Local(m.env(), c).Reads["db"]; db.Record == nil || db.Record.Fault != nil || db.Record.State != "none" {
		t.Fatalf("broken: %+v", db.Record)
	}
}
