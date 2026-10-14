package frigate

import (
	"strings"
	"testing"
	"time"
)

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// The live server runs in UTC and returned "2026-10-13T18:35:47Z" for an
// event that happened at 14:35 in the house (America/New_York). These cases
// pin the window a model gets for the questions people actually ask.
func TestWindow(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 22:24 on Oct 13 in New York, 02:24 UTC on Oct 14: the UTC date has
	// already rolled over, which is where a naive "today" goes wrong.
	evening := time.Date(2026, 10, 14, 2, 24, 0, 0, time.UTC)
	morning := time.Date(2026, 10, 14, 7, 5, 0, 0, ny)
	midAfternoon := time.Date(2026, 10, 13, 15, 0, 0, 0, ny)

	cases := []struct {
		name          string
		after, before string
		hours         int
		now           time.Time
		wantAfter     string
		wantBefore    string
		wantClamped   bool
		wantErr       string
	}{
		{name: "default is last 24h", now: evening,
			wantAfter: "2026-10-12T22:24:00-04:00", wantBefore: "2026-10-13T22:24:00-04:00"},
		{name: "hours lookback", hours: 3, now: evening,
			wantAfter: "2026-10-13T19:24:00-04:00", wantBefore: "2026-10-13T22:24:00-04:00"},
		{name: "this afternoon, asked in the evening (UTC already tomorrow)", after: "12:00", before: "18:00", now: evening,
			wantAfter: "2026-10-13T12:00:00-04:00", wantBefore: "2026-10-13T18:00:00-04:00"},
		{name: "this afternoon, asked during it", after: "12:00", before: "18:00", now: midAfternoon,
			wantAfter: "2026-10-13T12:00:00-04:00", wantBefore: "2026-10-13T18:00:00-04:00"},
		{name: "last night, asked in the morning", after: "20:00", before: "06:00", now: morning,
			wantAfter: "2026-10-13T20:00:00-04:00", wantBefore: "2026-10-14T06:00:00-04:00"},
		{name: "since 22:00, asked in the morning", after: "22:00", now: morning,
			wantAfter: "2026-10-13T22:00:00-04:00", wantBefore: "2026-10-14T07:05:00-04:00"},
		{name: "dated local", after: "2026-10-13T12:00", before: "2026-10-13 18:30", now: evening,
			wantAfter: "2026-10-13T12:00:00-04:00", wantBefore: "2026-10-13T18:30:00-04:00"},
		{name: "date only is local midnight", after: "2026-10-13", before: "2026-10-14", now: evening,
			wantAfter: "2026-10-13T00:00:00-04:00", wantBefore: "2026-10-14T00:00:00-04:00"},
		{name: "explicit UTC offset is respected", after: "2026-10-13T16:00:00Z", before: "2026-10-13T22:00:00Z", now: evening,
			wantAfter: "2026-10-13T12:00:00-04:00", wantBefore: "2026-10-13T18:00:00-04:00"},
		{name: "DST fall-back night is 4 real hours", after: "2026-11-01T00:00", before: "2026-11-01T03:00", now: time.Date(2026, 11, 2, 0, 0, 0, 0, ny),
			wantAfter: "2026-11-01T00:00:00-04:00", wantBefore: "2026-11-01T03:00:00-05:00"},
		{name: "clamped to 7 days", after: "2026-09-01", now: evening,
			wantAfter: "2026-10-06T22:24:00-04:00", wantBefore: "2026-10-13T22:24:00-04:00", wantClamped: true},
		{name: "garbage", after: "yesterday afternoon", now: evening, wantErr: "cannot parse after"},
		{name: "inverted", after: "2026-10-13T18:00", before: "2026-10-13T12:00", now: evening, wantErr: "must be earlier"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			after, before, clamped, err := window(tc.after, tc.before, tc.hours, ny, tc.now)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := after.In(ny).Format(time.RFC3339); got != tc.wantAfter {
				t.Errorf("after = %s, want %s", got, tc.wantAfter)
			}
			if got := before.In(ny).Format(time.RFC3339); got != tc.wantBefore {
				t.Errorf("before = %s, want %s", got, tc.wantBefore)
			}
			if clamped != tc.wantClamped {
				t.Errorf("clamped = %v", clamped)
			}
		})
	}
	// The DST case spans 4 hours of real time, not 3.
	a, b, _, _ := window("2026-11-01T00:00", "2026-11-01T03:00", 0, ny, time.Date(2026, 11, 2, 0, 0, 0, 0, ny))
	if b.Sub(a) != 4*time.Hour {
		t.Errorf("DST window = %v, want 4h", b.Sub(a))
	}
}

func TestResolveLocation(t *testing.T) {
	t.Setenv("TZ", "")
	loc, src, err := resolveLocation("Europe/London", "America/New_York")
	if err != nil || loc.String() != "Europe/London" || src != tzSourceArg {
		t.Errorf("arg: %v %s %v", loc, src, err)
	}
	loc, src, _ = resolveLocation("", "America/New_York")
	if loc.String() != "America/New_York" || src != tzSourceFrigate {
		t.Errorf("frigate: %v %s", loc, src)
	}
	t.Setenv("TZ", "America/Chicago")
	loc, src, _ = resolveLocation("", "")
	if loc.String() != "America/Chicago" || src != tzSourceEnv {
		t.Errorf("env: %v %s", loc, src)
	}
	if _, _, err := resolveLocation("Eastern", ""); err == nil || !strings.Contains(err.Error(), "IANA") {
		t.Errorf("bad arg err = %v", err)
	}
}
