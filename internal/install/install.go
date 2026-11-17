// Package install sets up the restricted backup identity on a Proxmox VE host from
// answers given on the host: user, key, sudoers entry, the binary, the settings file and
// the daily update timer.
package install

import (
	"bufio"
	"bytes"
	_ "embed"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/norandom/restic-pve/internal/wrapper"
)

// Installed paths.
const (
	Binary     = "/usr/local/sbin/restic-pve"
	confFile   = "/etc/restic-pve.conf"
	lockFile   = "/run/restic-pve.lock"
	sudoersDir = "/etc/sudoers.d"
	sudoers    = sudoersDir + "/restic-pve"
	loginShell = "/bin/sh"
	generated  = "# written by restic-pve install - do not edit, run restic-pve install again"
	searchPath = "/usr/sbin:/usr/bin:/sbin:/bin"
	unitDir    = "/etc/systemd/system"
	timerName  = "restic-pve-update.timer"
)

// The daily automatic update: a oneshot service running "restic-pve update" and its timer.
var (
	//go:embed units/restic-pve-update.service
	serviceUnit []byte
	//go:embed units/restic-pve-update.timer
	timerUnit []byte
)

// units maps each installed unit path to its content.
var units = []struct {
	path    string
	content []byte
}{
	{unitDir + "/restic-pve-update.service", serviceUnit},
	{unitDir + "/" + timerName, timerUnit},
}

// oldFiles belong to the bash wrapper and are removed after the install.
var oldFiles = []string{"/usr/local/sbin/restic-vzdump", "/etc/sudoers.d/restic-vzdump",
	"/etc/restic-vzdump.conf", "/etc/restic-vzdump.allow"}

// System is the host the install runs on. Tests use a temp Root and a fake Run.
type System struct {
	Root       string                                            // prefix for every path ("" in production)
	Run        func(name string, args ...string) ([]byte, error) // runs a command, returns its stdout
	Executable string                                            // the running binary, copied to Binary
	Chown      func(path string, uid, gid int) error
	IsRoot     func() bool
	Version    string
	Stdin      *bufio.Reader // the answers, one per line
	Stdout     io.Writer
	Stderr     io.Writer
}

// Default returns the production system.
func Default(version, executable string) *System {
	return &System{
		Run: func(name string, args ...string) ([]byte, error) {
			cmd := exec.Command(name, args...)
			cmd.Env = []string{"LC_ALL=C", "PATH=" + searchPath}
			cmd.Stderr = os.Stderr
			return cmd.Output()
		},
		Executable: executable,
		Chown:      os.Chown,
		IsRoot:     func() bool { return os.Geteuid() == 0 },
		Version:    version,
		Stdin:      bufio.NewReader(os.Stdin),
		Stdout:     os.Stdout,
		Stderr:     os.Stderr,
	}
}

// Install asks for the site values on stdin and installs the identity. It returns the
// exit code: 0 done, 1 not root, not a PVE host, stopped before the install (nothing
// changed) or a step failed.
func (s *System) Install() int {
	if !s.IsRoot() {
		fmt.Fprintln(s.Stderr, "restic-pve: install must run as root on the Proxmox VE host")
		return 1
	}
	for _, tool := range []string{"qm", "pvesm"} {
		if !s.onPath(tool) {
			fmt.Fprintf(s.Stderr, "restic-pve: %s not found in %s: this is not a Proxmox VE host; nothing changed\n", tool, searchPath)
			return 1
		}
	}
	v, err := s.ask()
	if err != nil {
		fmt.Fprintf(s.Stderr, "restic-pve: %v; nothing changed\n", err)
		return 1
	}
	if err := s.install(v); err != nil {
		fmt.Fprintf(s.Stderr, "restic-pve: install failed: %v\n", err)
		return 1
	}
	fmt.Fprintf(s.Stdout, "restic-pve %s ready: user %s, key accepted from %s, guests: %s\n"+
		"This host updates restic-pve daily to the latest signed release (%s).\n",
		s.Version, v.User, v.From, v.vmidList(" "), timerName)
	return 0
}

// ask lists the guests and storages, asks the questions and the confirmation.
func (s *System) ask() (*Values, error) {
	gs, err := s.guests()
	if err != nil {
		return nil, err
	}
	if len(gs) == 0 {
		return nil, errors.New("qm list shows no guests on this host")
	}
	sts, err := s.storages()
	if err != nil {
		return nil, err
	}
	if len(sts) == 0 {
		return nil, errors.New("pvesm status shows no active storage for fleecing")
	}
	fmt.Fprintf(s.Stdout, "restic-pve %s install. Enter takes the value in [brackets].\n\n", s.Version)
	v, err := s.questions(s.current(), gs, sts)
	if err != nil {
		return nil, err
	}
	s.summary(v)
	var answer string
	err = (&asker{in: s.Stdin, out: s.Stdout}).ask("Install with these values? [y/N]", "",
		func(x string) error { answer = strings.ToLower(x); return nil })
	if err != nil {
		return nil, err
	}
	if answer != "y" && answer != "yes" {
		return nil, errStop("not confirmed")
	}
	return v, nil
}

func (s *System) path(p string) string { return filepath.Join(s.Root, p) }

func (s *System) report(word, what string) { fmt.Fprintf(s.Stdout, "%-9s %s\n", word, what) }

func (s *System) run(name string, args ...string) error {
	if _, err := s.Run(name, args...); err != nil {
		return fmt.Errorf("%s %s: %w", name, strings.Join(args, " "), err)
	}
	return nil
}

func (s *System) install(v *Values) error {
	if err := s.sudoPackage(); err != nil {
		return err
	}
	uid, gid, home, err := s.user(v.User)
	if err != nil {
		return err
	}
	sshDir, err := s.homeDirs(home, uid, gid)
	if err != nil {
		return err
	}
	key := `restrict,from="` + v.From + `",command="sudo -n ` + Binary + ` request $SSH_ORIGINAL_COMMAND" ` + v.Pubkey + "\n"
	if err := s.putFile(filepath.Join(sshDir, "authorized_keys"), 0o600, uid, gid, []byte(key), nil); err != nil {
		return err
	}
	if err := s.sudoersFile(v.User); err != nil {
		return err
	}
	if err := s.binary(); err != nil {
		return err
	}
	conf := generated + "\n"
	for i, val := range v.confValues() {
		conf += wrapper.ConfKeys[i] + "=" + val + "\n"
	}
	if err := s.putFile(confFile, 0o644, 0, 0, []byte(conf), nil); err != nil {
		return err
	}
	if err := s.updateTimer(); err != nil {
		return err
	}
	for _, p := range oldFiles {
		if gone, err := s.remove(p); err != nil {
			return err
		} else if !gone {
			s.report("removed", p)
		}
	}
	return nil
}

// binary puts the running executable at Binary (owner root, mode 0755). When it already
// runs from Binary (the documented download goes straight there), nothing is copied and
// only owner and mode are set.
func (s *System) binary() error {
	src, err := os.Stat(s.Executable)
	if err != nil {
		return fmt.Errorf("read the running binary: %w", err)
	}
	if dst, err := os.Stat(s.path(Binary)); err == nil && os.SameFile(src, dst) {
		if err := s.ownMode(Binary, 0o755, 0, 0); err != nil {
			return err
		}
		s.report("unchanged", Binary)
		return nil
	}
	exe, err := os.ReadFile(s.Executable)
	if err != nil {
		return fmt.Errorf("read the running binary: %w", err)
	}
	return s.putFile(Binary, 0o755, 0, 0, exe, nil)
}

// updateTimer writes the update service and timer, reloads systemd and enables the timer.
// Both systemctl calls are idempotent and run on every install.
func (s *System) updateTimer() error {
	if _, err := os.Stat(s.path(unitDir)); err != nil {
		if err := s.mkdir(unitDir, 0o755, 0, 0); err != nil {
			return err
		}
	}
	for _, u := range units {
		if err := s.putFile(u.path, 0o644, 0, 0, u.content, nil); err != nil {
			return err
		}
	}
	if err := s.run("systemctl", "daemon-reload"); err != nil {
		return err
	}
	return s.run("systemctl", "enable", "--now", timerName)
}

// remove deletes p if present; gone is true when it was already absent.
func (s *System) remove(p string) (gone bool, err error) {
	if _, err := os.Lstat(s.path(p)); errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err := os.Remove(s.path(p)); err != nil {
		return false, fmt.Errorf("remove %s: %w", p, err)
	}
	return false, nil
}

// onPath reports whether name is an executable in searchPath.
func (s *System) onPath(name string) bool {
	for _, dir := range filepath.SplitList(searchPath) {
		if st, err := os.Stat(s.path(filepath.Join(dir, name))); err == nil && st.Mode().IsRegular() && st.Mode()&0o111 != 0 {
			return true
		}
	}
	return false
}

func (s *System) sudoPackage() error {
	if s.onPath("sudo") && s.onPath("visudo") {
		return nil
	}
	fmt.Fprintln(s.Stdout, "installing sudo")
	if err := s.run("apt-get", "update", "-qq"); err != nil {
		return err
	}
	return s.run("apt-get", "install", "-y", "-qq", "sudo")
}

type passwdEntry struct {
	uid, gid    int
	home, shell string
}

// errNoUser is returned by passwd when getent does not know the user.
var errNoUser = errors.New("no such user")

// passwd looks up name with getent.
func (s *System) passwd(name string) (passwdEntry, error) {
	pw, err := s.Run("getent", "passwd", name)
	if err != nil {
		return passwdEntry{}, errNoUser
	}
	f := strings.Split(strings.SplitN(string(pw), "\n", 2)[0], ":")
	if len(f) != 7 || !filepath.IsAbs(f[5]) {
		return passwdEntry{}, fmt.Errorf("getent passwd %s: unexpected entry", name)
	}
	uid, err1 := strconv.Atoi(f[2])
	gid, err2 := strconv.Atoi(f[3])
	if err1 != nil || err2 != nil {
		return passwdEntry{}, fmt.Errorf("getent passwd %s: unexpected uid or gid", name)
	}
	return passwdEntry{uid, gid, f[5], f[6]}, nil
}

// user creates the system user if needed, fixes its shell, locks its password and
// returns its uid, gid and home directory.
func (s *System) user(name string) (int, int, string, error) {
	pw, err := s.passwd(name)
	if errors.Is(err, errNoUser) {
		if err := s.run("useradd", "--system", "--create-home", "--shell", loginShell,
			"--comment", "restic backup (forced command only)", name); err != nil {
			return 0, 0, "", err
		}
		s.report("created", "user "+name)
		if pw, err = s.passwd(name); errors.Is(err, errNoUser) {
			return 0, 0, "", fmt.Errorf("getent passwd %s after useradd: %w", name, err)
		}
	}
	if err != nil {
		return 0, 0, "", err
	}
	if pw.shell != loginShell {
		if err := s.run("usermod", "--shell", loginShell, name); err != nil {
			return 0, 0, "", err
		}
		s.report("changed", "login shell of "+name+" to "+loginShell)
	}
	sh, _ := s.Run("getent", "shadow", name)
	if sf := strings.Split(string(sh), ":"); len(sf) < 2 || !strings.HasPrefix(sf[1], "!") {
		if err := s.run("usermod", "--lock", name); err != nil {
			return 0, 0, "", err
		}
		s.report("locked", "password of "+name)
	}
	return pw.uid, pw.gid, pw.home, nil
}

// homeDirs creates the home directory if missing and enforces ~/.ssh (0700, the user's).
func (s *System) homeDirs(home string, uid, gid int) (string, error) {
	if st, err := os.Stat(s.path(home)); err != nil || !st.IsDir() {
		if err := s.mkdir(home, 0o750, uid, gid); err != nil {
			return "", err
		}
	}
	sshDir := filepath.Join(home, ".ssh")
	st, err := os.Lstat(s.path(sshDir))
	switch {
	case errors.Is(err, os.ErrNotExist):
		return sshDir, s.mkdir(sshDir, 0o700, uid, gid)
	case err != nil:
		return "", err
	case !st.IsDir():
		return "", fmt.Errorf("%s is not a directory", sshDir)
	}
	return sshDir, s.ownMode(sshDir, 0o700, uid, gid)
}

func (s *System) mkdir(p string, mode os.FileMode, uid, gid int) error {
	if err := os.MkdirAll(filepath.Dir(s.path(p)), 0o755); err != nil {
		return err
	}
	if err := os.Mkdir(s.path(p), mode); err != nil && !errors.Is(err, os.ErrExist) {
		return err
	}
	return s.ownMode(p, mode, uid, gid)
}

func (s *System) ownMode(p string, mode os.FileMode, uid, gid int) error {
	if err := s.Chown(s.path(p), uid, gid); err != nil {
		return err
	}
	return os.Chmod(s.path(p), mode)
}

// sudoersLine allows exactly "request" alone (an empty ssh command, refused by the
// binary with exit 64) and "request" with arguments.
func sudoersLine(user string) string {
	return user + " ALL=(root) NOPASSWD: " + Binary + " request, " + Binary + " request *"
}

func (s *System) sudoersFile(user string) error {
	if _, err := os.Stat(s.path(sudoersDir)); err != nil {
		if err := s.mkdir(sudoersDir, 0o755, 0, 0); err != nil {
			return err
		}
	}
	text := generated + "\n" + sudoersLine(user) + "\n"
	return s.putFile(sudoers, 0o440, 0, 0, []byte(text), func(tmp string) error {
		if _, err := s.Run("visudo", "-cf", tmp); err != nil {
			return fmt.Errorf("visudo -cf rejected the new sudoers file: %w", err)
		}
		return nil
	})
}

// putFile installs content at p (atomic rename of a temp file in the same directory) when
// it changed, and enforces owner and mode either way. check runs on the temp file first.
func (s *System) putFile(p string, mode os.FileMode, uid, gid int, content []byte, check func(tmp string) error) error {
	full := s.path(p)
	if st, err := os.Lstat(full); err == nil && st.Mode().IsRegular() {
		if old, err := os.ReadFile(full); err == nil && bytes.Equal(old, content) {
			if err := s.ownMode(p, mode, uid, gid); err != nil {
				return err
			}
			s.report("unchanged", p)
			return nil
		}
	}
	// The temp name contains a dot, so sudo ignores it inside /etc/sudoers.d.
	f, err := os.CreateTemp(filepath.Dir(full), "."+filepath.Base(full)+".new-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	err = func() error {
		_, err := f.Write(content)
		if err == nil {
			err = f.Sync()
		}
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = s.Chown(tmp, uid, gid)
		}
		if err == nil {
			err = os.Chmod(tmp, mode)
		}
		if err == nil && check != nil {
			err = check(tmp)
		}
		if err == nil {
			err = os.Rename(tmp, full)
		}
		return err
	}()
	if err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", p, err)
	}
	s.report("written", p)
	return nil
}
