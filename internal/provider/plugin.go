// Package provider holds what restic-pve puts into Proxmox VE for changed-block reads: the
// backup provider plug-in ResticPVEPlugin.pm, embedded here and written to the host only from
// this copy.
package provider

import (
	"bytes"
	_ "embed"
)

// Plugin is the plug-in file, byte for byte as it is written to PluginPath.
//
//go:embed ResticPVEPlugin.pm
var Plugin []byte

const (
	// PluginPath is where Proxmox loads custom storage plug-ins from.
	PluginPath = "/usr/share/perl5/PVE/Storage/Custom/ResticPVEPlugin.pm"
	// Marker is the first line of the plug-in. A file at PluginPath without it is not ours
	// and is never touched.
	Marker = "# restic-pve plug-in, protocol 1. Written by restic-pve from its embedded copy; do not edit. Removed by restic-pve uninstall."
	// Protocol is the RPC protocol the plug-in sends in job_init (our $PROTOCOL).
	Protocol = 1
	// Socket is the job server's socket the plug-in connects to.
	Socket = "/run/restic-pve/provider.sock"
	// Refusal is the plug-in's job_init message when no restic-pve job is running.
	Refusal = "restic-pve: this storage takes only backups started by restic-pve on the backup machine; use another storage for Proxmox's own backup jobs"
)

// HasMarker reports whether content starts with the marker line.
func HasMarker(content []byte) bool {
	return bytes.HasPrefix(content, []byte(Marker+"\n"))
}
