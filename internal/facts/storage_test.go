package facts

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// snapshots is the JSON of restic snapshots for the test repositories.
var snapshots = map[string]string{
	"rclone:drive:backups/wiki": `[
 {"time": "2026-11-07T05:40:00.5+00:00", "tags": ["instance=wiki"], "paths": ["/data/wiki"], "id": "a1"},
 {"time": "2026-11-14T05:41:00Z", "tags": ["instance=wiki"], "id": "a2"},
 {"time": "2026-11-18T05:41:00Z", "tags": ["instance=wiki"], "id": "a3"}]`,
	"rclone:drive:backups/sites": `[
 {"time": "2026-11-14T05:20:00Z", "tags": ["instance=blog"]},
 {"time": "2026-11-14T05:30:00Z", "tags": ["instance=shop"]}]`,
	"rclone:drive:backups/proxmox": `[
 {"time": "2026-11-15T01:00:00Z", "tags": ["instance=web", "kind=guest-image"], "summary": {"total_bytes_processed": 34359738368}},
 {"time": "2026-11-15T01:30:00Z", "tags": ["instance=web", "kind=manifest"]},
 {"time": "2026-11-17T01:00:00Z", "tags": ["instance=web", "kind=guest-image"], "summary": {"total_bytes_processed": 34359738369}}]`,
}

const aboutJSON = `{"total": 2199023255552, "used": 1319413953331, "free": 879609302221, "trashed": 1024}`

// storage answers like a reachable Google Drive with the test repositories.
func (m *machine) storage() {
	m.answer = func(c Call) Result {
		switch {
		case c.Name == "restic" && c.Args[0] == "snapshots":
			return Result{Stdout: []byte(snapshots[c.Args[4]])}
		case c.Name == "rclone" && c.Args[0] == "about":
			return Result{Stdout: []byte(aboutJSON)}
		}
		return Result{Code: 1, Stderr: []byte("unexpected")}
	}
}

func byName(f Facts) map[string]Instance {
	out := map[string]Instance{}
	for _, in := range f.Instances {
		out[in.Name] = in
	}
	return out
}

func TestStorageFacts(t *testing.T) {
	m := newMachine(t)
	m.records()
	m.storage()
	f, err := Read(m.env())
	if err != nil {
		t.Fatal(err)
	}
	// one snapshot list per repository, one about, nothing else; no secret in any argv
	var argv []string
	for _, c := range m.calls {
		argv = append(argv, c.Name+" "+strings.Join(c.Args, " "))
		if strings.Contains(strings.Join(c.Args, " "), m.vars[PasswordEnv]) {
			t.Errorf("password in argv %q", c.Args)
		}
		if c.Timeout <= 0 {
			t.Errorf("no time-out: %v", c.Args)
		}
		pw := false
		for _, kv := range c.Env {
			pw = pw || kv == PasswordEnv+"="+m.vars[PasswordEnv]
			if strings.HasPrefix(kv, StateEnv) || strings.HasPrefix(kv, WebhookEnv) {
				t.Errorf("%s gets %s", c.Name, kv)
			}
		}
		if pw != (c.Name == "restic") {
			t.Errorf("%s: password in env %v", c.Name, pw)
		}
	}
	rc := " -o rclone.program=rclone -o rclone.args=serve restic --stdio --tpslimit 8 --drive-chunk-size 64M --drive-use-trash=false --low-level-retries 20"
	want := []string{"restic snapshots --json --no-lock -r rclone:drive:backups/wiki" + rc,
		"restic snapshots --json --no-lock -r rclone:drive:backups/sites" + rc,
		"restic snapshots --json --no-lock -r rclone:drive:backups/proxmox" + rc,
		"rclone about drive: --json"}
	sort.Strings(argv) // read in parallel: the order of the calls is not fixed
	sort.Strings(want)
	if strings.Join(argv, "\n") != strings.Join(want, "\n") {
		t.Errorf("calls:\n%s", strings.Join(argv, "\n"))
	}
	in := byName(f)
	ts := func(p *time.Time) string {
		if p == nil {
			return "-"
		}
		return p.Format(time.RFC3339)
	}
	for name, w := range map[string][3]string{
		// newest, newest checked (the check passed at 2026-11-14T05:50)
		"wiki":       {"2026-11-18T05:41:00Z", "2026-11-14T05:41:00Z", "files"},
		"sites/blog": {"2026-11-14T05:20:00Z", "-", "files"}, // its check failed
		"sites/shop": {"2026-11-14T05:30:00Z", "-", "files"},
		"web":        {"2026-11-17T01:00:00Z", "2026-11-15T01:00:00Z", "guest"},
		"db":         {"-", "-", "guest"},
	} {
		got := in[name]
		if ts(got.Newest) != w[0] || ts(got.NewestChecked) != w[1] || got.Kind != w[2] || !got.Known {
			t.Errorf("%s: %s %s %s known %v", name, ts(got.Newest), ts(got.NewestChecked), got.Kind, got.Known)
		}
	}
	if len(f.Instances) != 5 || in["sites/blog"].Check == nil || in["sites/blog"].Check.OK || in["web"].Interval != 7*24*time.Hour {
		t.Errorf("instances %+v", f.Instances)
	}
	if s := in["web"].Size; s == nil || *s != 34359738369 || f.Space.GuestSize["db"] != nil {
		t.Errorf("guest sizes %v %v", in["web"].Size, f.Space.GuestSize)
	}
	a := f.Space.About
	if a == nil || *a.Total != 2199023255552 || *a.Free != 879609302221 || a.Other != nil || *a.Trashed != 1024 {
		t.Errorf("about %+v", a)
	}
	if !f.CheckedAt.Equal(now) || f.FromCache || f.Deferred || len(f.Errors) != 0 {
		t.Errorf("checked %v cache %v deferred %v errors %v", f.CheckedAt, f.FromCache, f.Deferred, f.Errors)
	}

	// the cache holds no secret and no path; a second read within six hours calls nothing
	b, err := os.ReadFile(filepath.Join(m.state, CacheFile))
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{m.vars[PasswordEnv], "/data/wiki", "a1", "192.0.2.10"} {
		if strings.Contains(string(b), secret) {
			t.Errorf("cache holds %q", secret)
		}
	}
	m.calls = nil
	e := m.env()
	e.Now = now.Add(5 * time.Hour)
	f2, _ := Read(e)
	if len(m.calls) != 0 || !f2.FromCache || !f2.CheckedAt.Equal(now) || len(f2.Instances) != 5 || f2.Space.About == nil {
		t.Errorf("fresh cache: calls %v from cache %v", m.calls, f2.FromCache)
	}
	// r reads again
	e.Refresh = true
	if Read(e); len(m.calls) != 4 {
		t.Errorf("refresh: %d calls", len(m.calls))
	}
	// older than six hours: read again, except while a backup runs (the guest job holds its
	// lock): then the stale cache is shown and the read waits
	m.calls = nil
	e = m.env()
	e.Now = now.Add(12 * time.Hour)
	f3, _ := Read(e)
	if len(m.calls) != 0 || !f3.Deferred || !f3.FromCache {
		t.Errorf("running backup: %d calls, deferred %v", len(m.calls), f3.Deferred)
	}
	os.Remove(filepath.Join(m.state, JobsDir, "guests.json"))
	if Read(e); len(m.calls) != 4 {
		t.Errorf("stale cache: %d calls", len(m.calls))
	}
	// a newer records file makes the cache stale too
	m.calls = nil
	e.Now = now.Add(13 * time.Hour)
	later := e.Now.Add(-time.Minute)
	os.Chtimes(filepath.Join(m.state, GuestsDir, RecordsFile), later, later)
	if Read(e); len(m.calls) != 4 {
		t.Errorf("newer records: %d calls", len(m.calls))
	}
}

func TestStorageProblems(t *testing.T) {
	m := newMachine(t)
	m.storage()
	ok := m.answer
	// wrong password on one repository, about not supported, no repository yet on another
	m.answer = func(c Call) Result {
		switch {
		case c.Name == "restic" && strings.HasSuffix(c.Args[4], "/sites"):
			return Result{Code: 12, Stderr: []byte("Fatal: wrong password or no key found\n")}
		case c.Name == "restic" && strings.HasSuffix(c.Args[4], "/proxmox"):
			return Result{Code: 10, Stderr: []byte("Fatal: repository does not exist\n")}
		case c.Name == "rclone":
			return Result{Code: 1, Stderr: []byte("\n2026/11/19 ERROR : about is not supported by this remote\n")}
		}
		return ok(c)
	}
	f, err := Read(m.env())
	if err != nil {
		t.Fatal(err)
	}
	in := byName(f)
	if in["sites"].Known || !strings.Contains(in["sites"].Problem, "never make a new password") {
		t.Errorf("sites %+v", in["sites"])
	}
	if !in["web"].Known || in["web"].Newest != nil || !in["wiki"].Known {
		t.Errorf("no repository yet: %+v", in["web"])
	}
	if len(f.Errors) != 1 || f.Errors[0].Text != WrongPasswordText("rclone:drive:backups/sites") {
		t.Errorf("errors %+v", f.Errors)
	}
	if f.Space.About != nil || f.Space.AboutErr != "rclone said: 2026/11/19 ERROR : about is not supported by this remote" {
		t.Errorf("about %+v %q", f.Space.About, f.Space.AboutErr)
	}
	if _, cached := LoadCache(m.state); cached {
		t.Error("a read with a failed repository was cached")
	}
	// about with some fields only; a remote that reports nothing; a base that is not rclone
	for answer, want := range map[string]string{`{"free": 5}`: "free only", `{}`: "nothing"} {
		m.answer = func(c Call) Result {
			if c.Name == "rclone" {
				return Result{Stdout: []byte(answer)}
			}
			return ok(c)
		}
		a, problem := ReadAbout("rclone:drive:backups", nil, m.env().Run)
		if problem != "" || (want == "nothing") != (a == nil) || a != nil && (a.Total != nil || *a.Free != 5) {
			t.Errorf("%s: %+v %q", want, a, problem)
		}
	}
	m.calls = nil
	if a, p := ReadAbout("/srv/repos", nil, m.env().Run); a != nil || p != "" || len(m.calls) != 0 {
		t.Errorf("local base: %v %q %v", a, p, m.calls)
	}
	// without the password the storage is not read, and the message names the variable
	m.calls = nil
	delete(m.vars, PasswordEnv)
	f, _ = Read(m.env())
	if len(m.calls) != 0 || len(f.Errors) != 1 || !strings.Contains(f.Errors[0].Text, "RESTIC_PASSWORD is not set") || f.Instances[0].Known {
		t.Errorf("no password: %v %+v", m.calls, f.Errors)
	}
}

// standIn writes an executable shell script called name into dir.
func standIn(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
		t.Fatal(err)
	}
}

// TestExec: the real runner, with stand-in programs: low CPU and I/O priority, the time-out,
// a missing program.
func TestExec(t *testing.T) {
	bin := t.TempDir()
	t.Setenv("PATH", bin)
	pidFile := filepath.Join(t.TempDir(), "pid")
	standIn(t, bin, "restic", "echo $$ > "+pidFile+"\nread x < /proc/$$/stat\necho \"$@\"\nwhile [ -e "+pidFile+" ]; do :; done\necho done >&2\nexit 3\n")
	done := make(chan Result)
	go func() {
		done <- Exec(Call{Name: "restic", Args: []string{"snapshots", "--json"}, Env: []string{"PATH=" + bin}, Timeout: 20 * time.Second})
	}()
	var pid int
	for i := 0; i < 2000 && pid == 0; i++ {
		b, _ := os.ReadFile(pidFile)
		pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		time.Sleep(5 * time.Millisecond)
	}
	if pid == 0 {
		t.Fatal("the stand-in did not start")
	}
	stat, _ := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	fields := strings.Fields(string(stat[strings.LastIndexByte(string(stat), ')')+2:]))
	prio, _, _ := unix.Syscall(unix.SYS_IOPRIO_GET, 1, uintptr(pid), 0)
	os.Remove(pidFile)
	r := <-done
	if fields[16] != "19" {
		t.Errorf("nice %s, want 19", fields[16])
	}
	if prio>>13 != 3 {
		t.Errorf("I/O class %d, want 3 (idle)", prio>>13)
	}
	if r.Err != nil || r.Code != 3 || string(r.Stdout) != "snapshots --json\n" || FirstLine(r.Stderr) != "done" ||
		r.Problem("restic") != "restic said: done" {
		t.Errorf("result %+v %q", r, r.Problem("restic"))
	}
	standIn(t, bin, "rclone", "exec /bin/sleep 30\n")
	start := time.Now()
	r = Exec(Call{Name: "rclone", Args: []string{"about", "drive:"}, Timeout: 200 * time.Millisecond})
	if !errors.Is(r.Err, ErrTimeout) || time.Since(start) > 10*time.Second || r.Problem("rclone") != "rclone did not answer in time" {
		t.Errorf("time-out: %+v %q after %v", r, r.Problem("rclone"), time.Since(start))
	}
	r = Exec(Call{Name: "restic-missing", Timeout: time.Second})
	if p := r.Problem("restic"); p != "restic is not installed here (not found in PATH); the backup machine's image has it, run this command there" {
		t.Errorf("missing: %q", p)
	}
}
