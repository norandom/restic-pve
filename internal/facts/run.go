package facts

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
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
	ctx, cancel := context.WithTimeout(context.Background(), c.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Env = c.Env
	cmd.WaitDelay = 5 * time.Second
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()
	r := Result{Stdout: out.Bytes(), Stderr: errb.Bytes()}
	var ee *exec.ExitError
	switch {
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
	case errors.Is(r.Err, ErrTimeout):
		return fmt.Sprintf("%s %s", name, ErrTimeout.Error())
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
