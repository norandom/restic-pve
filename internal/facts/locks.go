package facts

import (
	"os"

	"golang.org/x/sys/unix"
)

// Locked reports whether another process holds the flock of path. It never creates the
// file. The probe takes the lock without waiting and gives it back at once, so a job that
// starts in exactly that moment could find it taken; the probe runs only when a screen reads
// its facts again (on open, on a changed file, on r), never in a loop of its own.
func Locked(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false // no lock file: nothing holds it
	}
	defer f.Close()
	fd := int(f.Fd())
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return err == unix.EWOULDBLOCK || err == unix.EAGAIN
	}
	_ = unix.Flock(fd, unix.LOCK_UN)
	return false
}
