package view

import (
	"fmt"
	"math"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

// FmtSize renders a size in bytes in decimal units with one decimal:
// "512 B", "1.2 kB", "340.5 MB", "5.8 GB". A negative size (unknown,
// from a failed size read) is the word "unknown".
func FmtSize(n int64) string {
	if n < 0 {
		return "unknown"
	}
	if n < 1000 {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n)
	i := -1
	for i < len(sizeUnits)-1 && v >= 1000 {
		v /= 1000
		i++
	}
	// One decimal, rounded up at .95: 999950 B is 1.0 MB, not
	// "1000.0 kB".
	v = math.Round(v*10) / 10
	if v >= 1000 && i < len(sizeUnits)-1 {
		v, i = 1, i+1
	}
	return fmt.Sprintf("%.1f %s", v, sizeUnits[i])
}

var sizeUnits = []string{"kB", "MB", "GB", "TB", "PB", "EB"}

// ansiSeq is one ANSI control sequence (CSI): ESC [ parameters final.
var ansiSeq = regexp.MustCompile("\x1b\\[[0-9;?]*[ -/]*[@-~]")

// CleanLogText removes every control character except tab from one log
// line: whole ANSI escape sequences, the bell, carriage returns, DEL and
// the rest of C0, and the C1 controls. C1 bytes that are invalid UTF-8
// arrive from the byte stream as replacement runes, which are removed
// the same way. The dashboard and the MCP server render logs as plain
// text, never as a terminal.
func CleanLogText(s string) string {
	s = ansiSeq.ReplaceAllString(s, "")
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r < 0x20 && r != '\t') || r == 0x7f || (r >= 0x80 && r <= 0x9f) || r == utf8.RuneError {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// FmtDur renders a duration rounded down to minutes: <1m, 23m, 1h, 1h23m
// below two days, 2d and 6d22h from two days up.
func FmtDur(d time.Duration) string {
	if d < time.Minute {
		return "<1m"
	}
	mins := int(d / time.Minute)
	if mins < 60 {
		return fmt.Sprintf("%dm", mins)
	}
	if mins >= 48*60 {
		if rem := (mins % (24 * 60)) / 60; rem > 0 {
			return fmt.Sprintf("%dd%dh", mins/(24*60), rem)
		}
		return fmt.Sprintf("%dd", mins/(24*60))
	}
	if rem := mins % 60; rem > 0 {
		return fmt.Sprintf("%dh%dm", mins/60, rem)
	}
	return fmt.Sprintf("%dh", mins/60)
}
