// Package cli is `restic-pve stream`, the former vmastream program. It converts a Proxmox VE
// VMA backup stream to RVS1 for restic deduplication and converts RVS1 back to VMA or raw
// files.
//
// Every subcommand writes human-readable errors plus exactly one final JSON line of type
// "result" to stderr. Stdout is reserved for stream data. The block view of a running backup
// is `restic-pve watch` (package watch), which reads the state file that backup writes.
package cli

import (
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"runtime/debug"
	"strconv"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/stream/blockmap"
	"github.com/norandom/restic-pve/internal/stream/reorder"
)

// exitCode is the process exit status; the values are part of the CLI contract.
type exitCode int

const (
	exitOK               exitCode = 0  // success
	exitUsage            exitCode = 2  // bad invocation (unknown subcommand, flags, arguments)
	exitIntegrity        exitCode = 3  // md5/uuid/reserved/blockinfo/digest mismatch, malformed RVS1
	exitForbiddenContent exitCode = 4  // memory state (vmstate) or unknown device in the stream
	exitFleecingInactive exitCode = 5  // vzdump reported fleecing as not active
	exitSourceFailed     exitCode = 6  // ssh/vzdump non-zero exit or truncated stream
	exitLocalIO          exitCode = 7  // local I/O failure (stdout, output dir)
	exitManifestMismatch exitCode = 8  // manifest mismatch or missing device
	exitInternal         exitCode = 9  // internal error (bug, contract violation, panic); not transient
	exitGuestFilesystem  exitCode = 10 // guest filesystem freeze error; abort, never transient
)

// result is the final stderr JSON line of every invocation.
type result struct {
	Type          string  `json:"type"`
	Status        string  `json:"status"`
	Code          int     `json:"code"`
	Transient     bool    `json:"transient"`
	Message       string  `json:"message"`
	Hint          string  `json:"hint,omitempty"`
	BufferRAMPeak *int64  `json:"buffer_ram_peak,omitempty"`
	PatchCount    *uint64 `json:"patch_count,omitempty"`
}

// writeResult writes the final {"type":"result",...} line. JSON encoding
// escapes newlines in message, so the record always stays on one line.
func writeResult(w io.Writer, code exitCode, transient bool, message string, stats ...reorder.Stats) {
	status := "error"
	if code == exitOK {
		status = "ok"
	}
	r := result{Type: "result", Status: status, Code: int(code), Transient: transient, Message: message}
	if code == exitGuestFilesystem {
		r.Hint = "check the guest (fsck); the backup was not written"
	}
	if len(stats) > 0 {
		r.BufferRAMPeak = &stats[0].RAMPeak
		r.PatchCount = &stats[0].PatchCount
	}
	b, err := json.Marshal(r)
	if err != nil { // cannot happen for this struct; keep the contract anyway
		b = []byte(`{"type":"result","status":"error","code":` + strconv.Itoa(int(code)) + `,"transient":false,"message":"result encoding failed"}`)
	}
	_, _ = w.Write(append(b, '\n'))
}

const usageText = `Usage: restic-pve stream <command> [flags]

Commands:
  backup   read a VMA stream (via ssh, or --input) and write RVS1 to stdout
  verify   read RVS1 (stdin or --in) and check the per-device fingerprints
  to-vma   read RVS1 (stdin or --in) and write an uncompressed VMA stream
  to-raw   read RVS1 (stdin or --in) and write one sparse raw file per device
  help     show this help

verify, to-vma and to-raw read the tar of a snapshot in the piece layout with --pieces
(restic dump --archive tar <snapshot> <root>).

Run 'restic-pve stream <command> -h' for the flags of a command.

Exit codes:
  0 ok, 2 usage, 3 integrity, 4 forbidden content, 5 fleecing not active,
  6 source failed, 7 local I/O, 8 manifest mismatch, 9 internal error
  10 guest filesystem error (not transient)
`

// usageError reports invalid invocations (exit 2).
type usageError struct{ msg string }

func (e *usageError) Error() string { return e.msg }

func usagef(format string, a ...any) error { return &usageError{fmt.Sprintf(format, a...)} }

// commandFunc parses args and runs a subcommand. A nil error means success.
type commandFunc func(args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode, bool, error)

var commands = map[string]commandFunc{
	"backup": cmdBackup,
	"verify": cmdVerify,
	"to-vma": cmdToVMA,
	"to-raw": cmdToRaw,
}

// Main runs `restic-pve stream <args>` and returns the exit code (0 and 2-10, see exitCode).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	return int(run(args, stdin, stdout, stderr))
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) (code exitCode) {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(stderr, "restic-pve stream: internal error: %v\n%s", r, debug.Stack())
			writeResult(stderr, exitInternal, false, fmt.Sprintf("internal error: %v", r))
			code = exitInternal
		}
	}()
	if len(args) == 0 {
		fmt.Fprint(stderr, usageText)
		writeResult(stderr, exitUsage, false, "missing command")
		return exitUsage
	}
	name := args[0]
	switch name {
	case "help", "-h", "-help", "--help":
		fmt.Fprint(stderr, usageText)
		return exitOK
	}
	cmd, ok := commands[name]
	if !ok {
		fmt.Fprintf(stderr, "restic-pve stream: unknown command %q\n", name)
		fmt.Fprint(stderr, usageText)
		writeResult(stderr, exitUsage, false, fmt.Sprintf("unknown command %q", name))
		return exitUsage
	}
	code, transient, err := cmd(args[1:], stdin, stdout, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		var ue *usageError
		if errors.As(err, &ue) {
			code, transient = exitUsage, false
		}
		if code == exitOK { // an error must never be reported as success
			code = exitLocalIO
		}
		fmt.Fprintf(stderr, "restic-pve stream %s: %v\n", name, err)
		message := fmt.Sprintf("%s: %v", name, err)
		if code == exitGuestFilesystem {
			message = err.Error()
		}
		writeResult(stderr, code, transient, message)
		return code
	}
	if name != "backup" {
		writeResult(stderr, exitOK, false, name+": ok")
	}
	return exitOK
}

// newFlagSet returns a flag set that reports parse errors instead of exiting.
func newFlagSet(name string, stderr io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet("restic-pve stream "+name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	return fs
}

// parseFlags parses args and rejects positional arguments.
func parseFlags(fs *flag.FlagSet, args []string) error {
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return err
		}
		return &usageError{err.Error()}
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q", fs.Arg(0))
	}
	return nil
}

// parseVMID accepts a plain decimal guest id (no sign, no spaces), 1..2^32-1.
func parseVMID(s string) (uint32, error) {
	if s == "" {
		return 0, usagef("--vmid is required")
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, usagef("--vmid must be a decimal number, got %q", s)
		}
	}
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil || v == 0 {
		return 0, usagef("--vmid out of range: %q", s)
	}
	return uint32(v), nil
}

// backupOptions holds the validated flags of the backup subcommand.
type backupOptions struct {
	SSHConfig, SSHHost string
	VMID               uint32
	RAMMiB             uint
	ManifestOut        string
	Input              string
	ProgressInterval   time.Duration
	ViewDir, ViewName  string // block view: state file directory ("" = off) and guest alias

	viewWriteFile blockmap.WriteFileFunc // tests: a full or slow disk under the block view
}

func parseBackup(args []string, stderr io.Writer) (*backupOptions, error) {
	fs := newFlagSet("backup", stderr)
	var o backupOptions
	var vmid string
	fs.StringVar(&o.SSHConfig, "ssh-config", "", "ssh client config file")
	fs.StringVar(&o.SSHHost, "ssh-host", "", "ssh host alias of the hypervisor")
	fs.StringVar(&vmid, "vmid", "", "guest id (decimal)")
	fs.UintVar(&o.RAMMiB, "ram-mib", 512, "reorder buffer RAM cap in MiB (buffered clusters, patch hashes, zero-cluster bitmaps);\n"+
		"the open RVS1 data frame (up to 64 MiB) and process overhead come on top of it")
	fs.StringVar(&o.ManifestOut, "manifest-out", "", "path of the manifest JSON written on success")
	fs.StringVar(&o.Input, "input", "", "read the VMA stream from this file instead of ssh (tests)")
	fs.DurationVar(&o.ProgressInterval, "progress-interval", 30*time.Second, "interval of progress lines")
	fs.StringVar(&o.ViewDir, "view-dir", "", "write the block view's state file (live, vm-<vmid>.last) to this directory\n"+
		"for 'restic-pve watch'; best effort, never affects the stream; "+viewEnv+"=off turns it off")
	fs.StringVar(&o.ViewName, "view-name", "", "guest alias shown by the block view")
	if err := parseFlags(fs, args); err != nil {
		return nil, err
	}
	if len(o.ViewName) > 64 || strings.ContainsFunc(o.ViewName, func(r rune) bool { return r < ' ' || r == 0x7f }) {
		return nil, usagef("--view-name must be at most 64 printable characters")
	}
	v, err := parseVMID(vmid)
	if err != nil {
		return nil, err
	}
	o.VMID = v
	if o.Input == "" && (o.SSHConfig == "" || o.SSHHost == "") {
		return nil, usagef("--ssh-config and --ssh-host are required unless --input is given")
	}
	if o.ManifestOut == "" {
		return nil, usagef("--manifest-out is required")
	}
	if o.ProgressInterval <= 0 {
		return nil, usagef("--progress-interval must be positive")
	}
	return &o, nil
}
