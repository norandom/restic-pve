package facts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// Call is one run of an external program (restic or rclone). Args never hold a secret; a
// secret reaches the program only through Env.
type Call struct {
	Name    string        // program name, looked up in PATH
	Args    []string      // never a secret
	Env     []string      // the program's whole environment
	Timeout time.Duration // every call has one
	// Ctx stops the call early: the screen was left. nil: only the time-out stops it.
	Ctx context.Context
}

// Result is what a program did. Err is set when it could not run to its end: not found, not
// started, or stopped at the time-out.
type Result struct {
	Stdout, Stderr []byte
	Code           int
	Err            error
}

// Runner runs a program. Every call of the backup role to restic or rclone goes through
// one; tests give a stand-in.
type Runner func(Call) Result

// ErrTimeout is the error of a call that ran out of time.
var ErrTimeout = errors.New("did not answer in time")

// ErrStopped is the error of a call that was stopped because the screen was left, or one
// that was not started because the tool is ending.
var ErrStopped = errors.New("was stopped")

// children are the process groups of the programs that run now. StopChildren kills them
// all when the tool ends on a signal; after it, no program starts.
var children = struct {
	sync.Mutex
	pgids   map[int]bool
	stopped bool
}{pgids: map[int]bool{}}

// StopChildren kills every program that runs now, with its own children, and refuses to
// start another. The tool calls it right before it exits on a signal.
func StopChildren() {
	children.Lock()
	defer children.Unlock()
	children.stopped = true
	for pg := range children.pgids {
		_ = unix.Kill(-pg, unix.SIGKILL)
	}
}

// Running is the number of programs that run now (tests).
func Running() int {
	children.Lock()
	defer children.Unlock()
	return len(children.pgids)
}

// Exec runs programs at the lowest CPU priority (nice 19) and the idle I/O class, so that
// reading the storage never slows a backup down. Both settings are made on a thread of its
// own before the program starts, and the program and its children inherit them.
func Exec(c Call) Result {
	done := make(chan Result, 1)
	go func() {
		// The thread is never given back: it carries the low priorities and ends with this
		// goroutine (a locked goroutine that returns takes its thread with it).
		runtime.LockOSThread()
		_ = unix.Setpriority(unix.PRIO_PROCESS, 0, 19)
		const whoProcess, classIdle = 1, 3
		_, _, _ = unix.Syscall(unix.SYS_IOPRIO_SET, whoProcess, 0, classIdle<<13)
		done <- execLocked(c)
	}()
	return <-done
}

func execLocked(c Call) Result {
	parent := c.Ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithTimeout(parent, c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	// a process group of its own: a stop or a time-out kills the program and whatever it
	// started, so nothing of it outlives the screen
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return unix.Kill(-cmd.Process.Pid, unix.SIGKILL) }
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	children.Lock()
	if children.stopped || parent.Err() != nil {
		children.Unlock()
		return Result{Err: ErrStopped}
	}
	err := cmd.Start()
	pid := 0
	if err == nil {
		pid = cmd.Process.Pid
		children.pgids[pid] = true
	}
	children.Unlock()
	if err == nil {
		err = cmd.Wait()
		children.Lock()
		delete(children.pgids, pid)
		children.Unlock()
	}
	r := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	switch {
	case parent.Err() != nil:
		r.Err = ErrStopped
	case ctx.Err() != nil:
		r.Err = ErrTimeout
	case errors.As(err, &ee):
		r.Code = ee.ExitCode()
	case err != nil:
		r.Err = err
	}
	return r
}

// Problem is a result as one plain sentence for the admin, "" when the program succeeded.
func (r Result) Problem(name string) string {
	var nf *exec.Error
	switch {
	case errors.As(r.Err, &nf) || errors.Is(r.Err, exec.ErrNotFound):
		return fmt.Sprintf("%s is not installed here (not found in PATH); the backup machine's image has it, run this command there", name)
	case errors.Is(r.Err, ErrTimeout), errors.Is(r.Err, ErrStopped):
		return fmt.Sprintf("%s %s", name, r.Err.Error())
	case r.Err != nil:
		return fmt.Sprintf("%s could not be started: %v", name, r.Err)
	case r.Code != 0:
		if l := FirstLine(r.Stderr); l != "" {
			return fmt.Sprintf("%s said: %s", name, l)
		}
		return fmt.Sprintf("%s ended with exit code %d and no message", name, r.Code)
	}
	return ""
}

// FirstLine is the first non-empty line of b, at most 200 characters.
func FirstLine(b []byte) string {
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" {
			if len(l) > 200 {
				l = l[:200] + "..."
			}
			return l
		}
	}
	return ""
}

// ChildEnv is the environment of a restic or rclone child: what restic and rclone need from
// the parent, never another secret of this machine (webhook, tokens, logins).
func ChildEnv(environ []string) []string {
	keep := map[string]bool{"PATH": true, "HOME": true, "TZ": true, "LANG": true, "LC_ALL": true,
		"TMPDIR": true, "RESTIC_CACHE_DIR": true, "XDG_CACHE_HOME": true, "SSL_CERT_FILE": true}
	var env []string
	for _, kv := range environ {
		k, _, _ := strings.Cut(kv, "=")
		if keep[k] || strings.HasPrefix(k, "RCLONE_") {
			env = append(env, kv)
		}
	}
	return env
}
