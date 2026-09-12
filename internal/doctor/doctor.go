// Package doctor checks that a laptop and a dev service are set up for
// tetherd, and says what to do about whatever is not (spec §6.5). Every
// decision here is a pure function of facts the caller gathered, so the
// judgements are unit-tested without a helper, an ECS service or root: this
// package performs no I/O at all.
package doctor

import (
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Status is how a check came out.
type Status int

const (
	// OK means nothing to do.
	OK Status = iota
	// Warn means tetherd works but something deserves attention.
	Warn
	// Fail means tetherd will not work until it is fixed.
	Fail
)

func (s Status) mark() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		return "!"
	default:
		return "✗"
	}
}

// Result is one check's outcome.
type Result struct {
	// Name is the left column's heading; Render aligns the rows on it.
	Name   string
	Status Status
	// Detail is what the check found, in one line.
	Detail string
	// Next is what to do about it, empty when the check passed.
	Next string
}

// Render prints the results and returns how many failed, so the caller can
// exit non-zero. Next steps print only where there is something to do, so a
// healthy machine is quiet. Every result is printed: a doctor that stopped at
// the first problem would make the developer run it once per problem.
func Render(w io.Writer, results []Result) int {
	// Names are padded by rune count, not bytes, so the detail column lines up
	// in display columns however wide the names are.
	width := 0
	for _, r := range results {
		if n := utf8.RuneCountInString(r.Name); n > width {
			width = n
		}
	}
	failed := 0
	for _, r := range results {
		if r.Status == Fail {
			failed++
		}
		line(w, fmt.Sprintf("%s %s  %s", r.Status.mark(), pad(r.Name, width), r.Detail))
		if r.Status != OK && r.Next != "" {
			// The arrow sits under the detail column: the mark is one display
			// column wide, so "mark + space" and the two leading spaces here
			// are the same width.
			line(w, fmt.Sprintf("  %s  → %s", pad("", width), r.Next))
		}
	}
	return failed
}

// line writes one row without trailing blanks.
func line(w io.Writer, s string) {
	fmt.Fprintln(w, strings.TrimRight(s, " "))
}

// pad right-pads s to width display columns.
func pad(s string, width int) string {
	if n := width - utf8.RuneCountInString(s); n > 0 {
		return s + strings.Repeat(" ", n)
	}
	return s
}
