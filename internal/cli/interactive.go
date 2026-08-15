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
	outf("%s", label)
	s, err := stdin.ReadString('\n')
	if err != nil {
		return "", fmt.Errorf("read input: %w", err)
	}
	return strings.TrimSpace(s), nil
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

// runRefactorInteractive is the guided refactor loop: analysis summary,
// catchall triage, decorator write-back, re-analyze, then a confirmed emit.
// Every accepted assignment becomes an @demono:move decorator in the source,
// so the session leaves a state a plain non-interactive run reproduces.
func runRefactorInteractive(rootDir string, f refactorFlags) error {
	if !stdinIsTTY() {
		return fmt.Errorf("--interactive requires a terminal")
	}
	for {
		a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: f.remainder})
		if err != nil {
			return err
		}
		reportAnalysis(a)

		assignments := map[string][]string{}
		if len(a.Placement.Catchall) > 0 {
			triage, err := promptYesNo(fmt.Sprintf("\nTriage the %d unannotated block(s)?", len(a.Placement.Catchall)), true)
			if err != nil {
				return err
			}
			if triage {
				for _, addr := range a.Placement.Catchall {
					isData := strings.HasPrefix(addr.String(), "data.")
					hint := "module name, Enter = keep in " + f.remainder
					if isData {
						hint += ", several comma-separated to duplicate"
					}
					s, err := promptLine(fmt.Sprintf("  %s (%s): ", addr, hint))
					if err != nil {
						return err
					}
					if s == "" {
						continue
					}
					targets := splitTargets(s)
					if !isData && len(targets) > 1 {
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
				return err
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
					return fmt.Errorf("cannot locate block %s in the source", addr)
				}
				outf("  %s:%d  # @demono:move %s\n", displayPath(rootDir, pos.file), pos.line, strings.Join(assignments[addr], " "))
			}
			write, err := promptYesNo("Write these decorators into the source?", false)
			if err != nil {
				return err
			}
			if !write {
				outln("Discarded; nothing written.")
				continue
			}
			if err := insertDecorators(positions, assignments); err != nil {
				return err
			}
			outln("Decorators written; re-analyzing.")
			continue
		}

		outDir := f.out
		if outDir == "" {
			outDir = filepath.Join(rootDir, ".demono", "modules")
		}
		emitOK, err := promptYesNo(fmt.Sprintf("\nEmit %d module root(s) to %s and write the manifest?", len(a.Placement.ModuleNames()), displayPath(rootDir, outDir)), true)
		if err != nil {
			return err
		}
		if !emitOK {
			outln("Aborted; source decorators kept, nothing else written.")
			return nil
		}
		ems, m, path, err := emitAndWriteManifest(a, rootDir, outDir)
		if err != nil {
			return err
		}
		outln("\nEmitted roots:")
		for _, em := range ems {
			outf("  %-16s %s (%d files)\n", em.Module, displayPath(rootDir, em.Dir), len(em.Files))
		}
		outf("\nManifest: %s (%d state moves, %d cross edges)\n", displayPath(rootDir, path), len(m.StateMoves), len(m.CrossEdges))
		return nil
	}
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

// confirmMigrate previews the pending manifests' move plans and asks for one
// whole-run confirmation.
func confirmMigrate(rootDir string, paths []string, f migrateFlags) (bool, error) {
	outln("Manifests to execute, in date order:")
	total := 0
	for _, path := range paths {
		name := filepath.Base(path)
		m, err := manifest.Load(path)
		if err != nil {
			return false, err
		}
		receipt, err := manifest.LatestReceiptFor(rootDir, name)
		if err != nil {
			return false, err
		}
		status := fmt.Sprintf("%d move(s)", len(m.StateMoves))
		if receipt != nil && receipt.Complete {
			status = "already applied; will skip"
		}
		outf("  %s — %s\n", name, status)
		if receipt == nil || !receipt.Complete {
			for _, mv := range m.StateMoves {
				outf("    state mv %-40s -> %s\n", mv.Address, mv.Module)
			}
			total += len(m.StateMoves)
		}
	}
	if f.dryRun {
		return true, nil
	}
	engine := f.engine
	if engine == "" {
		engine = f.execPath
	}
	return promptYesNo(fmt.Sprintf("\nExecute %d move(s) with %s (local copies only; a backup is written first)?", total, engine), false)
}
