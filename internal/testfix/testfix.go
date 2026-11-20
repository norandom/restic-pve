// Package testfix finds the outside programs some tests need: the pinned restic release and a
// Perl with the core modules the Proxmox plug-in uses. Nothing is downloaded. A test that needs
// a program it cannot find is skipped with a message that says how to provide it, so
// `go test ./...` passes offline in the release image, which has neither.
//
// Set RESTIC_PVE_TEST_REQUIRE=1 to turn every such skip into a failure; the full run in
// docs/reference/build-and-test.md does, so it cannot pass with a test silently left out.
package testfix

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The restic release the backup image pins (docker/backup/Dockerfile, RESTIC_VERSION).
const (
	ResticVersion = "0.19.1"
	// ResticArchive and ResticArchiveSHA256 are the release file and its line in the release's
	// SHA256SUMS (https://github.com/restic/restic/releases/download/v0.19.1/SHA256SUMS).
	ResticArchive       = "restic_0.19.1_linux_amd64.bz2"
	ResticArchiveSHA256 = "f415415624dcc452f2a02b8c33641791a8c6d6d3b65bbb3543fcf9a25151585c"
	// ResticBinarySHA256 is the SHA-256 of the binary inside that archive (bunzip2), which is
	// also the binary in the backup image.
	ResticBinarySHA256 = "20d4142678d0d95ec11a4759def1b73fd9190abc9ca19e4b62d067c0b387e639"
)

// Environment variables the fixtures read.
const (
	EnvRestic  = "RESTIC_PVE_TEST_RESTIC"  // path of the restic 0.19.1 binary
	EnvPerl    = "RESTIC_PVE_TEST_PERL"    // path of perl; default: perl in PATH
	EnvRequire = "RESTIC_PVE_TEST_REQUIRE" // "1": a missing program fails the test
)

// perlModules are the modules the plug-in loads; all are core modules of Perl 5.
var perlModules = []string{"IO::Socket::UNIX", "JSON::PP", "MIME::Base64"}

var (
	mu      sync.Mutex
	checked = map[string]error{} // program path -> result of its check
)

// skip skips t, or fails it when RESTIC_PVE_TEST_REQUIRE=1.
func skip(t testing.TB, format string, args ...any) {
	t.Helper()
	msg := fmt.Sprintf(format, args...)
	if os.Getenv(EnvRequire) == "1" {
		t.Fatalf("%s (%s=1)", msg, EnvRequire)
	}
	t.Skip(msg)
}

// DefaultResticPath is where Restic looks when RESTIC_PVE_TEST_RESTIC is unset:
// <user cache dir>/restic-pve-test/restic-0.19.1. Empty when there is no cache dir.
func DefaultResticPath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "restic-pve-test", "restic-"+ResticVersion)
}

// Restic returns the path of the pinned restic binary: RESTIC_PVE_TEST_RESTIC, else
// DefaultResticPath. It skips t when neither exists. A binary that exists but is not the
// pinned release (checksum or `restic version`) fails t: a test against another restic
// proves nothing about the one the backup image runs.
func Restic(t testing.TB) string {
	t.Helper()
	p, missing, err := findRestic()
	if missing != "" {
		skip(t, "%s", missing)
	}
	if err != nil {
		t.Fatalf("%v", err)
	}
	return p
}

// findRestic returns the binary, or why it is missing (a skip), or why it is not the pinned
// release (a failure).
func findRestic() (path, missing string, err error) {
	p := os.Getenv(EnvRestic)
	if p == "" {
		p = DefaultResticPath()
	}
	if p == "" {
		return "", fmt.Sprintf("restic %s not found: set %s to its path (docs/reference/build-and-test.md)", ResticVersion, EnvRestic), nil
	}
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Sprintf("restic %s not found at %s: set %s to its path (docs/reference/build-and-test.md)", ResticVersion, p, EnvRestic), nil
	}
	if err := once(p, checkRestic); err != nil {
		return "", "", fmt.Errorf("%s is not restic %s: %v", p, ResticVersion, err)
	}
	return p, "", nil
}

func once(p string, check func(string) error) error {
	mu.Lock()
	defer mu.Unlock()
	if err, ok := checked[p]; ok {
		return err
	}
	err := check(p)
	checked[p] = err
	return err
}

func checkRestic(p string) error {
	sum, err := fileSHA256(p)
	if err != nil {
		return err
	}
	if sum != ResticBinarySHA256 {
		return fmt.Errorf("sha256 %s, want %s", sum, ResticBinarySHA256)
	}
	out, err := run(p, nil, "version")
	if err != nil {
		return fmt.Errorf("restic version: %v", err)
	}
	if !strings.HasPrefix(out, "restic "+ResticVersion+" ") {
		return fmt.Errorf("restic version says %q", strings.TrimSpace(out))
	}
	return nil
}

// Perl returns the path of a perl that has the plug-in's core modules: RESTIC_PVE_TEST_PERL,
// else perl in PATH. It skips t when there is none. An explicitly set perl that cannot load
// the modules fails t.
func Perl(t testing.TB) string {
	t.Helper()
	p, missing, err := findPerl()
	if missing != "" {
		skip(t, "%s", missing)
	}
	if err != nil {
		t.Fatalf("%v", err)
	}
	return p
}

func findPerl() (path, missing string, err error) {
	p := os.Getenv(EnvPerl)
	explicit := p != ""
	if !explicit {
		if p, err = exec.LookPath("perl"); err != nil {
			return "", fmt.Sprintf("perl not found: install perl or set %s (docs/reference/build-and-test.md)", EnvPerl), nil
		}
	}
	if err := once(p, checkPerl); err != nil {
		if explicit {
			return "", "", fmt.Errorf("%s: %v", p, err)
		}
		return "", fmt.Sprintf("%s cannot load %s: %v", p, strings.Join(perlModules, ", "), err), nil
	}
	return p, "", nil
}

func checkPerl(p string) error {
	args := []string{}
	for _, m := range perlModules {
		args = append(args, "-M"+m)
	}
	_, err := run(p, nil, append(args, "-e", "1")...)
	return err
}

func run(name string, env []string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

func fileSHA256(p string) (string, error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Repo is a throwaway local restic repository in a test's temporary directory, with a made-up
// password. Nothing of it outlives the test.
type Repo struct {
	Bin string
	Dir string
	env []string
}

// NewRepo creates and initialises a repository with the pinned restic (skipping t without it).
func NewRepo(t testing.TB) *Repo {
	t.Helper()
	bin := Restic(t)
	base := t.TempDir()
	r := &Repo{Bin: bin, Dir: filepath.Join(base, "repo")}
	r.env = []string{
		"PATH=/usr/bin:/bin",
		"HOME=" + base,
		"RESTIC_REPOSITORY=" + r.Dir,
		"RESTIC_PASSWORD=made-up-test-password",
		"RESTIC_CACHE_DIR=" + filepath.Join(base, "cache"),
		"RESTIC_PROGRESS_FPS=0.1",
	}
	if out, err := r.Run("init"); err != nil {
		t.Fatalf("restic init: %v\n%s", err, out)
	}
	return r
}

// Run runs restic with args against the repository and returns its stdout.
func (r *Repo) Run(args ...string) ([]byte, error) {
	cmd := exec.Command(r.Bin, args...)
	cmd.Env = r.env
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, fmt.Errorf("restic %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}

// Cmd returns an unstarted restic command against the repository, for streaming output.
func (r *Repo) Cmd(args ...string) *exec.Cmd {
	cmd := exec.Command(r.Bin, args...)
	cmd.Env = r.env
	return cmd
}
