package wrapper

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"time"
)

// dump runs `vzdump --stdout` under the host-wide lock and the free-space guard of the
// fleecing storage. It returns vzdump's exit status, or 73 when the guard stopped it.
func (e *Env) dump(vmid string, c Conf) (int, error) {
	lock, err := e.lock(c)
	if err != nil {
		return 0, err
	}
	defer lock.Close()
	cmd := e.command(context.Background(), "nice", "-n", "19", "vzdump", vmid, "--mode", "snapshot", "--stdout",
		"--compress", "0", "--bwlimit", c.BwlimitKiB, "--ionice", "7", "--performance", "max-workers="+c.MaxWorkers,
		"--fleecing", "enabled=1,storage="+c.Storage)
	cmd.Stdout, cmd.Stderr = e.Stdout, e.Stderr // the stream goes straight to the ssh channel
	cmd.ExtraFiles = []*os.File{lock}           // vzdump holds the lock too, as in the bash wrapper
	if err := cmd.Start(); err != nil {
		return 0, fail(127, "start vzdump: %v", err)
	}
	sigs := make(chan os.Signal, 4)
	signal.Notify(sigs, syscall.SIGHUP, syscall.SIGINT, syscall.SIGTERM, syscall.SIGPIPE)
	defer signal.Stop(sigs)

	done := make(chan struct{})
	aborted := make(chan bool, 1)
	go e.watchSpace(cmd.Process, c, done, aborted)
	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()
	for {
		select {
		case <-sigs:
			_ = cmd.Process.Signal(syscall.SIGTERM)
		case err := <-waited:
			close(done)
			if <-aborted {
				return exNoSpace, nil
			}
			return exitStatus(err), nil
		}
	}
}

// watchSpace polls the free space of the fleecing storage and stops vzdump below the
// abort limit. It sends true on aborted after stopping vzdump, false when done closed.
func (e *Env) watchSpace(p *os.Process, c Conf, done <-chan struct{}, aborted chan<- bool) {
	t := time.NewTicker(e.SpacePoll)
	defer t.Stop()
	for {
		select {
		case <-done:
			aborted <- false
			return
		case <-t.C:
		}
		kib, ok := e.availKiB(c.Storage) // unknown for a moment: keep watching
		if !ok || kib >= c.abortGiB()*1048576 {
			continue
		}
		select {
		case <-done: // vzdump ended while we asked pvesm
			aborted <- false
			return
		default:
		}
		fmt.Fprintf(e.Stderr, "restic-pve: error: fleecing storage %s has %d GiB free, below FLEECING_ABORT_FREE_GIB=%s; stopping vzdump\n",
			c.Storage, kib/1048576, c.AbortFreeGiB)
		_ = p.Signal(syscall.SIGTERM)
		aborted <- true
		return
	}
}

// exitStatus maps a finished child to a shell-style exit status (killed by a signal: 128+signal).
func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if !errors.As(err, &ee) {
		return 1
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}
