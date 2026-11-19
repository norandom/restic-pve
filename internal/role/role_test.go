package role

import (
	"errors"
	"io/fs"
	"os"
	"testing"
	"time"
)

type fakeInfo struct{ dir bool }

func (f fakeInfo) Name() string       { return "x" }
func (f fakeInfo) Size() int64        { return 0 }
func (f fakeInfo) Mode() fs.FileMode  { return map[bool]fs.FileMode{true: fs.ModeDir}[f.dir] }
func (f fakeInfo) ModTime() time.Time { return time.Time{} }
func (f fakeInfo) IsDir() bool        { return f.dir }
func (f fakeInfo) Sys() any           { return nil }

// machine is a fake filesystem (path -> is a directory) and environment.
func machine(files map[string]bool, env map[string]string) (func(string) (fs.FileInfo, error), func(string) string) {
	stat := func(p string) (fs.FileInfo, error) {
		if d, ok := files[p]; ok {
			return fakeInfo{d}, nil
		}
		return nil, os.ErrNotExist
	}
	return stat, func(k string) string { return env[k] }
}

func TestDetect(t *testing.T) {
	state := map[string]string{StateEnv: "/state"}
	for _, c := range []struct {
		name  string
		files map[string]bool
		env   map[string]string
		want  Role
	}{
		{"nothing", nil, nil, None},
		{"pve folder", map[string]bool{"/etc/pve": true}, nil, Host},
		{"pve is a file, not a folder", map[string]bool{"/etc/pve": false}, nil, None},
		{"host config", map[string]bool{"/etc/restic-pve.conf": false}, nil, Host},
		{"state root", nil, state, Backup},
		{"empty state root", nil, map[string]string{StateEnv: ""}, None},
		{"host wins over state root", map[string]bool{"/etc/pve": true}, state, Host},
		{"host config wins over state root", map[string]bool{"/etc/restic-pve.conf": false}, state, Host},
	} {
		stat, getenv := machine(c.files, c.env)
		if got := Detect(stat, getenv); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
}

func TestRequire(t *testing.T) {
	for _, c := range []struct {
		cmd        string
		want, have Role
		msg        string
		code       int
	}{
		{"version", None, Host, "", 0},
		{"version", None, Backup, "", 0},
		{"install", Host, Host, "", 0},
		{"install", Host, None, "", 0}, // host commands as before when no role is found
		{"watch", Backup, Backup, "", 0},
		{"install", Host, Backup, "restic-pve install is for the Proxmox host; this is the backup machine. Nothing was changed. Run it in a root shell on the Proxmox host.", 64},
		{"request", Host, Backup, "restic-pve request is for the Proxmox host; this is the backup machine. Nothing was changed. Run it in a root shell on the Proxmox host.", 64},
		{"stream", Backup, Host, "restic-pve stream is for the backup machine; this is the Proxmox host. Nothing was changed. Run it on the backup machine.", 64},
		{"watch", Backup, None, "restic-pve watch is for the backup machine, and RESTIC_PVE_STATE is not set. Nothing was changed. Set RESTIC_PVE_STATE to the backup machine's state folder, for example RESTIC_PVE_STATE=/state.", 78},
	} {
		err := Require(c.cmd, c.want, c.have)
		if c.msg == "" {
			if err != nil {
				t.Errorf("%s %v on %v: %v", c.cmd, c.want, c.have, err)
			}
			continue
		}
		var re *Error
		if !errors.As(err, &re) || err.Error() != c.msg || re.Code != c.code {
			t.Errorf("%s %v on %v:\n got %v (%+v)\nwant %q code %d", c.cmd, c.want, c.have, err, re, c.msg, c.code)
		}
	}
}

func TestMachine(t *testing.T) {
	if Host.Machine() != "Proxmox host" || Backup.Machine() != "backup machine" || None.Machine() != "" {
		t.Error(Host.Machine(), Backup.Machine(), None.Machine())
	}
}
