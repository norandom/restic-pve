package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/norandom/restic-pve/internal/role"
)

// sys is a machine of role r without a terminal and with an empty environment.
func sys(r role.Role, out, errb *bytes.Buffer) env {
	return env{stdin: strings.NewReader(""), stdout: out, stderr: errb, role: r, getenv: func(string) string { return "" }}
}

func TestDispatch(t *testing.T) {
	var out, errb bytes.Buffer
	run := func(args []string, out, errb *bytes.Buffer) int { return run(args, sys(role.None, out, errb)) }
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

// TestVersion: stdout is the version text byte for byte on every machine, with or without a
// terminal (the host's self-update compares it with VERSION); the role goes to stderr, and only
// when stderr is a terminal.
func TestVersion(t *testing.T) {
	old := version
	version = "v1.2.3"
	t.Cleanup(func() { version = old })
	for _, c := range []struct {
		r    role.Role
		line string
	}{
		{role.Host, "role: Proxmox host\n"},
		{role.Backup, "role: backup machine\n"},
		{role.None, "role: none found (no /etc/pve, no /etc/restic-pve.conf, RESTIC_PVE_STATE not set)\n"},
	} {
		for _, tty := range []bool{false, true} {
			var out, errb bytes.Buffer
			e := sys(c.r, &out, &errb)
			e.stderrTTY = tty
			if code := run([]string{"version"}, e); code != 0 || out.String() != "v1.2.3\n" {
				t.Errorf("%v tty=%v: %d %q", c.r, tty, code, out.String())
			}
			want := ""
			if tty {
				want = c.line
			}
			if errb.String() != want {
				t.Errorf("%v tty=%v: stderr %q, want %q", c.r, tty, errb.String(), want)
			}
		}
	}
}

// TestRoleRefusals: a command of the other role is refused before it runs, with the message
// of the role guard, and nothing on stdout.
func TestRoleRefusals(t *testing.T) {
	host := "is for the Proxmox host; this is the backup machine. Nothing was changed. Run it in a root shell on the Proxmox host.\n"
	backup := "is for the backup machine; this is the Proxmox host. Nothing was changed. Run it on the backup machine.\n"
	unset := "is for the backup machine, and RESTIC_PVE_STATE is not set. Nothing was changed. Set RESTIC_PVE_STATE to the backup machine's state folder, for example RESTIC_PVE_STATE=/state.\n"
	for _, c := range []struct {
		r    role.Role
		args []string
		code int
		msg  string
	}{
		{role.Backup, []string{"request", "ping"}, 64, "restic-pve request " + host},
		{role.Backup, []string{"install"}, 64, "restic-pve install " + host},
		{role.Backup, []string{"install", "--plain"}, 64, "restic-pve install " + host},
		{role.Backup, []string{"update"}, 64, "restic-pve update " + host},
		{role.Backup, []string{"uninstall"}, 64, "restic-pve uninstall " + host},
		{role.Host, []string{"stream", "backup"}, 64, "restic-pve stream " + backup},
		{role.Host, []string{"watch"}, 64, "restic-pve watch " + backup},
		{role.None, []string{"stream", "verify"}, 78, "restic-pve stream " + unset},
		{role.None, []string{"watch", "--once"}, 78, "restic-pve watch " + unset},
	} {
		var out, errb bytes.Buffer
		if code := run(c.args, sys(c.r, &out, &errb)); code != c.code || out.Len() != 0 || errb.String() != c.msg {
			t.Errorf("%v %q: %d %q %q", c.r, c.args, code, out.String(), errb.String())
		}
	}
}

// TestUsagePerRole: without a command, the usage of the role of this machine.
func TestUsagePerRole(t *testing.T) {
	for _, c := range []struct {
		r    role.Role
		want string
	}{
		{role.None, hostUsage + "\n"},
		{role.Host, hostUsage + "\n"},
		{role.Backup, backupUsage + "\n"},
	} {
		var out, errb bytes.Buffer
		if code := run(nil, sys(c.r, &out, &errb)); code != 64 || errb.String() != c.want {
			t.Errorf("%v: %d %q", c.r, code, errb.String())
		}
		errb.Reset()
		if code := run([]string{"bogus"}, sys(c.r, &out, &errb)); code != 64 || errb.String() != c.want {
			t.Errorf("%v bogus: %d %q", c.r, code, errb.String())
		}
	}
	if !strings.Contains(backupUsage, "stream") || !strings.Contains(backupUsage, "watch") || strings.Contains(backupUsage, "install") {
		t.Error(backupUsage)
	}
}
