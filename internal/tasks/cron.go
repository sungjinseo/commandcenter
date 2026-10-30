package tasks

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// CronExpr is a parsed Quartz cron expression, the schedule kind
// "cron:<expression>": seconds, minutes, hours, day of month, month, day of
// week and an optional year, separated by spaces. Each field takes *, a
// value, a range a-b (a range may wrap: 22-2), a step (*/15, 5/10, 1-20/2)
// and comma lists; months and days of the week also take their English
// names (JAN-DEC, SUN-SAT, Sunday is 1). Exactly one of the day fields is
// "?". The day of month also takes L (the last day), L-n (n days before
// it), LW (the last weekday) and nW (the weekday nearest day n, within the
// month); the day of week takes nL (the month's last day n) and n#k (its
// k-th day n), each on its own. The year runs from 1970 to 2099.
//
// Times are wall-clock times in the server's zone, as for the other kinds:
// a time that DST skips runs at the transition, a repeated one runs once.
type CronExpr struct {
	src                   string // canonical: upper case, single spaces
	sec, min, hour        []int  // ascending
	month                 [13]bool
	year                  [2100]bool
	dom                   [32]bool // day-of-month set; unused with domSpecial or dowRules
	domLast               int      // L, L-n: n (0 for L); -1 when unused
	domNearest            int      // nW: n; LW: LastDay; 0 when unused
	dowRules              bool     // the day of month is "?": the day of week decides
	dow                   [7]bool  // day-of-week set (Sunday 0)
	dowLast, dowNth, dowK int      // nL: weekday+1; n#k: weekday+1 and k; 0 when unused
}

const (
	cronMaxLen  = 200
	cronMaxYear = 2099
)

var (
	monthNames = map[string]int{"JAN": 1, "FEB": 2, "MAR": 3, "APR": 4, "MAY": 5, "JUN": 6,
		"JUL": 7, "AUG": 8, "SEP": 9, "OCT": 10, "NOV": 11, "DEC": 12}
	dayNames = map[string]int{"SUN": 1, "MON": 2, "TUE": 3, "WED": 4, "THU": 5, "FRI": 6, "SAT": 7}
)

// ParseCron parses a Quartz cron expression (see CronExpr).
func ParseCron(expr string) (*CronExpr, error) {
	if len(expr) > cronMaxLen {
		return nil, fmt.Errorf("the cron expression is longer than %d characters", cronMaxLen)
	}
	f := strings.Fields(strings.ToUpper(expr))
	if len(f) < 6 || len(f) > 7 {
		return nil, errors.New(`a cron expression has 6 or 7 fields: seconds minutes hours day-of-month month day-of-week [year], e.g. "0 0 1 * * ?"`)
	}
	c := &CronExpr{src: strings.Join(f, " "), domLast: -1}
	field := func(i int, name string, lo, hi int, names map[string]int) ([]bool, error) {
		set, err := cronSet(f[i], lo, hi, names)
		if err != nil {
			return nil, fmt.Errorf("%s %q: %w", name, f[i], err)
		}
		return set, nil
	}
	var err error
	var set []bool
	list := func(set []bool) []int {
		var out []int
		for v, ok := range set {
			if ok {
				out = append(out, v)
			}
		}
		return out
	}
	if set, err = field(0, "seconds", 0, 59, nil); err != nil {
		return nil, err
	}
	c.sec = list(set)
	if set, err = field(1, "minutes", 0, 59, nil); err != nil {
		return nil, err
	}
	c.min = list(set)
	if set, err = field(2, "hours", 0, 23, nil); err != nil {
		return nil, err
	}
	c.hour = list(set)
	if set, err = field(4, "month", 1, 12, monthNames); err != nil {
		return nil, err
	}
	copy(c.month[:], set)
	years := "*"
	if len(f) == 7 {
		years = f[6]
	}
	if set, err = cronSet(years, 1970, cronMaxYear, nil); err != nil {
		return nil, fmt.Errorf("year %q: %w", years, err)
	}
	copy(c.year[:], set)

	dom, dow := f[3], f[5]
	switch {
	case dom == "?" && dow == "?":
		return nil, errors.New(`"?" goes in one of day-of-month and day-of-week, not both`)
	case dom != "?" && dow != "?":
		return nil, errors.New(`one of day-of-month and day-of-week must be "?" (e.g. "0 0 1 * * ?" or "0 0 1 ? * MON")`)
	case dom == "?":
		c.dowRules = true
		if err := c.parseDow(dow); err != nil {
			return nil, fmt.Errorf("day-of-week %q: %w", dow, err)
		}
	default:
		if err := c.parseDom(dom); err != nil {
			return nil, fmt.Errorf("day-of-month %q: %w", dom, err)
		}
	}
	return c, nil
}

func (c *CronExpr) parseDom(s string) error {
	switch {
	case s == "LW":
		c.domNearest = LastDay
		return nil
	case s == "L":
		c.domLast = 0
		return nil
	case strings.HasPrefix(s, "L-"):
		n, ok := number(s[2:], 0, 30)
		if !ok {
			return errors.New("L-n takes n from 0 to 30")
		}
		c.domLast = n
		return nil
	case strings.HasSuffix(s, "W"):
		n, ok := number(strings.TrimSuffix(s, "W"), 1, 31)
		if !ok {
			return errors.New("nW takes one day n from 1 to 31")
		}
		c.domNearest = n
		return nil
	case strings.ContainsAny(s, "LW"):
		return errors.New("L and W stand alone (L, L-3, LW, 15W)")
	}
	set, err := cronSet(s, 1, 31, nil)
	copy(c.dom[:], set)
	return err
}

func (c *CronExpr) parseDow(s string) error {
	day := func(v string) (int, bool) {
		if n, ok := dayNames[v]; ok {
			return n, true
		}
		return number(v, 1, 7)
	}
	switch {
	case s == "L": // alone, the last day of the week: Saturday
		c.dow[time.Saturday] = true
		return nil
	case strings.HasSuffix(s, "L"):
		n, ok := day(strings.TrimSuffix(s, "L"))
		if !ok {
			return errors.New("nL takes one day n, 1 (SUN) to 7 (SAT)")
		}
		c.dowLast = n
		return nil
	case strings.Contains(s, "#"):
		d, k, _ := strings.Cut(s, "#")
		n, ok := day(d)
		kn, okK := number(k, 1, 5)
		if !ok || !okK {
			return errors.New("n#k takes one day n, 1 (SUN) to 7 (SAT), and k from 1 to 5")
		}
		c.dowNth, c.dowK = n, kn
		return nil
	case strings.ContainsAny(s, "L#"):
		return errors.New("L and # stand alone (6L, 6#3)")
	}
	set, err := cronSet(s, 1, 7, dayNames)
	for v := 1; v <= 7 && err == nil; v++ {
		c.dow[v-1] = set[v]
	}
	return err
}

// cronSet parses a comma list of *, values, ranges (which may wrap) and
// steps into a set indexed by value (length hi+1).
func cronSet(s string, lo, hi int, names map[string]int) ([]bool, error) {
	set := make([]bool, hi+1)
	value := func(v string) (int, error) {
		if n, ok := names[v]; ok {
			return n, nil
		}
		n, err := strconv.Atoi(v)
		if err != nil || !allDigits(v) || n < lo || n > hi {
			return 0, fmt.Errorf("%q is not a value from %d to %d", v, lo, hi)
		}
		return n, nil
	}
	size := hi - lo + 1
	for _, item := range strings.Split(s, ",") {
		rng, stepStr, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || !allDigits(stepStr) || n < 1 || n > size {
				return nil, fmt.Errorf("the step %q is not a number from 1 to %d", stepStr, size)
			}
			step = n
		}
		a, b := lo, hi
		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			as, bs, _ := strings.Cut(rng, "-")
			var err error
			if a, err = value(as); err != nil {
				return nil, err
			}
			if b, err = value(bs); err != nil {
				return nil, err
			}
		default:
			var err error
			if a, err = value(rng); err != nil {
				return nil, err
			}
			if !hasStep {
				b = a // a single value; a/n runs from a to the end
			}
		}
		for i, n := 0, (b-a+size)%size+1; i < n; i += step {
			set[lo+(a-lo+i)%size] = true
		}
	}
	return set, nil
}

func allDigits(s string) bool { return s != "" && strings.Trim(s, "0123456789") == "" }

// String is the canonical expression.
func (c *CronExpr) String() string { return c.src }

// runsOn reports whether the expression has times on date (in UTC, as a
// calendar date).
func (c *CronExpr) runsOn(date time.Time) bool {
	day := date.Day()
	last := time.Date(date.Year(), date.Month()+1, 0, 0, 0, 0, 0, time.UTC).Day()
	wd := date.Weekday()
	if c.dowRules {
		switch {
		case c.dowLast > 0:
			return int(wd) == c.dowLast-1 && day+7 > last
		case c.dowNth > 0:
			return int(wd) == c.dowNth-1 && (day-1)/7+1 == c.dowK
		}
		return c.dow[wd]
	}
	switch {
	case c.domLast >= 0:
		return day == last-c.domLast
	case c.domNearest > 0:
		n := c.domNearest
		if n == LastDay {
			n = last
		}
		if n > last {
			return false // the month has no such day
		}
		switch time.Date(date.Year(), date.Month(), n, 0, 0, 0, 0, time.UTC).Weekday() {
		case time.Saturday:
			if n == 1 {
				return day == 3 // Monday the 3rd: the nearest weekday stays in the month
			}
			return day == n-1
		case time.Sunday:
			if n == last {
				return day == n-2
			}
			return day == n+1
		}
		return day == n
	}
	return c.dom[day]
}

// Next returns the first time of the expression strictly after after, in
// loc's wall clock (see wall), or the zero time when there is none before
// the year 2100.
func (c *CronExpr) Next(after time.Time, loc *time.Location) time.Time {
	t := after.In(loc)
	y, mo, d := t.Date()
	for date, first := time.Date(y, mo, d, 0, 0, 0, 0, time.UTC), true; date.Year() <= cronMaxYear; first = false {
		switch {
		case !c.year[date.Year()]:
			date = time.Date(date.Year()+1, 1, 1, 0, 0, 0, 0, time.UTC)
			continue
		case !c.month[date.Month()]:
			date = time.Date(date.Year(), date.Month()+1, 1, 0, 0, 0, 0, time.UTC)
			continue
		}
		if c.runsOn(date) {
			if at := c.on(date, first, t, after, loc); !at.IsZero() {
				return at
			}
		}
		date = date.AddDate(0, 0, 1)
	}
	return time.Time{}
}

// on returns the first time of the expression on date strictly after
// after, or the zero time. On the first day, the wall times before t's
// map to instants before after (the first occurrence of a repeated hour,
// the transition for a skipped one), so they are not tried.
func (c *CronExpr) on(date time.Time, first bool, t, after time.Time, loc *time.Location) time.Time {
	y, mo, d := date.Date()
	for _, h := range c.hour {
		if first && h < t.Hour() {
			continue
		}
		for _, m := range c.min {
			if first && h == t.Hour() && m < t.Minute() {
				continue
			}
			for _, s := range c.sec {
				if first && h == t.Hour() && m == t.Minute() && s < t.Second() {
					continue
				}
				if at := wall(y, mo, d, h, m, s, loc); at.After(after) {
					return at
				}
			}
		}
	}
	return time.Time{}
}
