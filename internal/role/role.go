// Package role knows which machine restic-pve runs on: the Proxmox host or the backup machine.
// Every command belongs to one role; a command of the other role is refused before it does
// anything.
package role

import (
	"fmt"
	"io/fs"
)

// Role is the machine the tool runs on.
type Role int

const (
	None   Role = iota // neither fact holds: host commands behave as before
	Host               // the Proxmox host
	Backup             // the backup machine
)

// StateEnv names the state root of the backup machine. Its presence makes a backup machine.
const StateEnv = "RESTIC_PVE_STATE"

// The facts of a Proxmox host: its cluster config folder, or the tool's host settings file.
const (
	pveDir   = "/etc/pve"
	hostConf = "/etc/restic-pve.conf"
)

// Machine is the name of the machine for messages.
func (r Role) Machine() string {
	switch r {
	case Host:
		return "Proxmox host"
	case Backup:
		return "backup machine"
	}
	return ""
}

// Detect returns Host when /etc/pve is a folder or /etc/restic-pve.conf exists, Backup when
// RESTIC_PVE_STATE is set, and None otherwise. Host wins when both hold: a Proxmox host must
// never run a backup-role command by accident.
func Detect(stat func(string) (fs.FileInfo, error), getenv func(string) string) Role {
	if fi, err := stat(pveDir); err == nil && fi.IsDir() {
		return Host
	}
	if _, err := stat(hostConf); err == nil {
		return Host
	}
	if getenv(StateEnv) != "" {
		return Backup
	}
	return None
}

// Error refuses a command; Code is the exit status (64 wrong machine, 78 state root not set).
type Error struct {
	Msg  string
	Code int
}

func (e *Error) Error() string { return e.Msg }

// Require returns nil when cmd (of role want; None means both) may run where have was
// detected. Host commands run when no role was detected, as before the roles existed.
func Require(cmd string, want, have Role) error {
	switch {
	case want == None || want == have || want == Host && have == None:
		return nil
	case have == None:
		return &Error{fmt.Sprintf("restic-pve %s is for the backup machine, and %s is not set. Nothing was changed. "+
			"Set %s to the backup machine's state folder, for example %s=/state.", cmd, StateEnv, StateEnv, StateEnv), 78}
	}
	next := "Run it on the backup machine."
	if want == Host {
		next = "Run it in a root shell on the Proxmox host."
	}
	return &Error{fmt.Sprintf("restic-pve %s is for the %s; this is the %s. Nothing was changed. %s",
		cmd, want.Machine(), have.Machine(), next), 64}
}
