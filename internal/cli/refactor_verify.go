package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/bootstrap"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

func refactorVerifyCmd() *cobra.Command {
	var f refactorFlags
	cmd := &cobra.Command{
		Use:   "verify",
		Short: "Gate: do the emitted roots and the map still match the source?",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutput(f.output)
			if err != nil {
				return err
			}
			return runRefactorVerify(resolveRoot(f.rootDir), mode, f)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	flags.BoolVarP(&f.quiet, "quiet", "q", false, "verdict only; skip the per-module placement listing")
	flags.BoolVar(&f.silent, "silent", false, "no output at all; the result is the exit code")
	flags.BoolVar(&f.validate, "validate", false, "also engine-validate each committed root (init -backend=false + validate; credential-free, needs --engine)")
	flags.StringVar(&f.engine, "engine", "", "engine for --validate: terraform or tofu")
	flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	return cmd
}

// verifyReport is the machine-facing result of refactor verify.
type verifyReport struct {
	Differs        bool     `json:"differs"`
	Manifest       string   `json:"map,omitempty"`
	ChangedFiles   []string `json:"changed_files,omitempty"`
	DiffDetail     []string `json:"-"`
	Reasons        []string `json:"reasons,omitempty"`
	InvalidModules []string `json:"invalid_modules,omitempty"`
}

// runRefactorVerify re-runs analysis+emit into a temp dir and compares against
// the committed roots and manifest. The remainder-module name comes from the
// committed manifest, so no placement flag can skew the comparison. Nothing is
// written to the root or output dirs; any mismatch is a negative verdict.
func runRefactorVerify(rootDir string, mode outputMode, f refactorFlags) error {
	rep := verifyReport{}
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "no "+manifest.FileName+" found; run `demonolith refactor map` first")
		return reportRefactorVerify(rep, nil, mode, f)
	}
	rep.Manifest = manifest.FileName
	committed, err := manifest.Load(path)
	if err != nil {
		return err
	}
	if !committed.IsRun() {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "the map has not been run yet; run `demonolith refactor run` and commit its output")
		return reportRefactorVerify(rep, committed, mode, f)
	}

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: committed.Source.RemainderModule})
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "demono-verify-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	var block *emit.BackendBlock
	if committed.Backend != nil {
		block, err = emit.ParseBackend(rootDir)
		if err != nil {
			return err
		}
	}
	e := &emit.Emitter{SrcDir: rootDir, OutDir: tmp, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary, Monorepo: committed.Output.Monorepo, Backend: block, PathBase: committed.OutDir(rootDir)}
	ems, err := e.Emit()
	if err != nil {
		return err
	}
	freshDirs := map[string]string{}
	for _, em := range ems {
		freshDirs[em.Module] = em.Dir
	}
	fresh, err := freshSemantic(a, rootDir, committed)
	if err != nil {
		return err
	}
	if committed.Output.BootstrapDir != "" {
		bsDir, err := bootstrap.Emit(committed, rootDir, tmp)
		if err != nil {
			return err
		}
		freshDirs["snapcd-bootstrap"] = bsDir
	}

	committedHashes, err := manifest.FileHashes(committed.ChecksumDirs(rootDir))
	if err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("roots unreadable: %v", err))
		return reportRefactorVerify(rep, committed, mode, f)
	}
	freshHashes, err := manifest.FileHashes(freshDirs)
	if err != nil {
		return err
	}

	rep.ChangedFiles = diffHashes(freshHashes, committedHashes)
	if len(rep.ChangedFiles) > 0 {
		rep.DiffDetail = diffPreview(rep.ChangedFiles, committed.ChecksumDirs(rootDir), freshDirs)
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "the roots on disk differ from what the source produces")
	}
	if committed.EmitChecksum != manifest.ChecksumOf(committedHashes) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "the roots on disk were edited after the map was written (emit_checksum mismatch)")
	}
	if !manifest.SemanticEqual(fresh, committed) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "the map differs from what the source produces")
	}

	if f.validate && !rep.Differs {
		invalid, err := validateRoots(committed, rootDir, f)
		if err != nil {
			return err
		}
		if len(invalid) > 0 {
			rep.Differs = true
			rep.InvalidModules = invalid
			rep.Reasons = append(rep.Reasons, "engine validation failed for: "+joinComma(invalid))
		}
	}
	return reportRefactorVerify(rep, committed, mode, f)
}

// validateRoots runs `init -backend=false` + `validate` on each committed root
// (bootstrap included): engine-grade validity, still credential-free.
func validateRoots(m *manifest.Manifest, rootDir string, f refactorFlags) ([]string, error) {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return nil, fmt.Errorf("--validate needs an engine: %w", err)
	}
	ctx := context.Background()
	dirs := m.ChecksumDirs(rootDir)
	names := make([]string, 0, len(dirs))
	for name := range dirs {
		names = append(names, name)
	}
	sort.Strings(names)
	var invalid []string
	for _, name := range names {
		tf, err := tfexec.NewTerraform(dirs[name], execPath)
		if err != nil {
			return nil, err
		}
		if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
			return nil, fmt.Errorf("init %s: %w", name, err)
		}
		out, err := tf.Validate(ctx)
		if err != nil {
			return nil, fmt.Errorf("validate %s: %w", name, err)
		}
		if !out.Valid {
			invalid = append(invalid, name)
			for _, d := range out.Diagnostics {
				outf("  %s: %s: %s\n", name, d.Severity, d.Summary)
			}
		}
	}
	return invalid, nil
}

func joinComma(items []string) string {
	out := ""
	for i, it := range items {
		if i > 0 {
			out += ", "
		}
		out += it
	}
	return out
}

// diffPreview shows, per differing file (capped at three), the first line
// where the content on disk and the freshly produced content diverge.
func diffPreview(files []string, onDisk, produced map[string]string) []string {
	var out []string
	shown := 0
	for _, f := range files {
		if strings.Contains(f, " (") {
			continue // added/removed files carry their explanation already
		}
		if shown == 3 {
			out = append(out, "(further diffs omitted)")
			break
		}
		name, rel, ok := strings.Cut(f, "/")
		if !ok {
			continue
		}
		line, have, want := firstLineDiff(filepath.Join(onDisk[name], filepath.FromSlash(rel)), filepath.Join(produced[name], filepath.FromSlash(rel)))
		if line == 0 {
			continue
		}
		out = append(out,
			fmt.Sprintf("%s, first difference at line %d:", f, line),
			"  on disk:       "+have,
			"  source yields: "+want)
		shown++
	}
	return out
}

// firstLineDiff returns the first differing line number (1-based) and the two
// versions of that line; 0 when the files are unreadable or identical.
func firstLineDiff(onDiskPath, producedPath string) (int, string, string) {
	a, errA := os.ReadFile(onDiskPath)
	b, errB := os.ReadFile(producedPath)
	if errA != nil || errB != nil {
		return 0, "", ""
	}
	al := strings.Split(string(a), "\n")
	bl := strings.Split(string(b), "\n")
	n := len(al)
	if len(bl) < n {
		n = len(bl)
	}
	render := func(l string) string {
		if strings.TrimSpace(l) == "" {
			return "<empty line>"
		}
		return strings.TrimSpace(l)
	}
	for i := 0; i < n; i++ {
		if al[i] != bl[i] {
			return i + 1, render(al[i]), render(bl[i])
		}
	}
	if len(al) != len(bl) {
		if len(al) > n {
			return n + 1, strings.TrimSpace(al[n]), "<end of file>"
		}
		return n + 1, "<end of file>", strings.TrimSpace(bl[n])
	}
	return 0, "", ""
}

// diffHashes lists "<module>/<relpath>" entries that differ between the fresh
// and committed trees, tagging additions and removals.
func diffHashes(fresh, committed map[string]string) []string {
	var out []string
	for k, fv := range fresh {
		cv, ok := committed[k]
		if !ok {
			out = append(out, k+" (missing from committed roots)")
		} else if cv != fv {
			out = append(out, k)
		}
	}
	for k := range committed {
		if _, ok := fresh[k]; !ok {
			out = append(out, k+" (not produced by source)")
		}
	}
	sort.Strings(out)
	return out
}

func reportRefactorVerify(rep verifyReport, m *manifest.Manifest, mode outputMode, f refactorFlags) error {
	if f.silent {
		// Exit code only: an empty-message verdict that main does not print.
		if rep.Differs {
			return &VerdictError{}
		}
		return nil
	}
	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else {
		if !f.quiet && m != nil {
			printPlacement(m)
		}
		if rep.Differs {
			outln(fail("Out of sync") + " — the output on disk does not match what the source produces:")
			for _, r := range rep.Reasons {
				outf("  - %s\n", r)
			}
			for _, fl := range rep.ChangedFiles {
				outf("    %s\n", fl)
			}
			for _, d := range rep.DiffDetail {
				outf("  %s\n", d)
			}
		} else {
			outf("%s: the roots and map %s match the source.\n", success("In sync"), rep.Manifest)
		}
	}
	if rep.Differs {
		return verdictf("the output on disk does not match what the source produces; re-run `demonolith refactor` to regenerate it (and commit the result if the roots are tracked)")
	}
	return nil
}

// printPlacement lists what the committed manifest claims — every block and
// the root it was carved into — so an in-sync verdict shows what was actually
// confirmed.
func printPlacement(m *manifest.Manifest) {
	outln("Map under comparison:")
	names := make([]string, 0, len(m.Modules))
	for name := range m.Modules {
		names = append(names, name)
	}
	sort.Strings(names)
	blocks := 0
	for _, name := range names {
		mod := m.Modules[name]
		outf("  %-16s %s\n", name, mod.Dir)
		for _, b := range mod.Blocks {
			outf("    %s\n", b)
			blocks++
		}
	}
	outf("  (%d modules, %d blocks, %d state moves, %d cross edges)\n\n", len(names), blocks, len(m.StateMoves), len(m.CrossEdges))
}
