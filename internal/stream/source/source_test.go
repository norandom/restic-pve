package source

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// --- vzdump log classification ---

func classifyFile(t *testing.T, name string) (*Classifier, []string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	c := NewClassifier()
	var forwarded []string
	for _, l := range strings.Split(strings.TrimSuffix(string(b), "\n"), "\n") {
		v := c.Line(l)
		if v.Forward {
			forwarded = append(forwarded, string(v.Level)+"|"+l)
		}
	}
	return c, forwarded
}

func TestClassifyApplication(t *testing.T) {
	c, fwd := classifyFile(t, "application.log")
	v, d := c.Consistency()
	if v != Application || !strings.Contains(d, "fs-freeze") {
		t.Fatalf("consistency = %q %q, want application", v, d)
	}
	if p := c.FleecingProblem(); p != "" {
		t.Fatalf("fleecing info line taken as a problem: %q", p)
	}
	if len(fwd) != 0 || len(c.Warnings()) != 0 {
		t.Fatalf("info-only log forwarded %v / warnings %v", fwd, c.Warnings())
	}
}

func TestClassifyStoppedGuest(t *testing.T) {
	c, fwd := classifyFile(t, "stopped.log")
	v, d := c.Consistency()
	if v != Stopped || !strings.Contains(d, "stopped") {
		t.Fatalf("consistency = %q %q, want stopped", v, d)
	}
	if c.FleecingProblem() != "" || c.GuestFSProblem() != "" {
		t.Fatalf("stopped log taken as a problem: %q %q", c.FleecingProblem(), c.GuestFSProblem())
	}
	if len(fwd) != 0 || len(c.Warnings()) != 0 {
		t.Fatalf("info-only log forwarded %v / warnings %v", fwd, c.Warnings())
	}
	// a freeze problem in the same log wins (never expected in stop mode)
	c.Line("ERROR: unable to freeze guest fs - got timeout")
	if v, _ := c.Consistency(); v != Crash {
		t.Fatalf("freeze problem in a stop-mode log: %q", v)
	}
	// a running guest's snapshot log is not stopped
	c, _ = classifyFile(t, "application.log")
	if v, _ := c.Consistency(); v == Stopped {
		t.Fatal("snapshot-mode log classified as stopped")
	}
}

func TestClassifyFreezeSkipped(t *testing.T) {
	c, _ := classifyFile(t, "freeze-skipped.log")
	v, d := c.Consistency()
	if v != Crash || !strings.Contains(d, "skipped") || !strings.Contains(d, "agent configured but not running") {
		t.Fatalf("consistency = %q %q, want crash/skipped", v, d)
	}
}

func TestClassifyFreezeFailed(t *testing.T) {
	c, fwd := classifyFile(t, "freeze-failed.log")
	v, d := c.Consistency()
	if v != Crash || !strings.Contains(d, "failed") || !strings.Contains(d, "got timeout") {
		t.Fatalf("consistency = %q %q, want crash/failed", v, d)
	}
	if len(fwd) != 1 || !strings.HasPrefix(fwd[0], "error|ERROR: VM 100 qmp command 'guest-fsfreeze-freeze' failed") {
		t.Fatalf("forwarded = %v, want the one error line", fwd)
	}
	if w := c.Warnings(); len(w) != 1 || !strings.Contains(w[0], "guest-fsfreeze-freeze") {
		t.Fatalf("warnings = %v", w)
	}
	if c.FleecingProblem() != "" {
		t.Fatal("freeze failure taken as fleecing problem")
	}
}

func TestClassifyUnknown(t *testing.T) {
	c, _ := classifyFile(t, "unknown.log")
	if v, d := c.Consistency(); v != Crash || d != DetailUnknown {
		t.Fatalf("consistency = %q %q, want crash/%q", v, d, DetailUnknown)
	}
	// no log at all (e.g. --input) is unknown as well
	if v, d := NewClassifier().Consistency(); v != Crash || d != DetailUnknown {
		t.Fatalf("empty log: %q %q", v, d)
	}
	// an unrecognised log is unknown
	c = NewClassifier()
	c.Line("something entirely different")
	if v, d := c.Consistency(); v != Crash || d != DetailUnknown {
		t.Fatalf("unrecognised log: %q %q", v, d)
	}
}

func TestClassifyFleecingWarningAborts(t *testing.T) {
	c, fwd := classifyFile(t, "fleecing-warning.log")
	p := c.FleecingProblem()
	if !strings.Contains(p, "fleecing not supported") {
		t.Fatalf("fleecing problem = %q", p)
	}
	if len(fwd) != 1 || !strings.HasPrefix(fwd[0], "warn|WARN: drive-scsi0: fleecing") {
		t.Fatalf("forwarded = %v", fwd)
	}
	c2 := NewClassifier()
	if v := c2.Line("ERROR: fleecing image allocation failed on fast-ssd"); !v.Fleecing || !v.Forward {
		t.Fatalf("fleecing error not flagged: %+v", v)
	}
	c3 := NewClassifier()
	if v := c3.Line("INFO: fleecing not supported, continuing without fleecing"); !v.Fleecing {
		t.Fatalf("informational fleecing fallback not flagged: %+v", v)
	}
	c4 := NewClassifier()
	for _, l := range []string{
		"INFO: drive-scsi0: attaching fleecing image fast-ssd:vm-100-fleece-0 to QEMU",
		"INFO: removing (old) fleecing image 'fast-ssd:vm-100-fleece-0'",
	} {
		if v := c4.Line(l); v.Fleecing {
			t.Fatalf("normal fleecing line %q flagged", l)
		}
	}
}

func TestLevels(t *testing.T) {
	cases := map[string]Level{
		"INFO: x":                       LevelInfo,
		"WARN: x":                       LevelWarn,
		"WARNING: x":                    LevelWarn,
		"ERROR: x":                      LevelError,
		"Host key verification failed.": LevelError, // not from vzdump (ssh/wrapper): forwarded
		"refused":                       LevelError,
	}
	for line, want := range cases {
		c := NewClassifier()
		v := c.Line(line)
		if v.Level != want {
			t.Errorf("%q: level %q, want %q", line, v.Level, want)
		}
		if v.Forward != (want != LevelInfo) {
			t.Errorf("%q: forward = %v", line, v.Forward)
		}
	}
	if v := NewClassifier().Line(""); v.Forward {
		t.Fatal("empty line forwarded")
	}
}

func TestWarningsAreBounded(t *testing.T) {
	c := NewClassifier()
	long := "WARN: " + strings.Repeat("x", 5000)
	for i := 0; i < 500; i++ {
		c.Line(long)
	}
	w := c.Warnings()
	if len(w) > MaxWarnings {
		t.Fatalf("%d warnings kept", len(w))
	}
	for _, s := range w {
		if len(s) > MaxLineBytes {
			t.Fatalf("warning of %d bytes kept", len(s))
		}
	}
}

// --- argv ---

func TestArgsFixedArgv(t *testing.T) {
	got, err := Args(Options{SSHConfig: "/config/ssh/ssh_config", Host: "pve.example", VMID: 100})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-F", "/config/ssh/ssh_config", "-T", "-o", "BatchMode=yes", "pve.example", "dump", "100"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q, want %q", got, want)
	}
}

func TestArgsRejectsBadOptions(t *testing.T) {
	for name, o := range map[string]Options{
		"no config":      {Host: "pve.example", VMID: 100},
		"no host":        {SSHConfig: "c", VMID: 100},
		"option as host": {SSHConfig: "c", Host: "-oProxyCommand=x", VMID: 100},
		"space in host":  {SSHConfig: "c", Host: "pve.example dump 1", VMID: 100},
		"zero vmid":      {SSHConfig: "c", Host: "pve.example"},
		"option config":  {SSHConfig: "-x", Host: "pve.example", VMID: 100},
		"program path":   {SSHConfig: "c", Host: "pve.example", VMID: 100, SSHProgram: "/usr/bin/ssh"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, err := Open(context.Background(), o, nopEvents{}); err == nil {
				t.Fatal("accepted")
			}
		})
	}
}

// --- fake ssh process ---

type nopEvents struct{}

func (nopEvents) Consistency(string, string) {}
func (nopEvents) VzdumpLine(string, string)  {}

type recEvents struct {
	mu          sync.Mutex
	consistency []string
	lines       []string
}

func (r *recEvents) Consistency(v, d string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.consistency = append(r.consistency, v+"|"+d)
}

func (r *recEvents) VzdumpLine(level, line string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.lines = append(r.lines, level+"|"+line)
}

// fakeSSH puts testdata/fake-ssh.sh first on PATH as "ssh" and returns the
// file that records its argv.
func fakeSSH(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	b, err := os.ReadFile(filepath.Join("testdata", "fake-ssh.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ssh"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	args := filepath.Join(dir, "args")
	t.Setenv("FAKE_SSH_ARGS", args)
	t.Setenv("FAKE_SSH_LOG", "")
	t.Setenv("FAKE_SSH_STDOUT", "")
	t.Setenv("FAKE_SSH_SLEEP", "")
	t.Setenv("FAKE_SSH_EXIT", "")
	return args
}

func testOptions() Options {
	return Options{SSHConfig: "ssh_config", Host: "pve.example", VMID: 100}
}

func TestOpenStreamsStdoutAndForwardsLog(t *testing.T) {
	argsFile := fakeSSH(t)
	payload := filepath.Join(t.TempDir(), "stream")
	want := bytes.Repeat([]byte("vma-bytes"), 100000)
	if err := os.WriteFile(payload, want, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("FAKE_SSH_STDOUT", payload)
	abs, _ := filepath.Abs(filepath.Join("testdata", "freeze-failed.log"))
	t.Setenv("FAKE_SSH_LOG", abs)
	ev := &recEvents{}
	p, w, err := Open(context.Background(), testOptions(), ev)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("stream differs (%d bytes, want %d)", len(got), len(want))
	}
	if err := w.Wait(); err != nil {
		t.Fatalf("Wait: %v", err)
	}
	argv, _ := os.ReadFile(argsFile)
	if string(argv) != "-F\nssh_config\n-T\n-o\nBatchMode=yes\npve.example\ndump\n100\n" {
		t.Fatalf("fake ssh argv = %q", argv)
	}
	if len(ev.consistency) != 1 || !strings.HasPrefix(ev.consistency[0], "crash|") {
		t.Fatalf("consistency events = %v", ev.consistency)
	}
	if len(ev.lines) != 1 || !strings.HasPrefix(ev.lines[0], "error|ERROR: VM 100 qmp command") {
		t.Fatalf("forwarded lines = %v", ev.lines)
	}
	if v, _ := w.Classifier().Consistency(); v != Crash {
		t.Fatalf("classifier = %q", v)
	}
	if err := w.Wait(); err != nil { // idempotent
		t.Fatalf("second Wait: %v", err)
	}
	if len(ev.consistency) != 1 {
		t.Fatalf("consistency emitted %d times", len(ev.consistency))
	}
}

func TestExitStatusMapping(t *testing.T) {
	cases := []struct {
		exit      string
		code      int
		transient bool
	}{
		{"255", 255, true}, // ssh connection error
		{"75", 75, true},   // wrapper busy (PVE-side lock)
		{"73", 73, false},  // fleecing storage below its free-space limit
		{"1", 1, false},    // vzdump failed
		{"64", 64, false},  // refused by the wrapper
		{"2", 2, false},
	}
	for _, c := range cases {
		t.Run("exit "+c.exit, func(t *testing.T) {
			fakeSSH(t)
			t.Setenv("FAKE_SSH_EXIT", c.exit)
			p, w, err := Open(context.Background(), testOptions(), nopEvents{})
			if err != nil {
				t.Fatal(err)
			}
			io.Copy(io.Discard, p)
			err = w.Wait()
			var se *Error
			if !errors.As(err, &se) || !errors.Is(err, ErrSourceFailed) {
				t.Fatalf("Wait = %v, want source-failed *Error", err)
			}
			if se.ExitCode != c.code || se.Transient != c.transient {
				t.Fatalf("Error = %+v, want code %d transient %v", se, c.code, c.transient)
			}
			if errors.Is(err, ErrFleecing) {
				t.Fatal("classified as fleecing")
			}
		})
	}
}

func TestFleecingWarningKillsSource(t *testing.T) {
	fakeSSH(t)
	abs, _ := filepath.Abs(filepath.Join("testdata", "fleecing-warning.log"))
	t.Setenv("FAKE_SSH_LOG", abs)
	t.Setenv("FAKE_SSH_SLEEP", "60")
	ev := &recEvents{}
	start := time.Now()
	p, w, err := Open(context.Background(), testOptions(), ev)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p) // ends when the source is killed
	err = w.Wait()
	if time.Since(start) > 20*time.Second {
		t.Fatalf("source not killed promptly (%v)", time.Since(start))
	}
	var se *Error
	if !errors.Is(err, ErrFleecing) || !errors.As(err, &se) || se.Transient {
		t.Fatalf("Wait = %v (%+v), want non-transient fleecing error", err, se)
	}
	if !strings.Contains(err.Error(), "fleecing not supported") {
		t.Fatalf("message lacks the vzdump line: %v", err)
	}
	if len(ev.consistency) != 1 {
		t.Fatalf("consistency events = %v", ev.consistency)
	}
}

func TestFleecingWarningWinsOverCleanExit(t *testing.T) {
	fakeSSH(t)
	abs, _ := filepath.Abs(filepath.Join("testdata", "fleecing-warning.log"))
	t.Setenv("FAKE_SSH_LOG", abs)
	p, w, err := Open(context.Background(), testOptions(), nopEvents{})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p)
	if err := w.Wait(); !errors.Is(err, ErrFleecing) {
		t.Fatalf("Wait = %v, want fleecing error", err)
	}
}

func TestAbortStopsSource(t *testing.T) {
	fakeSSH(t)
	t.Setenv("FAKE_SSH_SLEEP", "60")
	start := time.Now()
	p, w, err := Open(context.Background(), testOptions(), nopEvents{})
	if err != nil {
		t.Fatal(err)
	}
	w.Abort()
	io.Copy(io.Discard, p)
	err = w.Wait()
	if time.Since(start) > 20*time.Second {
		t.Fatalf("abort did not stop the source (%v)", time.Since(start))
	}
	var se *Error
	if !errors.As(err, &se) || !errors.Is(err, ErrSourceFailed) || se.Transient {
		t.Fatalf("Wait after Abort = %v (%+v), want non-transient source failure", err, se)
	}
}

func TestProgramNotOnPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	_, _, err := Open(context.Background(), testOptions(), nopEvents{})
	var se *Error
	if !errors.As(err, &se) || !errors.Is(err, ErrSourceFailed) || se.Transient {
		t.Fatalf("Open = %v, want non-transient source failure", err)
	}
}

func TestDefaultProgramIsResolvedFromPath(t *testing.T) {
	// a different program name is resolved from PATH as well
	dir := t.TempDir()
	b, _ := os.ReadFile(filepath.Join("testdata", "fake-ssh.sh"))
	if err := os.WriteFile(filepath.Join(dir, "my-ssh"), b, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	t.Setenv("FAKE_SSH_ARGS", "")
	o := testOptions()
	o.SSHProgram = "my-ssh"
	p, w, err := Open(context.Background(), o, nopEvents{})
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, p)
	if err := w.Wait(); err != nil {
		t.Fatal(err)
	}
	if DefaultProgram != "ssh" {
		t.Fatalf("DefaultProgram = %q", DefaultProgram)
	}
}
