// Package wrapper answers the forced-command requests of the backup identity on a
// Proxmox VE host: ping, info <vmid>, dump <vmid> and changes <vmid> probe|use|new. ping,
// info and dump are a port of the bash wrapper restic-vzdump with the same grammar, checks,
// output and exit codes. The allow list is the VMIDS key of the one settings file
// /etc/restic-pve.conf.
//
// The wrapper never writes guest or storage state. It runs no qm write operations,
// snapshots or storage write commands. The only files it writes are the lock, the job socket
// of a changes request and, under the lock, the plug-in file from the binary's own copy when
// another version of it is installed.
package wrapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Exit codes of the request contract.
const (
	exMissing = 2
	exRefused = 64
	exNoSpace = 73
	exBusy    = 75
	exConfig  = 78
)

// Env holds the paths and limits of one wrapper run. Default returns the production values.
type Env struct {
	ConfFile, LockFile string
	Path               string // PATH for every child process
	SpacePoll          time.Duration
	AgentTimeout       time.Duration
	Version            string
	Stdout, Stderr     *os.File // vzdump gets these descriptors directly
	Stdin              *os.File // the ack line of a changes request

	Root         string        // prefix of the plug-in file, the storage configuration and the job folder ("" in production)
	QemuRun      string        // where qemu-server opens the NBD socket of a backup job
	Proc         string        // the proc file system, for the job socket's peer check
	CallTimeout  time.Duration // the longest wait for the plug-in's next call
	AckTimeout   time.Duration // the longest wait for the ack line after the stream: AckLimit
	StallTimeout time.Duration // the longest a write to stdout may make no progress: StallLimit
}

// Default returns the production environment.
func Default(version string) *Env {
	return &Env{
		ConfFile:     "/etc/restic-pve.conf",
		LockFile:     "/run/restic-pve.lock",
		Path:         "/usr/sbin:/usr/bin:/sbin:/bin",
		SpacePoll:    15 * time.Second,
		AgentTimeout: 10 * time.Second,
		Version:      version,
		Stdout:       os.Stdout,
		Stderr:       os.Stderr,
		Stdin:        os.Stdin,
		QemuRun:      "/run/qemu-server",
		Proc:         "/proc",
		CallTimeout:  time.Hour,
		AckTimeout:   AckLimit,
		StallTimeout: StallLimit,
	}
}

// exit carries an exit code and its stderr message through the call chain.
type exit struct {
	code int
	msg  string
}

func (e *exit) Error() string { return e.msg }

func refuse(msg string) error { return &exit{exRefused, "refused: " + msg} }

func fail(code int, format string, a ...any) error {
	return &exit{code, "error: " + fmt.Sprintf(format, a...)}
}

// Run answers one request (the arguments after "request") and returns the exit code.
func (e *Env) Run(args []string) int {
	syscall.Umask(0o077)
	code, err := e.request(args)
	if err != nil {
		var x *exit
		if !errors.As(err, &x) {
			x = &exit{1, "error: " + err.Error()}
		}
		fmt.Fprintf(e.Stderr, "restic-pve: %s\n", x.msg)
		return x.code
	}
	return code
}

func (e *Env) request(args []string) (int, error) {
	if len(args) < 1 {
		return 0, refuse("no request (interactive login is not available)")
	}
	switch args[0] {
	case "ping":
		if len(args) != 1 {
			return 0, refuse("usage: ping")
		}
		fmt.Fprintln(e.Stdout, "ok")
		return 0, nil
	case "info", "dump":
		if len(args) != 2 {
			return 0, refuse("usage: " + args[0] + " <vmid>")
		}
		vmid := args[1]
		if !ValidVMID(vmid) {
			return 0, refuse("invalid vmid")
		}
		conf, err := e.loadConf()
		if err != nil {
			return 0, err
		}
		if !conf.Allowed(vmid) {
			return 0, refuse("VM " + vmid + " is not on the allow list")
		}
		if args[0] == "info" {
			return 0, e.info(vmid, conf)
		}
		return e.dump(vmid, conf)
	case "changes":
		if len(args) != 3 || !changesModes[args[2]] {
			return 0, refuse("usage: changes <vmid> probe|use|new")
		}
		vmid := args[1]
		if !ValidVMID(vmid) {
			return 0, refuse("invalid vmid")
		}
		conf, err := e.loadConf()
		if err != nil {
			return 0, err
		}
		if !conf.Allowed(vmid) {
			return 0, refuse("VM " + vmid + " is not on the allow list")
		}
		return e.changes(vmid, args[2], conf)
	}
	return 0, refuse("unknown request")
}

func (e *Env) loadConf() (Conf, error) {
	data, ok := readable(e.ConfFile)
	if !ok {
		return Conf{}, fail(exConfig, "%s missing (run restic-pve install again)", e.ConfFile)
	}
	c, err := ParseConf(e.ConfFile, data)
	if err != nil {
		return Conf{}, fail(exConfig, "%v", err)
	}
	return c, nil
}

// command builds a child process found on e.Path with the minimal environment.
func (e *Env) command(ctx context.Context, name string, args ...string) *exec.Cmd {
	dirs := filepath.SplitList(e.Path)
	path := filepath.Join(append(dirs[:1:1], name)...) // not found: fails to start, never searches $PATH
	for _, dir := range dirs {
		p := filepath.Join(dir, name)
		if st, err := os.Stat(p); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			path = p
			break
		}
	}
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Args[0] = name
	cmd.Env = []string{"LC_ALL=C", "PATH=" + e.Path}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 5 * time.Second
	return cmd
}

// output runs a child and returns its stdout; stderr is discarded.
func (e *Env) output(name string, args ...string) (string, error) {
	out, err := e.command(context.Background(), name, args...).Output()
	return string(out), err
}

// fields splits a line like `read` with the default IFS (spaces and tabs).
func fields(line string) []string {
	return strings.FieldsFunc(line, func(r rune) bool { return r == ' ' || r == '\t' })
}

func field(f []string, i int) string {
	if i < len(f) {
		return f[i]
	}
	return ""
}

// availKiB returns the free KiB of the fleecing storage; false when unknown or inactive.
func (e *Env) availKiB(storage string) (int64, bool) {
	out, err := e.output("pvesm", "status", "--storage", storage)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(out, "\n") {
		f := fields(line)
		if field(f, 0) == storage && field(f, 2) == "active" && kibRe.MatchString(field(f, 5)) {
			return atoi(field(f, 5)), true
		}
	}
	return 0, false
}
