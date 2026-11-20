// Package source runs the PVE side of a guest backup. It starts ssh with a
// fixed argv (no shell) to the PVE forced-command wrapper. It provides the VMA
// stream from stdout and classifies the vzdump log on stderr (consistency,
// forwarded warn/error lines, fleecing and guest filesystem aborts).
package source

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

// DefaultProgram is the ssh client name, resolved through PATH (never an
// absolute path), so a test or integration override can put a fake first.
const DefaultProgram = "ssh"

// waitDelay bounds how long Wait waits for the stdio pipes after the process
// exited or was killed (a lingering grandchild must not hang the backup).
const waitDelay = 5 * time.Second

// Options selects the guest and the ssh client configuration.
type Options struct {
	SSHConfig, Host string
	VMID            uint32
	SSHProgram      string // bare program name looked up in PATH; "" = DefaultProgram
	// Request replaces "dump <vmid>" (for example "changes 100 probe"); each word must be a
	// plain argument.
	Request []string
	// Ack: the process gets a stdin pipe (Process.Stdin) for the ack line of a changes
	// request; otherwise stdin is empty.
	Ack bool
	// Env is the environment of ssh; nil inherits this process's.
	Env []string
}

// Events receives the classified log. Calls come from a background
// goroutine; implementations must be safe for concurrent use.
type Events interface {
	Consistency(value, detail string) // exactly once, from Wait
	VzdumpLine(level, line string)    // warn/error lines only
}

// Waiter waits for the source process and controls it.
type Waiter interface {
	// Wait waits for the process (stdout must have been read to EOF, or Abort
	// called) and returns nil or an *Error. It is idempotent.
	Wait() error
	// Abort kills the source (the backup failed on the reading side).
	Abort()
	// Classifier exposes the log classification (consistency, warnings).
	Classifier() *Classifier
}

var (
	// ErrOptions rejects invalid options (CLI usage error).
	ErrOptions = errors.New("source: invalid options")
	// ErrSourceFailed is the class of ssh/vzdump failures (CLI exit 6).
	ErrSourceFailed = errors.New("source failed")
	// ErrFleecing is the class of a backup that ran without fleecing (CLI exit 5).
	ErrFleecing = errors.New("fleecing not active")
	// ErrGuestFilesystem is a guest filesystem freeze error (CLI exit 10).
	// It is fatal and requires checking the guest filesystem before retrying.
	ErrGuestFilesystem = errors.New("guest filesystem error")
)

// Error is a source failure. errors.Is(err, Kind) holds.
type Error struct {
	Kind      error // ErrSourceFailed, ErrFleecing or ErrGuestFilesystem
	ExitCode  int   // ssh exit status; -1 when killed or not started
	Transient bool  // a retry may succeed (ssh connection error, PVE busy)
	Detail    string
}

func (e *Error) Error() string { return e.Kind.Error() + ": " + e.Detail }

// Unwrap places the error in its class.
func (e *Error) Unwrap() error { return e.Kind }

// Transient ssh/wrapper exit statuses.
const (
	exitSSHConnection = 255 // ssh: connection, authentication or host key failure
	exitWrapperBusy   = 75  // wrapper: PVE-side lock held (EX_TEMPFAIL)
	exitWrapperSpace  = 73  // wrapper: fleecing storage below its free-space limit
	exitNotReady      = 79  // wrapper: the plug-in or the storage entry of changed reads is not ready
)

func optionErr(format string, a ...any) error {
	return fmt.Errorf("%w: %s", ErrOptions, fmt.Sprintf(format, a...))
}

func plainArg(s string) bool {
	if s == "" || strings.HasPrefix(s, "-") {
		return false
	}
	for _, r := range s {
		if unicode.IsSpace(r) || unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Args returns the fixed ssh argv (without the program name):
// -F <config> -T -o BatchMode=yes <host> dump <vmid>.
func Args(o Options) ([]string, error) {
	if !plainArg(o.SSHConfig) {
		return nil, optionErr("ssh config %q must be a path not starting with '-' and without spaces", o.SSHConfig)
	}
	if !plainArg(o.Host) {
		return nil, optionErr("ssh host %q must not start with '-' or contain spaces", o.Host)
	}
	if o.VMID == 0 {
		return nil, optionErr("vmid must be a positive decimal number")
	}
	req := []string{"dump", strconv.FormatUint(uint64(o.VMID), 10)}
	if o.Request != nil {
		if len(o.Request) == 0 || len(o.Request) > 4 {
			return nil, optionErr("request of %d words", len(o.Request))
		}
		for _, w := range o.Request {
			if !plainArg(w) {
				return nil, optionErr("request word %q must be a plain argument", w)
			}
		}
		req = o.Request
	}
	args := append([]string{"-F", o.SSHConfig, "-T", "-o", "BatchMode=yes", o.Host}, req...)
	return args, nil
}

// Process is a running source.
type Process struct {
	parent context.Context
	cancel context.CancelFunc
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stdin  io.WriteCloser
	lines  *lineWriter
	cls    *Classifier
	ev     Events

	mu      sync.Mutex
	aborted bool

	once    sync.Once
	waitErr error
}

// Open validates o, resolves the ssh program in PATH and starts it. The
// returned reader is the VMA stream; the Waiter reports the outcome.
func Open(ctx context.Context, o Options, ev Events) (io.ReadCloser, Waiter, error) {
	args, err := Args(o)
	if err != nil {
		return nil, nil, err
	}
	prog := o.SSHProgram
	if prog == "" {
		prog = DefaultProgram
	}
	if strings.ContainsRune(prog, '/') {
		return nil, nil, optionErr("ssh program %q must be a bare name resolved through PATH", prog)
	}
	path, err := exec.LookPath(prog)
	if err != nil {
		return nil, nil, &Error{Kind: ErrSourceFailed, ExitCode: -1, Detail: fmt.Sprintf("ssh client %q not found in PATH: %v", prog, err)}
	}
	cctx, cancel := context.WithCancel(ctx)
	p := &Process{parent: ctx, cancel: cancel, cls: NewClassifier(), ev: ev}
	p.cmd = exec.CommandContext(cctx, path, args...)
	p.cmd.WaitDelay = waitDelay
	p.cmd.Env = o.Env
	p.lines = &lineWriter{onLine: p.line}
	p.cmd.Stderr = p.lines
	if o.Ack {
		if p.stdin, err = p.cmd.StdinPipe(); err != nil {
			cancel()
			return nil, nil, &Error{Kind: ErrSourceFailed, ExitCode: -1, Detail: fmt.Sprintf("ssh stdin: %v", err)}
		}
	}
	p.stdout, err = p.cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, nil, &Error{Kind: ErrSourceFailed, ExitCode: -1, Detail: fmt.Sprintf("ssh stdout: %v", err)}
	}
	if err := p.cmd.Start(); err != nil {
		cancel()
		return nil, nil, &Error{Kind: ErrSourceFailed, ExitCode: -1, Detail: fmt.Sprintf("start ssh: %v", err)}
	}
	return p, p, nil
}

// line handles one stderr line (called from the stderr copy goroutine).
func (p *Process) line(l string) {
	v := p.cls.Line(l)
	if v.Forward {
		p.ev.VzdumpLine(string(v.Level), clip(l))
	}
	if v.Fleecing || v.GuestFS {
		p.cancel() // abort immediately: inactive fleecing or a damaged guest filesystem
	}
}

func (p *Process) Read(b []byte) (int, error) { return p.stdout.Read(b) }

// Stdin is the process's stdin (Options.Ack), nil otherwise.
func (p *Process) Stdin() io.WriteCloser { return p.stdin }

// Close closes the stream side; the process is reaped by Wait.
func (p *Process) Close() error { return p.stdout.Close() }

// Classifier returns the log classification.
func (p *Process) Classifier() *Classifier { return p.cls }

// Abort kills the source.
func (p *Process) Abort() {
	p.mu.Lock()
	p.aborted = true
	p.mu.Unlock()
	p.cancel()
}

// Wait reaps the process, flushes the log and reports the outcome. A
// guest filesystem or fleecing problem in the log always wins
// (non-transient), even after a zero exit status.
func (p *Process) Wait() error {
	p.once.Do(func() {
		err := p.cmd.Wait()
		p.lines.flush()
		p.cancel()
		p.ev.Consistency(p.cls.Consistency())
		p.waitErr = p.outcome(err)
	})
	return p.waitErr
}

func (p *Process) outcome(err error) error {
	code := -1
	if p.cmd.ProcessState != nil {
		code = p.cmd.ProcessState.ExitCode()
	}
	if f := p.cls.GuestFSProblem(); f != "" {
		return &Error{Kind: ErrGuestFilesystem, ExitCode: code, Detail: f}
	}
	if f := p.cls.FleecingProblem(); f != "" {
		return &Error{Kind: ErrFleecing, ExitCode: code, Detail: "vzdump ran without active fleecing: " + f}
	}
	if reason, transient := p.cls.BackupFailure(); reason != "" {
		return &Error{Kind: ErrSourceFailed, ExitCode: code, Transient: transient, Detail: reason}
	}
	if err == nil {
		return nil
	}
	p.mu.Lock()
	aborted := p.aborted
	p.mu.Unlock()
	switch {
	case aborted:
		return &Error{Kind: ErrSourceFailed, ExitCode: code, Detail: "source aborted: " + err.Error()}
	case p.parent.Err() != nil:
		return &Error{Kind: ErrSourceFailed, ExitCode: code, Detail: "source cancelled: " + p.parent.Err().Error()}
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return &Error{Kind: ErrSourceFailed, ExitCode: code, Detail: "ssh: " + err.Error()}
	}
	e := &Error{Kind: ErrSourceFailed, ExitCode: code}
	switch code {
	case exitSSHConnection:
		e.Transient = true
		e.Detail = "ssh exited 255 (connection, authentication or host key failure)"
	case exitWrapperBusy:
		e.Transient = true
		e.Detail = "PVE wrapper busy (exit 75)"
	case exitWrapperSpace:
		e.Detail = "fleecing storage on PVE below its free-space limit (exit 73); see the vzdump log line"
	case exitNotReady:
		e.Detail = "the Proxmox plug-in is not ready (exit 79); run restic-pve install on the Proxmox host"
	case -1:
		e.Transient = true // killed by a signal we did not send
		e.Detail = "ssh terminated: " + ee.Error()
	default:
		e.Detail = fmt.Sprintf("ssh/vzdump exited %d", code)
	}
	return e
}

// maxPartialLine bounds a stderr line without newline before it is split.
const maxPartialLine = 64 << 10

// lineWriter splits the stderr byte stream into lines.
type lineWriter struct {
	mu     sync.Mutex
	buf    []byte
	onLine func(string)
}

func (w *lineWriter) Write(b []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf = append(w.buf, b...)
	for {
		i := bytes.IndexByte(w.buf, '\n')
		if i < 0 {
			break
		}
		w.onLine(string(w.buf[:i]))
		w.buf = w.buf[i+1:]
	}
	if len(w.buf) > maxPartialLine {
		w.onLine(string(w.buf))
		w.buf = nil
	}
	return len(b), nil
}

func (w *lineWriter) flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.buf) > 0 {
		w.onLine(string(w.buf))
		w.buf = nil
	}
}
