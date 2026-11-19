package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/gdamore/tcell/v2"
	"github.com/norandom/restic-pve/internal/facts"
	"github.com/norandom/restic-pve/internal/keys"
	"github.com/norandom/restic-pve/internal/space"
	"github.com/norandom/restic-pve/internal/status"
	"github.com/norandom/restic-pve/internal/tui"
	"github.com/norandom/restic-pve/internal/watch"
)

// terminalScreen is tcell's screen on the terminal, initialised: raw mode and the alternate
// screen until Fini.
func terminalScreen() (tcell.Screen, error) {
	s, err := tcell.NewScreen()
	if err == nil {
		err = s.Init()
	}
	return s, err
}

// flags parses the flags of a screen command: each one a bool, at most one given.
func flags(args []string, usage string, stderr io.Writer, names ...string) (map[string]bool, bool) {
	fs := flag.NewFlagSet(usage, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := map[string]*bool{}
	for _, n := range names {
		v[n] = fs.Bool(n, false, "")
	}
	err := fs.Parse(args)
	out, set := map[string]bool{}, 0
	for n, p := range v {
		out[n] = *p
		if *p {
			set++
		}
	}
	if err != nil || fs.NArg() > 0 || set > 1 {
		fmt.Fprintln(stderr, "usage: "+usage)
		return nil, false
	}
	return out, true
}

// plain is true when a screen command must print plain lines: --plain, no terminal,
// TERM=dumb, or a terminal smaller than 60 x 20.
func (e env) plain(flag bool) bool {
	if flag || !e.tty || e.getenv("TERM") == "dumb" || e.newScreen == nil {
		return true
	}
	if e.size != nil {
		c, r, err := e.size()
		// A terminal handed to a new container reports 0 x 0 for a moment, until the size
		// arrives. That is "not known yet", not "too small": wait up to a second for it, and
		// if it never comes, open the screen (it adapts when the size arrives).
		for i := 0; err == nil && (c == 0 || r == 0) && i < 20; i++ {
			time.Sleep(50 * time.Millisecond)
			c, r, err = e.size()
		}
		if err == nil && c > 0 && r > 0 && tui.Small(c, r) {
			return true
		}
	}
	return false
}

func (e env) facts(refresh, cacheOnly bool) facts.Env {
	var environ []string
	if e.environ != nil {
		environ = e.environ()
	}
	return facts.Env{Getenv: e.getenv, Environ: environ, Run: e.runner, Now: time.Now(), Refresh: refresh,
		CacheOnly: cacheOnly, Version: version}
}

func title(screen string) string { return "restic-pve " + version + " · backup machine · " + screen }

// fail prints the reason a command cannot work and returns its exit code: 78 for a missing
// variable or a config that cannot be read, 1 otherwise.
func fail(e env, cmd string, err error) int {
	fmt.Fprintf(e.stderr, "restic-pve %s: %v\n", cmd, err)
	var me *facts.MissingError
	var ce *facts.ConfigError
	if errors.As(err, &me) || errors.As(err, &ce) {
		return 78
	}
	return 1
}

// stamp sums up the files the screens read, for the redraw on change.
func (e env) stamp() string {
	st := e.getenv(facts.StateEnv)
	return tui.FileStamp(filepath.Join(st, facts.RecordsFile), filepath.Join(st, facts.GuestsDir, facts.RecordsFile),
		filepath.Join(st, facts.JobsDir), filepath.Join(st, facts.StatusFile), filepath.Join(st, facts.GuestsDir, facts.StatusFile),
		filepath.Join(st, facts.CacheFile), filepath.Join(st, facts.MarkerFile))
}

// cmdStatus is `restic-pve status [--plain]`.
func cmdStatus(args []string, e env) int {
	fl, ok := flags(args, "restic-pve status [--plain]", e.stderr, "plain")
	if !ok {
		return 64
	}
	if err := facts.Require(e.getenv, facts.StateEnv, facts.ConfigEnv, facts.PasswordEnv, facts.StartHintEnv); err != nil {
		return fail(e, "status", err)
	}
	if e.plain(fl["plain"]) {
		f, err := facts.Read(e.facts(false, false))
		if err != nil {
			return fail(e, "status", err)
		}
		m := status.Build(f, e.getenv(facts.StartHintEnv), time.Now())
		for _, l := range m.Plain() {
			fmt.Fprintln(e.stdout, l)
		}
		return m.Code()
	}
	// the first frame from the local files and the cache; the storage is read on the screen
	f, err := facts.Read(e.facts(false, true))
	if err != nil {
		return fail(e, "status", err)
	}
	return e.screens("status", status.Build(f, e.getenv(facts.StartHintEnv), time.Now()))
}

// cmdKeys is `restic-pve keys [--plain]`.
func cmdKeys(args []string, e env) int {
	fl, ok := flags(args, "restic-pve keys [--plain]", e.stderr, "plain")
	if !ok {
		return 64
	}
	if err := facts.Require(e.getenv, facts.StateEnv, facts.KeysEnv); err != nil {
		return fail(e, "keys", err)
	}
	if !e.plain(fl["plain"]) {
		return e.screens("keys", status.Model{})
	}
	m, prover, cfgErr := e.keysModel()
	for _, l := range m.Plain() {
		fmt.Fprintln(e.stdout, l)
	}
	if cfgErr != nil {
		fmt.Fprintf(e.stdout, "problem  %v\n", cfgErr)
	}
	if !e.tty {
		fmt.Fprintln(e.stdout, "proof  "+keys.NeedsTerminal)
		return 0
	}
	fmt.Fprintln(e.stdout, "To prove your password copy now, type p and press Enter. To leave, press Enter.")
	var answer string
	fmt.Fscanln(e.stdin, &answer)
	if strings.TrimSpace(answer) != "p" {
		return 0
	}
	fmt.Fprintln(e.stdout, keys.ProofIntro)
	copy, err := keys.ReadHidden(0, e.stdout)
	if err != nil {
		clear(copy)
		fmt.Fprintln(e.stdout, err)
		return 1
	}
	r := prover.Prove(copy) // wipes copy
	fmt.Fprintln(e.stdout, r.Text)
	if !r.OK {
		return 1
	}
	return 0
}

// keysModel builds the key table and the prover; a config that cannot be read leaves the
// repositories out and is returned to be shown.
func (e env) keysModel() (keys.Model, keys.Prover, error) {
	var c facts.Config
	var err error
	if p := e.getenv(facts.ConfigEnv); p == "" {
		err = &facts.MissingError{Var: facts.ConfigEnv}
	} else {
		c, err = facts.LoadConfig(p, e.getenv(facts.BaseEnv))
	}
	m := keys.Build(e.getenv, c)
	var environ []string
	if e.environ != nil {
		environ = e.environ()
	}
	return m, keys.Prover{State: e.getenv(facts.StateEnv), InUse: e.getenv(facts.PasswordEnv), Repos: m.Repos,
		Environ: environ, Run: e.runner}, err
}

// cmdSpace is `restic-pve space [--plain|--check]`.
func cmdSpace(args []string, e env) int {
	fl, ok := flags(args, "restic-pve space [--plain|--check]", e.stderr, "plain", "check")
	if !ok {
		return 64
	}
	if err := facts.Require(e.getenv, facts.StateEnv, facts.ConfigEnv); err != nil {
		return fail(e, "space", err)
	}
	if fl["check"] {
		// one rclone about, never restic: backupctl runs this at the end of every run
		c, err := facts.LoadConfig(e.getenv(facts.ConfigEnv), e.getenv(facts.BaseEnv))
		if err != nil {
			return fail(e, "space", err)
		}
		fe := e.facts(false, true)
		f := facts.Local(fe, c)
		f.Space.About, f.Space.AboutErr = facts.ReadAbout(c.Base, fe.Environ, e.runner)
		line, code := space.Build(f).CheckLine()
		fmt.Fprintln(e.stdout, line)
		return code
	}
	if err := facts.Require(e.getenv, facts.PasswordEnv); err != nil {
		return fail(e, "space", err)
	}
	if e.plain(fl["plain"]) {
		f, err := facts.Read(e.facts(false, false))
		if err != nil {
			return fail(e, "space", err)
		}
		for _, l := range space.Build(f).Plain() {
			fmt.Fprintln(e.stdout, l)
		}
		for _, fe := range f.Errors {
			fmt.Fprintln(e.stdout, "problem  "+fe.Text)
		}
		return 0
	}
	return e.screens("space", status.Model{})
}

// checked is the "checked N min ago" text of the facts.
func checked(f facts.Facts) string {
	if f.CheckedAt.IsZero() {
		return "storage not read"
	}
	ago := time.Since(f.CheckedAt)
	switch {
	case ago < time.Minute:
		return "checked just now"
	case ago < time.Hour:
		return fmt.Sprintf("checked %d min ago", int(ago.Minutes()))
	}
	return fmt.Sprintf("checked %d h ago", int(ago.Hours()))
}

// screens runs the full screens on one terminal screen, starting with first. The keys,
// space and watch screens opened from status go back to it; opened directly, they quit.
// It returns the exit code of the status (0 when every copy is safe) or 0.
func (e env) screens(first string, m status.Model) int {
	scr, err := e.newScreen()
	if err != nil { // no usable terminal after all
		fmt.Fprintf(e.stderr, "restic-pve %s: no usable terminal (%v); run it with --plain\n", first, err)
		return 1
	}
	stop := restoreOnSignal(scr)
	defer stop()
	color := e.getenv("NO_COLOR") == ""
	zone, _ := time.Now().Zone()
	hint := e.getenv(facts.StartHintEnv)
	code, idle := 0, false
	last := m
	readStatus := func(refresh, cacheOnly bool) status.Model {
		f, err := facts.Read(e.facts(refresh, cacheOnly))
		if err != nil {
			last.F.Errors = append(last.F.Errors[:0:0], facts.FactError{What: "config", Text: err.Error()})
			return last
		}
		last = status.Build(f, hint, time.Now())
		return last
	}
	back, cur := "", first
	for cur != "" && !idle {
		backWord := map[bool]string{true: "Back", false: "Quit"}[back != ""]
		var err error
		next := back
		switch cur {
		case "status":
			next, err = status.Run(status.Options{Screen: scr, Color: color, Title: title("Status"), Zone: zone,
				Read: readStatus, Stamp: e.stamp}, last)
			code, back = last.Code(), "status"
		case "keys":
			err = keys.Run(keys.Options{Screen: scr, Color: color, Title: title("Keys"), Back: backWord,
				Model:  func() keys.Model { m, _, _ := e.keysModel(); return m },
				Prover: func() keys.Prover { _, p, _ := e.keysModel(); return p }()})
		case "space":
			readSpace := func(refresh, cacheOnly bool) (space.Model, string) {
				f, err := facts.Read(e.facts(refresh, cacheOnly))
				if err != nil {
					return space.Model{}, err.Error()
				}
				return space.Build(f), checked(f)
			}
			err = space.Run(space.Options{Screen: scr, Color: color, Title: title("Space"), Back: backWord,
				Read: readSpace, Stamp: e.stamp})
		case "watch":
			st := e.getenv(facts.StateEnv)
			err = watch.Run(scr, watch.Options{Dir: filepath.Join(st, facts.GuestsDir, "view"), State: st, Color: color,
				Words: watch.Words{Title: title("watch"), Start: hint}})
			if errors.Is(err, watch.ErrIdle) && back != "" {
				err = nil // back to status, which has its own idle time
			}
		}
		if errors.Is(err, tui.ErrIdle) || errors.Is(err, watch.ErrIdle) {
			idle = true
		} else if err != nil {
			scr.Fini()
			fmt.Fprintf(e.stderr, "restic-pve %s: %v\n", cur, err)
			return 1
		}
		cur = next
	}
	scr.Fini()
	if idle {
		fmt.Fprintln(e.stdout, "The screen closed itself after 10 minutes without a key press (or a running backup). Nothing was changed.")
	}
	if first == "status" {
		fmt.Fprintln(e.stdout, last.VerdictLine())
	}
	return code
}

// restoreOnSignal gives the terminal back when the session ends (SIGHUP) or the screen is
// stopped (SIGTERM, SIGQUIT); raw mode turns Ctrl-C into a key.
func restoreOnSignal(scr tcell.Screen) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		select {
		case <-sigs:
			scr.Fini()
			fmt.Fprintln(os.Stderr, "restic-pve: stopped; nothing was changed")
			os.Exit(1)
		case <-done:
		}
	}()
	return func() {
		signal.Stop(sigs)
		close(done)
	}
}
