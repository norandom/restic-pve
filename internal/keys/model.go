// Package keys is `restic-pve keys`: every key of the backup machine, what it does, where it
// is kept, whether it needs a copy away from the machine, what happens when it is lost, and
// whether it is there. Presence is checked by existence only; no value is ever read into
// output. The password proof (proof.go) checks the admin's copy without showing it.
package keys

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

// Key is one row of the key table.
type Key struct {
	Name   string // "Repository password"
	Does   string // what it does
	KeptIn string // where the tool sees it
	Copy   bool   // a copy away from this machine is needed
	IfLost string
	State  string // "set", "present", "MISSING", "not set", "not used ..."
	Bad    bool   // the state needs the admin
}

// Other is a secret on this machine that is not one of the admin's keys.
type Other struct{ Name, State string }

// Model is the keys screen.
type Model struct {
	Keys       []Key
	Others     []Other
	Proof      *time.Time // the last passed proof for the password in use
	ProofStale bool       // a marker exists, but for another password
	Password   bool       // RESTIC_PASSWORD is set
	Repos      []string   // the repositories the proof opens
}

// Paths of the identity files below RESTIC_PVE_KEYS.
const (
	ProxmoxKey = "ssh/id_ed25519"
	GuestKey   = "guest-ssh/id_ed25519"
)

// Build looks at the keys of the environment and the key folder. c may be the zero Config
// when backup.toml cannot be read; then the repositories are not known.
func Build(getenv func(string) string, c facts.Config) Model {
	m := Model{Password: getenv(facts.PasswordEnv) != ""}
	state, dir := getenv(facts.StateEnv), getenv(facts.KeysEnv)
	m.Proof = facts.ProofDate(state, getenv(facts.PasswordEnv))
	if _, err := os.Stat(filepath.Join(state, facts.MarkerFile)); err == nil && m.Proof == nil {
		m.ProofStale = true
	}
	for _, s := range c.Files {
		m.Repos = append(m.Repos, c.Repository(s))
	}
	for _, p := range c.Proxmox {
		m.Repos = append(m.Repos, c.Repository(p.Name))
	}
	set := func(v string) (string, bool) {
		if getenv(v) != "" {
			return "set", false
		}
		return "MISSING", true
	}
	file := func(path string, needed bool) (string, bool) {
		switch fi, err := os.Stat(path); {
		case err == nil && fi.Mode().IsRegular():
			return "present", false
		case !needed:
			return "not needed: no guests", false
		}
		return "MISSING", true
	}
	pw := Key{Name: "Repository password", Does: "encrypts every backup", KeptIn: "RESTIC_PASSWORD (.env)", Copy: true,
		IfLost: "Lost with its copy: nobody can read the backups"}
	pw.State, pw.Bad = set(facts.PasswordEnv)
	m.Keys = append(m.Keys, pw)

	st := Key{Name: "Storage sign-in", Does: "writes the backups to the storage", IfLost: "Lost: nothing lost; sign in again"}
	st.KeptIn, st.State, st.Bad = storage(getenv, c)
	m.Keys = append(m.Keys, st)

	guests := len(c.Proxmox) > 0
	px := Key{Name: "Proxmox access", Does: "fetches guest images",
		IfLost: "Lost: make a new one, run restic-pve install again"}
	gh := Key{Name: "Guest health", Does: "checks a guest's disk in a backup",
		IfLost: "Lost: run ./setup.sh --guest <alias>"}
	if dir == "" {
		px.KeptIn, px.State, px.Bad = facts.KeysEnv+" not set", "not known", guests
		gh.KeptIn, gh.State = facts.KeysEnv+" not set", "not known"
	} else {
		px.KeptIn, gh.KeptIn = filepath.Join(dir, ProxmoxKey), filepath.Join(dir, GuestKey)
		px.State, px.Bad = file(px.KeptIn, guests)
		gh.State, _ = file(gh.KeptIn, false)
		if gh.State == "not needed: no guests" && guests {
			gh.State = "not made yet (optional)"
		}
	}
	m.Keys = append(m.Keys, px, gh)

	al := Key{Name: "Alert address", Does: "sends a message when something is wrong", KeptIn: "DISCORD_WEBHOOK_URL (.env)",
		IfLost: "Lost: make a new webhook", State: "set"}
	if getenv(facts.WebhookEnv) == "" {
		al.State = "not set: alerts are off"
	}
	m.Keys = append(m.Keys, al)

	both := func(a, b string) string {
		if getenv(a) != "" && getenv(b) != "" {
			return "set"
		}
		return "not set"
	}
	m.Others = []Other{
		{"GitHub runner token", map[bool]string{true: "set", false: "not set (empty after use)"}[getenv("RUNNER_TOKEN") != ""]},
		{"OpenObserve login", both("OPENOBSERVE_USER", "OPENOBSERVE_PASSWORD")},
	}
	return m
}

// storage finds the rclone remote of the repository base in the rclone config: only the
// section names are looked at, and the bytes read are wiped.
func storage(getenv func(string) string, c facts.Config) (keptIn, state string, bad bool) {
	remote := c.Remote()
	if c.Base == "" {
		return "rclone config", "not known: backup.toml not read", false
	}
	if remote == "" {
		return "none", "not used: the storage is not reached through rclone", false
	}
	path := getenv(facts.RcloneConfigEnv)
	if path == "" {
		return facts.RcloneConfigEnv + " not set", "MISSING", true
	}
	keptIn = path + " [" + remote + "]"
	b, err := os.ReadFile(path)
	if err != nil {
		return keptIn, "MISSING", true
	}
	defer clear(b)
	if bytes.HasPrefix(b, []byte("# Encrypted rclone configuration File")) {
		return keptIn, "present (encrypted; the section is not checked)", false
	}
	sc := bufio.NewScanner(bytes.NewReader(b))
	for sc.Scan() {
		if bytes.Equal(bytes.TrimSpace(sc.Bytes()), []byte("["+remote+"]")) {
			return keptIn, "present", false
		}
	}
	return keptIn, "MISSING: no section [" + remote + "]", true
}
