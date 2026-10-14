package esphome

import (
	"regexp"
	"strings"
)

// Output from the dashboard arrives as one string per `output` event. Two
// facts about it, both taken from the device-builder source, shape this file:
//
//   - devices/validate and devices/logs strip each line's terminator before
//     sending it (`line.rstrip("\n\r")`), while firmware/follow_job sends the
//     raw chunk with its terminator kept, including bare "\r" for esptool and
//     PlatformIO progress overwrites. Concatenating events as-is therefore
//     glues every validate/log line together.
//   - `esphome --dashboard` replaces every real ANSI escape with the literal
//     four characters `\033`, and `esphome config`/`esphome logs` wrap
//     secret-ish values (passwords, keys, SSIDs, BSSIDs) in the conceal SGR
//     (`\033[8m ... \033[28m`) instead of removing them. The dashboard only
//     redacts those runs for validate, not for logs, so we redact everything.

const (
	// maxLines is how many trailing lines a stream retains. The tail is what
	// matters for builds and validation (the error is at the end), so older
	// lines are dropped first.
	maxLines = 2000
	// maxLineBytes caps one line so a single runaway frame cannot dominate.
	maxLineBytes = 2048
	// maxErrorLines caps the error lines collected over a whole stream.
	maxErrorLines = 25
)

// ansiEsc matches the escape byte or the literal `\033` spelling the
// dashboard uses.
const ansiEsc = `(?:\x1b|\\033)`

var (
	ansiCSI = regexp.MustCompile(ansiEsc + `\[[0-9;?]*[A-Za-z]`)
	// ansiConcealed matches a concealed run; the second alternative catches a
	// run whose closing code never arrived (a truncated line), so a cut-off
	// secret is still removed rather than leaked.
	ansiConcealed = regexp.MustCompile(ansiEsc + `\[8m(?:.*?` + ansiEsc + `\[28m|.*$)`)
)

// cleanLine redacts concealed runs, strips ANSI escapes and line terminators,
// and caps the length. Redaction runs first: stripping the conceal codes
// alone would expose what they wrap.
func cleanLine(s string) string {
	s = ansiConcealed.ReplaceAllString(s, "<removed>")
	s = ansiCSI.ReplaceAllString(s, "")
	s = strings.TrimRight(s, "\r\n")
	if len(s) > maxLineBytes {
		s = s[:maxLineBytes] + "…[line truncated]"
	}
	return s
}

// lineBuffer accumulates stream output as cleaned lines, keeping the last
// maxLines and collapsing carriage-return progress overwrites.
type lineBuffer struct {
	lines   []string
	dropped int
	// overwrite is set when the last line ended in a bare "\r": the next line
	// replaces it, as a terminal would render it.
	overwrite bool
	// observe, when set, sees every settled, non-empty cleaned line
	// (including ones later dropped from the tail), so callers can extract
	// facts from the whole stream.
	observe func(string)
}

// add ingests one output event's text.
func (b *lineBuffer) add(chunk string) {
	segs := strings.Split(chunk, "\n")
	for i, seg := range segs {
		last := i == len(segs)-1
		if last && seg == "" && i > 0 {
			break // chunk ended with "\n"
		}
		// A bare-"\r" terminator only means "overwrite" on the final
		// segment; "\r\n" is an ordinary line end.
		progress := last && strings.HasSuffix(seg, "\r")
		seg = strings.TrimRight(seg, "\r")
		// A "\r" inside the segment is a redraw: only the text after the
		// last one is visible.
		if k := strings.LastIndexByte(seg, '\r'); k >= 0 {
			seg = seg[k+1:]
		}
		b.push(cleanLine(seg), progress)
	}
}

func (b *lineBuffer) push(line string, progress bool) {
	if b.overwrite && len(b.lines) > 0 {
		b.lines = b.lines[:len(b.lines)-1]
	}
	b.overwrite = progress
	// Progress redraws are not observed: nothing worth extracting (errors,
	// version banners) is ever printed as one.
	if !progress && line != "" && b.observe != nil {
		b.observe(line)
	}
	b.lines = append(b.lines, line)
	if len(b.lines) >= 2*maxLines {
		n := len(b.lines) - maxLines
		b.dropped += n
		b.lines = append([]string(nil), b.lines[n:]...)
	}
}

// tail returns the retained lines, at most maxLines of them.
func (b *lineBuffer) tail() (lines []string, dropped int) {
	if len(b.lines) > maxLines {
		n := len(b.lines) - maxLines
		return b.lines[n:], b.dropped + n
	}
	return b.lines, b.dropped
}

// errorCollector keeps the first error-looking lines of a stream. The first
// compiler error is the one that matters; later ones are usually fallout.
type errorCollector struct {
	lines []string
	seen  map[string]bool
	more  int
}

func (e *errorCollector) observe(line string) {
	if !isErrorLine(line) {
		return
	}
	if e.seen == nil {
		e.seen = map[string]bool{}
	}
	if e.seen[line] {
		return
	}
	e.seen[line] = true
	if len(e.lines) >= maxErrorLines {
		e.more++
		return
	}
	e.lines = append(e.lines, line)
}

// errorMarkers are substrings (lower-cased) that mark a line as an error in
// esphome CLI, PlatformIO, GCC, linker or esptool output.
var errorMarkers = []string{
	"error:", "[failed]", "*** [", "failed config", "overflowed",
	"will not fit in region", "is greater than maximum allowed",
	"undefined reference to", "fatal error",
}

func isErrorLine(line string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "ERROR ") || strings.HasPrefix(trimmed, "CRITICAL ") {
		return true
	}
	lower := strings.ToLower(trimmed)
	for _, m := range errorMarkers {
		if strings.Contains(lower, m) {
			return true
		}
	}
	return false
}

// logLevelPrefixes are the esphome CLI's own log-line prefixes. On a
// successful validate everything else is the resolved-config dump.
var logLevelPrefixes = []string{"INFO ", "WARNING ", "ERROR ", "CRITICAL "}

func isCLILogLine(line string) bool {
	for _, p := range logLevelPrefixes {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	return false
}

// tailLines returns at most n trailing lines, and whether any were cut.
func tailLines(lines []string, n int) ([]string, bool) {
	if len(lines) <= n {
		return lines, false
	}
	return lines[len(lines)-n:], true
}

// capBytes trims lines from the front until their joined size fits within
// budget bytes, so a handful of very long lines cannot blow up a result.
func capBytes(lines []string, budget int) ([]string, bool) {
	total := 0
	for i := len(lines) - 1; i >= 0; i-- {
		total += len(lines[i]) + 1
		if total > budget {
			return lines[i+1:], true
		}
	}
	return lines, false
}
