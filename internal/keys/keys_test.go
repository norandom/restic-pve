package keys

import (
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/retro"
)

const (
	password = "Tr0ub4dor-and-3-horses-staple-xyzzy"
	webhook  = "https://discord.example/api/webhooks/1/SECRET-WEBHOOK-VALUE"
	sshKey   = "-----BEGIN OPENSSH PRIVATE KEY----- SECRET-KEY-MATERIAL"
	token    = "SECRET-RCLONE-TOKEN-VALUE"
)

var secrets = []string{password, webhook, sshKey, token, "SECRET-RUNNER", "SECRET-OO"}

type machine struct {
	state, keys string
	vars        map[string]string
	cfg         facts.Config
}

func newMachine(t *testing.T) *machine {
	t.Helper()
	m := &machine{state: t.TempDir(), keys: t.TempDir()}
	rc := filepath.Join(m.keys, "rclone", "rclone.conf")
	mk(t, rc, "[drive]\ntype = drive\ntoken = {\"access_token\":\""+token+"\"}\n")
	mk(t, filepath.Join(m.keys, ProxmoxKey), sshKey)
	m.vars = map[string]string{facts.StateEnv: m.state, facts.KeysEnv: m.keys, facts.PasswordEnv: password,
		facts.RcloneConfigEnv: rc, facts.WebhookEnv: webhook, "RUNNER_TOKEN": "", "OPENOBSERVE_USER": "SECRET-OO",
		"OPENOBSERVE_PASSWORD": "SECRET-OO"}
	m.cfg = facts.Config{Base: "rclone:drive:backups", Files: []string{"wiki"},
		Proxmox: []facts.GuestSource{{Name: "proxmox", Guests: []facts.Guest{{VMID: 100, Alias: "web", KeepLast: 2}}}}}
	return m
}

func mk(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

func (m *machine) getenv(k string) string { return m.vars[k] }

func states(md Model) map[string]string {
	out := map[string]string{}
	for _, k := range md.Keys {
		out[k.Name] = k.State
	}
	return out
}

func noSecret(t *testing.T, where, text string) {
	t.Helper()
	for _, s := range secrets {
		if strings.Contains(text, s) {
			t.Errorf("%s shows a secret value %q", where, s)
		}
	}
}

func TestKeyTable(t *testing.T) {
	m := newMachine(t)
	md := Build(m.getenv, m.cfg)
	want := map[string]string{"Repository password": "set", "Storage sign-in": "present", "Proxmox access": "present",
		"Guest health": "not made yet (optional)", "Alert address": "set"}
	for k, v := range want {
		if states(md)[k] != v {
			t.Errorf("%s: %q, want %q", k, states(md)[k], v)
		}
	}
	if !md.Keys[0].Copy || md.Keys[1].Copy || md.Keys[0].IfLost != "Lost with its copy: nobody can read the backups" {
		t.Errorf("copy rule %+v", md.Keys[0])
	}
	if md.Keys[1].KeptIn != m.vars[facts.RcloneConfigEnv]+" [drive]" || md.Keys[2].KeptIn != filepath.Join(m.keys, "ssh/id_ed25519") {
		t.Errorf("kept in %q %q", md.Keys[1].KeptIn, md.Keys[2].KeptIn)
	}
	plain := strings.Join(md.Plain(), "\n")
	noSecret(t, "plain", plain)
	for _, w := range []string{"key  Repository password  set  copy-away: yes", "password-proof  not proven yet",
		"other  GitHub runner token  not set (empty after use)", "other  OpenObserve login  set", "note  Release signing key: belongs to the maintainer"} {
		if !strings.Contains(plain, w) {
			t.Errorf("plain lacks %q:\n%s", w, plain)
		}
	}
	// the screen at 80 x 24
	s := sim(t, 80, 24)
	sc := &Screen{Title: "restic-pve v9.9.9 · backup machine · Keys", Model: md, Back: "Back"}
	sc.Draw(s, retro.NewTheme(s, true))
	l := text(s)
	all := strings.Join(l, "\n")
	noSecret(t, "screen", all)
	for i, w := range map[int]string{1: "Your keys: only the first needs a copy away", 2: " Repository password  copy away: YES set · RESTIC_PASSWORD (.env)",
		3: "Encrypts every backup. Lost with its copy: nobody can read the backups.", 13: "Your password copy",
		14: "not proven yet", 17: "Also on this machine", 18: "Release signing key", 20: "Other secrets here, in .env and secrets/: GitHub runner token",
		23: "p Prove your password copy  q Back"} {
		if !strings.Contains(l[i], w) {
			t.Errorf("row %d lacks %q:\n%s", i, w, all)
		}
	}

	// missing pieces each show as such, still without values
	os.Remove(filepath.Join(m.keys, ProxmoxKey))
	mk(t, m.vars[facts.RcloneConfigEnv], "[other]\ntoken = "+token+"\n")
	m.vars[facts.PasswordEnv], m.vars[facts.WebhookEnv] = "", ""
	md = Build(m.getenv, m.cfg)
	want = map[string]string{"Repository password": "MISSING", "Storage sign-in": "MISSING: no section [drive]",
		"Proxmox access": "MISSING", "Alert address": "not set: alerts are off"}
	for k, v := range want {
		if states(md)[k] != v {
			t.Errorf("%s: %q, want %q", k, states(md)[k], v)
		}
	}
	if md.ProofWords(time.Time{}) != "not possible: RESTIC_PASSWORD is not set" {
		t.Errorf("proof words %q", md.ProofWords(time.Time{}))
	}
	noSecret(t, "plain", strings.Join(md.Plain(), "\n"))
	// no guests: the identities are not needed; a base without rclone needs no sign-in
	md = Build(m.getenv, facts.Config{Base: "/srv/repos", Files: []string{"wiki"}})
	if s := states(md); s["Proxmox access"] != "not needed: no guests" || s["Storage sign-in"] != "not used: the storage is not reached through rclone" {
		t.Errorf("no guests: %v", s)
	}
	delete(m.vars, facts.KeysEnv)
	if s := states(Build(m.getenv, m.cfg)); s["Proxmox access"] != "not known" {
		t.Errorf("no key folder: %v", s)
	}
}

func sim(t *testing.T, cols, rows int) tcell.SimulationScreen {
	t.Helper()
	s := tcell.NewSimulationScreen("UTF-8")
	if err := s.Init(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Fini)
	s.SetSize(cols, rows)
	return s
}

func text(s tcell.SimulationScreen) []string {
	s.Show()
	cells, w, h := s.GetContents()
	out := make([]string, h)
	for y := range h {
		var b strings.Builder
		for x := range w {
			b.WriteRune(cells[y*w+x].Runes[0])
		}
		out[y] = strings.TrimRight(b.String(), " ")
	}
	return out
}

// standInRestic is a restic that logs its argv and the sha256 of the password it got (never
// the password), and exits with the code in codeFile for a repository named there.
func standInRestic(t *testing.T) (bin, log, codeFile string) {
	t.Helper()
	bin, dir := t.TempDir(), t.TempDir()
	log, codeFile = filepath.Join(dir, "log"), filepath.Join(dir, "code")
	mk(t, codeFile, "0")
	script := "#!/bin/sh\necho \"argv: $*\" >> " + log + "\n" +
		"printf 'env: %s\\n' \"$(printf %s \"$RESTIC_PASSWORD\" | sha256sum | cut -c1-64)\" >> " + log + "\n" +
		"exit $(cat " + codeFile + ")\n"
	if err := os.WriteFile(filepath.Join(bin, "restic"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return bin, log, codeFile
}

// everything is the text of every file below the folders.
func everything(t *testing.T, dirs ...string) string {
	var b strings.Builder
	for _, d := range dirs {
		filepath.WalkDir(d, func(p string, e fs.DirEntry, err error) error {
			if err == nil && !e.IsDir() {
				x, _ := os.ReadFile(p)
				b.WriteString(p + "\n" + string(x) + "\n")
			}
			return nil
		})
	}
	return b.String()
}

// TestProof drives the keys screen: p, a wrong copy, then the right one, through the real
// runner and a stand-in restic. No frame, file, argv or written byte holds either value.
func TestProof(t *testing.T) {
	m := newMachine(t)
	bin, log, codeFile := standInRestic(t)
	t.Setenv("PATH", bin+":/bin:/usr/bin")
	wrong := "Tr0ub4dor-and-3-horses-staple-WRONG"
	secrets = append(secrets, wrong)
	s := sim(t, 80, 24)
	var mu sync.Mutex
	var frames []string
	ready := make(chan struct{}, 256)
	o := Options{Screen: s, Title: "restic-pve v9.9.9 · backup machine · Keys", Back: "Back", Poll: 10 * time.Millisecond,
		Model: func() Model { return Build(m.getenv, m.cfg) },
		Prover: Prover{State: m.state, InUse: password, Repos: []string{"rclone:drive:backups/wiki", "rclone:drive:backups/proxmox"},
			Environ: []string{"PATH=" + bin + ":/bin:/usr/bin", "HOME=/nonexistent", "DISCORD_WEBHOOK_URL=" + webhook}, Run: facts.Exec},
		Ready: func() {
			mu.Lock()
			frames = append(frames, strings.Join(text(s), "\n"))
			mu.Unlock()
			ready <- struct{}{}
		},
	}
	done := make(chan error)
	go func() { done <- Run(o) }()
	waitFor := func(want string) string {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for {
			select {
			case <-ready:
				mu.Lock()
				f := frames[len(frames)-1]
				mu.Unlock()
				if strings.Contains(f, want) {
					return f
				}
			case <-deadline:
				t.Fatalf("no frame with %q; last:\n%s", want, frames[len(frames)-1])
			}
		}
	}
	paste := func(text string) {
		s.PostEvent(tcell.NewEventPaste(true))
		for _, r := range text {
			s.InjectKey(tcell.KeyRune, r, 0)
		}
		s.PostEvent(tcell.NewEventPaste(false))
		s.InjectKey(tcell.KeyEnter, 0, 0)
	}
	waitFor("Press p to prove it")
	s.InjectKey(tcell.KeyRune, 'p', 0)
	waitFor("paste the repository password here")
	paste(wrong)
	f := waitFor("This copy is not the password in use")
	if !strings.Contains(f, Mask) {
		t.Errorf("mask not shown:\n%s", f)
	}
	if b, _ := os.ReadFile(log); len(b) != 0 {
		t.Errorf("a wrong copy reached restic: %s", b)
	}
	if _, err := os.Stat(filepath.Join(m.state, facts.MarkerFile)); err == nil {
		t.Error("a wrong copy wrote the marker")
	}
	s.InjectKey(tcell.KeyRune, 'x', 0) // closes the result
	waitFor("Press p to prove it")
	s.InjectKey(tcell.KeyRune, 'p', 0)
	waitFor("paste the repository password here")
	s.InjectKey(tcell.KeyRune, 'q', 0) // q is part of a password while one is entered
	s.InjectKey(tcell.KeyBackspace2, 0, 0)
	paste(password)
	f = waitFor("Proven today")
	if !strings.Contains(f, "Your copy opens all 2 repositories.") {
		t.Errorf("result:\n%s", f)
	}
	s.InjectKey(tcell.KeyRune, 'x', 0)
	waitFor("Copy in your password manager: proven ")
	s.InjectKey(tcell.KeyRune, 'q', 0)
	if err := <-done; err != nil {
		t.Fatal(err)
	}

	// the marker holds the fingerprint backupctl's setup writes, mode 0600
	marker := filepath.Join(m.state, facts.MarkerFile)
	b, err := os.ReadFile(marker)
	fi, _ := os.Stat(marker)
	if err != nil || string(b) != facts.Fingerprint([]byte(password))+"\n" || fi.Mode().Perm() != 0o600 {
		t.Errorf("marker %q %v %v", b, fi.Mode(), err)
	}
	// restic got the copy through its environment only, and nothing else of the machine
	sum := sha256.Sum256([]byte(password))
	lb, _ := os.ReadFile(log)
	wantLog := "argv: cat config --no-lock -r rclone:drive:backups/wiki -o rclone.program=rclone -o rclone.args=serve restic --stdio --tpslimit 8 --drive-chunk-size 64M --drive-use-trash=false --low-level-retries 20\n" +
		"env: " + hex.EncodeToString(sum[:]) + "\n" +
		"argv: cat config --no-lock -r rclone:drive:backups/proxmox -o rclone.program=rclone -o rclone.args=serve restic --stdio --tpslimit 8 --drive-chunk-size 64M --drive-use-trash=false --low-level-retries 20\n" +
		"env: " + hex.EncodeToString(sum[:]) + "\n"
	if string(lb) != wantLog {
		t.Errorf("restic log:\n%s", lb)
	}
	mu.Lock()
	for _, fr := range frames {
		noSecret(t, "a frame", fr)
	}
	mu.Unlock()
	noSecret(t, "a file", everything(t, m.state, filepath.Dir(log)))

	// restic says wrong password (exit 12): stop with the 8.8 words, no marker
	os.Remove(marker)
	mk(t, codeFile, "12")
	r := o.Prover.Prove([]byte(password))
	if r.OK || r.Text != "Stop: The password in use does not open rclone:drive:backups/wiki. Take the copy from your password manager into .env (RESTIC_PASSWORD=...); never make a new password for existing backups. Nothing was written." {
		t.Errorf("exit 12: %+v", r)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Error("exit 12 wrote the marker")
	}
	// a storage that does not answer, a missing restic
	mk(t, codeFile, "1")
	if r := o.Prover.Prove([]byte(password)); r.OK || !strings.Contains(r.Text, "could not open rclone:drive:backups/wiki: restic ended with exit code 1") {
		t.Errorf("exit 1: %+v", r)
	}
	t.Setenv("PATH", t.TempDir())
	if r := o.Prover.Prove([]byte(password)); r.OK || !strings.Contains(r.Text, "restic is not installed here") {
		t.Errorf("no restic: %+v", r)
	}
	noSecret(t, "a file", everything(t, m.state, filepath.Dir(log)))
	// the copy is wiped after a proof, whatever the result
	c := []byte(password)
	o.Prover.Prove(c)
	if strings.Trim(string(c), "\x00") != "" {
		t.Error("the copy was not wiped")
	}
}

func TestReadHiddenRefusesAPipe(t *testing.T) {
	r, w, _ := os.Pipe()
	defer r.Close()
	defer w.Close()
	if _, err := ReadHidden(int(r.Fd()), w); err != ErrNoTerminal || !strings.Contains(err.Error(), "not read from a pipe") {
		t.Errorf("%v", err)
	}
}
