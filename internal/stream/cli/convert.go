package cli

import (
	"bufio"
	"errors"
	"flag"
	"io"
	"os"

	"github.com/norandom/restic-pve/internal/stream/convert"
	"github.com/norandom/restic-pve/internal/stream/rvs"
	"github.com/norandom/restic-pve/internal/stream/verify"
)

// convertFlags are the flags shared by verify, to-vma and to-raw.
type convertFlags struct {
	manifest string
	in       string
}

func addConvertFlags(fs *flag.FlagSet) *convertFlags {
	var c convertFlags
	fs.StringVar(&c.manifest, "manifest", "", "manifest JSON to compare against (devices, sizes, sha256ct)")
	fs.StringVar(&c.in, "in", "", "read RVS1 from this file instead of stdin")
	return &c
}

// convertExit maps a convert error to its exit code.
func convertExit(err error) exitCode {
	switch {
	case errors.Is(err, rvs.ErrIntegrity):
		return exitIntegrity
	case errors.Is(err, verify.ErrManifest):
		return exitManifestMismatch
	case errors.Is(err, convert.ErrLocalIO):
		return exitLocalIO
	default:
		return exitInternal
	}
}

// prepare loads the manifest (open error → 7, malformed → 8) and opens the
// input. The returned close function is never nil.
func (c *convertFlags) prepare(stdin io.Reader) (io.Reader, convert.Options, func(), exitCode, error) {
	var o convert.Options
	nop := func() {}
	if c.manifest != "" {
		m, err := verify.ReadManifestFile(c.manifest)
		if err != nil {
			if errors.Is(err, verify.ErrManifest) {
				return nil, o, nop, exitManifestMismatch, err
			}
			return nil, o, nop, exitLocalIO, err
		}
		o.Manifest = m
	}
	if c.in == "" {
		return bufio.NewReaderSize(stdin, 1<<20), o, nop, exitOK, nil
	}
	f, err := os.Open(c.in)
	if err != nil {
		return nil, o, nop, exitLocalIO, err
	}
	return bufio.NewReaderSize(f, 1<<20), o, func() { f.Close() }, exitOK, nil
}

func cmdVerify(args []string, stdin io.Reader, _, stderr io.Writer) (exitCode, bool, error) {
	fs := newFlagSet("verify", stderr)
	c := addConvertFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return exitUsage, false, err
	}
	in, o, done, code, err := c.prepare(stdin)
	defer done()
	if err != nil {
		return code, false, err
	}
	if _, err := convert.Verify(in, o); err != nil {
		return convertExit(err), false, err
	}
	return exitOK, false, nil
}

func cmdToVMA(args []string, stdin io.Reader, stdout, stderr io.Writer) (exitCode, bool, error) {
	fs := newFlagSet("to-vma", stderr)
	c := addConvertFlags(fs)
	outPath := fs.String("out", "-", "output file (must not exist), or - for stdout")
	if err := parseFlags(fs, args); err != nil {
		return exitUsage, false, err
	}
	if *outPath == "" {
		return exitUsage, false, usagef("--out must be a file name or -")
	}
	in, o, done, code, err := c.prepare(stdin)
	defer done()
	if err != nil {
		return code, false, err
	}
	out := stdout
	var f *os.File
	if *outPath != "-" {
		// never overwrite: a failed run leaves its truncated VMA file behind
		f, err = os.OpenFile(*outPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
		if err != nil {
			return exitLocalIO, false, err
		}
		defer f.Close()
		out = f
	}
	if _, err := convert.ToVMA(in, out, o); err != nil {
		return convertExit(err), false, err
	}
	if f != nil {
		if err := f.Sync(); err != nil {
			return exitLocalIO, false, err
		}
		if err := f.Close(); err != nil {
			return exitLocalIO, false, err
		}
	}
	return exitOK, false, nil
}

func cmdToRaw(args []string, stdin io.Reader, _, stderr io.Writer) (exitCode, bool, error) {
	fs := newFlagSet("to-raw", stderr)
	dir := fs.String("dir", "", "output directory for <device>.raw and SHA256SUMS (created if missing)")
	c := addConvertFlags(fs)
	if err := parseFlags(fs, args); err != nil {
		return exitUsage, false, err
	}
	if *dir == "" {
		return exitUsage, false, usagef("--dir is required")
	}
	in, o, done, code, err := c.prepare(stdin)
	defer done()
	if err != nil {
		return code, false, err
	}
	if _, err := convert.ToRaw(in, *dir, o); err != nil {
		return convertExit(err), false, err
	}
	return exitOK, false, nil
}
