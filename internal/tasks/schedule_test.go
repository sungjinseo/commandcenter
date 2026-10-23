package tasks

import (
	"testing"
	"time"
)

func TestParseSchedule(t *testing.T) {
	good := map[string]string{
		"manual":                    "manual",
		"hourly:05":                 "hourly:05",
		"hourly:5":                  "hourly:05",
		"daily:03:00":               "daily:03:00",
		"daily:3:07":                "daily:03:07",
		"weekly:mon:23:59":          "weekly:mon:23:59",
		"weekly:SUN:00:00":          "weekly:sun:00:00",
		"monthly:1:03:00":           "monthly:1:03:00",
		"monthly:28:12:30":          "monthly:28:12:30",
		"once:2026-10-21T03:00:00Z": "once:2026-10-21T03:00:00Z",
		" daily:03:00 ":             "daily:03:00",
	}
	for in, want := range good {
		s, err := ParseSchedule(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		if got := s.String(); got != want {
			t.Errorf("%q: String() = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{
		"", "Manual", "never", "hourly", "hourly:60", "hourly:-1", "hourly:aa", "daily:24:00", "daily:03:60",
		"daily:03", "daily:0300", "weekly:mon", "weekly:xyz:03:00", "weekly:mon:25:00", "monthly:0:03:00",
		"monthly:29:03:00", "monthly:31:03:00", "monthly:x:03:00", "once:", "once:tomorrow", "once:2026-13-01T00:00:00Z",
		"daily:03:00:00", "cron:* * * * *",
	} {
		if _, err := ParseSchedule(in); err == nil {
			t.Errorf("%q: accepted", in)
		}
	}
}

func next(t *testing.T, sched string, after time.Time, loc *time.Location) time.Time {
	t.Helper()
	s, err := ParseScheduleIn(sched, loc)
	if err != nil {
		t.Fatal(err)
	}
	return s.Next(after, loc)
}

func TestNextUTC(t *testing.T) {
	utc := time.UTC
	at := func(y int, m time.Month, d, hh, mm, ss int) time.Time { return time.Date(y, m, d, hh, mm, ss, 0, utc) }
	cases := []struct {
		sched string
		after time.Time
		want  time.Time
	}{
		{"manual", at(2026, 10, 20, 12, 0, 0), time.Time{}},
		// hourly
		{"hourly:15", at(2026, 10, 20, 12, 0, 0), at(2026, 10, 20, 12, 15, 0)},
		{"hourly:15", at(2026, 10, 20, 12, 15, 0), at(2026, 10, 20, 13, 15, 0)}, // strictly after
		{"hourly:15", at(2026, 10, 20, 23, 30, 0), at(2026, 10, 21, 0, 15, 0)},
		{"hourly:00", at(2026, 12, 31, 23, 0, 1), at(2027, 1, 1, 0, 0, 0)},
		// daily
		{"daily:03:00", at(2026, 10, 20, 2, 59, 59), at(2026, 10, 20, 3, 0, 0)},
		{"daily:03:00", at(2026, 10, 20, 3, 0, 0), at(2026, 10, 21, 3, 0, 0)},
		{"daily:03:00", at(2026, 10, 31, 4, 0, 0), at(2026, 11, 1, 3, 0, 0)}, // month end
		{"daily:03:00", at(2026, 12, 31, 4, 0, 0), at(2027, 1, 1, 3, 0, 0)},  // year end
		{"daily:00:00", at(2028, 2, 28, 0, 0, 0), at(2028, 2, 29, 0, 0, 0)},  // leap day
		// weekly: 2026-10-20 is a Tuesday
		{"weekly:tue:03:00", at(2026, 10, 20, 2, 0, 0), at(2026, 10, 20, 3, 0, 0)},
		{"weekly:tue:03:00", at(2026, 10, 20, 3, 0, 0), at(2026, 10, 27, 3, 0, 0)},
		{"weekly:mon:03:00", at(2026, 10, 20, 0, 0, 0), at(2026, 10, 26, 3, 0, 0)},
		{"weekly:sun:23:00", at(2026, 10, 30, 0, 0, 0), at(2026, 11, 1, 23, 0, 0)}, // across month end
		// monthly
		{"monthly:1:03:00", at(2026, 10, 20, 0, 0, 0), at(2026, 11, 1, 3, 0, 0)},
		{"monthly:28:03:00", at(2026, 1, 28, 3, 0, 0), at(2026, 2, 28, 3, 0, 0)}, // into February
		{"monthly:28:03:00", at(2026, 1, 31, 0, 0, 0), at(2026, 2, 28, 3, 0, 0)},
		{"monthly:15:00:00", at(2026, 12, 15, 0, 0, 0), at(2027, 1, 15, 0, 0, 0)}, // year end
		{"monthly:20:12:00", at(2026, 10, 20, 11, 0, 0), at(2026, 10, 20, 12, 0, 0)},
		// once
		{"once:2026-10-21T03:00:00Z", at(2026, 10, 20, 0, 0, 0), at(2026, 10, 21, 3, 0, 0)},
		{"once:2026-10-21T03:00:00Z", at(2026, 10, 21, 3, 0, 0), time.Time{}},
	}
	for _, c := range cases {
		if got := next(t, c.sched, c.after, utc); !got.Equal(c.want) {
			t.Errorf("%s after %s: got %s, want %s", c.sched, c.after, got, c.want)
		}
	}
}

// Schedules are in server local time: the TZ is honoured, offsets included.
func TestNextTimeZone(t *testing.T) {
	seoul := time.FixedZone("KST", 9*3600)
	// 2026-10-20 20:00 UTC is 2026-10-21 05:00 in Seoul: today's 03:00 has passed.
	got := next(t, "daily:03:00", time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC), seoul)
	if want := time.Date(2026, 10, 22, 3, 0, 0, 0, seoul); !got.Equal(want) {
		t.Fatalf("daily in KST: got %s, want %s", got, want)
	}
	if want := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("daily in KST as UTC: got %s", got.UTC())
	}
	// A half-hour zone: hourly:00 is on the local hour, not the UTC hour.
	india := time.FixedZone("IST", 5*3600+1800)
	got = next(t, "hourly:00", time.Date(2026, 10, 20, 10, 0, 0, 0, time.UTC), india)
	if want := time.Date(2026, 10, 20, 10, 30, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("hourly in IST: got %s, want %s", got.UTC(), want)
	}
	// once without an offset is read in the server zone.
	got = next(t, "once:2026-10-21T03:00", time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC), seoul)
	if want := time.Date(2026, 10, 20, 18, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("once in KST: got %s", got.UTC())
	}
	// weekly/monthly use the local weekday and date.
	got = next(t, "weekly:wed:03:00", time.Date(2026, 10, 20, 20, 0, 0, 0, time.UTC), seoul) // Wed 05:00 KST
	if want := time.Date(2026, 10, 28, 3, 0, 0, 0, seoul); !got.Equal(want) {
		t.Fatalf("weekly in KST: got %s", got)
	}

	// A zone with DST: the next daily run keeps the wall-clock time.
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata:", err)
	}
	// DST ends on 2026-11-01 at 02:00 EDT.
	got = next(t, "daily:03:00", time.Date(2026, 10, 31, 12, 0, 0, 0, ny), ny)
	if want := time.Date(2026, 11, 1, 3, 0, 0, 0, ny); !got.Equal(want) || got.Sub(time.Date(2026, 10, 31, 3, 0, 0, 0, ny)) != 25*time.Hour {
		t.Fatalf("daily across DST end: got %s", got)
	}
	utc := func(mo time.Month, d, h, m int) time.Time { return time.Date(2027, mo, d, h, m, 0, 0, time.UTC) }
	// 02:30 does not exist on 2027-03-14 (clocks jump from 02:00 EST to
	// 03:00 EDT): the run is at the first valid instant, 03:00 EDT.
	for _, sched := range []string{"daily:02:30", "weekly:sun:02:30", "monthly:14:02:30"} {
		got = next(t, sched, time.Date(2027, 3, 13, 12, 0, 0, 0, ny), ny)
		if want := utc(3, 14, 7, 0); !got.Equal(want) {
			t.Errorf("%s into the DST gap: got %s (%s), want %s", sched, got, got.UTC(), want)
		}
	}
	// Just after the gap's run, the next one is the next day's 02:30 EDT.
	if got = next(t, "daily:02:30", utc(3, 14, 7, 0), ny); !got.Equal(utc(3, 15, 6, 30)) {
		t.Errorf("after the gap run: got %s", got.UTC())
	}
	// 01:30 happens twice on 2027-11-07 (EDT, then EST): the first one,
	// and only once.
	got = next(t, "daily:01:30", time.Date(2027, 11, 6, 12, 0, 0, 0, ny), ny)
	if want := utc(11, 7, 5, 30); !got.Equal(want) {
		t.Fatalf("repeated hour: got %s, want %s", got.UTC(), want)
	}
	for _, after := range []time.Time{utc(11, 7, 5, 30), utc(11, 7, 6, 0), utc(11, 7, 6, 30)} {
		if got = next(t, "daily:01:30", after, ny); !got.Equal(utc(11, 8, 6, 30)) {
			t.Errorf("after the repeated hour run (%s): got %s, want the next day", after, got.UTC())
		}
	}
}
