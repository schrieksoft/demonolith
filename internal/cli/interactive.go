package cli

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/schrieksoft/demonolith/internal/decorator"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

var stdin = bufio.NewReader(os.Stdin)

func promptLine(label string) (string, error) {
	outf("%s", prompt(label))
	s, err := stdin.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(s), nil
}

// promptString asks for a value, showing the default; Enter keeps it.
func promptString(label, def string) (string, error) {
	s, err := promptLine(fmt.Sprintf("%s [%s]: ", label, def))
	if err != nil {
		return "", err
	}
	if s == "" {
		return def, nil
	}
	return s, nil
}

// promptYesNo asks a y/n question; empty input takes the default.
func promptYesNo(label string, def bool) (bool, error) {
	suffix := " [y/N] "
	if def {
		suffix = " [Y/n] "
	}
	s, err := promptLine(label + suffix)
	if err != nil {
		return false, err
	}
	switch strings.ToLower(s) {
	case "":
		return def, nil
	case "y", "yes":
		return true, nil
	case "n", "no":
		return false, nil
	}
	return false, fmt.Errorf("unrecognized answer %q", s)
}

// runRefactorMapInteractive is the guided plan loop: the run's parameters
// (root, output dir, remainder name, monorepo, bootstrap), then analysis
// summary, catchall triage, decorator write-back, re-analyze, and a confirmed
// manifest write. Every accepted assignment becomes an @demono:move decorator
// in the source, so the session leaves a state a plain non-interactive run
// reproduces.
func runRefactorMapInteractive(f refactorFlags) error {
	_, err := refactorMapInteractive(f)
	return err
}

// refactorMapInteractive returns the resolved flags so the pipeline can
// continue with run/verify after an interactive plan.
func refactorMapInteractive(f refactorFlags) (refactorFlags, error) {
	if !stdinIsTTY() {
		return f, fmt.Errorf("--interactive requires a terminal")
	}
	outln("Interactive refactor map — Enter keeps the value in brackets.")

	rootIn, err := promptString("Monolith root", f.rootDir)
	if err != nil {
		return f, err
	}
	f.rootDir = rootIn
	rootDir := resolveRoot(rootIn)
	if _, err := os.Stat(rootDir); err != nil {
		return f, fmt.Errorf("root %s: %w", rootDir, err)
	}

	defOut := f.out
	if defOut == "" {
		defOut = "modules"
	}
	for {
		outIn, err := promptString("Output directory inside the root", defOut)
		if err != nil {
			return f, err
		}
		if _, err := resolveOut(rootDir, outIn); err != nil {
			outf("  %v\n", err)
			continue
		}
		f.out = outIn
		break
	}

	remainder, err := promptString("Catchall module name for unannotated blocks", f.remainder)
	if err != nil {
		return f, err
	}
	f.remainder = remainder

	monorepo, err := promptYesNo("Link in-repo child modules by relative path instead of copying them (monorepo layout)?", f.monorepo)
	if err != nil {
		return f, err
	}
	f.monorepo = monorepo

	withBootstrap, err := promptYesNo("Generate the Snap CD bootstrap module (<out>/snapcd)?", !f.noBootstrap)
	if err != nil {
		return f, err
	}
	f.noBootstrap = !withBootstrap

	for {
		a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: f.remainder})
		if err != nil {
			return f, err
		}
		reportAnalysis(a)

		assignments := map[string][]string{}
		if len(a.Placement.Catchall) > 0 {
			outf("\nThe %d block(s) above carry no @demono:move decorator; unless assigned, they stay in the catchall module %q.\n", len(a.Placement.Catchall), f.remainder)
			triage, err := promptYesNo(fmt.Sprintf("\nAssign them to modules now? (choices are written into the source as @demono:move decorators; \"n\" keeps them in %q)", f.remainder), false)
			if err != nil {
				return f, err
			}
			if triage {
				for _, addr := range a.Placement.Catchall {
					if strings.HasPrefix(addr.String(), "data.") {
						// Data sources are never decorated: they follow their
						// consumers. One in the catchall has no placed consumer.
						outf("  %s: data source with no placed consumer; stays in %s (it will follow wherever a consumer is placed)\n", addr, f.remainder)
						continue
					}
					s, err := promptLine(fmt.Sprintf("  %s (module name, Enter = keep in %s): ", addr, f.remainder))
					if err != nil {
						return f, err
					}
					if s == "" {
						continue
					}
					targets := splitTargets(s)
					if len(targets) > 1 {
						outf("    %s is stateful and takes exactly one target; keeping it in %s\n", addr, f.remainder)
						continue
					}
					assignments[addr.String()] = targets
				}
			}
		}

		if len(assignments) > 0 {
			positions, err := blockPositions(rootDir)
			if err != nil {
				return f, err
			}
			outln("\nDecorators to write:")
			addrs := make([]string, 0, len(assignments))
			for addr := range assignments {
				addrs = append(addrs, addr)
			}
			sort.Strings(addrs)
			for _, addr := range addrs {
				pos, ok := positions[addr]
				if !ok {
					return f, fmt.Errorf("cannot locate block %s in the source", addr)
				}
				outf("  %s:%d  # @demono:move %s\n", displayPath(rootDir, pos.file), pos.line, strings.Join(assignments[addr], " "))
			}
			write, err := promptYesNo("\nWrite these decorators into the source?", false)
			if err != nil {
				return f, err
			}
			if !write {
				outln("Discarded; nothing written.")
				continue
			}
			if err := insertDecorators(positions, assignments); err != nil {
				return f, err
			}
			outln("Decorators written; re-analyzing.")
			continue
		}

		outf("\nNext step: write the manifest (%s) recording this map of %d module root(s) — the reviewable plan that `refactor run` executes.\n", manifest.FileName, len(a.Placement.ModuleNames()))
		writeOK, err := promptYesNo("\nWrite it? (\"n\" aborts; decorators already written stay in the source)", true)
		if err != nil {
			return f, err
		}
		if !writeOK {
			outln("Aborted; source decorators kept, nothing else written.")
			return f, errInteractiveAborted
		}
		if _, err := runRefactorMap(f, outputText); err != nil {
			return f, err
		}
		return f, nil
	}
}

// errInteractiveAborted marks a user abort inside a guided walkthrough.
var errInteractiveAborted = fmt.Errorf("aborted")

// runRefactorInteractivePipeline is the bare `refactor -i`: interactive plan,
// then confirmed run and a quiet verify.
func runRefactorInteractivePipeline(f refactorFlags) error {
	resolved, err := refactorMapInteractive(f)
	if err != nil {
		if err == errInteractiveAborted {
			return nil
		}
		return err
	}
	rootDir := resolveRoot(resolved.rootDir)
	runOK, err := promptYesNo("\nRun the refactor now?", true)
	if err != nil {
		return err
	}
	if !runOK {
		outln("Map written; run later with `demonolith refactor run`.")
		return nil
	}
	outf("\n%s\n\n", banner("── refactor run ──"))
	if err := runRefactorRun(rootDir, outputText); err != nil {
		return err
	}
	outf("\n%s\n\n", banner("── refactor verify ──"))
	vf := resolved
	vf.quiet = true
	return runRefactorVerify(rootDir, outputText, vf)
}

func splitTargets(s string) []string {
	var out []string
	for _, part := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' }) {
		if part != "" {
			out = append(out, part)
		}
	}
	return out
}

type blockPos struct {
	file string
	line int
}

// blockPositions maps every decoratable block's address to its source position.
func blockPositions(rootDir string) (map[string]blockPos, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}
	out := map[string]blockPos{}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".tf" {
			continue
		}
		path := filepath.Join(rootDir, e.Name())
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		bds, err := decorator.Scan(path, src)
		if err != nil {
			return nil, err
		}
		for _, bd := range bds {
			out[bd.Addr] = blockPos{file: path, line: bd.DefRange.Start.Line}
		}
	}
	return out, nil
}

// insertDecorators writes "# @demono:move <targets>" immediately above each
// assigned block, applying edits bottom-up per file so line numbers stay valid.
func insertDecorators(positions map[string]blockPos, assignments map[string][]string) error {
	type edit struct {
		line    int
		comment string
	}
	byFile := map[string][]edit{}
	for addr, targets := range assignments {
		pos := positions[addr]
		byFile[pos.file] = append(byFile[pos.file], edit{line: pos.line, comment: "# @demono:move " + strings.Join(targets, " ")})
	}
	for file, edits := range byFile {
		b, err := os.ReadFile(file)
		if err != nil {
			return err
		}
		lines := strings.Split(string(b), "\n")
		sort.Slice(edits, func(i, j int) bool { return edits[i].line > edits[j].line })
		for _, ed := range edits {
			at := ed.line - 1
			if at < 0 || at > len(lines) {
				return fmt.Errorf("line %d out of range in %s", ed.line, file)
			}
			lines = append(lines[:at], append([]string{ed.comment}, lines[at:]...)...)
		}
		if err := os.WriteFile(file, []byte(strings.Join(lines, "\n")), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// promptEngine asks for the engine when --engine was omitted interactively.
func promptEngine() (string, error) {
	for {
		s, err := promptLine("Engine (terraform/tofu): ")
		if err != nil {
			return "", err
		}
		if s == "terraform" || s == "tofu" {
			return s, nil
		}
		outln("  answer terraform or tofu")
	}
}

// confirmMigrateMap previews the manifest's move plan and asks for one
// whole-run confirmation.
func confirmMigrateMap(rootDir string, m *manifest.Manifest, f migrateFlags) (bool, error) {
	receipt, err := manifest.LatestReceiptFor(rootDir, m.EmitChecksum, manifest.ActionMap)
	if err != nil {
		return false, err
	}
	status := fmt.Sprintf("%d move(s)", len(m.StateMoves))
	if receipt != nil && receipt.Complete {
		status = "already carved; will skip"
	}
	outf("\n%s %s\n", heading("State moves to carve")+" ("+manifest.FileName+"):", dim(status))
	total := 0
	if receipt == nil || !receipt.Complete {
		for _, mv := range m.StateMoves {
			outf("  state mv %-40s -> %s\n", mv.Address, mv.Module)
		}
		total = len(m.StateMoves)
	}
	engine := f.engine
	if engine == "" {
		engine = f.execPath
	}
	return promptYesNo(fmt.Sprintf("\nCarve %d move(s) with %s (local copies only; a backup is written first)?", total, engine), true)
}
