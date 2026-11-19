package facts

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// now is a Thursday; the local sources run Saturdays 05:00 UTC, the guests Sundays 00:00 UTC.
var now = time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)

const testConfig = `
[repository]
base = "rclone:drive:backups"

[[source]]
name = "wiki"
type = "paths"
paths = ["/data/wiki"]

[[source]]
name = "sites"
type = "paths"
paths = ["/data/sites/*"]

[retention]
keep_weekly = 8
keep_monthly = 6
keep_yearly = 2

[alerts]
stale_days = 9

[schedule]
local = "0 5 * * 6"
guests = "0 0 * * 0"
guest_restore_test = "0 0 15 * *"

[[source]]
name = "proxmox"
type = "proxmox"
host = "192.0.2.10"
keep_last = 3

[[source.guest]]
vmid = 100
alias = "web"
description = "web server"

[[source.guest]]
vmid = 101
alias = "db"
keep_last = 5
`

// machine is a state root with the config and an environment that points at both.
type machine struct {
	t     *testing.T
	state string
	vars  map[string]string
	calls []Call
	// answers per program and first argument
	answer func(Call) Result
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{t: t, state: t.TempDir()}
	cfg := filepath.Join(t.TempDir(), "backup.toml")
	write(t, cfg, testConfig)
	m.vars = map[string]string{StateEnv: m.state, ConfigEnv: cfg, PasswordEnv: "correct horse battery staple 42",
		StartHintEnv: "./backup.sh <source>"}
	m.answer = func(c Call) Result { return Result{Stdout: []byte("[]")} }
	return m
}

func (m *machine) env() Env {
	var environ []string
	for k, v := range m.vars {
		environ = append(environ, k+"="+v)
	}
	environ = append(environ, "PATH=/usr/bin", "RCLONE_CONFIG=/x/rclone.conf")
	return Env{Getenv: func(k string) string { return m.vars[k] }, Environ: environ, Now: now, Version: "v9.9.9",
		Run: func(c Call) Result { m.calls = append(m.calls, c); return m.answer(c) }}
}

func write(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// hold takes the flock of path as a running job does, until the test ends.
func hold(t *testing.T, path string) {
	t.Helper()
	write(t, path, "")
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
}

func TestConfig(t *testing.T) {
	m := newMachine(t)
	c, err := LoadConfig(m.vars[ConfigEnv], "")
	if err != nil {
		t.Fatal(err)
	}
	if c.Base != "rclone:drive:backups" || c.Remote() != "drive" || c.Repository("wiki") != "rclone:drive:backups/wiki" {
		t.Errorf("base %q remote %q", c.Base, c.Remote())
	}
	if strings.Join(c.Files, ",") != "wiki,sites" || len(c.Proxmox) != 1 || c.Proxmox[0].Name != "proxmox" {
		t.Errorf("sources %v %v", c.Files, c.Proxmox)
	}
	if g := c.Proxmox[0].Guests; len(g) != 2 || g[0] != (Guest{100, "web", 3}) || g[1] != (Guest{101, "db", 5}) {
		t.Errorf("guests %+v", g)
	}
	if c.Retention.KeepWords() != "8 weekly, 6 monthly, 2 yearly" || c.StaleDays != 9 {
		t.Errorf("keep %q stale %d", c.Retention.KeepWords(), c.StaleDays)
	}
	if c.Interval(ProfileLocal, now) != 7*24*time.Hour || c.Interval(ProfileRestoreTest, now) != 31*24*time.Hour {
		t.Errorf("intervals %v %v", c.Interval(ProfileLocal, now), c.Interval(ProfileRestoreTest, now))
	}
	// the environment overrides the base; without [schedule] the interval is stale_days
	c, _ = LoadConfig(m.vars[ConfigEnv], "/srv/repos")
	if c.Base != "/srv/repos" || c.Remote() != "" {
		t.Errorf("override: %q %q", c.Base, c.Remote())
	}
	write(t, m.vars[ConfigEnv], strings.Split(testConfig, "[schedule]")[0])
	c, _ = LoadConfig(m.vars[ConfigEnv], "")
	if c.Interval(ProfileLocal, now) != 9*24*time.Hour {
		t.Errorf("no schedule: %v", c.Interval(ProfileLocal, now))
	}
	// broken files say what to fix
	for text, want := range map[string]string{
		"[repository\n": "not valid TOML",
		"[repository]\nbase = \"/r\"\n[schedule]\nlocal = \"*/5 * * * *\"\n": "the part \"*/5\" is not allowed",
		"[[source]]\nname = \"a\"\n":                                         "has no [repository] base",
	} {
		write(t, m.vars[ConfigEnv], text)
		_, err := LoadConfig(m.vars[ConfigEnv], "")
		if _, ok := err.(*ConfigError); !ok || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: %v", text, err)
		}
	}
	if _, err := LoadConfig(filepath.Join(m.state, "none.toml"), ""); err == nil || !strings.Contains(err.Error(),
		"cannot read the config") || !strings.Contains(err.Error(), "BACKUP_CONFIG=/app/config/backup.toml") {
		t.Errorf("missing file: %v", err)
	}
}

func TestCron(t *testing.T) {
	for _, c := range []struct {
		line, next string
		every      time.Duration
	}{
		{"0 5 * * 6", "2026-11-21T05:00:00Z", 7 * 24 * time.Hour},
		{"0 0 * * 0", "2026-11-22T00:00:00Z", 7 * 24 * time.Hour},
		{"0 0 15 * *", "2026-12-15T00:00:00Z", 31 * 24 * time.Hour},
		{"30 1,13 * * 1-5", "2026-11-19T13:30:00Z", 12 * time.Hour},
		{"0 10 * * *", "2026-11-20T10:00:00Z", 24 * time.Hour},    // strictly after now
		{"0 0 1 * 1", "2026-11-23T00:00:00Z", 7 * 24 * time.Hour}, // day of month or Monday, as cron
	} {
		cr, err := ParseCron(c.line)
		if err != nil {
			t.Fatal(c.line, err)
		}
		if got := cr.Next(now).Format(time.RFC3339); got != c.next {
			t.Errorf("%s: next %s, want %s", c.line, got, c.next)
		}
		if got := cr.Interval(now); got != c.every {
			t.Errorf("%s: interval %v, want %v", c.line, got, c.every)
		}
	}
	for _, bad := range []string{"0 5 * *", "*/5 * * * *", "0 5 * * sat", "0 24 * * *", "5-1 * * * *", "0 0 * * 7"} {
		if _, err := ParseCron(bad); err == nil || !strings.Contains(err.Error(), "for example \"0 5 * * 6\"") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	never, _ := ParseCron("0 0 31 2 *")
	if !never.Next(now).IsZero() || never.Interval(now) != 0 {
		t.Error("31 February fires")
	}
}

func TestMissingVariable(t *testing.T) {
	m := newMachine(t)
	for _, v := range []string{StateEnv, ConfigEnv} {
		delete(m.vars, v)
		_, err := Read(m.env())
		if me, ok := err.(*MissingError); !ok || me.Var != v || !strings.Contains(err.Error(), "for example "+v+"=/") {
			t.Errorf("%s: %v", v, err)
		}
		m = newMachine(t)
	}
	for v, ex := range map[string]string{PasswordEnv: "RESTIC_PASSWORD=<password> in .env", KeysEnv: "RESTIC_PVE_KEYS=/config",
		StartHintEnv: "RESTIC_PVE_START_HINT='./backup.sh <source>'"} {
		err := Require(func(string) string { return "" }, v)
		if err == nil || !strings.Contains(err.Error(), ex) || !strings.Contains(err.Error(), "Nothing was changed") {
			t.Errorf("%s: %v", v, err)
		}
	}
}

// records writes both records files and the job records of a machine in the middle of a guest run.
func (m *machine) records() {
	t := m.t
	write(t, filepath.Join(m.state, RecordsFile), `{"schema": 1, "restore_test": {"ts": "2026-11-01T03:10:00Z", "status": "failed", "unit": null, "snapshot_id": null},
 "checks": {"wiki": {"ts": "2026-11-14T05:50:12Z", "ok": true}, "sites": {"ts": "2026-11-14T05:52:00Z", "ok": false}},
 "repo_sizes": {"wiki": {"ts": "2026-11-14T05:55:00Z", "bytes": 1073741824}},
 "runs": [{"run_id": "r1", "ended": "2026-11-14T05:55:00Z", "status": "success", "data_added": 1000},
          {"run_id": "r2", "ended": "2026-11-07T05:55:00Z", "status": "partial", "data_added": 5000}],
 "later": {"unknown": "keys are ignored"}}`)
	write(t, filepath.Join(m.state, GuestsDir, RecordsFile), `{"schema": 1,
 "checks": {"proxmox": {"ts": "2026-11-16T03:00:00Z", "ok": true}},
 "restore_test": {"ts": "2026-11-15T03:10:00Z", "status": "success", "unit": "proxmox/web", "snapshot_id": "0a1b2c3d"},
 "drill": {"ts": "2026-11-01T00:00:00Z"},
 "repo_sizes": {"proxmox": {"ts": "2026-11-16T02:00:00Z", "bytes": 53687091200}},
 "runs": [{"run_id": "g1", "ended": "2026-11-16T02:00:00Z", "status": "success", "data_added": 9000}]}`)
	jobs := filepath.Join(m.state, JobsDir)
	write(t, filepath.Join(jobs, "local.json"), `{"schema": 1, "name": "local", "started": "2026-11-14T05:00:00Z", "ended": "2026-11-14T05:55:10Z", "exit": 3, "run_id": "r1"}`)
	write(t, filepath.Join(jobs, "guests.json"), `{"schema": 1, "name": "guests", "started": "2026-11-19T09:00:00Z", "ended": null, "exit": null, "run_id": null}`)
	hold(t, filepath.Join(jobs, "guests.lock"))
	write(t, filepath.Join(jobs, "wiki.json"), `{"schema": 1, "name": "wiki", "started": "2026-11-18T08:00:00Z", "ended": null, "exit": null, "run_id": null}`)
	write(t, filepath.Join(jobs, "broken.json"), `{"schema": 1, "name": `)
}

func TestLocalFacts(t *testing.T) {
	m := newMachine(t)
	m.records()
	c, _ := LoadConfig(m.vars[ConfigEnv], "")
	f := Local(m.env(), c)
	if len(m.calls) != 0 {
		t.Fatalf("local facts called %v", m.calls)
	}
	if len(f.Running) != 1 || f.Running[0].Name != "guests" || !f.Running[0].Started.Equal(time.Date(2026, 11, 19, 9, 0, 0, 0, time.UTC)) {
		t.Errorf("running %+v", f.Running)
	}
	if len(f.LastJobs) != 2 || f.LastJobs[0].Name != "wiki" || !f.LastJobs[0].NoResult() ||
		f.LastJobs[1].Name != "local" || *f.LastJobs[1].Exit != 3 || f.LastJobs[1].RunID != "r1" {
		t.Errorf("last jobs %+v", f.LastJobs)
	}
	want := []NextRun{{ProfileLocal, time.Date(2026, 11, 21, 5, 0, 0, 0, time.UTC)}, {ProfileGuests, time.Date(2026, 11, 22, 0, 0, 0, 0, time.UTC)}}
	if fmt.Sprint(f.Next) != fmt.Sprint(want) {
		t.Errorf("next %v", f.Next)
	}
	if f.RestoreTest == nil || f.RestoreTest.Unit != "proxmox/web" || f.RestoreTest.Status != "success" {
		t.Errorf("restore test %+v", f.RestoreTest)
	}
	if len(f.Space.Runs) != 3 || f.Space.Runs[0].RunID != "r2" || f.Space.Runs[2].RunID != "g1" ||
		f.Space.RepoSizes["proxmox"].Bytes != 53687091200 || f.Space.Remote != "drive" {
		t.Errorf("space %+v", f.Space)
	}
	if f.Alerts || f.Proof != nil || len(f.Errors) != 0 || f.Drill != nil {
		t.Errorf("alerts %v proof %v errors %v", f.Alerts, f.Proof, f.Errors)
	}
	// a run lock without a job record is a running job too; the webhook turns alerts on
	os.RemoveAll(filepath.Join(m.state, JobsDir))
	hold(t, filepath.Join(m.state, RunLock))
	write(t, filepath.Join(m.state, StatusFile), `{"run_id": "r3", "started": "2026-11-19T08:00:00.123456Z", "run_status": "running"}`)
	m.vars[WebhookEnv] = "https://discord.example/api/webhooks/1/x"
	f = Local(m.env(), c)
	if len(f.Running) != 1 || f.Running[0].Name != "local" || f.Running[0].Started.Hour() != 8 || !f.Alerts {
		t.Errorf("run lock: %+v alerts %v", f.Running, f.Alerts)
	}
	// a records file that cannot be read is one fact error with what happens next
	write(t, filepath.Join(m.state, RecordsFile), `{"schema": 1, "checks": [`)
	write(t, filepath.Join(m.state, GuestsDir, RecordsFile), `{"schema": 2}`)
	f = Local(m.env(), c)
	if len(f.Errors) != 2 || !strings.Contains(f.Errors[0].Text, "not readable") || !strings.Contains(f.Errors[1].Text, "this version of restic-pve is too old for it") ||
		!strings.Contains(f.Errors[0].Text, "the next run writes it again") {
		t.Errorf("errors %+v", f.Errors)
	}
}

func TestLockProbeCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	if Locked(filepath.Join(dir, "run.lock")) {
		t.Error("a missing lock file is held")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Errorf("the probe created %v", entries)
	}
	write(t, filepath.Join(dir, "free.lock"), "")
	if Locked(filepath.Join(dir, "free.lock")) {
		t.Error("a free lock is held")
	}
	hold(t, filepath.Join(dir, "held.lock"))
	if !Locked(filepath.Join(dir, "held.lock")) {
		t.Error("a held lock is free")
	}
}

func TestProofDate(t *testing.T) {
	m := newMachine(t)
	if ProofDate(m.state, m.vars[PasswordEnv]) != nil {
		t.Error("proof without a marker")
	}
	// the bytes backupctl's setup writes: the hex fingerprint and a newline
	write(t, filepath.Join(m.state, MarkerFile), Fingerprint([]byte(m.vars[PasswordEnv]))+"\n")
	// the same value as Python's hashlib.sha256(b"restic-backup escrow:" + password).hexdigest()
	if Fingerprint([]byte(m.vars[PasswordEnv])) != "66894fca55add2fe2510f9af55542a858596e262e8297c7da597a1028261a425" {
		t.Errorf("fingerprint %s", Fingerprint([]byte(m.vars[PasswordEnv])))
	}
	if p := ProofDate(m.state, m.vars[PasswordEnv]); p == nil {
		t.Error("valid marker not seen")
	}
	if ProofDate(m.state, "another password") != nil || ProofDate(m.state, "") != nil {
		t.Error("a marker of another password counts")
	}
}
