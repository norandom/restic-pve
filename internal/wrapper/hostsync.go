package wrapper

import (
	"fmt"
	"os"
	"syscall"

	"github.com/norandom/restic-pve/internal/provider"
)

// HostSync is `restic-pve host-sync`, which update runs with the new binary right after the
// rename: it brings the plug-in file to this binary's embedded copy. It takes the host-wide
// lock without waiting; while a backup holds it, it changes nothing, because the running
// backup keeps the plug-in code it loaded at its start, and the next changes request syncs
// the file under the lock before it starts vzdump. It never creates the file (changed reads
// are off when it is absent) and never touches one restic-pve did not write.
// Exit 0: synced, unchanged, off or postponed; 1: the file could not be brought up to date.
func (e *Env) HostSync() int {
	lock, err := os.OpenFile(e.LockFile, os.O_WRONLY|os.O_CREATE, 0o600)
	if err != nil {
		fmt.Fprintf(e.Stderr, "restic-pve: host-sync: open lock file: %v\n", err)
		return 1
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		fmt.Fprintln(e.Stdout, "restic-pve: host-sync: a backup is running; the plug-in is brought up to date when the next backup starts")
		return 0
	}
	h := e.host()
	h.Report = func(word, what string) { fmt.Fprintf(e.Stdout, "%-9s %s\n", word, what) }
	st, err := h.SyncPlugin()
	if err != nil {
		fmt.Fprintf(e.Stderr, "restic-pve: host-sync: %v\n", err)
		return 1
	}
	if st == provider.Absent {
		fmt.Fprintln(e.Stdout, "restic-pve: host-sync: changed reads are off on this host; nothing to do")
		return 0
	}
	if h.Clustered() {
		fmt.Fprintln(e.Stdout, "restic-pve: host-sync: this host is part of a Proxmox cluster, where changed reads are refused; "+
			"run restic-pve install and answer no to changed reads to remove the plug-in")
	}
	if a, err := h.ProxmoxAPI(); err == nil {
		if p := a.Problem(); p != nil {
			fmt.Fprintf(e.Stdout, "restic-pve: host-sync: %v\n", p)
		} else if a.Newer() {
			fmt.Fprintf(e.Stdout, "restic-pve: host-sync: %s\n", a.NewerNote())
		}
	}
	return 0
}
