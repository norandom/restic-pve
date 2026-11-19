package facts

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Cron is a five-field cron line in UTC from the subset of [schedule]: numbers, "*", lists
// and ranges. Steps and names are refused.
type Cron struct {
	Text                     string
	min, hour, dom, mon, dow uint64 // bit n set: value n matches
	domStar, dowStar         bool
}

const cronRule = "use five fields (minute hour day-of-month month day-of-week) with numbers, '*', " +
	"lists like 1,15 and ranges like 1-5, in UTC; for example \"0 5 * * 6\" is Saturday 05:00 UTC"

// ParseCron parses a line of the subset.
func ParseCron(s string) (Cron, error) {
	f := strings.Fields(s)
	if len(f) != 5 {
		return Cron{}, fmt.Errorf("%q has %d fields: %s", s, len(f), cronRule)
	}
	c := Cron{Text: strings.Join(f, " "), domStar: f[2] == "*", dowStar: f[4] == "*"}
	lims := [5][2]int{{0, 59}, {0, 23}, {1, 31}, {1, 12}, {0, 6}}
	dst := [5]*uint64{&c.min, &c.hour, &c.dom, &c.mon, &c.dow}
	for i, field := range f {
		lo, hi := lims[i][0], lims[i][1]
		if field == "*" {
			for v := lo; v <= hi; v++ {
				*dst[i] |= 1 << v
			}
			continue
		}
		for _, part := range strings.Split(field, ",") {
			a, b, isRange := strings.Cut(part, "-")
			x, err1 := strconv.Atoi(a)
			y := x
			var err2 error
			if isRange {
				y, err2 = strconv.Atoi(b)
			}
			if err1 != nil || err2 != nil || x < lo || y > hi || x > y {
				return Cron{}, fmt.Errorf("%q: the part %q is not allowed (values %d-%d): %s", s, part, lo, hi, cronRule)
			}
			for v := x; v <= y; v++ {
				*dst[i] |= 1 << v
			}
		}
	}
	return c, nil
}

func (c Cron) day(t time.Time) bool {
	if c.mon&(1<<int(t.Month())) == 0 {
		return false
	}
	dom := c.dom&(1<<t.Day()) != 0
	dow := c.dow&(1<<int(t.Weekday())) != 0
	switch {
	case c.domStar && c.dowStar:
		return true
	case c.domStar:
		return dow
	case c.dowStar:
		return dom
	}
	return dom || dow // both restricted: either matches, as in cron
}

// Next is the first fire time strictly after t, in UTC; zero when there is none within five
// years (for example 31 February).
func (c Cron) Next(t time.Time) time.Time {
	t = t.UTC().Truncate(time.Minute).Add(time.Minute)
	day := time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5*366; i, day = i+1, day.AddDate(0, 0, 1) {
		if !c.day(day) {
			continue
		}
		for h := 0; h < 24; h++ {
			if c.hour&(1<<h) == 0 {
				continue
			}
			for m := 0; m < 60; m++ {
				if c.min&(1<<m) == 0 {
					continue
				}
				at := day.Add(time.Duration(h)*time.Hour + time.Duration(m)*time.Minute)
				if !at.Before(t) {
					return at
				}
			}
		}
	}
	return time.Time{}
}

// Interval is the gap between the next two fire times after t.
func (c Cron) Interval(t time.Time) time.Duration {
	a := c.Next(t)
	if a.IsZero() {
		return 0
	}
	b := c.Next(a)
	if b.IsZero() {
		return 0
	}
	return b.Sub(a)
}
