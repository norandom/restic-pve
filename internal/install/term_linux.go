package install

import (
	"errors"
	"fmt"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"unsafe"
)

// tty is the terminal on stdin (keys) and stdout (screen).
type tty struct{ in, out int }

// Escape sequences of the form's terminal: the alternate screen and bracketed paste on;
// and off again with the cursor shown.
const (
	ttyEnter = "\x1b[?1049h\x1b[?2004h"
	ttyLeave = "\x1b[?2004l\x1b[?25h\x1b[?1049l"
)

// newTTY returns the terminal when both stdin and stdout are one, else nil.
func newTTY() Terminal {
	if _, err := getTermios(0); err != nil {
		return nil
	}
	if _, err := getTermios(1); err != nil {
		return nil
	}
	return &tty{in: 0, out: 1}
}

func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

func getTermios(fd int) (*syscall.Termios, error) {
	t := &syscall.Termios{}
	return t, ioctl(fd, syscall.TCGETS, unsafe.Pointer(t))
}

func setTermios(fd int, t *syscall.Termios) error {
	return ioctl(fd, syscall.TCSETS, unsafe.Pointer(t))
}

func (t *tty) Size() (int, int, error) {
	var ws struct{ Row, Col, X, Y uint16 }
	if err := ioctl(t.out, syscall.TIOCGWINSZ, unsafe.Pointer(&ws)); err != nil {
		return 0, 0, err
	}
	if ws.Col == 0 || ws.Row == 0 {
		return 0, 0, errors.New("terminal size unknown")
	}
	return int(ws.Col), int(ws.Row), nil
}

// Start puts stdin into raw mode (no echo, no line editing, Ctrl-C arrives as a key) and
// stdout on the alternate screen. The returned stop restores both; it runs once, also on
// SIGINT, SIGTERM, SIGHUP or SIGQUIT, which then end the program with exit 1.
func (t *tty) Start() (func(), error) {
	old, err := getTermios(t.in)
	if err != nil {
		return nil, err
	}
	raw := *old
	raw.Iflag &^= syscall.ICRNL | syscall.INLCR | syscall.IGNCR | syscall.IXON | syscall.BRKINT | syscall.INPCK | syscall.ISTRIP
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := setTermios(t.in, &raw); err != nil {
		return nil, err
	}
	os.Stdout.WriteString(ttyEnter)

	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	done := make(chan struct{})
	var once sync.Once
	stop := func() {
		once.Do(func() {
			signal.Stop(sigs)
			os.Stdout.WriteString(ttyLeave)
			_ = setTermios(t.in, old)
			close(done)
		})
	}
	go func() {
		select {
		case sig := <-sigs:
			stop()
			fmt.Fprintf(os.Stderr, "restic-pve: stopped by %v; nothing changed\n", sig)
			os.Exit(1)
		case <-done:
		}
	}()
	return stop, nil
}
