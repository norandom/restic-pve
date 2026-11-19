package source

import "testing"

func TestBenignLinesAreInfo(t *testing.T) {
	for _, line := range []string{
		"Formatting '/tmp/vm-100-fleece-0.qcow2', fmt=qcow2 size=1048576",
		"WARN: 'root@pam' does not have a configured email address",
		"ERROR: could not notify via target `mail-to-root`: no recipients provided",
		"ERROR: could not notify via endpoint `mail-to-root`: failed",
		"ERROR: no recipients provided",
	} {
		t.Run(line, func(t *testing.T) {
			c := NewClassifier()
			v := c.Line(line)
			if v.Level != LevelInfo || v.Forward || v.Fleecing || v.GuestFS || len(c.Warnings()) != 0 {
				t.Fatalf("benign line: %+v, warnings %v", v, c.Warnings())
			}
		})
	}
}

func TestGuestFilesystemFreezeErrors(t *testing.T) {
	for _, reason := range []string{"Input/output error", "I/O error", "Structure needs cleaning", "Read-only file system"} {
		t.Run(reason, func(t *testing.T) {
			c := NewClassifier()
			line := "ERROR: unable to freeze guest fs - failed to freeze /: " + reason
			if v := c.Line(line); !v.GuestFS || !v.Forward || v.Level != LevelError {
				t.Fatalf("freeze verdict: %+v", v)
			}
			if c.GuestFSProblem() != line {
				t.Fatalf("guest filesystem problem = %q", c.GuestFSProblem())
			}
		})
	}
	for _, line := range []string{
		"ERROR: unable to freeze guest fs - got timeout",
		"ERROR: VM 100 qmp command 'guest-fsfreeze-freeze' failed - agent not running",
	} {
		c := NewClassifier()
		if v := c.Line(line); v.GuestFS || c.GuestFSProblem() != "" {
			t.Fatalf("ordinary freeze failure: %+v", v)
		}
		if value, detail := c.Consistency(); value != Crash || detail == DetailUnknown {
			t.Fatalf("consistency = %q, %q", value, detail)
		}
	}
	c := NewClassifier()
	if v := c.Line("ERROR: disk image '/tmp/vm-100-fleece-0.qcow2': Input/output error"); v.GuestFS {
		t.Fatal("disk image error classified as a guest freeze error")
	}
}

func TestBackupFailureReasons(t *testing.T) {
	for _, tc := range []struct {
		reason    string
		transient bool
	}{
		{"unable to acquire lock", true},
		{"VM is locked (backup)", true},
		{"device busy", true},
		{"timeout", true},
		{"operation timed out", true},
		{"temporarily unavailable", true},
		{"try again", true},
		{"connection reset", true},
		{"disk image '/tmp/vm-100-fleece-0.qcow2' already exists", false},
		{"permission denied", false},
		{"no space left on device", false},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			c := NewClassifier()
			c.Line("ERROR: Backup of VM 100 failed - " + tc.reason)
			reason, transient := c.BackupFailure()
			if reason != tc.reason || transient != tc.transient {
				t.Fatalf("failure = %q, %v", reason, transient)
			}
		})
	}
}
