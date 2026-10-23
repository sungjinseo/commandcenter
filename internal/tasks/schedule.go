package tasks

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Schedule kinds (plan 11 Decision 7).
const (
	Manual  = "manual"
	Once    = "once"
	Hourly  = "hourly"
	Daily   = "daily"
	Weekly  = "weekly"
	Monthly = "monthly"
)

// Schedule is a parsed schedule string:
//
//	manual
//	once:<RFC3339>          (an offset-less "2006-01-02T15:04[:05]" is in server time)
//	hourly:<MM>
//	daily:<HH:MM>
//	weekly:<mon..sun>:<HH:MM>
//	monthly:<1-28>:<HH:MM>
//
// Times are wall-clock times in the server's zone (time.Local, from TZ).
// Monthly days stop at 28 so that every month has the day.
type Schedule struct {
	Kind    string
	At      time.Time    // once
	Hour    int          // daily, weekly, monthly
	Minute  int          // every kind but manual and once
	Weekday time.Weekday // weekly
	Day     int          // monthly
}

var weekdays = map[string]time.Weekday{"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday,
	"wed": time.Wednesday, "thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday}

var weekdayNames = [...]string{"sun", "mon", "tue", "wed", "thu", "fri", "sat"}

// ParseSchedule parses s; an offset-less once time is in time.Local.
func ParseSchedule(s string) (Schedule, error) { return ParseScheduleIn(s, time.Local) }

// ParseScheduleIn parses s, reading an offset-less once time in loc.
func ParseScheduleIn(s string, loc *time.Location) (Schedule, error) {
	s = strings.TrimSpace(s)
	kind, rest, _ := strings.Cut(s, ":")
	bad := func(format string) (Schedule, error) {
		return Schedule{}, fmt.Errorf("invalid schedule %q: use %s", s, format)
	}
	switch kind {
	case Manual:
		if s != Manual {
			return bad("manual")
		}
		return Schedule{Kind: Manual}, nil
	case Once:
		at, err := time.Parse(time.RFC3339, rest)
		if err != nil {
			var err2 error
			if at, err2 = time.ParseInLocation("2006-01-02T15:04:05", rest, loc); err2 != nil {
				if at, err2 = time.ParseInLocation("2006-01-02T15:04", rest, loc); err2 != nil {
					return bad("once:<RFC3339 time>, e.g. once:2026-10-21T03:00:00+09:00")
				}
			}
		}
		return Schedule{Kind: Once, At: at}, nil
	case Hourly:
		m, ok := number(rest, 0, 59)
		if !ok {
			return bad("hourly:<MM>, minutes 00-59")
		}
		return Schedule{Kind: Hourly, Minute: m}, nil
	case Daily:
		h, m, ok := hhmm(rest)
		if !ok {
			return bad("daily:<HH:MM>")
		}
		return Schedule{Kind: Daily, Hour: h, Minute: m}, nil
	case Weekly:
		day, hm, _ := strings.Cut(rest, ":")
		wd, okDay := weekdays[strings.ToLower(day)]
		h, m, ok := hhmm(hm)
		if !okDay || !ok {
			return bad("weekly:<mon|tue|wed|thu|fri|sat|sun>:<HH:MM>")
		}
		return Schedule{Kind: Weekly, Weekday: wd, Hour: h, Minute: m}, nil
	case Monthly:
		day, hm, _ := strings.Cut(rest, ":")
		d, okDay := number(day, 1, 28)
		h, m, ok := hhmm(hm)
		if !okDay || !ok {
			return bad("monthly:<day 1-28>:<HH:MM>")
		}
		return Schedule{Kind: Monthly, Day: d, Hour: h, Minute: m}, nil
	}
	return bad("manual, once:<time>, hourly:<MM>, daily:<HH:MM>, weekly:<day>:<HH:MM> or monthly:<1-28>:<HH:MM>")
}

// number parses a decimal of one or two digits in [lo, hi].
func number(s string, lo, hi int) (int, bool) {
	if len(s) < 1 || len(s) > 2 {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < lo || n > hi || s[0] == '+' || s[0] == '-' {
		return 0, false
	}
	return n, true
}

// hhmm parses "HH:MM" (one-digit hours allowed).
func hhmm(s string) (h, m int, ok bool) {
	hs, ms, found := strings.Cut(s, ":")
	if !found || len(ms) != 2 {
		return 0, 0, false
	}
	h, okH := number(hs, 0, 23)
	m, okM := number(ms, 0, 59)
	return h, m, okH && okM
}

// String returns the canonical form of s.
func (s Schedule) String() string {
	switch s.Kind {
	case Once:
		return Once + ":" + s.At.Format(time.RFC3339)
	case Hourly:
		return fmt.Sprintf("%s:%02d", Hourly, s.Minute)
	case Daily:
		return fmt.Sprintf("%s:%02d:%02d", Daily, s.Hour, s.Minute)
	case Weekly:
		return fmt.Sprintf("%s:%s:%02d:%02d", Weekly, weekdayNames[s.Weekday], s.Hour, s.Minute)
	case Monthly:
		return fmt.Sprintf("%s:%d:%02d:%02d", Monthly, s.Day, s.Hour, s.Minute)
	}
	return Manual
}

// Next returns the first run time strictly after after, in loc's wall
// clock; the zero time when there is none (manual, or once in the past).
// Month and year ends carry over (see wall for DST).
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	t := after.In(loc)
	y, mo, d := t.Date()
	at := func(day int) time.Time { return wall(y, mo, day, s.Hour, s.Minute, loc) }
	switch s.Kind {
	case Once:
		if s.At.After(after) {
			return s.At
		}
		return time.Time{}
	case Hourly:
		// Back to the start of the local hour in absolute time, so zones
		// with half-hour offsets get the local minute.
		start := t.Add(-time.Duration(t.Minute())*time.Minute - time.Duration(t.Second())*time.Second - time.Duration(t.Nanosecond()))
		c := start.Add(time.Duration(s.Minute) * time.Minute)
		if !c.After(after) {
			c = c.Add(time.Hour)
		}
		return c
	case Daily:
		if c := at(d); c.After(after) {
			return c
		}
		return at(d + 1)
	case Weekly:
		days := (int(s.Weekday) - int(t.Weekday()) + 7) % 7
		if c := at(d + days); c.After(after) {
			return c
		}
		return at(d + days + 7)
	case Monthly:
		if c := wall(y, mo, s.Day, s.Hour, s.Minute, loc); c.After(after) {
			return c
		}
		return wall(y, mo+1, s.Day, s.Hour, s.Minute, loc)
	}
	return time.Time{}
}

// wall returns the instant of wall-clock time h:m on the given date in loc
// (day and month may overflow, as with time.Date). A wall time that occurs
// twice (the hour repeated when DST ends) gives the first occurrence, so a
// schedule runs once. A wall time that does not exist (skipped when DST
// starts) gives the first valid instant after the gap, the transition.
// time.Date leaves both cases unspecified, so the offsets in force a day
// before and a day after are tried and the wall clock is checked.
func wall(y int, mo time.Month, day, h, m int, loc *time.Location) time.Time {
	naive := time.Date(y, mo, day, h, m, 0, 0, time.UTC) // normalized fields
	ny, nmo, nd := naive.Date()
	_, offBefore := naive.Add(-24 * time.Hour).In(loc).Zone()
	_, offAfter := naive.Add(24 * time.Hour).In(loc).Zone()
	var best time.Time
	for _, off := range []int{offBefore, offAfter} {
		c := naive.Add(-time.Duration(off) * time.Second).In(loc)
		cy, cmo, cd := c.Date()
		if cy == ny && cmo == nmo && cd == nd && c.Hour() == h && c.Minute() == m && (best.IsZero() || c.Before(best)) {
			best = c
		}
	}
	if !best.IsZero() {
		return best
	}
	// In a gap: read with the old offset, the wall time lands after the
	// transition, and its zone period starts at the transition.
	x := naive.Add(-time.Duration(offBefore) * time.Second).In(loc)
	if start, _ := x.ZoneBounds(); !start.IsZero() && !start.After(x) {
		return start.In(loc)
	}
	return time.Date(y, mo, day, h, m, 0, 0, loc)
}
