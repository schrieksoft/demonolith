package cli

import (
	"os"

	"golang.org/x/term"
)

// Semantic color roles, mapped onto the terminal's OWN 16-color palette so
// they follow the user's theme on any background — never RGB values, never
// background colors:
//
//	heading   section titles                       bold
//	prompt    questions awaiting user input        bold cyan
//	emphasis  key values inside information text   cyan
//	dim       secondary / parenthetical detail     faint
//	success   a passing verdict or completed push  green
//	warn      handled but notable (skips, holds)   yellow
//	fail      a failing verdict or refusal         red
//
// The house rule: any line that introduces an indented list is a heading;
// outcome words (moved, pushed, skipped, zero changes, FAILED) use the status
// roles; ordinary informational sentences stay plain.
//
// colorEnabled gates ANSI output: a real terminal on stdout, NO_COLOR unset,
// and a capable TERM. Computed once at startup.
var colorEnabled = func() bool {
	if os.Getenv("NO_COLOR") != "" || os.Getenv("TERM") == "dumb" {
		return false
	}
	return term.IsTerminal(int(os.Stdout.Fd()))
}()

func colorize(code, s string) string {
	if !colorEnabled {
		return s
	}
	return "\033[" + code + "m" + s + "\033[0m"
}

func heading(s string) string  { return colorize("1", s) }
func prompt(s string) string   { return colorize("1;36", s) }
func emphasis(s string) string { return colorize("36", s) }
func dim(s string) string      { return colorize("2", s) }
func success(s string) string  { return colorize("32", s) }
func warn(s string) string     { return colorize("33", s) }
func fail(s string) string     { return colorize("31", s) }

// colorVerdict colors a per-module proof verdict line: zero changes success,
// anything else fail.
func colorVerdict(v string) string {
	if v == "zero changes" {
		return success(v)
	}
	return fail(v)
}

// banner styles the pipeline step separators ("── migrate map ──").
func banner(s string) string { return colorize("1;35", s) }
