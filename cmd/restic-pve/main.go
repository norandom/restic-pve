// Command restic-pve is the one binary of the backup identity on a Proxmox VE host.
//
//	restic-pve request <ping|info <vmid>|dump <vmid>>   forced command (through sudo)
//	restic-pve install                                  root: asks for the site values, installs or changes the identity
//	restic-pve update                                   root: replace itself with the latest signed release
//	restic-pve uninstall                                root: remove the identity, its files and the binary
//	restic-pve version                                  print the version
package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"github.com/norandom/restic-pve/internal/install"
	"github.com/norandom/restic-pve/internal/relkeys"
	"github.com/norandom/restic-pve/internal/update"
	"github.com/norandom/restic-pve/internal/wrapper"
)

// Set at link time with -ldflags "-X main.version=... -X main.releaseRepo=...".
var (
	version     = "dev"
	releaseRepo = "norandom/restic-pve"
)

const usage = "usage: restic-pve request <ping|info <vmid>|dump <vmid>> | install | update | uninstall | version"

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, usage)
		return 64
	}
	switch {
	case args[0] == "request":
		return wrapper.Default(version).Run(args[1:])
	case args[0] == "version" && len(args) == 1:
		fmt.Fprintln(stdout, version)
		return 0
	case args[0] == "install" && len(args) == 1:
		exe, err := executable()
		if err != nil {
			fmt.Fprintf(stderr, "restic-pve: %v\n", err)
			return 1
		}
		return install.Default(version, exe).Install()
	case args[0] == "uninstall" && len(args) == 1:
		return install.Default(version, "").Uninstall()
	case args[0] == "update" && len(args) == 1:
		exe, err := executable()
		if err != nil {
			fmt.Fprintf(stderr, "restic-pve: %v\n", err)
			return 1
		}
		return update.Run(update.Config{
			BaseURL: update.BaseURL(releaseRepo), Target: exe, CurrentVersion: version,
			Keys: relkeys.PublicKeys, Arch: runtime.GOARCH, HTTPClient: update.Client(),
			IsRoot: func() bool { return os.Geteuid() == 0 }, Stdout: stdout, Stderr: stderr,
		})
	}
	fmt.Fprintln(stderr, usage)
	return 64
}

// executable returns the path of the running binary with symlinks resolved.
func executable() (string, error) {
	exe, err := os.Executable()
	if err == nil {
		exe, err = filepath.EvalSymlinks(exe)
	}
	if err != nil {
		return "", fmt.Errorf("locate the running binary: %w", err)
	}
	return exe, nil
}
