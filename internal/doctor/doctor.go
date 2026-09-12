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
//
// There are four rather than three because "could not be checked" is not a
// warning about the setup, and the report said it was: a row whose facts were
// never gathered - the agent did not answer, --skip-agent was given, an
// earlier row failed - was a Warn carrying "not checked: …" in its detail, so
// one mark stood for both "your setup works but look at this" and "I have no
// idea". A developer cannot act on the first without being able to tell it
// from the second, and the same finding (LocalOverlaps') reached them under
// two marks and two nouns.
//
// ⚠ is `tetherd run`'s mark for a warning, which is what makes the one
// finding both commands really do print - the overlap between the captured
// set and this machine's own addresses - read the same in both. That is the
// whole of the claim: it is not a promise that a fact the two commands
// share gets the same mark. The task role row answers with ✗ or ? where
// run's iam line prints ⚠ for the same two facts, deliberately, because run
// has already decided to start the child and is reporting what it found,
// while doctor is being asked whether the setup works at all.
//
// Only Fail is counted by Render, so an Unknown row never fails the command
// - which is the other half of the separation: a row that says nothing must
// not decide the exit code.
type Status int

const (
	// OK means nothing to do.
	OK Status = iota
	// Warn means tetherd works but something deserves attention.
	Warn
	// Fail means tetherd will not work until it is fixed.
	Fail
	// Unknown means the check could not be run at all, so this row says
	// nothing about the setup either way. It is never a reason to fail:
	// whatever stopped the check has its own row, and counting this one
	// again would fail a report over a single missing session.
	Unknown
)

func (s Status) mark() string {
	switch s {
	case OK:
		return "✓"
	case Warn:
		// ⚠, not !, because `tetherd run` already prints its warnings as ⚠
		// and the two commands describe one system.
		return "⚠"
	case Unknown:
		return "?"
	default:
		// Fail, and any status a later hand adds without a mark: a row
		// nobody has decided about reads as a problem rather than as
		// health.
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
//
// Only Fail is counted. A Warn is something to look at rather than a reason
// to fail a script, and an Unknown was never checked at all - failing on it
// would fail the command for a row that made no claim, on top of the row that
// actually broke and is counted here already.
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
