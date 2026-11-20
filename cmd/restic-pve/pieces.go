package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"

	"github.com/norandom/restic-pve/internal/pieces"
	prun "github.com/norandom/restic-pve/internal/pieces/run"
	"github.com/norandom/restic-pve/internal/role"
	"github.com/norandom/restic-pve/internal/stream/source"
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

// cmdPieces is `restic-pve pieces --job FILE`: one guest run in the piece layout. Job mode
// probe asks the Proxmox host for its record of changed blocks and stores nothing; modes auto
// and full-check are the stored runs (prun.Run), with restic as a child in the environment
// backupctl passes. The JSON lines and the result line go to stderr. On a terminal the block
// view shows the run while it runs and after it, until q; the JSON lines follow when the view
// is left. Without a terminal one plain line goes to stdout. The exit code is the result's
// code.
func cmdPieces(args []string, e env) int {
	fs := flag.NewFlagSet("restic-pve pieces", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	jobFile := fs.String("job", "", "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 || *jobFile == "" {
		fmt.Fprintln(e.stderr, piecesUsage)
		return prun.CodeUsage
	}
	// restic gets the environment backupctl passes; ssh and the stream never see a secret of it
	resticEnv := os.Environ()
	for _, kv := range os.Environ() {
		if k, _, _ := strings.Cut(kv, "="); source.SecretName(k) {
			os.Unsetenv(k)
		}
	}
	j, err := prun.LoadJob(*jobFile)
	if err != nil {
		fmt.Fprintf(e.stderr, "restic-pve pieces: %v\n", err)
		return prun.CodeUsage
	}
	re := prun.Env{State: e.getenv(role.StateEnv), Stderr: e.stderr, Restic: &pieces.Restic{Env: resticEnv}, Producer: "restic-pve " + version}
	runJob, what := prun.Run, "guest backup"
	if j.Mode == prun.ModeProbe {
		runJob, what = prun.Probe, "probe"
	}
	// a signal ends the run as a failure does: the host hears fail, the run's partial
	// snapshots are forgotten, the result line is written
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	var screen tcell.Screen
	if !e.plain(false) {
		screen, err = e.newScreen()
	}
	if screen == nil || err != nil {
		res := runJob(ctx, re, j)
		fmt.Fprintln(e.stdout, res.Message)
		return res.Code
	}
	var held lockedBuffer
	re.Stderr = &held
	done := make(chan prun.Result, 1)
	go func() { done <- runJob(ctx, re, j) }()
	go func() { // leave the view on a signal; the run ends by itself
		<-ctx.Done()
		screen.PostEvent(tcell.NewEventKey(tcell.KeyRune, 'q', tcell.ModNone))
	}()
	verr := watch.Run(screen, watch.Options{Dir: j.ViewDir, Color: e.getenv("NO_COLOR") == "", Follow: true,
		Words: watch.Words{Title: title(what), Start: "restic-pve pieces --job " + *jobFile}})
	screen.Fini()
	var res prun.Result
	select {
	case res = <-done:
	default:
		fmt.Fprintln(e.stdout, "the view was left; the "+what+" goes on until it ends")
		select {
		case res = <-done:
		case <-time.After(time.Until(j.DeadlineTime()) + time.Minute):
			fmt.Fprintln(e.stderr, "restic-pve pieces: the "+what+" did not end before its deadline")
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
