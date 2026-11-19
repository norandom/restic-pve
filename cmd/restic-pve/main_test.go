package main

import (
	"bytes"
	"testing"
)

func TestDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	if code := run([]string{"version"}, &out, &errb); code != 0 || out.String() != "dev\n" {
		t.Fatalf("version: %d %q", code, out.String())
	}
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"version", "x"}, {"install", "x"}, {"install", "a", "b"}, {"install", "--plain", "x"}, {"install", "-plain"}, {"install", "x", "--plain"}, {"update", "x"}, {"uninstall", "x"}, {"ping"}, {"demo", "x"}, {"demo", "full", "x"}, {"demo", "pack", "x"}, {"demo", "incremental", "pack"}, {"demo", "pack", "full", "x"}} {
		out.Reset()
		errb.Reset()
		if code := run(args, &out, &errb); code != 64 || out.Len() != 0 || errb.Len() == 0 {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
	// a mistyped demo mode names the demo's modes
	errb.Reset()
	if code := run([]string{"demo", "incremential"}, &out, &errb); code != 64 ||
		errb.String() != "usage: restic-pve demo [pack] [full|incremental]\n" {
		t.Errorf("mistyped demo mode: %d %q", code, errb.String())
	}
	// the demo is not in the usage line and needs a terminal
	errb.Reset()
	if code := run([]string{"demo"}, &out, &errb); code != 1 || errb.String() != "restic-pve demo: the demo needs a terminal\n" {
		t.Errorf("demo without a terminal: %d %q", code, errb.String())
	}
	for _, args := range [][]string{{"demo", "pack"}, {"demo", "pack", "incremental"}, {"demo", "pack", "full"}, {"demo", "incremental"}} {
		errb.Reset()
		if code := run(args, &out, &errb); code != 1 || errb.String() != "restic-pve demo: the demo needs a terminal\n" {
			t.Errorf("%q without a terminal: %d %q", args, code, errb.String())
		}
	}
	if run(nil, &out, &errb); bytes.Contains(errb.Bytes(), []byte("demo |")) {
		t.Error("demo in the usage line")
	}
}
