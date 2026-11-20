package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	prun "github.com/norandom/restic-pve/internal/pieces/run"
	"github.com/norandom/restic-pve/internal/role"
	"github.com/norandom/restic-pve/internal/watch"
)

const piecesUsage = "usage: restic-pve pieces --job FILE"

// lockedBuffer keeps the JSON lines while the view covers the terminal.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

// cmdPieces is `restic-pve pieces --job FILE`: one guest run in the piece layout. This release
// runs the probe (job mode "probe"): the Proxmox host is asked for its record of changed
// blocks, nothing is read and nothing is stored. The JSON lines and the result line go to
// stderr. On a terminal the block view shows the record while the probe runs and after it,
// until q; the JSON lines follow when the view is left. Without a terminal one plain line goes
// to stdout. The exit code is the result's code.
func cmdPieces(args []string, e env) int {
	fs := flag.NewFlagSet("restic-pve pieces", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jobFile := fs.String("job", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *jobFile == "" {
		fmt.Fprintln(e.stderr, piecesUsage)
		return prun.CodeUsage
	}
	// the job's children (ssh) never see a secret of this environment
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); secretName(k) {
			os.Unsetenv(k)
		}
	}
	j, err := prun.LoadJob(*jobFile)
	if err != nil {
		fmt.Fprintf(e.stderr, "restic-pve pieces: %v\n", err)
		return prun.CodeUsage
	}
	if j.Mode != prun.ModeProbe {
		fmt.Fprintf(e.stderr, "restic-pve pieces: mode %s is not in this release; it runs the probe (mode probe) only. Nothing was changed.\n", j.Mode)
		return prun.CodeUsage
	}
	re := prun.Env{State: e.getenv(role.StateEnv), Stderr: e.stderr}
	if e.plain(false) {
		res := prun.Probe(context.Background(), re, j)
		fmt.Fprintln(e.stdout, res.Message)
		return res.Code
	}
	screen, err := e.newScreen()
	if err != nil {
		res := prun.Probe(context.Background(), re, j)
		fmt.Fprintln(e.stdout, res.Message)
		return res.Code
	}
	var held lockedBuffer
	re.Stderr = &held
	done := make(chan prun.Result, 1)
	go func() { done <- prun.Probe(context.Background(), re, j) }()
	stop := restoreOnSignal(screen)
	verr := watch.Run(screen, watch.Options{Dir: j.ViewDir, Color: e.getenv("NO_COLOR") == "", Follow: true,
		Words: watch.Words{Title: title("probe"), Start: "restic-pve pieces --job " + *jobFile}})
	screen.Fini()
	stop()
	var res prun.Result
	select {
	case res = <-done:
	default:
		fmt.Fprintln(e.stdout, "the view was left; the probe goes on until it ends")
		select {
		case res = <-done:
		case <-time.After(time.Until(j.DeadlineTime()) + time.Minute):
			fmt.Fprintln(e.stderr, "restic-pve pieces: the probe did not end before its deadline")
			return prun.CodeSource
		}
	}
	held.mu.Lock()
	e.stderr.Write(held.b.Bytes())
	held.mu.Unlock()
	fmt.Fprintln(e.stdout, res.Message)
	if verr != nil && !errors.Is(verr, watch.ErrIdle) {
		fmt.Fprintf(e.stderr, "restic-pve pieces: view: %v\n", verr)
	}
	return res.Code
}
