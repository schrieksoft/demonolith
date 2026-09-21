package cli

// Semantic color roles, mapped onto the terminal's OWN 16-color palette so
// they follow the user's theme on any background - never RGB values, never
// background colors:
//
//	heading   section titles                       bold
//	prompt    questions awaiting user input        bold cyan
//	emphasis  the name of the object a line is     cyan
//	          about (a module, root, receiver)
//	dim       secondary / parenthetical detail     faint
//	success   a passing verdict or completed push  green
//	warn      handled but notable (skips, holds)   yellow
//	fail      a failing verdict or refusal         red
//
// The house rule: any line that introduces an indented list is a heading;
// outcome words (moved, pushed, skipped, zero changes, FAILED) use the status
// roles; ordinary informational sentences stay plain. emphasis marks exactly
// one thing per line - the name of the object the line is about (a module,
// root, or receiver), whether as the left column of a listing or the name:
// prefix of a progress line. Paths, addresses and other detail stay plain or
// dim; a padded name is padded first, colored second (ANSI bytes count
// against printf widths).
//
// colorEnabled gates ANSI output. On unless --no-color is passed, which is the
// only thing that turns it off: every other demonolith setting is a flag, and
// an environment variable that silently changes the output would be the one
// exception.
//
// Not gated on stdout being a terminal: the report is as often read through a
// pipe - a CI log, a job log rendered as HTML - as on one.
var colorEnabled = true

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
