package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/vma/vmatest"
)

// runBackupOpts runs `vmastream backup` through backup() with a test hook on the options.
func runBackupOpts(t *testing.T, args []string, hook func(*backupOptions)) (exitCode, []byte, string) {
	t.Helper()
	var out, errb bytes.Buffer
	o, err := parseBackup(args, &errb)
	if err != nil {
		t.Fatal(err)
	}
	if hook != nil {
		hook(o)
	}
	code, transient, err := backup(t.Context(), o, &out, &errb)
	if err != nil {
		errb.WriteString(err.Error())
	}
	_ = transient
	return code, out.Bytes(), errb.String()
}

func readLive(t *testing.T, dir string) *blockmap.State {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, blockmap.LiveFile))
	if err != nil {
		t.Fatal(err)
	}
	s, err := blockmap.Decode(b)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

// TestBlockViewNeverTouchesTheStream: stdout and the exit status are the same with the block
// view off, on, on an unwritable directory, on a full disk and on a disk that hangs; a failing
// stream fails the same way with and without it.
func TestBlockViewNeverTouchesTheStream(t *testing.T) {
	input := writeVMA(t, testGuest())
	b := testGuest()
	b.Tamper = vmatest.Tamper{Kind: vmatest.TamperExtentMD5, Extent: 3}
	tampered := writeVMA(t, b)
	file := filepath.Join(t.TempDir(), "file")
	os.WriteFile(file, nil, 0o644)
	hang := make(chan struct{})
	defer close(hang)

	for _, in := range []struct {
		name, path string
		code       exitCode
	}{{"good stream", input, exitOK}, {"tampered stream", tampered, exitIntegrity}} {
		args := func(extra ...string) []string {
			m := filepath.Join(t.TempDir(), "m.json")
			return append([]string{"--vmid", "100", "--manifest-out", m, "--ram-mib", "16", "--input", in.path}, extra...)
		}
		offCode, offOut, _ := runBackupOpts(t, args(), nil)
		if offCode != in.code || in.code == exitOK && len(offOut) == 0 {
			t.Fatalf("%s: view off: exit %d, %d bytes", in.name, offCode, len(offOut))
		}
		cases := []struct {
			name string
			dir  string
			hook func(*backupOptions)
			live bool
		}{
			{"on", t.TempDir(), nil, true},
			{"unwritable directory", filepath.Join(file, "view"), nil, false},
			{"full disk", t.TempDir(), func(o *backupOptions) {
				o.viewWriteFile = func(string, []byte, os.FileMode) error { return syscall.ENOSPC }
			}, false},
			{"disk hangs", t.TempDir(), func(o *backupOptions) {
				o.viewWriteFile = func(string, []byte, os.FileMode) error { <-hang; return nil }
			}, false},
		}
		for _, c := range cases {
			start := time.Now()
			code, out, stderr := runBackupOpts(t, args("--view-dir", c.dir, "--view-name", "web"), c.hook)
			if code != offCode || !bytes.Equal(out, offOut) {
				t.Fatalf("%s, view %s: exit %d (off: %d), stdout %d bytes (off: %d), same %v\n%s", in.name, c.name,
					code, offCode, len(out), len(offOut), bytes.Equal(out, offOut), stderr)
			}
			if d := time.Since(start); d > viewCloseWait+3*time.Second {
				t.Errorf("%s, view %s: took %v", in.name, c.name, d)
			}
			if c.name == "unwritable directory" || c.name == "full disk" {
				if n := strings.Count(stderr, `"type":"view"`); n != 1 {
					t.Errorf("%s, view %s: %d view lines:\n%s", in.name, c.name, n, stderr)
				}
			}
			if !c.live {
				continue
			}
			s := readLive(t, c.dir)
			if s.VMID != 100 || s.Name != "web" || s.Mode != "full" || s.Disk == 0 {
				t.Fatalf("%s: live %+v", in.name, s)
			}
			last := filepath.Join(c.dir, blockmap.LastFile(100))
			_, lastErr := os.Stat(last)
			if in.code == exitOK {
				if s.Status != blockmap.Done || s.Read != s.Disk || s.Handed != s.Data || s.Written != uint64(len(out)) || lastErr != nil {
					t.Fatalf("%s: final %+v, last %v", in.name, s, lastErr)
				}
				if strings.ContainsAny(string(s.Map), "?o") {
					t.Fatalf("%s: final map %s", in.name, s.Map)
				}
			} else {
				if s.Status != blockmap.Failed || !strings.Contains(s.Message, "md5") || lastErr == nil {
					t.Fatalf("%s: final %+v, last %v", in.name, s, lastErr)
				}
			}
		}
	}

	// VMASTREAM_VIEW=off turns it off whatever the flags say
	t.Setenv(viewEnv, "off")
	dir := t.TempDir()
	m := filepath.Join(t.TempDir(), "m.json")
	if code, _, _ := runBackupOpts(t, []string{"--vmid", "100", "--manifest-out", m, "--input", input, "--view-dir", dir}, nil); code != exitOK {
		t.Fatal(code)
	}
	if ents, _ := os.ReadDir(dir); len(ents) != 0 {
		t.Errorf("view written with %s=off", viewEnv)
	}
}

func TestViewNameIsChecked(t *testing.T) {
	for _, name := range []string{"bad\nname", strings.Repeat("x", 65)} {
		if _, err := parseBackup([]string{"--vmid", "1", "--manifest-out", "m", "--input", "i", "--view-name", name}, &bytes.Buffer{}); err == nil {
			t.Errorf("%q accepted", name)
		}
	}
}
