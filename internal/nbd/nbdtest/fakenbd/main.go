// Command fakenbd is a test stand-in for the NBD export QEMU opens for a Proxmox backup
// provider job. It is never part of a release; the end-to-end trial without Proxmox and the
// fake vzdump use it.
package main

import (
	"fmt"
	"os"

	"github.com/norandom/restic-pve/internal/nbd/nbdtest"
)

func main() {
	if err := nbdtest.Main(os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, "fakenbd:", err)
		os.Exit(1)
	}
}
