package keys

import (
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/norandom/restic-pve/internal/facts"
	"golang.org/x/term"
)

// The proof's texts.
const (
	ProofIntro = "Open your password manager and copy the repository password. Paste it here and press Enter. " +
		"Nothing you paste is shown or stored."
	NotEqual = "This copy is not the password in use. Fix the entry in your password manager, then prove again. " +
		"Nothing was written."
	NeedsTerminal = "The proof needs a terminal, so that the password is not read from a pipe. " +
		"Run restic-pve keys in an interactive shell on the backup machine. Nothing was written."
	Mask = "●●●●●●" // shown after the first key, whatever the length
)

// Result is the end of a proof.
type Result struct {
	OK   bool
	Text string
}

// Prover checks a pasted copy against the password in use and against every repository.
type Prover struct {
	State   string   // RESTIC_PVE_STATE: where the marker goes
	InUse   string   // RESTIC_PASSWORD
	Repos   []string // the repositories of backup.toml
	Environ []string
	Run     facts.Runner
}

// Prove checks copy and wipes it before it returns, whatever the result. The copy reaches
// restic only through the child's environment; it is never printed, logged, put in an argv
// or written anywhere. Only when it equals the password in use and opens every repository
// is the marker written: the one-way fingerprint backupctl's setup writes, mode 0600.
func (p Prover) Prove(copy []byte) Result {
	defer clear(copy)
	if len(copy) == 0 {
		return Result{Text: "Nothing was pasted. Copy the password from your password manager and paste it. Nothing was written."}
	}
	if p.InUse == "" {
		return Result{Text: "RESTIC_PASSWORD is not set on this machine, so there is nothing to compare with. " +
			"Put the copy from your password manager into .env as RESTIC_PASSWORD=..., then prove again. Nothing was written."}
	}
	inUse := []byte(p.InUse)
	equal := subtle.ConstantTimeCompare(copy, inUse) == 1
	clear(inUse)
	if !equal {
		return Result{Text: NotEqual}
	}
	if len(p.Repos) == 0 {
		return Result{Text: "backup.toml lists no repository to open, so the copy cannot be proven yet. Nothing was written."}
	}
	// the child's environment entry is built in a buffer of our own, so it can be wiped
	entry := make([]byte, 0, len(facts.PasswordEnv)+1+len(copy))
	entry = append(append(entry, facts.PasswordEnv+"="...), copy...)
	defer clear(entry)
	env := append(facts.ChildEnv(p.Environ), unsafe.String(&entry[0], len(entry)))
	opened := 0
	for _, repo := range p.Repos {
		r := p.Run(facts.Call{Name: "restic", Args: facts.ResticArgs(repo, "cat", "config"), Env: env, Timeout: facts.CatConfigTimeout})
		switch {
		case r.Err == nil && r.Code == facts.ResticWrongPassword:
			return Result{Text: "Stop: " + facts.WrongPasswordText(repo) + " Nothing was written."}
		case r.Err == nil && r.Code == 10:
			continue // no backup there yet: nothing to open
		case r.Err != nil || r.Code != 0:
			return Result{Text: fmt.Sprintf("The proof could not open %s: %s. Nothing was written; try again when the storage answers.",
				repo, r.Problem("restic"))}
		}
		opened++
	}
	env = nil
	if err := writeMarker(p.State, facts.Fingerprint(copy)); err != nil {
		return Result{Text: "Your copy is right, but the proof date could not be saved: " + err.Error() + "."}
	}
	if opened == 0 {
		return Result{OK: true, Text: "Your copy is the password in use. No backup exists yet to open; " +
			"prove it again after the first backup."}
	}
	n := fmt.Sprintf("all %d repositories", opened)
	if opened == 1 {
		n = "the repository"
	}
	return Result{OK: true, Text: "Your copy opens " + n + ". Proven today. Keep it in your password manager."}
}

// writeMarker writes the fingerprint and a newline, as backupctl's setup does, through a
// temp file and a rename; the mtime is the proof date.
func writeMarker(state, fingerprint string) error {
	tmp, err := os.CreateTemp(state, "."+facts.MarkerFile+".")
	if err != nil {
		return err
	}
	_, err = tmp.WriteString(fingerprint + "\n")
	if err2 := tmp.Close(); err == nil {
		err = err2
	}
	if err == nil {
		err = os.Chmod(tmp.Name(), 0o600)
	}
	if err == nil {
		err = os.Rename(tmp.Name(), filepath.Join(state, facts.MarkerFile))
	}
	if err != nil {
		os.Remove(tmp.Name())
	}
	return err
}

// ErrNoTerminal refuses a proof without a terminal.
var ErrNoTerminal = errors.New(NeedsTerminal)

// ReadHidden reads one line from the terminal fd without echo (the plain form). Without a
// terminal it refuses: a password must not come from a pipe or a file.
func ReadHidden(fd int, prompt io.Writer) ([]byte, error) {
	if !term.IsTerminal(fd) {
		return nil, ErrNoTerminal
	}
	fmt.Fprint(prompt, "Paste the copy and press Enter (nothing is shown): ")
	b, err := term.ReadPassword(fd)
	fmt.Fprintln(prompt)
	return b, err
}
