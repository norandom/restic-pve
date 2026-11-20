// Package update replaces the running binary with the latest signed release.
//
// Order: fetch SHA256SUMS and its signature, verify the signature, fetch VERSION and
// check its hash, require a strictly newer version, fetch the binary into the target
// directory, check its hash, run it with "version" and compare, chmod 0755, rename.
// Every failure removes the temp file and leaves the installed binary untouched.
package update

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/relkeys"
)

// Download limits.
const (
	smallLimit  = 64 << 10 // SHA256SUMS, the signature, VERSION
	binaryLimit = 64 << 20
	fetchTime   = 10 * time.Minute
)

// Config describes one update run. Target is the installed binary (symlinks resolved).
type Config struct {
	BaseURL        string // ends with "/"
	Target         string
	CurrentVersion string
	Keys           []string
	Arch           string
	HTTPClient     *http.Client
	IsRoot         func() bool
	Stdout, Stderr io.Writer
	// HostSync: after the rename, run "<Target> host-sync", so the new binary brings the
	// Proxmox plug-in to its own embedded copy (it waits for a running backup). Its failure
	// does not fail the update: the next backup syncs the plug-in under the lock.
	HostSync bool
}

// BaseURL returns the latest-release download URL of a GitHub repository ("owner/name").
func BaseURL(repo string) string {
	return "https://github.com/" + repo + "/releases/latest/download/"
}

// Client returns the production HTTP client: 30 s connect, TLS and header timeouts,
// redirects only to https.
func Client() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   30 * time.Second,
			ResponseHeaderTimeout: 30 * time.Second,
		},
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if req.URL.Scheme != "https" {
				return errors.New("redirect to a non-https URL refused")
			}
			if len(via) >= 10 {
				return errors.New("too many redirects")
			}
			return nil
		},
	}
}

var (
	sumLineRe = regexp.MustCompile(`^([0-9a-f]{64})  ([^/\\\s]+)$`)
	versionRe = regexp.MustCompile(`^v([0-9]+)\.([0-9]+)\.([0-9]+)$`)
)

// Run performs the update and returns the exit code (0 updated or up to date, 1 otherwise).
func Run(c Config) int {
	if !c.IsRoot() {
		fmt.Fprintln(c.Stderr, "restic-pve: update must run as root")
		return 1
	}
	msg, replaced, err := c.update()
	if err != nil {
		fmt.Fprintf(c.Stderr, "restic-pve: update failed: %v\n", err)
		return 1
	}
	fmt.Fprintln(c.Stdout, msg)
	if replaced && c.HostSync {
		c.hostSync()
	}
	return 0
}

// hostSync runs the new binary's host-sync step and passes its lines on.
func (c Config) hostSync() {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, c.Target, "host-sync")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.CombinedOutput()
	if len(out) > 4096 {
		out = out[:4096]
	}
	c.Stdout.Write(out)
	if err != nil {
		fmt.Fprintf(c.Stderr, "restic-pve: plug-in sync after the update did not finish (%v); the next backup brings the plug-in up to date\n", err)
	}
}

func (c Config) update() (string, bool, error) {
	msg, err := c.check()
	if err != nil || msg != "" {
		return msg, false, err
	}
	return c.install()
}

// check returns a message when there is nothing to install.
func (c Config) check() (string, error) {
	if len(c.Keys) == 0 {
		return "", errors.New("no release keys in this build; install a release built with a signing key")
	}
	if c.Arch != "amd64" && c.Arch != "arm64" {
		return "", fmt.Errorf("no release binary for architecture %q (amd64 and arm64 only)", c.Arch)
	}
	return "", nil
}

func (c Config) install() (string, bool, error) {
	msg, err := c.fetchAndReplace()
	return msg, err == nil && strings.HasPrefix(msg, "restic-pve updated"), err
}

func (c Config) fetchAndReplace() (string, error) {
	asset := "restic-pve-linux-" + c.Arch
	sums, err := c.fetch("SHA256SUMS", smallLimit)
	if err != nil {
		return "", err
	}
	sig, err := c.fetch("SHA256SUMS.sig", smallLimit)
	if err != nil {
		return "", err
	}
	if err := relkeys.Verify(c.Keys, sums, sig); err != nil {
		return "", fmt.Errorf("SHA256SUMS: %w", err)
	}
	hashes, err := parseSums(sums)
	if err != nil {
		return "", err
	}
	verText, err := c.fetch("VERSION", smallLimit)
	if err != nil {
		return "", err
	}
	if err := checkHash(hashes, "VERSION", verText); err != nil {
		return "", err
	}
	newV, ok := parseVersion(strings.TrimSuffix(string(verText), "\n"))
	if !ok || !strings.HasSuffix(string(verText), "\n") || strings.Count(string(verText), "\n") != 1 {
		return "", errors.New("VERSION is not vMAJOR.MINOR.PATCH plus newline")
	}
	newName := strings.TrimSuffix(string(verText), "\n")
	if cur, ok := parseVersion(c.CurrentVersion); ok {
		switch cmp := compare(newV, cur); {
		case cmp == 0:
			return "restic-pve " + c.CurrentVersion + " is up to date", nil
		case cmp < 0:
			return "", fmt.Errorf("latest release %s is older than the installed %s; nothing changed", newName, c.CurrentVersion)
		}
	}
	if _, ok := hashes[asset]; !ok {
		return "", fmt.Errorf("SHA256SUMS lists no %s", asset)
	}
	if err := c.replace(asset, hashes[asset], verText); err != nil {
		return "", err
	}
	return "restic-pve updated from " + c.CurrentVersion + " to " + newName, nil
}

// replace downloads the binary next to the target, checks it and renames it over the target.
func (c Config) replace(asset, wantHash string, verText []byte) (err error) {
	f, err := os.CreateTemp(filepath.Dir(c.Target), ".restic-pve.update-*")
	if err != nil {
		return fmt.Errorf("temp file next to %s: %w", c.Target, err)
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tmp)
		}
	}()
	if err := f.Chmod(0o700); err != nil {
		f.Close()
		return err
	}
	h := sha256.New()
	err = c.download(asset, binaryLimit, io.MultiWriter(f, h))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != wantHash {
		return fmt.Errorf("%s: SHA-256 %s does not match SHA256SUMS", asset, got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tmp, "version")
	cmd.Env = []string{"LC_ALL=C", "PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("new binary failed to run 'version': %w", err)
	}
	if string(out) != string(verText) {
		return fmt.Errorf("new binary reports version %q, VERSION says %q", strings.TrimSpace(string(out)), strings.TrimSpace(string(verText)))
	}
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	return os.Rename(tmp, c.Target)
}

func (c Config) fetch(name string, limit int64) ([]byte, error) {
	var b strings.Builder
	if err := c.download(name, limit, &b); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}

// download writes one release asset to w, at most limit bytes.
func (c Config) download(name string, limit int64, w io.Writer) error {
	ctx, cancel := context.WithTimeout(context.Background(), fetchTime)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTPClient.Do(req)
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	defer resp.Body.Close()
	if resp.Request.URL.Scheme != "https" {
		return fmt.Errorf("download %s: not served over https", name)
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("download %s: HTTP %d", name, resp.StatusCode)
	}
	if resp.ContentLength > limit {
		return fmt.Errorf("download %s: larger than %d bytes", name, limit)
	}
	n, err := io.Copy(w, io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return fmt.Errorf("download %s: %w", name, err)
	}
	if n > limit {
		return fmt.Errorf("download %s: larger than %d bytes", name, limit)
	}
	return nil
}

// parseSums reads "<64 hex>  <name>" lines; names have no path separators, no duplicates.
func parseSums(data []byte) (map[string]string, error) {
	text := string(data)
	if !strings.HasSuffix(text, "\n") {
		return nil, errors.New("SHA256SUMS: missing final newline")
	}
	out := map[string]string{}
	for i, line := range strings.Split(strings.TrimSuffix(text, "\n"), "\n") {
		m := sumLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("SHA256SUMS line %d: expected '<sha256>  <name>'", i+1)
		}
		if _, dup := out[m[2]]; dup {
			return nil, fmt.Errorf("SHA256SUMS line %d: %s listed twice", i+1, m[2])
		}
		out[m[2]] = m[1]
	}
	return out, nil
}

func checkHash(hashes map[string]string, name string, data []byte) error {
	sum := sha256.Sum256(data)
	if want, ok := hashes[name]; !ok || want != hex.EncodeToString(sum[:]) {
		return fmt.Errorf("%s does not match SHA256SUMS", name)
	}
	return nil
}

// parseVersion parses vMAJOR.MINOR.PATCH; "dev" and anything else is not a version.
func parseVersion(s string) ([3]uint64, bool) {
	var v [3]uint64
	m := versionRe.FindStringSubmatch(s)
	if m == nil {
		return v, false
	}
	for i := range v {
		n, err := strconv.ParseUint(m[i+1], 10, 64)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func compare(a, b [3]uint64) int {
	for i := range a {
		switch {
		case a[i] > b[i]:
			return 1
		case a[i] < b[i]:
			return -1
		}
	}
	return 0
}
