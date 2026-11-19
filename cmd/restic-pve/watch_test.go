package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/role"
)

// backupMachine is a backup machine without a terminal whose state root and start hint come
// from the given environment.
func backupMachine(vars map[string]string, out, errb *bytes.Buffer) env {
	e := sys(role.Backup, out, errb)
	e.getenv = func(k string) string { return vars[k] }
	return e
}

// listing is every path below dir with its size and modification time.
func listing(t *testing.T, dir string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(dir, func(p string, fi os.FileInfo, err error) error {
		if err == nil {
			b.WriteString(p + " " + fi.ModTime().Format(time.RFC3339Nano) + " " + fi.Mode().String() + "\n")
		}
		return nil
	})
	return b.String()
}

// TestWatchPlain: without a terminal, or with --once, watch prints one plain line from the
// state root of the environment, never writes, and prints no result line.
func TestWatchPlain(t *testing.T) {
	state := t.TempDir()
	vars := map[string]string{"RESTIC_PVE_STATE": state, "RESTIC_PVE_START_HINT": "./start <guest>"}
	for _, args := range [][]string{{"watch"}, {"watch", "--once"}} {
		var out, errb bytes.Buffer
		before := listing(t, state)
		if code := run(args, backupMachine(vars, &out, &errb)); code != 0 || errb.Len() != 0 {
			t.Fatalf("%q: %d %q", args, code, errb.String())
		}
		if out.String() != "no guest backup is running; start one with ./start <guest>\n" {
			t.Errorf("%q: %q", args, out.String())
		}
		if listing(t, state) != before {
			t.Errorf("%q wrote to the state root", args)
		}
	}
	// a backup writes the view below the state root; watch then reports it
	view := filepath.Join(state, "guests", "view")
	var out, errb bytes.Buffer
	if code := run([]string{"stream", "backup", "--input", guestVMA(t, false), "--vmid", "100", "--manifest-out",
		filepath.Join(t.TempDir(), "m.json"), "--view-dir", view, "--view-name", "web"}, backupMachine(vars, &out, &errb)); code != 0 {
		t.Fatal(code, errb.String())
	}
	out.Reset()
	before := listing(t, state)
	if code := run([]string{"watch", "--once"}, backupMachine(vars, &out, &errb)); code != 0 ||
		!strings.HasPrefix(out.String(), "no guest backup is running; the last one, guest 100 (web), finished at ") {
		t.Errorf("%d %q", code, out.String())
	}
	if listing(t, state) != before {
		t.Error("watch wrote to the view folder")
	}
}

// TestWatchNeedsItsWords: a missing start hint is named with an example; bad flags are a usage
// error.
func TestWatchNeedsItsWords(t *testing.T) {
	var out, errb bytes.Buffer
	vars := map[string]string{"RESTIC_PVE_STATE": t.TempDir()}
	if code := run([]string{"watch", "--once"}, backupMachine(vars, &out, &errb)); code != 78 || out.Len() != 0 ||
		errb.String() != "restic-pve watch: RESTIC_PVE_START_HINT is not set. Set it to the command that starts a backup on this machine, for example RESTIC_PVE_START_HINT='./backup.sh <source>'.\n" {
		t.Errorf("%d %q", code, errb.String())
	}
	vars["RESTIC_PVE_START_HINT"] = "x"
	for _, args := range [][]string{{"watch", "x"}, {"watch", "--dir", "/tmp"}, {"watch", "--once", "--once=maybe"}} {
		errb.Reset()
		if code := run(args, backupMachine(vars, &out, &errb)); code != 64 || out.Len() != 0 || !strings.Contains(errb.String(), "usage: restic-pve watch [--once]") {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
}
