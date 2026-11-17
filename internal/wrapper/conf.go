package wrapper

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The patterns of the bash wrapper restic-vzdump, unchanged, plus the VMIDS list.
var (
	vmidRe    = regexp.MustCompile(`^[1-9][0-9]{2,8}$`)
	storageRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]*$`)
	diskRe    = regexp.MustCompile(`^(scsi|virtio|sata|ide|efidisk|tpmstate)[0-9]+$`)
	blankRe   = regexp.MustCompile(`^[[:space:]]*(#.*)?$`)
	confRe    = regexp.MustCompile(`^([A-Z_]+)=(.*)$`)
	vmidsRe   = regexp.MustCompile(`^[1-9][0-9]{2,8}( [1-9][0-9]{2,8})*$`)
	gibRe     = regexp.MustCompile(`^[1-9][0-9]{0,5}$`)
	bwlimitRe = regexp.MustCompile(`^[1-9][0-9]{0,8}$`)
	workersRe = regexp.MustCompile(`^([1-9]|1[0-6])$`)
)

// ConfKeys lists the keys of /etc/restic-pve.conf in the order install writes them.
var ConfKeys = []string{"FLEECING_STORAGE", "FLEECING_MIN_FREE_GIB", "FLEECING_ABORT_FREE_GIB", "BWLIMIT_KIB", "MAX_WORKERS", "VMIDS"}

// Conf holds the values of /etc/restic-pve.conf as written in the file.
type Conf struct {
	Storage, MinFreeGiB, AbortFreeGiB, BwlimitKiB, MaxWorkers string
	VMIDs                                                     []string // the allowed guests
}

// configError is a fatal /etc file problem (exit 78).
type configError struct{ msg string }

func (e *configError) Error() string { return e.msg }

// ValidVMID reports whether s is a VMID the wrapper accepts.
func ValidVMID(s string) bool { return vmidRe.MatchString(s) }

// CheckConfValue applies the conf rule of key to value. The error text is the wrapper's.
func CheckConfValue(key, value string) error {
	var ok bool
	var msg string
	switch key {
	case "FLEECING_STORAGE":
		ok, msg = storageRe.MatchString(value), "invalid storage name"
	case "FLEECING_MIN_FREE_GIB", "FLEECING_ABORT_FREE_GIB":
		ok, msg = gibRe.MatchString(value), key+" must be 1-999999 (GiB)"
	case "BWLIMIT_KIB":
		ok, msg = bwlimitRe.MatchString(value), "BWLIMIT_KIB must be a positive integer"
	case "MAX_WORKERS":
		ok, msg = workersRe.MatchString(value), "MAX_WORKERS must be 1-16"
	case "VMIDS":
		ok, msg = vmidsRe.MatchString(value), "VMIDS must be guest IDs separated by single spaces"
		seen := map[string]bool{}
		for _, id := range strings.Split(value, " ") {
			if ok && seen[id] {
				ok, msg = false, "VMIDS: VM "+id+" listed twice"
			}
			seen[id] = true
		}
	default:
		msg = "unknown key " + key
	}
	if !ok {
		return fmt.Errorf("%s", msg)
	}
	return nil
}

// lines splits file content like `while IFS= read -r line || [[ -n $line ]]`.
func lines(data []byte) []string {
	s := string(data)
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// readable reports whether path is a regular file that can be read (`[[ -f p && -r p ]]`).
func readable(path string) ([]byte, bool) {
	st, err := os.Stat(path)
	if err != nil || !st.Mode().IsRegular() {
		return nil, false
	}
	data, err := os.ReadFile(path)
	return data, err == nil
}

// ParseConf parses conf file content strictly: KEY=value lines, every key exactly once.
// name is the file name used in error messages.
func ParseConf(name string, data []byte) (Conf, error) {
	vals := map[string]string{}
	for i, line := range lines(data) {
		n := i + 1
		if blankRe.MatchString(line) {
			continue
		}
		m := confRe.FindStringSubmatch(line)
		if m == nil {
			return Conf{}, &configError{fmt.Sprintf("%s line %d: expected KEY=value", name, n)}
		}
		key, value := m[1], m[2]
		if _, dup := vals[key]; dup {
			return Conf{}, &configError{fmt.Sprintf("%s line %d: duplicate %s", name, n, key)}
		}
		if err := CheckConfValue(key, value); err != nil {
			return Conf{}, &configError{fmt.Sprintf("%s line %d: %v", name, n, err)}
		}
		vals[key] = value
	}
	for _, key := range ConfKeys {
		if vals[key] == "" {
			return Conf{}, &configError{fmt.Sprintf("%s: %s missing", name, key)}
		}
	}
	c := Conf{vals["FLEECING_STORAGE"], vals["FLEECING_MIN_FREE_GIB"], vals["FLEECING_ABORT_FREE_GIB"],
		vals["BWLIMIT_KIB"], vals["MAX_WORKERS"], strings.Split(vals["VMIDS"], " ")}
	if c.abortGiB() >= c.minGiB() {
		return Conf{}, &configError{fmt.Sprintf("%s: FLEECING_ABORT_FREE_GIB must be below FLEECING_MIN_FREE_GIB", name)}
	}
	return c, nil
}

// The values passed their patterns, so they parse.
func (c Conf) minGiB() int64   { v, _ := strconv.ParseInt(c.MinFreeGiB, 10, 64); return v }
func (c Conf) abortGiB() int64 { v, _ := strconv.ParseInt(c.AbortFreeGiB, 10, 64); return v }

// Allowed reports whether vmid is listed in VMIDS.
func (c Conf) Allowed(vmid string) bool { return slices.Contains(c.VMIDs, vmid) }
