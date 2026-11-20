// Command restic-pve is the one binary of both machines: the backup identity on a Proxmox VE
// host (host role) and the stream program of the backup machine (backup role). Each command
// belongs to one role; the role guard (internal/role) refuses a command of the other role
// before it does anything.
//
// Host role:
//
//	restic-pve request <ping|info <vmid>|dump <vmid>|changes <vmid> probe|use|new>
//	                                                    forced command (through sudo)
//	restic-pve install [--plain]                        root: asks for the site values, installs or changes the identity
//	restic-pve update                                   root: replace itself with the latest signed release
//	restic-pve uninstall                                root: remove the identity, its files and the binary
//
// Backup role (RESTIC_PVE_STATE set):
//
//	restic-pve status [--plain]                         is there a recent copy away from the site? (exit 0: all safe)
//	restic-pve keys [--plain]                           every key, and the proof of the password copy
//	restic-pve space [--plain|--check]                  storage space; --check: one line, exit 0 ok, 1 low, 2 unknown
//	restic-pve watch [--once]                           the backup that runs now: guest block view or local progress
//	restic-pve stream <backup|verify|to-vma|to-raw> ... the stream program (the former vmastream)
//	restic-pve pieces --job FILE                        one guest run in the piece layout: a probe or a stored run
//
// Both:
//
//	restic-pve version                                  print the version (stdout); the role on stderr, only on a terminal
//
// Not in the usage line: restic-pve host-sync (host role, root), which update runs with the new
// binary after the rename to bring the Proxmox plug-in to the new binary's copy. And
// restic-pve demo [pack] [full|incremental] shows the block map of the
// backup view with a simulated run, to judge the retro look; it reads no data. Without "pack"
// blocks turn yellow in place; with "pack" they are moved into one solid run at the top, like
// the reference defragmenter.
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/demo"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/install"
	"github.com/norandom/restic-pve/internal/relkeys"
	"github.com/norandom/restic-pve/internal/role"
	"github.com/norandom/restic-pve/internal/update"
	"github.com/norandom/restic-pve/internal/wrapper"
	"golang.org/x/term"
)

// Set at link time with -ldflags "-X main.version=... -X main.releaseRepo=...".
var (
	version     = "dev"
	releaseRepo = "norandom/restic-pve"
)

const (
	hostUsage   = "usage: restic-pve request <ping|info <vmid>|dump <vmid>|changes <vmid> probe|use|new> | install [--plain] | update | uninstall | version"
	backupUsage = "usage: restic-pve status [--plain] | keys [--plain] | space [--plain|--check] | watch [--once] | " +
		"stream <backup|verify|to-vma|to-raw> [flags] | pieces --job FILE | version"
)

// roles is the command table: the role of each command. A command not listed (version, demo)
// runs on both machines.
var roles = map[string]role.Role{
	"request": role.Host, "install": role.Host, "update": role.Host, "uninstall": role.Host, "host-sync": role.Host,
	"stream": role.Backup, "pieces": role.Backup, "watch": role.Backup, "status": role.Backup, "keys": role.Backup, "space": role.Backup,
}

// env is what a command sees of the machine.
type env struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	role           role.Role
	stderrTTY      bool // the role line of version goes only to a terminal
	tty            bool // stdin and stdout are a terminal: watch draws its screen
	getenv         func(string) string
	environ        func() []string              // the whole environment (children get an allow list of it)
	size           func() (int, int, error)     // the terminal's columns and rows
	runner         facts.Runner                 // restic and rclone; tests give stand-ins
	newScreen      func() (tcell.Screen, error) // an initialised full screen
}

func main() {
	os.Exit(run(os.Args[1:], env{stdin: os.Stdin, stdout: os.Stdout, stderr: os.Stderr,
		role: role.Detect(os.Stat, os.Getenv), stderrTTY: term.IsTerminal(2),
		tty: term.IsTerminal(0) && term.IsTerminal(1), getenv: os.Getenv, environ: os.Environ,
		size: func() (int, int, error) { return term.GetSize(1) }, runner: facts.Exec, newScreen: terminalScreen}))
}

func run(args []string, e env) int {
	stdout, stderr := e.stdout, e.stderr
	usage := hostUsage
	if e.role == role.Backup {
		usage = backupUsage
	}
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 64
	}
	if err := role.Require(args[0], roles[args[0]], e.role); err != nil {
		fmt.Fprintln(stderr, err)
		return err.(*role.Error).Code
	}
	switch {
	case args[0] == "stream":
		return cmdStream(args[1:], e)
	case args[0] == "watch":
		return cmdWatch(args[1:], e)
	case args[0] == "pieces":
		return cmdPieces(args[1:], e)
	case args[0] == "status":
		return cmdStatus(args[1:], e)
	case args[0] == "keys":
		return cmdKeys(args[1:], e)
	case args[0] == "space":
		return cmdSpace(args[1:], e)
	case args[0] == "request":
		return wrapper.Default(version).Run(args[1:])
	case args[0] == "version" && len(args) == 1:
		fmt.Fprintln(stdout, version) // byte for byte: released hosts compare it with VERSION
		if e.stderrTTY {
			fmt.Fprintln(stderr, "role:", roleLine(e.role))
		}
		return 0
	case args[0] == "install" && (len(args) == 1 || len(args) == 2 && args[1] == "--plain"):
		exe, err := executable()
		if err != nil {
			fmt.Fprintf(stderr, "restic-pve: %v\n", err)
			return 1
		}
		sys := install.Default(version, exe)
		sys.Plain = len(args) == 2
		return sys.Install()
	case args[0] == "demo":
		if !demoArgs(args[1:]) { // a mistyped mode gets the demo's own usage, not the general one
			fmt.Fprintln(stderr, "usage: restic-pve demo [pack] [full|incremental]")
			return 64
		}
		if err := demo.Main(args[1:], os.Getenv); err != nil {
			fmt.Fprintf(stderr, "restic-pve demo: %v\n", err)
			return 1
		}
		return 0
	case args[0] == "host-sync" && len(args) == 1:
		if os.Geteuid() != 0 {
			fmt.Fprintln(stderr, "restic-pve: host-sync must run as root")
			return 1
		}
		return wrapper.Default(version).HostSync()
	case args[0] == "uninstall" && len(args) == 1:
		return install.Default(version, "").Uninstall()
	case args[0] == "update" && len(args) == 1:
		exe, err := executable()
		if err != nil {
			fmt.Fprintf(stderr, "restic-pve: %v\n", err)
			return 1
		}
		return update.Run(update.Config{
			BaseURL: update.BaseURL(releaseRepo), Target: exe, CurrentVersion: version,
			Keys: relkeys.PublicKeys, Arch: runtime.GOARCH, HTTPClient: update.Client(),
			IsRoot: func() bool { return os.Geteuid() == 0 }, Stdout: stdout, Stderr: stderr, HostSync: true,
		})
	}
	fmt.Fprintln(stderr, usage)
	return 64
}

func roleLine(r role.Role) string {
	if r == role.None {
		return "none found (no /etc/pve, no /etc/restic-pve.conf, " + role.StateEnv + " not set)"
	}
	return r.Machine()
}

func demoArgs(args []string) bool {
	_, _, ok := demo.Args(args)
	return ok
}

// executable returns the path of the running binary with symlinks resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	return exe, nil
}
