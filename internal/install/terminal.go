package install

import (
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"

	"github.com/gdamore/tcell/v2"
	"golang.org/x/term"
)

// tty is the terminal on stdin (keys) and stdout (screen).
type tty struct{}

// newTTY returns the terminal when both stdin and stdout are one, else nil.
func newTTY() Terminal {
	if !term.IsTerminal(0) || !term.IsTerminal(1) {
		return nil
	}
	return tty{}
}

func (tty) Size() (int, int, error) { return term.GetSize(1) }

// Screen is tcell's screen on the terminal: raw mode, the alternate screen and bracketed
// paste once it is initialised; Fini restores the terminal.
func (tty) Screen() (tcell.Screen, error) { return tcell.NewScreen() }

// Watch restores the terminal on SIGTERM, SIGHUP or SIGQUIT (and SIGINT, which raw mode
// turns into a key) and ends the program with exit 1.
func (tty) Watch(fini func()) func() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	go func() {
		select {
		case sig := <-sigs:
			fini()
			fmt.Fprintf(os.Stderr, "restic-pve: stopped by %v; nothing changed\n", sig)
			os.Exit(1)
		case <-done:
		}
	}()
	var once sync.Once
	return func() {
		once.Do(func() {
			signal.Stop(sigs)
			close(done)
		})
	}
}
