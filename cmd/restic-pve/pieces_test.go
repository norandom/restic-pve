package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/role"
)

func TestPiecesUsageAndRefusals(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"pieces", "--job", "x"}, sys(role.Host, &out, &errb)); code != 64 ||
		!strings.Contains(errb.String(), "restic-pve pieces is for the backup machine; this is the Proxmox host") {
		t.Fatalf("on the host: %d %q", code, errb.String())
	}
	for _, args := range [][]string{{"pieces"}, {"pieces", "--job"}, {"pieces", "--job", "a", "b"}, {"pieces", "--jobs", "a"}} {
		errb.Reset()
		if code := run(args, sys(role.Backup, &out, &errb)); code != 64 || errb.String() != piecesUsage+"\n" {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
	dir := t.TempDir()
	job := filepath.Join(dir, "job.json")
	os.WriteFile(job, []byte(`{"schema":1,"mode":"auto","vmid":100,"alias":"web","source":"s","ssh_config":"/c","ssh_host":"pve",`+
		`"restic_host":"b","tags":[],"info":{},"view_dir":"/v","progress_interval_s":30,"deadline":"2026-11-21T03:00:00Z","full_check_every":7}`), 0o600)
	errb.Reset()
	if code := run([]string{"pieces", "--job", job}, sys(role.Backup, &out, &errb)); code != 64 ||
		!strings.Contains(errb.String(), "mode auto is not in this release; it runs the probe (mode probe) only. Nothing was changed.") {
		t.Fatalf("mode auto: %d %q", code, errb.String())
	}
	os.WriteFile(job, []byte(`{"schema":1}`), 0o600)
	errb.Reset()
	if code := run([]string{"pieces", "--job", job}, sys(role.Backup, &out, &errb)); code != 64 || !strings.Contains(errb.String(), "restic-pve pieces: pieces: job file: mode") {
		t.Fatalf("bad job: %d %q", code, errb.String())
	}
}
