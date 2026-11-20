package provider

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// The storage API window of the plug-in (ResticPVEPlugin.pm: $APIVER_MIN, $APIVER_TESTED).
// Proxmox loads a custom plug-in whose api() lies between its APIVER - APIAGE and APIVER
// (pve-storage src/PVE/Storage.pm), and warns "Plugin ... is implementing an older storage
// API, an upgrade is recommended" in every process that loads it when api() != APIVER.
const (
	APIMin    = 11 // the first storage API with backup providers (Proxmox VE 8.4)
	APITested = 17 // the highest version the plug-in was checked against
)

// CorosyncConfig exists on a node that is part of a Proxmox cluster.
const CorosyncConfig = "/etc/pve/corosync.conf"

// ErrCluster refuses changed reads on a cluster node: the storage entry is cluster-wide, the
// plug-in file only on this node.
var ErrCluster = errors.New("this host is part of a Proxmox cluster (" + CorosyncConfig + " exists): " +
	"the storage entry would be cluster-wide, but the plug-in only on this node, so the other nodes would " +
	"warn about an unknown storage type and could drop the entry; changed reads work on single hosts for now. " +
	"Run restic-pve install again and answer no to changed reads")

// Clustered reports whether this host is a cluster node.
func (h *Host) Clustered() bool {
	_, err := os.Lstat(h.path(CorosyncConfig))
	return err == nil
}

// API is what Perl reports of this Proxmox's storage API.
type API struct {
	Ver, Age int
	Base     bool // PVE::BackupProvider::Plugin::Base is installed
}

// The Perl one-liners run through Host.Run, bounded by timeout(1). Warnings are printed on
// stdout, so a refusal can say what Proxmox said.
const (
	perlTimeout = "60"
	apiScript   = `local $SIG{__WARN__} = sub { print "warn: $_[0]" }; ` +
		`if (!eval { require PVE::Storage; 1 }) { print "no PVE::Storage\n"; exit 0 } ` +
		`my $b = eval { require PVE::BackupProvider::Plugin::Base; 1 } ? 1 : 0; ` +
		`print "api ", PVE::Storage::APIVER(), " ", PVE::Storage::APIAGE(), " base $b\n";`
	loadScript = `local $SIG{__WARN__} = sub { print "warn: $_[0]" }; require PVE::Storage; ` +
		`print eval { PVE::Storage::Plugin->lookup('` + StorageType + `'); 1 } ? "loaded\n" : "not loaded\n";`
)

func (h *Host) perl(script string) (string, error) {
	out, err := h.Run("timeout", perlTimeout, "perl", "-e", script)
	return string(out), err
}

// ProxmoxAPI asks Perl for the storage API version and age and whether the backup provider
// base class is there.
func (h *Host) ProxmoxAPI() (API, error) {
	out, err := h.perl(apiScript)
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 5 && f[0] == "api" && f[3] == "base" {
			v, e1 := strconv.Atoi(f[1])
			a, e2 := strconv.Atoi(f[2])
			if e1 == nil && e2 == nil {
				return API{Ver: v, Age: a, Base: f[4] == "1"}, nil
			}
		}
	}
	if err == nil {
		err = errors.New(strings.TrimSpace(out))
	}
	return API{}, fmt.Errorf("cannot ask Perl about this Proxmox's storage API (%v); nothing was written", err)
}

// Problem says why the plug-in cannot work on this Proxmox, or nil.
func (a API) Problem() error {
	switch {
	case !a.Base || a.Ver < APIMin:
		return fmt.Errorf("this Proxmox has no backup provider interface (storage API %d); changed reads need "+
			"Proxmox VE 8.4 or later (storage API %d); nothing was written", a.Ver, APIMin)
	case a.Ver-a.Age > APITested:
		return fmt.Errorf("this Proxmox (storage API %d) no longer accepts the plug-in of this restic-pve "+
			"(API %d); run restic-pve update, then restic-pve install again; nothing was written", a.Ver, APITested)
	}
	return nil
}

// Newer reports a Proxmox newer than the plug-in was checked against: it still loads the
// plug-in, and warns about the older API in every process that loads storage plug-ins.
func (a API) Newer() bool { return a.Ver > APITested }

// NewerNote is the plain message for Newer.
func (a API) NewerNote() string {
	return fmt.Sprintf("this Proxmox (storage API %d) is newer than the plug-in was checked against (%d); "+
		"Proxmox prints \"implementing an older storage API, an upgrade is recommended\" in its commands "+
		"until restic-pve is updated: run restic-pve update", a.Ver, APITested)
}

// Loaded checks that Proxmox's own loader accepts the plug-in file now on disk.
func (h *Host) Loaded() error {
	out, err := h.perl(loadScript)
	if err == nil && (strings.Contains(out, "\nloaded\n") || strings.HasPrefix(out, "loaded\n")) {
		return nil
	}
	detail := strings.Join(strings.Fields(out), " ")
	if err != nil {
		detail = strings.TrimSpace(detail + " " + err.Error())
	}
	return fmt.Errorf("Proxmox did not load the plug-in (%s)", detail)
}
