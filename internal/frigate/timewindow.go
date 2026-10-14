package frigate

import (
	"fmt"
	"os"
	"strings"
	"time"

	// Embed the zone database so IANA names resolve even on an image
	// without /usr/share/zoneinfo.
	_ "time/tzdata"
)

// Where a time zone came from, reported alongside every time so the model
// can tell a deliberate house zone from a server default.
const (
	tzSourceArg     = "argument"
	tzSourceFrigate = "frigate ui.timezone"
	tzSourceEnv     = "TZ environment variable"
	tzSourceServer  = "server default"
)

// resolveLocation picks the zone that "this afternoon" and "last night"
// mean. Frigate stores epoch seconds and its own clock (and the timestamp
// burned into its images) is usually UTC inside its container, so neither
// tells us where the house is. In order: an explicit argument, Frigate's
// ui.timezone, TZ, then the server's local zone.
func resolveLocation(arg, frigateTZ string) (*time.Location, string, error) {
	if arg != "" {
		loc, err := time.LoadLocation(arg)
		if err != nil {
			return nil, "", fmt.Errorf("unknown timezone %q: use an IANA name such as America/New_York", arg)
		}
		return loc, tzSourceArg, nil
	}
	if frigateTZ != "" {
		if loc, err := time.LoadLocation(frigateTZ); err == nil {
			return loc, tzSourceFrigate, nil
		}
	}
	if tz := os.Getenv("TZ"); tz != "" {
		if loc, err := time.LoadLocation(strings.TrimPrefix(tz, ":")); err == nil {
			return loc, tzSourceEnv, nil
		}
	}
	return time.Local, tzSourceServer, nil
}

// localLayouts are the dated wall-clock forms accepted for after/before,
// read in the resolved house zone.
var localLayouts = []string{
	"2006-01-02T15:04:05",
	"2006-01-02T15:04",
	"2006-01-02 15:04:05",
	"2006-01-02 15:04",
	"2006-01-02",
}

const timeFormatsHelp = "use YYYY-MM-DDTHH:MM (house local time), YYYY-MM-DD, HH:MM, or RFC 3339 with an offset"

// parseAbsolute parses a dated after/before argument. A value with an
// explicit offset is taken as given; anything else is wall-clock time in loc.
func parseAbsolute(s string, loc *time.Location) (time.Time, bool) {
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	for _, layout := range localLayouts {
		if t, err := time.ParseInLocation(layout, s, loc); err == nil {
			return t, true
		}
	}
	return time.Time{}, false
}

// parseClock parses a bare HH:MM.
func parseClock(s string) (hour, minute int, ok bool) {
	t, err := time.Parse("15:04", s)
	if err != nil {
		return 0, 0, false
	}
	return t.Hour(), t.Minute(), true
}

// atClock returns the wall-clock time hh:mm on ref's date in loc.
func atClock(ref time.Time, hour, minute int, loc *time.Location) time.Time {
	r := ref.In(loc)
	return time.Date(r.Year(), r.Month(), r.Day(), hour, minute, 0, 0, loc)
}

// maxLookback bounds a query window. Frigate's default event retention is
// days, not weeks; a wider window is almost always a mistaken argument.
const maxLookback = 7 * 24 * time.Hour

// window resolves after/before/hours into an absolute range.
//
// Models often do not know today's date, so a bare HH:MM is accepted and
// resolved against now in the house zone: in after it means the most recent
// such time (asked at 07:00, "22:00" is last night), and in before the first
// such time after `after` ("12:00" to "18:00" is this afternoon even while
// it is still 15:00). With no after, the window is the last `hours` hours
// (default 24) before `before`. A window wider than maxLookback is narrowed
// to its most recent part and clamped is set.
func window(afterArg, beforeArg string, hours int, loc *time.Location, now time.Time) (after, before time.Time, clamped bool, err error) {
	afterArg, beforeArg = strings.TrimSpace(afterArg), strings.TrimSpace(beforeArg)

	var beforeSet, beforeIsClock bool
	var bh, bm int
	if beforeArg != "" {
		if t, ok := parseAbsolute(beforeArg, loc); ok {
			before, beforeSet = t, true
		} else if h, m, ok := parseClock(beforeArg); ok {
			bh, bm, beforeIsClock = h, m, true
		} else {
			return after, before, false, fmt.Errorf("cannot parse before %q: %s", beforeArg, timeFormatsHelp)
		}
	}

	if afterArg != "" {
		if t, ok := parseAbsolute(afterArg, loc); ok {
			after = t
		} else if h, m, ok := parseClock(afterArg); ok {
			ref := now
			if beforeSet {
				ref = before
			}
			after = atClock(ref, h, m, loc)
			if after.After(ref) {
				after = after.AddDate(0, 0, -1)
			}
		} else {
			return after, before, false, fmt.Errorf("cannot parse after %q: %s", afterArg, timeFormatsHelp)
		}
	}

	switch {
	case beforeIsClock && !after.IsZero():
		before = atClock(after, bh, bm, loc)
		if !before.After(after) {
			before = before.AddDate(0, 0, 1)
		}
	case beforeIsClock:
		before = atClock(now, bh, bm, loc)
		if before.After(now) {
			before = before.AddDate(0, 0, -1)
		}
	case !beforeSet:
		before = now
	}

	if after.IsZero() {
		if hours <= 0 {
			hours = 24
		}
		after = before.Add(-time.Duration(hours) * time.Hour)
	}

	if !after.Before(before) {
		return after, before, false, fmt.Errorf("after (%s) must be earlier than before (%s)",
			after.In(loc).Format(time.RFC3339), before.In(loc).Format(time.RFC3339))
	}
	if before.Sub(after) > maxLookback {
		after, clamped = before.Add(-maxLookback), true
	}
	return after, before, clamped, nil
}

// epochTime converts Frigate's fractional epoch seconds.
func epochTime(sec float64) time.Time {
	whole := int64(sec)
	return time.Unix(whole, int64((sec-float64(whole))*1e9))
}
