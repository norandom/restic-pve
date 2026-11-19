package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/role"
)

const viewPassword = "a-long-test-password-SECRET-1234567890"

const viewConfig = `
[repository]
base = "rclone:drive:backups"
[[source]]
name = "wiki"
type = "paths"
paths = ["/data/wiki"]
[retention]
keep_weekly = 8
[schedule]
local = "0 5 * * 6"
guests = "0 0 * * 0"
[[source]]
name = "proxmox"
type = "proxmox"
host = "192.0.2.10"
[[source.guest]]
vmid = 100
alias = "web"
`

// viewMachine is a backup machine without a terminal: a state root, a config, a key folder
// and a stand-in storage whose newest copies are fresh as of now.
type viewMachine struct {
	t     *testing.T
	vars  map[string]string
	calls []facts.Call
	snaps string // the JSON restic gives for every repository
	about string
	code  int // restic's exit code
}

func newViewMachine(t *testing.T) *viewMachine {
	t.Helper()
	state, cfgDir := t.TempDir(), t.TempDir()
	cfg := filepath.Join(cfgDir, "backup.toml")
	os.WriteFile(cfg, []byte(viewConfig), 0o644)
	recent := time.Now().UTC().Add(-26 * time.Hour).Format(time.RFC3339)
	os.WriteFile(filepath.Join(state, "records.json"), []byte(`{"schema": 1, "checks": {"wiki": {"ts": "`+
		time.Now().UTC().Add(-25*time.Hour).Format(time.RFC3339)+`", "ok": true}}, "runs": [{"run_id": "r1", "ended": "`+recent+
		`", "status": "success", "data_added": 1073741824}]}`), 0o644)
	m := &viewMachine{t: t, vars: map[string]string{
		facts.StateEnv: state, facts.ConfigEnv: cfg, facts.PasswordEnv: viewPassword, facts.KeysEnv: t.TempDir(),
		facts.StartHintEnv: "./backup.sh <source>", facts.WebhookEnv: "https://discord.example/api/webhooks/1/SECRET-HOOK"},
		snaps: `[{"time": "` + recent + `", "tags": ["instance=wiki"]}, {"time": "` + recent + `", "tags": ["instance=web", "kind=guest-image"]}]`,
		about: `{"total": 2199023255552, "free": 879609302221}`}
	return m
}

func (m *viewMachine) env(out, errb *bytes.Buffer) env {
	e := sys(role.Backup, out, errb)
	e.getenv = func(k string) string { return m.vars[k] }
	e.environ = func() []string {
		var l []string
		for k, v := range m.vars {
			l = append(l, k+"="+v)
		}
		return append(l, "PATH=/usr/bin")
	}
	e.runner = func(c facts.Call) facts.Result {
		m.calls = append(m.calls, c)
		if c.Name == "rclone" {
			return facts.Result{Stdout: []byte(m.about)}
		}
		if m.code != 0 {
			return facts.Result{Code: m.code, Stderr: []byte("Fatal: wrong password or no key found")}
		}
		return facts.Result{Stdout: []byte(m.snaps)}
	}
	return e
}

func (m *viewMachine) run(args ...string) (int, string, string) {
	var out, errb bytes.Buffer
	code := run(args, m.env(&out, &errb))
	return code, out.String(), errb.String()
}

// noSecret checks every output, every written file and every child argv.
func (m *viewMachine) noSecret(outs ...string) {
	m.t.Helper()
	all := strings.Join(outs, "\n")
	filepath.Walk(m.vars[facts.StateEnv], func(p string, fi os.FileInfo, err error) error {
		if err == nil && !fi.IsDir() {
			b, _ := os.ReadFile(p)
			all += "\n" + string(b)
		}
		return nil
	})
	for _, c := range m.calls {
		all += "\n" + strings.Join(c.Args, " ")
		for _, kv := range c.Env {
			if strings.Contains(kv, "SECRET-HOOK") {
				m.t.Errorf("%s got the webhook", c.Name)
			}
			if c.Name == "rclone" && strings.Contains(kv, viewPassword) {
				m.t.Error("rclone got the repository password")
			}
		}
	}
	for _, s := range []string{viewPassword, "SECRET-HOOK"} {
		if strings.Contains(all, s) {
			m.t.Errorf("a secret value appears: %q", s)
		}
	}
}

func TestStatusPlain(t *testing.T) {
	m := newViewMachine(t)
	code, out, errb := m.run("status")
	if code != 0 || errb != "" || !strings.Contains(out, "copy  wiki  yes  1d") || !strings.Contains(out, "copy  web  yes  1d") ||
		!strings.HasSuffix(out, "verdict  safe\n") || !strings.Contains(out, "alerts  on") {
		t.Fatalf("safe: %d %q\n%s", code, errb, out)
	}
	m.noSecret(out, errb)
	// the plain form also with --plain on a terminal, with TERM=dumb, and below 60 x 20
	for _, c := range []struct {
		args []string
		vars map[string]string
		size [2]int
	}{{[]string{"status", "--plain"}, nil, [2]int{80, 24}}, {[]string{"status"}, map[string]string{"TERM": "dumb"}, [2]int{80, 24}},
		{[]string{"status"}, nil, [2]int{59, 24}}, {[]string{"status"}, nil, [2]int{80, 19}}} {
		for k, v := range c.vars {
			m.vars[k] = v
		}
		var o, eb bytes.Buffer
		e := m.env(&o, &eb)
		e.tty = true
		e.size = func() (int, int, error) { return c.size[0], c.size[1], nil }
		e.newScreen = func() (tcell.Screen, error) { t.Fatalf("%v: a full screen", c); return nil, nil }
		if code := run(c.args, e); code != 0 || !strings.HasSuffix(o.String(), "verdict  safe\n") {
			t.Errorf("%v %v: %d %q", c.args, c.size, code, o.String())
		}
		delete(m.vars, "TERM")
	}
	// one copy too old: not safe, exit 1, the action from the start hint
	m.snaps = `[{"time": "2026-01-01T00:00:00Z", "tags": ["instance=wiki"]}]`
	os.Remove(filepath.Join(m.vars[facts.StateEnv], facts.CacheFile))
	code, out, _ = m.run("status")
	if code != 1 || !strings.Contains(out, "copy  web  no  -  no copy  do: ./backup.sh web") || !strings.HasSuffix(out, "verdict  not-safe\n") {
		t.Errorf("not safe: %d\n%s", code, out)
	}
	// the wrong password: not known, with the words of 8.8
	m.code = 12
	os.Remove(filepath.Join(m.vars[facts.StateEnv], facts.CacheFile))
	code, out, errb = m.run("status")
	if code != 1 || !strings.Contains(out, "problem  The password in use does not open rclone:drive:backups/wiki.") ||
		!strings.Contains(out, "never make a new password") {
		t.Errorf("wrong password: %d\n%s", code, out)
	}
	m.noSecret(out, errb)
}

func TestScreenCommandCodes(t *testing.T) {
	m := newViewMachine(t)
	for _, args := range [][]string{{"status", "--plain", "x"}, {"status", "--json"}, {"keys", "x"}, {"space", "--plain", "--check"}, {"space", "-x"}} {
		if code, out, errb := m.run(args...); code != 64 || out != "" || !strings.HasPrefix(errb, "usage: restic-pve "+args[0]) {
			t.Errorf("%q: %d %q %q", args, code, out, errb)
		}
	}
	// a missing variable: exit 78, the variable named with an example; nothing written
	for cmd, v := range map[string]string{"status": facts.StartHintEnv, "keys": facts.KeysEnv, "space": facts.ConfigEnv} {
		old := m.vars[v]
		delete(m.vars, v)
		before := listing(t, m.vars[facts.StateEnv])
		code, _, errb := m.run(cmd)
		if code != 78 || !strings.Contains(errb, v+" is not set") || !strings.Contains(errb, "for example "+v+"=") {
			t.Errorf("%s without %s: %d %q", cmd, v, code, errb)
		}
		if listing(t, m.vars[facts.StateEnv]) != before {
			t.Errorf("%s wrote", cmd)
		}
		m.vars[v] = old
	}
	// a config that cannot be read: 78 too
	os.WriteFile(m.vars[facts.ConfigEnv], []byte("[repository\n"), 0o644)
	if code, _, errb := m.run("status"); code != 78 || !strings.Contains(errb, "not valid TOML") {
		t.Errorf("bad config: %d %q", code, errb)
	}
}

// TestRoleGuardViews: on a Proxmox host the backup-role screens are refused and change nothing.
func TestRoleGuardViews(t *testing.T) {
	m := newViewMachine(t)
	for _, cmd := range []string{"status", "keys", "space", "watch"} {
		var out, errb bytes.Buffer
		e := m.env(&out, &errb)
		e.role = role.Host
		before := listing(t, m.vars[facts.StateEnv])
		if code := run([]string{cmd}, e); code != 64 || !strings.Contains(errb.String(),
			"restic-pve "+cmd+" is for the backup machine; this is the Proxmox host. Nothing was changed.") {
			t.Errorf("%s on the host: %d %q", cmd, code, errb.String())
		}
		if len(m.calls) != 0 || listing(t, m.vars[facts.StateEnv]) != before || out.Len() != 0 {
			t.Errorf("%s on the host did something", cmd)
		}
		e.role = role.None
		errb.Reset()
		if code := run([]string{cmd}, e); code != 78 || !strings.Contains(errb.String(), "RESTIC_PVE_STATE=/state") {
			t.Errorf("%s without a role: %d %q", cmd, code, errb.String())
		}
	}
	var out, errb bytes.Buffer
	e := m.env(&out, &errb)
	run(nil, e)
	if !strings.Contains(errb.String(), "status [--plain] | keys [--plain] | space [--plain|--check] | watch [--once]") {
		t.Errorf("usage %q", errb.String())
	}
}

func TestSpaceCheck(t *testing.T) {
	m := newViewMachine(t)
	code, out, _ := m.run("space", "--check")
	if code != 0 || out != "space ok: 819 GiB free of 2 TiB, the largest recent run added 1 GiB\n" {
		t.Errorf("ok: %d %q", code, out)
	}
	// --check asks rclone only, never restic
	if len(m.calls) != 1 || m.calls[0].Name != "rclone" || strings.Join(m.calls[0].Args, " ") != "about drive: --json" {
		t.Errorf("calls %+v", m.calls)
	}
	m.about = `{"total": 2199023255552, "free": 1073741824}`
	if code, out, _ := m.run("space", "--check"); code != 1 || !strings.HasPrefix(out, "space low: 1 GiB free, the largest recent run added 1 GiB; lower the keep rules") {
		t.Errorf("low: %d %q", code, out)
	}
	m.about = `{}`
	if code, out, _ := m.run("space", "--check"); code != 2 || !strings.HasPrefix(out, "space not known") {
		t.Errorf("unknown: %d %q", code, out)
	}
	// the space view in plain lines
	m.about = `{"total": 2199023255552, "free": 879609302221}`
	code, out, errb := m.run("space")
	if code != 0 || !strings.Contains(out, "storage  total 2 TiB  used not reported  free 819 GiB") ||
		!strings.Contains(out, "note  Do not delete or move files inside the backup folder") {
		t.Errorf("plain: %d %q", code, out)
	}
	m.noSecret(out, errb)
}

func TestKeysPlain(t *testing.T) {
	m := newViewMachine(t)
	code, out, errb := m.run("keys")
	if code != 0 || !strings.Contains(out, "key  Repository password  set  copy-away: yes") ||
		!strings.HasSuffix(out, "proof  The proof needs a terminal, so that the password is not read from a pipe. Run restic-pve keys in an interactive shell on the backup machine. Nothing was written.\n") {
		t.Errorf("%d %q\n%s", code, errb, out)
	}
	if len(m.calls) != 0 {
		t.Errorf("keys called %v", m.calls)
	}
	m.noSecret(out, errb)
}

// TestStatusScreenCode: on a terminal the status screen opens on the screen it is given, q
// leaves, the verdict line is printed after it, and the exit code is the plain form's.
func TestStatusScreenCode(t *testing.T) {
	m := newViewMachine(t)
	var out, errb bytes.Buffer
	e := m.env(&out, &errb)
	e.tty = true
	e.size = func() (int, int, error) { return 80, 24, nil }
	scr := tcell.NewSimulationScreen("UTF-8")
	e.newScreen = func() (tcell.Screen, error) { return scr, scr.Init() }
	go func() {
		for i := 0; i < 400; i++ {
			time.Sleep(10 * time.Millisecond)
			cells, w, _ := scr.GetContents()
			var b strings.Builder
			for _, c := range cells[:min(len(cells), w*24)] {
				b.WriteString(string(c.Runes))
			}
			if strings.Contains(b.String(), "checked just now") {
				break
			}
		}
		scr.InjectKey(tcell.KeyRune, 'q', 0)
	}()
	if code := run([]string{"status"}, e); code != 0 || out.String() != "All copies are recent and checked.\n" {
		t.Errorf("%d %q %q", code, out.String(), errb.String())
	}
	m.noSecret(out.String(), errb.String())
}
