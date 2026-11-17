package install

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// Uninstall returns the host to clean: update timer and service, sudoers entry, settings,
// old wrapper files, lock, the backup user with its home, and last the binary. The sudo
// package stays.
// Exit codes: 0 done, 1 not root or a step failed, 75 a dump holds the lock.
func (s *System) Uninstall() int {
	if !s.IsRoot() {
		fmt.Fprintln(s.Stderr, "restic-pve: uninstall must run as root on the Proxmox VE host")
		return 1
	}
	lock, err := os.OpenFile(s.path(lockFile), os.O_WRONLY, 0)
	if err == nil {
		defer lock.Close()
		if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			fmt.Fprintln(s.Stderr, "restic-pve: uninstall refused: a dump is running (lock held); nothing changed")
			return 75
		}
	}
	if err := s.removeTimer(); err != nil {
		fmt.Fprintf(s.Stderr, "restic-pve: uninstall failed: %v\n", err)
		return 1
	}
	user := s.sudoersUser()
	if user == "" {
		fmt.Fprintf(s.Stderr, "restic-pve: backup user unknown (no valid entry in %s); users left alone\n", sudoers)
	}
	paths := append(append([]string{sudoers, confFile}, oldFiles...), lockFile)
	for _, p := range paths {
		if err := s.removeReport(p); err != nil {
			fmt.Fprintf(s.Stderr, "restic-pve: uninstall failed: %v\n", err)
			return 1
		}
	}
	if user != "" {
		if _, err := s.Run("getent", "passwd", user); err != nil {
			s.report("absent", "user "+user)
		} else if err := s.run("userdel", "--remove", user); err != nil {
			fmt.Fprintf(s.Stderr, "restic-pve: uninstall failed: %v\n", err)
			return 1
		} else {
			s.report("removed", "user "+user+" and its home")
		}
	}
	if err := s.removeReport(Binary); err != nil {
		fmt.Fprintf(s.Stderr, "restic-pve: uninstall failed: %v\n", err)
		return 1
	}
	return 0
}

// removeTimer stops and disables the update timer if its unit file is there, removes both
// unit files and reloads systemd when one was removed.
func (s *System) removeTimer() error {
	if _, err := os.Lstat(s.path(unitDir + "/" + timerName)); err == nil {
		if err := s.run("systemctl", "disable", "--now", timerName); err != nil {
			return err
		}
	}
	removed := false
	for _, u := range units {
		gone, err := s.remove(u.path)
		if err != nil {
			return err
		}
		removed = removed || !gone
		if gone {
			s.report("absent", u.path)
		} else {
			s.report("removed", u.path)
		}
	}
	if removed {
		return s.run("systemctl", "daemon-reload")
	}
	return nil
}

func (s *System) removeReport(p string) error {
	gone, err := s.remove(p)
	if err != nil {
		return err
	}
	if gone {
		s.report("absent", p)
	} else {
		s.report("removed", p)
	}
	return nil
}

// sudoersUser reads the user name from the entry install wrote; "" when unknown.
func (s *System) sudoersUser() string {
	data, err := os.ReadFile(s.path(sudoers))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		user, rest, _ := strings.Cut(line, " ")
		if !userRe.MatchString(user) || rest != strings.TrimPrefix(sudoersLine(user), user+" ") {
			return ""
		}
		return user
	}
	return ""
}
