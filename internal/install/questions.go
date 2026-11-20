package install

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"text/tabwriter"
	"unicode"

	"github.com/norandom/restic-pve/internal/provider"
	"github.com/norandom/restic-pve/internal/wrapper"
)

// DefaultUser is the backup user a fresh install creates.
const DefaultUser = "restic-backup"

// Values are the site values of one install, answered on the PVE host. Not secret.
type Values struct {
	User, Pubkey, From string // Pubkey is "ssh-ed25519 <base64>" without the comment
	VMIDs              []int
	Storage            string
	MinFreeGiB         int
	AbortFreeGiB       int
	BwlimitKiB         int
	MaxWorkers         int
	Changes            bool // changed reads: the Proxmox plug-in and the storage entry restic-pve
}

var (
	userRe   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	pubkeyRe = regexp.MustCompile(`^(ssh-ed25519 [A-Za-z0-9+/]{16,}={0,3})( [A-Za-z0-9@._-]{1,128})?$`)
	fromRe   = regexp.MustCompile(`^[A-Za-z0-9.:,*?!/_-]{1,255}$`)
	keyRe    = regexp.MustCompile(`^restrict,from="([^"]*)",command="sudo -n ` + regexp.QuoteMeta(Binary) +
		` request \$SSH_ORIGINAL_COMMAND" (\S+ \S+)$`)
)

// ed25519Blob is the start of an OpenSSH ed25519 public key blob (type name, key length 32).
var ed25519Blob = []byte("\x00\x00\x00\x0bssh-ed25519\x00\x00\x00\x20")

// The answer rules. Each error is the one line shown before the question is asked again.

func checkFrom(s string) error {
	if !fromRe.MatchString(s) {
		return errors.New("an address or pattern for authorized_keys from=: 1-255 of A-Z a-z 0-9 . : , * ? ! / _ -")
	}
	return nil
}

// checkPubkey returns "ssh-ed25519 <base64>" of one key line (the comment is dropped).
func checkPubkey(s string) (string, error) {
	rule := errors.New("one line 'ssh-ed25519 <base64> [comment]' with an OpenSSH ed25519 public key")
	m := pubkeyRe.FindStringSubmatch(s)
	if m == nil {
		return "", rule
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(m[1], "ssh-ed25519 "))
	if err != nil || len(blob) != len(ed25519Blob)+32 || !bytes.HasPrefix(blob, ed25519Blob) {
		return "", rule
	}
	return m[1], nil
}

// parseVMIDs reads VM numbers separated by spaces or commas; each must be on this host.
func parseVMIDs(s string, onHost map[int]bool) ([]int, error) {
	words := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || unicode.IsSpace(r) })
	if len(words) == 0 {
		return nil, errors.New("give at least one VM number from the list above")
	}
	var ids []int
	seen := map[int]bool{}
	for _, w := range words {
		if !wrapper.ValidVMID(w) {
			return nil, fmt.Errorf("%q is not a VM number (100-999999999)", w)
		}
		v, _ := strconv.Atoi(w)
		if seen[v] {
			return nil, fmt.Errorf("VM %d is listed twice", v)
		}
		if !onHost[v] {
			return nil, fmt.Errorf("VM %d is not on this host (qm list)", v)
		}
		seen[v] = true
		ids = append(ids, v)
	}
	return ids, nil
}

func checkStorage(s string, active map[string]bool) error {
	if !active[s] {
		return errors.New("the name of one of the active storages listed above")
	}
	return wrapper.CheckConfValue("FLEECING_STORAGE", s)
}

// checkNumber parses a conf value of key and returns it.
func checkNumber(key, s string) (int, error) {
	if err := wrapper.CheckConfValue(key, s); err != nil {
		return 0, err
	}
	v, _ := strconv.Atoi(s)
	return v, nil
}

// checkAbort parses FLEECING_ABORT_FREE_GIB, which must be below min (FLEECING_MIN_FREE_GIB).
func checkAbort(s string, min int) (int, error) {
	v, err := checkNumber("FLEECING_ABORT_FREE_GIB", s)
	if err == nil && v >= min {
		err = fmt.Errorf("FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB (%d)", min)
	}
	return v, err
}

const maxBwlimitMiB = 976562 // BWLIMIT_KIB has at most 9 digits

// roundMiB is BWLIMIT_KIB as whole MiB/s (at least 1), the default of the read limit question.
func roundMiB(kib int) int {
	if m := (kib + 512) / 1024; m > 1 {
		return m
	}
	return 1
}

func checkBwlimitMiB(s string) (int, error) {
	v, err := strconv.Atoi(s)
	if err != nil || s != strconv.Itoa(v) || v < 1 || v > maxBwlimitMiB {
		return 0, fmt.Errorf("the read limit must be a whole number of MiB/s, 1-%d", maxBwlimitMiB)
	}
	return v, nil
}

// confValues returns the conf values in wrapper.ConfKeys order.
func (v *Values) confValues() []string {
	return []string{v.Storage, strconv.Itoa(v.MinFreeGiB), strconv.Itoa(v.AbortFreeGiB),
		strconv.Itoa(v.BwlimitKiB), strconv.Itoa(v.MaxWorkers), v.vmidList(" ")}
}

func (v *Values) vmidList(sep string) string {
	s := make([]string, len(v.VMIDs))
	for i, id := range v.VMIDs {
		s[i] = strconv.Itoa(id)
	}
	return strings.Join(s, sep)
}

func (v *Values) limits() string {
	return fmt.Sprintf("FLEECING_MIN_FREE_GIB %d, FLEECING_ABORT_FREE_GIB %d, read limit %s MiB/s (BWLIMIT_KIB %d), MAX_WORKERS %d",
		v.MinFreeGiB, v.AbortFreeGiB, mib(v.BwlimitKiB), v.BwlimitKiB, v.MaxWorkers)
}

func mib(kib int) string {
	if kib%1024 == 0 {
		return strconv.Itoa(kib / 1024)
	}
	return strconv.FormatFloat(float64(kib)/1024, 'f', 2, 64)
}

// errStop ends the questions; the message is printed and nothing is changed.
type errStop string

func (e errStop) Error() string { return string(e) }

const maxTries = 3

// asker reads one answer per line from in and writes the prompts to out.
type asker struct {
	in  *bufio.Reader
	out io.Writer
}

// ask shows prompt with def in brackets; Enter takes def. check sees the answer and
// returns the rule when it is invalid; after maxTries invalid answers ask stops.
func (a *asker) ask(prompt, def string, check func(string) error) error {
	if def != "" {
		prompt += " [" + def + "]"
	}
	for try := 0; try < maxTries; try++ {
		fmt.Fprint(a.out, prompt+": ")
		line, err := a.in.ReadString('\n')
		if err != nil && line == "" {
			fmt.Fprintln(a.out)
			return errStop("no terminal to ask on")
		}
		v := strings.TrimSpace(line)
		if v == "" {
			v = def
		}
		if err := check(v); err != nil {
			fmt.Fprintln(a.out, "  "+err.Error())
			continue
		}
		return nil
	}
	return errStop(fmt.Sprintf("%d invalid answers", maxTries))
}

// yes asks a [y/N] question.
func (a *asker) yes(prompt string) (bool, error) { return a.yesDefault(prompt, false) }

// yesDefault asks a yes/no question; Enter takes def ([Y/n] or [y/N]).
func (a *asker) yesDefault(prompt string, def bool) (bool, error) {
	ok := def
	hint := " [y/N]"
	if def {
		hint = " [Y/n]"
	}
	err := a.ask(prompt+hint, "", func(s string) error {
		switch strings.ToLower(s) {
		case "y", "yes":
			ok = true
		case "n", "no":
			ok = false
		case "":
			ok = def
		default:
			return errors.New("answer y or n")
		}
		return nil
	})
	return ok, err
}

// guest is one line of qm list; storage one active line of pvesm status.
type guest struct{ vmid, name, status string }
type storage struct {
	name, typ string
	freeGiB   int64
}

func (s *System) guests() ([]guest, error) {
	out, err := s.Run("qm", "list")
	if err != nil {
		return nil, fmt.Errorf("qm list: %w", err)
	}
	var gs []guest
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && wrapper.ValidVMID(f[0]) {
			gs = append(gs, guest{f[0], f[1], f[2]})
		}
	}
	return gs, nil
}

func (s *System) storages() ([]storage, error) {
	out, err := s.Run("pvesm", "status")
	if err != nil {
		return nil, fmt.Errorf("pvesm status: %w", err)
	}
	var st []storage
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		// restic-pve's own storage holds no images, so it is no fleecing storage
		if len(f) < 6 || f[2] != "active" || f[0] == provider.StorageID || wrapper.CheckConfValue("FLEECING_STORAGE", f[0]) != nil {
			continue
		}
		kib, _ := strconv.ParseInt(f[5], 10, 64) // pvesm reports KiB
		st = append(st, storage{f[0], f[1], kib / (1 << 20)})
	}
	return st, nil
}

// current returns the values of an existing install as defaults: user from the sudoers
// entry (else DefaultUser), address and key from its authorized_keys, the rest from
// /etc/restic-pve.conf (else the built-in limits). Anything unreadable is no default.
func (s *System) current() *Values {
	v := &Values{User: s.sudoersUser(), MinFreeGiB: 32, AbortFreeGiB: 8, BwlimitKiB: 40960, MaxWorkers: 1, Changes: s.changesOn()}
	if v.User == "" {
		v.User = DefaultUser
	}
	if pw, err := s.passwd(v.User); err == nil {
		data, _ := os.ReadFile(s.path(filepath.Join(pw.home, ".ssh", "authorized_keys")))
		if m := keyRe.FindStringSubmatch(strings.TrimSuffix(string(data), "\n")); m != nil && checkFrom(m[1]) == nil {
			if key, err := checkPubkey(m[2]); err == nil {
				v.From, v.Pubkey = m[1], key
			}
		}
	}
	data, err := os.ReadFile(s.path(confFile))
	if err != nil {
		return v
	}
	c, err := wrapper.ParseConf(confFile, data)
	if err != nil {
		return v
	}
	v.Storage = c.Storage
	for _, id := range c.VMIDs {
		n, _ := strconv.Atoi(id)
		v.VMIDs = append(v.VMIDs, n)
	}
	v.MinFreeGiB, _ = strconv.Atoi(c.MinFreeGiB)
	v.AbortFreeGiB, _ = strconv.Atoi(c.AbortFreeGiB)
	v.BwlimitKiB, _ = strconv.Atoi(c.BwlimitKiB)
	v.MaxWorkers, _ = strconv.Atoi(c.MaxWorkers)
	return v
}

// changesOn reports whether changed reads are on now: the plug-in file is ours or the storage
// entry is there. A reinstall keeps that choice as its default.
func (s *System) changesOn() bool {
	h := s.provider()
	st, _ := h.PluginState()
	typ, _ := h.StorageEntry()
	return st == provider.Current || st == provider.Different || typ == provider.StorageType
}

// questions asks for the site values; defaults are the values of an existing install.
func (s *System) questions(def *Values, gs []guest, sts []storage) (*Values, error) {
	a := &asker{in: s.Stdin, out: s.Stdout}
	v := *def
	err := a.ask("Backup machine address the key is accepted from, e.g. "+exampleFrom+" (authorized_keys from=)", def.From,
		func(x string) error { v.From = x; return checkFrom(x) })
	if err != nil {
		return nil, err
	}
	err = a.ask("Public key of the backup identity, the whole line, e.g. "+exampleKey+"\n"+
		"(./setup.sh --proxmox on the backup machine shows it)", def.Pubkey,
		func(x string) (err error) { v.Pubkey, err = checkPubkey(x); return err })
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(s.Stdout, "\nGuests on this host (qm list):")
	tw := tabwriter.NewWriter(s.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  VMID\tNAME\tSTATUS")
	onHost := map[int]bool{}
	for _, g := range gs {
		fmt.Fprintf(tw, "  %s\t%s\t%s\n", g.vmid, g.name, g.status)
		n, _ := strconv.Atoi(g.vmid)
		onHost[n] = true
	}
	tw.Flush()
	err = a.ask("VM numbers to back up, separated by spaces or commas", def.vmidList(" "),
		func(x string) (err error) { v.VMIDs, err = parseVMIDs(x, onHost); return err })
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(s.Stdout, "\nActive storages on this host (pvesm status):")
	tw = tabwriter.NewWriter(s.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "  NAME\tTYPE\tFREE GiB")
	active := map[string]bool{}
	for _, st := range sts {
		fmt.Fprintf(tw, "  %s\t%s\t%d\n", st.name, st.typ, st.freeGiB)
		active[st.name] = true
	}
	tw.Flush()
	err = a.ask("Fleecing storage", def.Storage, func(x string) error { v.Storage = x; return checkStorage(x, active) })
	if err != nil {
		return nil, err
	}

	fmt.Fprintln(s.Stdout, "\nLimits: "+def.limits())
	change, err := a.yes("Change the limits?")
	if err != nil {
		return nil, err
	}
	if change {
		if err := s.limitQuestions(a, def, &v); err != nil {
			return nil, err
		}
	}
	fmt.Fprintln(s.Stdout, "\nChanged reads (optional). "+changesText)
	v.Changes, err = a.yesDefault("Read only the changed parts of running guests?", def.Changes)
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// changesText is the explanation of the changed-reads question: what is added, why, and how
// to remove it. The form and the plain questions show the same text.
const changesText = "Yes adds two things to this host: the file " + provider.PluginPath +
	" and the storage " + provider.StorageID + ", which the Proxmox screens show as a backup target that " +
	"refuses jobs started there. Proxmox hands its record of changed blocks only to such a plug-in; with it " +
	"a backup of a running guest reads only what changed since the last one. Answer no here, or run " +
	"restic-pve uninstall, and both are removed again."

func (v *Values) changesLine() string {
	if v.Changes {
		return "on: plug-in " + provider.PluginPath + ", storage " + provider.StorageID
	}
	return "off (no plug-in, no storage entry)"
}

func (s *System) limitQuestions(a *asker, def, v *Values) error {
	err := a.ask("Free GiB the fleecing storage needs to start a backup (FLEECING_MIN_FREE_GIB)", strconv.Itoa(def.MinFreeGiB),
		func(x string) (err error) { v.MinFreeGiB, err = checkNumber("FLEECING_MIN_FREE_GIB", x); return err })
	if err != nil {
		return err
	}
	err = a.ask("Free GiB below which a running backup stops (FLEECING_ABORT_FREE_GIB)", strconv.Itoa(def.AbortFreeGiB),
		func(x string) (err error) { v.AbortFreeGiB, err = checkAbort(x, v.MinFreeGiB); return err })
	if err != nil {
		return err
	}
	err = a.ask("Read limit in MiB/s (stored as BWLIMIT_KIB)", strconv.Itoa(roundMiB(def.BwlimitKiB)),
		func(x string) error { n, err := checkBwlimitMiB(x); v.BwlimitKiB = n * 1024; return err })
	if err != nil {
		return err
	}
	return a.ask("Parallel workers (MAX_WORKERS)", strconv.Itoa(def.MaxWorkers),
		func(x string) (err error) { v.MaxWorkers, err = checkNumber("MAX_WORKERS", x); return err })
}

// summary prints what install will write.
func (s *System) summary(v *Values) {
	fmt.Fprintf(s.Stdout, "\nrestic-pve install will write:\n"+
		"  user              %s (created if missing; shell %s, password locked)\n"+
		"  authorized_keys   from=%q, key %s\n"+
		"  sudoers           %s\n"+
		"  binary            %s\n"+
		"  settings          %s: guests %s, fleecing storage %s,\n"+
		"                    %s\n"+
		"  update timer      %s: restic-pve update daily (always on)\n"+
		"  changed reads     %s\n",
		v.User, loginShell, v.From, v.Pubkey, sudoers, Binary, confFile, v.vmidList(" "), v.Storage, v.limits(), timerName, v.changesLine())
}
