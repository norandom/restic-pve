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
	for _, args := range [][]string{nil, {"help"}, {"--help"}, {"version", "x"}, {"install", "x"}, {"install", "a", "b"}, {"install", "--plain", "x"}, {"install", "-plain"}, {"install", "x", "--plain"}, {"update", "x"}, {"uninstall", "x"}, {"ping"}} {
		out.Reset()
		errb.Reset()
		if code := run(args, &out, &errb); code != 64 || out.Len() != 0 || errb.Len() == 0 {
			t.Errorf("%q: %d %q", args, code, errb.String())
		}
	}
}
