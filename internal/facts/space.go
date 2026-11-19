package facts

import (
	"fmt"
	"strings"
)

// SpaceState is the result of the warning rule.
type SpaceState int

const (
	SpaceUnknown SpaceState = iota // the numbers to decide are missing
	SpaceOK
	SpaceLow
)

// Largest is the largest new data of the recorded runs (records.json keeps the last four of
// each profile); ok is false when no run is recorded.
func (s SpaceFacts) Largest() (int64, bool) {
	var d int64
	for _, r := range s.Runs {
		d = max(d, r.DataAdded)
	}
	return d, len(s.Runs) > 0
}

// Check applies the warning rule: low when free is less than twice the largest recent new
// data, or, when the total is known, less than a tenth of it. Unknown when free is not
// reported, or when neither a run nor the total is known.
func (s SpaceFacts) Check() SpaceState {
	if s.About == nil || s.About.Free == nil {
		return SpaceUnknown
	}
	free := *s.About.Free
	d, runs := s.Largest()
	total := s.About.Total
	switch {
	case runs && free < 2*d:
		return SpaceLow
	case total != nil && free < *total/10:
		return SpaceLow
	case !runs && total == nil:
		return SpaceUnknown
	}
	return SpaceOK
}

// Size formats bytes for the screens: "512 KiB", "31 MiB", "819 GiB", "2 TiB", "1.2 TiB".
func Size(b int64) string {
	f := float64(b)
	unit := func(v float64, u string) string {
		s := fmt.Sprintf("%.1f", v)
		if v >= 10 {
			s = fmt.Sprintf("%.0f", v)
		}
		return strings.TrimSuffix(s, ".0") + " " + u
	}
	switch {
	case f >= 1<<40:
		return unit(f/(1<<40), "TiB")
	case f >= 1<<30:
		return unit(f/(1<<30), "GiB")
	case f >= 1<<20:
		return unit(f/(1<<20), "MiB")
	case f >= 1<<10:
		return unit(f/(1<<10), "KiB")
	}
	return fmt.Sprintf("%d B", b)
}
