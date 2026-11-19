package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/role"
	"github.com/norandom/restic-pve/internal/watch"
)

// startHintEnv names the command that starts a backup on this machine, shown when none runs.
const startHintEnv = "RESTIC_PVE_START_HINT"

// cmdWatch is `restic-pve watch [--once]`: the block view of the guest backup that runs now,
// read from <RESTIC_PVE_STATE>/guests/view, or the progress of the local sources from
// <RESTIC_PVE_STATE>/status.json. Without a terminal, with TERM=dumb or on a small terminal,
// or with --once, it prints one plain line. It only reads: quitting it never stops the backup. It closes itself ten minutes
// after the last running backup.
func cmdWatch(args []string, e env) int {
	fs := flag.NewFlagSet("restic-pve watch", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	once := fs.Bool("once", false, "")
	if err := fs.Parse(args); err != nil || fs.NArg() > 0 {
		fmt.Fprintln(e.stderr, "usage: restic-pve watch [--once]")
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 64
	}
	hint := e.getenv(startHintEnv)
	if hint == "" {
		fmt.Fprintf(e.stderr, "restic-pve watch: %s is not set. Set it to the command that starts a backup on this machine, "+
			"for example %s='./backup.sh <source>'.\n", startHintEnv, startHintEnv)
		return 78
	}
	st := e.getenv(role.StateEnv)
	dir := filepath.Join(st, facts.ViewDir)
	line := func() {
		var sp watch.LocalSpeed
		lc := watch.LoadLocal(st)
		sp.Add(lc.S)
		fmt.Fprintln(e.stdout, watch.JobsLine(watch.Jobs(st), watch.Load(dir), lc, &sp, time.Now(), hint))
	}
	if *once || e.plain(false) {
		line()
		return 0
	}
	screen, err := e.newScreen()
	if err != nil { // no usable terminal after all: the plain line
		line()
		return 0
	}
	stop := restoreOnSignal(screen)
	defer stop()
	err = watch.Run(screen, watch.Options{Dir: dir, State: st, Color: e.getenv("NO_COLOR") == "",
		Words: watch.Words{Title: title("watch"), Start: hint}})
	screen.Fini()
	if errors.Is(err, watch.ErrIdle) {
		// closed by itself: say why and leave the last state on the normal screen
		fmt.Fprintf(e.stdout, "the view closed itself after %d minutes without a running backup\n", int(watch.Idle.Minutes()))
		line()
		return 0
	}
	if err != nil {
		fmt.Fprintf(e.stderr, "restic-pve watch: %v\n", err)
		return 1
	}
	return 0
}
