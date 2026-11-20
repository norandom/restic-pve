package status

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/norandom/restic-pve/internal/facts"
)

// ReadsRow is one guest that reads only changed blocks: its last run, its last full check
// and a fault.
type ReadsRow struct {
	Alias string
	Run   string     // "changes: read 1.2 GiB, skipped 123.8 GiB (unchanged)"
	Ended *time.Time // the last run; nil: none recorded
	Check string     // "full check: ok 2026-11-27", "full check: none yet"
	Fault string     // "" or the fault and the command that clears it
	plain []string
}

var kindWords = map[string]string{"changes": "changes", "full": "full read", "check": "full check", "probe": "probe"}

// size1 formats bytes with one decimal, as the status line of a run: "1.2 GiB", "0 B".
func size1(b int64) string {
	units := []string{"KiB", "MiB", "GiB", "TiB"}
	if b < 1<<10 {
		return fmt.Sprintf("%d B", b)
	}
	v, u := float64(b)/(1<<10), 0
	for v >= 1<<10 && u < len(units)-1 {
		v, u = v/(1<<10), u+1
	}
	return fmt.Sprintf("%.1f %s", v, units[u])
}

func readsRow(source, alias string, g facts.GuestReads, hint string) ReadsRow {
	r := ReadsRow{Alias: alias, Run: "no run with changed reads recorded yet", Check: "full check: none yet"}
	nb := func(b int64) string { return strings.ReplaceAll(size1(b), " ", "") }
	p := []string{"reads", alias, "none"}
	if l := g.Last; l != nil {
		kind := kindWords[l.Kind]
		if kind == "" {
			kind = l.Kind
		}
		r.Run = kind + ": read " + size1(l.ReadBytes)
		p = []string{"reads", alias, l.Kind}
		if !l.Ended.IsZero() {
			r.Ended = &l.Ended
			p = append(p, "ended "+utc(l.Ended))
		}
		p = append(p, "read "+nb(l.ReadBytes))
		if l.SkippedBytes > 0 || l.Kind == "changes" {
			r.Run += ", skipped " + size1(l.SkippedBytes) + " (unchanged)"
			p = append(p, "skipped "+nb(l.SkippedBytes))
		}
	}
	p = append(p, "full-check none")
	if rec := g.Record; rec != nil {
		if c := rec.LastCheck; c != nil {
			r.Check = "full check: " + c.Result + " " + c.Date
			p[len(p)-1] = "full-check " + c.Result + " " + c.Date
		}
		if fl := rec.Fault; fl != nil {
			do := facts.Hint(hint, source) + " --clear-fault " + alias
			r.Fault = fmt.Sprintf("FAULT: full check found a difference on %s (%d pieces of %s); every run reads in full "+
				"until you look at it and run %s", fl.Date, fl.Pieces, strings.Join(fl.Disks, ", "), do)
			r.plain = []string{strings.Join(p, "  "), strings.Join([]string{"fault", alias, fl.Date,
				fmt.Sprintf("%d pieces", fl.Pieces), strings.Join(fl.Disks, ","), "do: " + do}, "  ")}
			return r
		}
	}
	r.plain = []string{strings.Join(p, "  ")}
	return r
}

// readsRows lists the guests of backup.toml that read only changes, those with a fault first.
func readsRows(f facts.Facts, hint string) []ReadsRow {
	var out []ReadsRow
	for _, p := range f.Config.Proxmox {
		for _, g := range p.Guests {
			if r, ok := f.Reads[g.Alias]; ok {
				out = append(out, readsRow(p.Name, g.Alias, r, hint))
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Fault != "" && out[j].Fault == "" })
	return out
}

// lines are the guest's two lines on the screen; the second is the fault when there is one.
func (r ReadsRow) lines() (string, string) {
	second := r.Check
	if r.Ended != nil {
		second = local(*r.Ended, "Mon 2 Jan 15:04") + " · " + r.Check
	}
	if r.Fault != "" {
		second = r.Fault
	}
	return fmt.Sprintf("%-8s %s", r.Alias, r.Run), fmt.Sprintf("%-8s %s", "", second)
}
