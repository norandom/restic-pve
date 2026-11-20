package wrapper

import (
	"context"
	"regexp"
	"strconv"
	"strings"

	"github.com/norandom/restic-pve/internal/provider"
)

var (
	kibRe      = regexp.MustCompile(`^[0-9]{1,15}$`)
	cfgLineRe  = regexp.MustCompile(`^([a-z0-9_-]+): (.*)$`)
	statusRe   = regexp.MustCompile(`^[a-z]+$`)
	pveRe      = regexp.MustCompile(`pve-manager/([0-9]+)\.([0-9]+)(\.[0-9]+)?`)
	fleecingRe = regexp.MustCompile(`^fleecing:`)
)

func atoi(s string) int64 {
	v, _ := strconv.ParseInt(s, 10, 64)
	return v
}

// jsonStr returns a JSON string literal like the bash json_str: escape \ and ", drop control characters.
func jsonStr(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c < 0x20:
		case c == '\\' || c == '"':
			b.WriteByte('\\')
			b.WriteByte(c)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
	return b.String()
}

func jsonList(items []string) string { return "[" + strings.Join(items, ",") + "]" }

// info prints one JSON line from read-only PVE commands. Key order is part of the contract.
func (e *Env) info(vmid string, c Conf) error {
	cfg, err := e.command(context.Background(), "qm", "config", vmid).CombinedOutput()
	if err != nil {
		if strings.Contains(string(cfg), "does not exist") {
			return fail(exMissing, "VM %s does not exist", vmid)
		}
		return fail(1, "qm config %s failed", vmid)
	}
	var name, lock, ostype, agent string
	vmstate, inConfig := false, false
	var disks []string
	for _, line := range strings.Split(string(cfg), "\n") {
		if line == "[special:fleecing]" || fleecingRe.MatchString(line) {
			inConfig = true
		}
		m := cfgLineRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		switch key, val := m[1], m[2]; key {
		case "name":
			name = val
		case "lock":
			lock = val
		case "ostype":
			ostype = val
		case "agent":
			agent = val
		case "vmstate":
			vmstate = true
		default:
			if diskRe.MatchString(key) && !strings.HasPrefix(val, "none") && !strings.Contains(val, "media=cdrom") {
				disks = append(disks, `"`+key+`"`)
			}
		}
	}
	a := "," + agent + ","
	agentEnabled := strings.Contains(a, ",1,") || strings.Contains(a, ",enabled=1,")

	st := e.command(context.Background(), "qm", "status", vmid)
	st.Stderr = e.Stderr
	out, err := st.Output()
	if err != nil {
		return fail(1, "qm status %s failed", vmid)
	}
	status := strings.TrimPrefix(strings.TrimRight(string(out), "\n"), "status: ")
	if !statusRe.MatchString(status) {
		status = "unknown"
	}
	agentRunning := false
	if agentEnabled && status == "running" {
		ctx, cancel := context.WithTimeout(context.Background(), e.AgentTimeout)
		agentRunning = e.command(ctx, "qm", "guest", "cmd", vmid, "ping").Run() == nil
		cancel()
	}

	var major, minor int64
	pveVersion := ""
	pv, _ := e.output("pveversion")
	if m := pveRe.FindStringSubmatch(pv); m != nil {
		major, minor = atoi(m[1]), atoi(m[2])
		pveVersion = m[1] + "." + m[2] + m[3]
	}
	active, avail := false, "null"
	if out, err := e.output("pvesm", "status", "--storage", c.Storage); err == nil {
		for _, line := range strings.Split(out, "\n") {
			f := fields(line)
			if field(f, 0) != c.Storage {
				continue
			}
			if field(f, 2) == "active" {
				active = true
			}
			if kibRe.MatchString(field(f, 5)) {
				avail = strconv.FormatInt(atoi(field(f, 5))*1024, 10) // pvesm reports KiB
			}
		}
	}
	fleecingOK := active && (major > 8 || (major == 8 && minor >= 2))
	leftovers := "null"
	if out, err := e.output("pvesm", "list", c.Storage, "--vmid", vmid); err == nil {
		re := regexp.MustCompile(`^vm-` + vmid + `-fleece-[0-9]+(\.[^/]+)?$`)
		var found []string
		for _, line := range strings.Split(out, "\n") {
			volid := field(fields(line), 0)
			volname := volid[strings.LastIndex(volid, ":")+1:]
			volname = volname[strings.LastIndex(volname, "/")+1:]
			if re.MatchString(volname) {
				found = append(found, jsonStr(volid))
			}
		}
		leftovers = jsonList(found)
	}
	lockJSON := "null"
	if lock != "" {
		lockJSON = jsonStr(lock)
	}

	var b strings.Builder
	w := func(key, val string) {
		if b.Len() > 0 {
			b.WriteByte(',')
		}
		b.WriteString(`"` + key + `":` + val)
	}
	w("vmid", vmid)
	w("name", jsonStr(name))
	w("status", `"`+status+`"`)
	w("lock", lockJSON)
	w("ostype", jsonStr(ostype))
	w("agent_enabled", strconv.FormatBool(agentEnabled))
	w("agent_running", strconv.FormatBool(agentRunning))
	w("disks", jsonList(disks))
	w("vmstate_present", strconv.FormatBool(vmstate))
	w("fleecing_ok", strconv.FormatBool(fleecingOK))
	w("fleecing_avail_bytes", avail)
	w("fleecing_leftovers", leftovers)
	w("fleecing_in_config", strconv.FormatBool(inConfig))
	w("conf", `{"fleecing_storage":"`+c.Storage+`","fleecing_min_free_gib":"`+c.MinFreeGiB+
		`","fleecing_abort_free_gib":"`+c.AbortFreeGiB+`","bwlimit_kib":"`+c.BwlimitKiB+
		`","max_workers":"`+c.MaxWorkers+`"}`)
	w("pve_version", jsonStr(pveVersion))
	w("version", jsonStr(e.Version))
	w("provider", e.providerInfo())
	_, err = e.Stdout.WriteString("{" + b.String() + "}\n")
	return err
}

// providerInfo is the state of changed reads on this host: the plug-in file (absent, current:
// this binary's copy, different: another release's, foreign: not restic-pve's), whether the
// storage entry exists and whether Proxmox shows it active. pvesm is asked only when the
// entry exists, so a host without the feature runs no extra command.
func (e *Env) providerInfo() string {
	h := e.host()
	st, err := h.PluginState()
	plugin := st.String()
	if err != nil {
		plugin = "unreadable"
	}
	typ, _ := h.StorageEntry()
	entry, active := typ == provider.StorageType, false
	if entry {
		if out, err := e.output("pvesm", "status", "--storage", provider.StorageID); err == nil {
			for _, line := range strings.Split(out, "\n") {
				f := fields(line)
				active = active || field(f, 0) == provider.StorageID && field(f, 2) == "active"
			}
		}
	}
	// the storage API, only for a plug-in of ours: "ok", "newer" (Proxmox warns about the older
	// API in its commands until restic-pve is updated), "unsupported" (Proxmox refuses it), or
	// "unknown"; "unchecked" without a plug-in
	api := "unchecked"
	if st == provider.Current || st == provider.Different {
		a, err := h.ProxmoxAPI()
		switch {
		case err != nil:
			api = "unknown"
		case a.Problem() != nil:
			api = "unsupported"
		case a.Newer():
			api = "newer"
		default:
			api = "ok"
		}
	}
	return `{"plugin":"` + plugin + `","storage":` + strconv.FormatBool(entry) + `,"active":` + strconv.FormatBool(active) +
		`,"cluster":` + strconv.FormatBool(h.Clustered()) + `,"api":"` + api + `"}`
}
