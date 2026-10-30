package tasks

import (
	"fmt"
	"slices"
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
	Cron    = "cron"
)

// LastDay in Schedule.Days is the last day of the month ("last").
const LastDay = 32

// Schedule is a parsed schedule string:
//
//	manual
//	once:<RFC3339>          (an offset-less "2006-01-02T15:04[:05]" is in server time)
//	hourly:<MM>
//	daily:<HH:MM>
//	weekly:<days>:<HH:MM>   days: mon..sun, comma-separated (mon,wed,fri)
//	monthly:<days>:<HH:MM>  days: 1-31 or last, comma-separated (1,15,last)
//	cron:<expression>       a Quartz cron expression (cron.go)
//
// Times are wall-clock times in the server's zone (time.Local, from TZ). A
// monthly day a month does not have (31 in April) is skipped that month,
// as Quartz does; "last" is the month's last day.
type Schedule struct {
	Kind     string
	At       time.Time      // once
	Hour     int            // daily, weekly, monthly
	Minute   int            // every kind but manual, once and cron
	Weekdays []time.Weekday // weekly: at least one, Monday first
	Days     []int          // monthly: at least one, ascending, LastDay last
	Cron     *CronExpr      // cron
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
		days, hm, _ := strings.Cut(rest, ":")
		wds, okDays := weekdaySet(days)
		h, m, ok := hhmm(hm)
		if !okDays || !ok {
			return bad("weekly:<days>:<HH:MM>, days mon, tue, wed, thu, fri, sat or sun, comma-separated (weekly:mon,thu:03:00)")
		}
		return Schedule{Kind: Weekly, Weekdays: wds, Hour: h, Minute: m}, nil
	case Monthly:
		days, hm, _ := strings.Cut(rest, ":")
		ds, okDays := daySet(days)
		h, m, ok := hhmm(hm)
		if !okDays || !ok {
			return bad("monthly:<days>:<HH:MM>, days 1-31 or last, comma-separated (monthly:1,15,last:03:00)")
		}
		return Schedule{Kind: Monthly, Days: ds, Hour: h, Minute: m}, nil
	case Cron:
		c, err := ParseCron(rest)
		if err != nil {
			return Schedule{}, fmt.Errorf("invalid schedule %q: %w", s, err)
		}
		return Schedule{Kind: Cron, Cron: c}, nil
	}
	return bad("manual, once:<time>, hourly:<MM>, daily:<HH:MM>, weekly:<days>:<HH:MM>, monthly:<days>:<HH:MM> or cron:<Quartz expression>")
}

// weekdaySet parses a comma-separated list of day names, Monday first.
func weekdaySet(s string) ([]time.Weekday, bool) {
	var seen [7]bool
	for _, name := range strings.Split(s, ",") {
		wd, ok := weekdays[strings.ToLower(strings.TrimSpace(name))]
		if !ok {
			return nil, false
		}
		seen[wd] = true
	}
	var out []time.Weekday
	for i := 1; i <= 7; i++ {
		if wd := time.Weekday(i % 7); seen[wd] {
			out = append(out, wd)
		}
	}
	return out, true
}

// daySet parses a comma-separated list of month days 1-31 and "last".
func daySet(s string) ([]int, bool) {
	var seen [LastDay + 1]bool
	for _, d := range strings.Split(s, ",") {
		d = strings.TrimSpace(d)
		if strings.EqualFold(d, "last") {
			seen[LastDay] = true
			continue
		}
		n, ok := number(d, 1, 31)
		if !ok {
			return nil, false
		}
		seen[n] = true
	}
	var out []int
	for d := 1; d <= LastDay; d++ {
		if seen[d] {
			out = append(out, d)
		}
	}
	return out, true
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
		names := make([]string, len(s.Weekdays))
		for i, wd := range s.Weekdays {
			names[i] = weekdayNames[wd]
		}
		return fmt.Sprintf("%s:%s:%02d:%02d", Weekly, strings.Join(names, ","), s.Hour, s.Minute)
	case Monthly:
		days := make([]string, len(s.Days))
		for i, d := range s.Days {
			days[i] = strconv.Itoa(d)
			if d == LastDay {
				days[i] = "last"
			}
		}
		return fmt.Sprintf("%s:%s:%02d:%02d", Monthly, strings.Join(days, ","), s.Hour, s.Minute)
	case Cron:
		return Cron + ":" + s.Cron.String()
	}
	return Manual
}

// runsOn reports whether a daily, weekly or monthly schedule runs on the
// date y-mo-d (normalized as time.Date does).
func (s Schedule) runsOn(y int, mo time.Month, d int) bool {
	date := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC)
	switch s.Kind {
	case Weekly:
		return slices.Contains(s.Weekdays, date.Weekday())
	case Monthly:
		day := date.Day()
		last := time.Date(date.Year(), date.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
		return slices.Contains(s.Days, day) || day == last && slices.Contains(s.Days, LastDay)
	}
	return true
}

// Next returns the first run time strictly after after, in loc's wall
// clock; the zero time when there is none (manual, once in the past, a
// cron expression with no time left). Month and year ends carry over (see
// wall for DST).
func (s Schedule) Next(after time.Time, loc *time.Location) time.Time {
	if loc == nil {
		loc = time.Local
	}
	t := after.In(loc)
	y, mo, d := t.Date()
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
	case Daily, Weekly, Monthly:
		// Every day set is met within 62 days (a 31st after January 31st
		// is March 31st); a wall time on the first day may have passed.
		for i := 0; i <= 62; i++ {
			if !s.runsOn(y, mo, d+i) {
				continue
			}
			if c := wall(y, mo, d+i, s.Hour, s.Minute, 0, loc); c.After(after) {
				return c
			}
		}
	case Cron:
		return s.Cron.Next(after, loc)
	}
	return time.Time{}
}

// wall returns the instant of wall-clock time h:m:sec on the given date in loc
// (day and month may overflow, as with time.Date). A wall time that occurs
// twice (the hour repeated when DST ends) gives the first occurrence, so a
// schedule runs once. A wall time that does not exist (skipped when DST
// starts) gives the first valid instant after the gap, the transition.
// time.Date leaves both cases unspecified, so the offsets in force a day
// before and a day after are tried and the wall clock is checked.
func wall(y int, mo time.Month, day, h, m, sec int, loc *time.Location) time.Time {
	naive := time.Date(y, mo, day, h, m, sec, 0, time.UTC) // normalized fields
	ny, nmo, nd := naive.Date()
	_, offBefore := naive.Add(-24 * time.Hour).In(loc).Zone()
	_, offAfter := naive.Add(24 * time.Hour).In(loc).Zone()
	var best time.Time
	for _, off := range []int{offBefore, offAfter} {
		c := naive.Add(-time.Duration(off) * time.Second).In(loc)
		cy, cmo, cd := c.Date()
		if cy == ny && cmo == nmo && cd == nd && c.Hour() == h && c.Minute() == m && c.Second() == sec && (best.IsZero() || c.Before(best)) {
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
	return time.Date(y, mo, day, h, m, sec, 0, loc)
}
