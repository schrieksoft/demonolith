package cli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/proof"
	"github.com/schrieksoft/demonolith/internal/transfer"
)

type transferFlags struct {
	rootDir    string
	engine     string
	execPath   string
	snapcdRoot string
	snapcdSet  bool
	yes        bool
	all        bool
}

func transferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Move decorated blocks into pre-existing roots: refactor (the code move), then migrate (the state move) (experimental)",
		Long: `Move decorated blocks (# @demono:move ../some-root) from the source root into
pre-existing receiver roots. Both sides keep living: the source's state and every
receiver's state are rewritten.

Each decorator names its destination as a directory relative to the source
root; the directory must already exist. Two halves, same grammar as the split:
  transfer refactor   map → run → diff       the code move (commit and review this)
  transfer migrate    map → prove → run → verify   the state move (post-merge, once)

Every command acts on the root --root-dir names (default: the current
directory). The code move is authored at the source root; the state move is
per slice — each migrate step touches exactly one root's state, exchanging
fragments, output values, and receipts as files in that root's .demono-transfer.
With --all (migrate and refactor diff, from the source root's sibling layout)
demonolith orchestrates every slice itself.

From committing the code move until the state move completes, source and
receivers plan dirty — freeze their pipelines and keep that window short.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(transferRefactorCmd(), transferMigrateCmd())
	return cmd
}

func transferCommonFlags(cmd *cobra.Command, f *transferFlags, withEngine, withAll bool) {
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the root this command acts on (the source root for whole-transfer commands and --all)")
	flags.StringVar(&f.snapcdRoot, "snapcd-root", "snapcd", "the Snap CD root holding the roots' snapcd_module resources, relative to the source root's parent; when present, the transfer writes the cross-root wiring (snapcd_module_input_from_output, snapcd_depends_on_module) into it")
	if withEngine {
		flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
		flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
	}
	if withAll {
		flags.BoolVar(&f.all, "all", false, "act on every slice of the transfer, from the source root's sibling layout")
	}
	cmd.PreRun = func(cmd *cobra.Command, args []string) {
		f.snapcdSet = cmd.Flags().Changed("snapcd-root")
	}
}

// --- transfer refactor: the code half --------------------------------------

func transferRefactorCmd() *cobra.Command {
	var f transferFlags
	cmd := &cobra.Command{
		Use:   "refactor",
		Short: "The code move: map → run → diff (authored at the source root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			outf("%s\n\n", banner("── transfer refactor map ──"))
			if err := runTransferRefactorMap(ctx, f); err != nil {
				return err
			}
			if err := approvePause(f.yes, "Proceed with the code move?"); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer refactor run ──"))
			if err := runTransferRefactorRun(ctx, f); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer refactor diff ──"))
			fd := f
			fd.all = true
			return runTransferRefactorDiff(ctx, fd)
		},
	}
	transferCommonFlags(cmd, &f, false, false)
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "approve the code move automatically instead of pausing after the map")

	var mf transferFlags
	mapCmd := &cobra.Command{
		Use:   "map",
		Short: "Analyze the source and write the transfer map (pure, offline)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorMap(cmd.Context(), mf) },
	}
	transferCommonFlags(mapCmd, &mf, false, false)

	var rf transferFlags
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the map: append the moved code into each receiver's own files, remove the blocks from the source",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorRun(cmd.Context(), rf) },
	}
	transferCommonFlags(runCmd, &rf, false, false)

	var df transferFlags
	diffCmd := &cobra.Command{
		Use:   "diff",
		Short: "Gate: this root's code matches its map copy (--all: every touched root, from the source)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorDiff(cmd.Context(), df) },
	}
	transferCommonFlags(diffCmd, &df, false, true)

	cmd.AddCommand(mapCmd, runCmd, diffCmd)
	return cmd
}

// --- transfer migrate: the state half ---------------------------------------

func transferMigrateCmd() *cobra.Command {
	var f transferFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "The state move, one slice at a time: map → prove → run → verify (bare: this root; --all: every root)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			pausePrompt := "Proceed with the state move? This root's state is rewritten."
			if f.all {
				pausePrompt = "Proceed with the state move? Every slice's state is rewritten."
			}
			outf("%s\n\n", banner("── transfer migrate map ──"))
			if err := runTransferMigrateMap(ctx, f); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer migrate prove ──"))
			if err := runTransferMigrateProve(ctx, f); err != nil {
				return err
			}
			if err := approvePause(f.yes, pausePrompt); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer migrate run ──"))
			if err := runTransferMigrateRun(ctx, f); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer migrate verify ──"))
			return runTransferMigrateVerify(ctx, f)
		},
	}
	transferCommonFlags(cmd, &f, true, true)
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "approve the state move automatically instead of pausing after the proof")

	mkStep := func(use, short string, fn func(context.Context, transferFlags) error) *cobra.Command {
		var cf transferFlags
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return fn(cmd.Context(), cf) },
		}
		transferCommonFlags(c, &cf, true, true)
		return c
	}
	cmd.AddCommand(
		mkStep("map", "Pull this root's state read-only, back it up, pin it; the source also writes each receiver's fragment, a receiver applies its fragment to the local copy", runTransferMigrateMap),
		mkStep("prove", "Gate: this root plans to zero changes against its moved local copy, producer values from outputs artifacts", runTransferMigrateProve),
		mkStep("run", "Execute this slice's state write (guarded, never forced); the source requires every receiver's run receipt first", runTransferMigrateRun),
		mkStep("verify", "Gate: replan this root against its real backend", runTransferMigrateVerify),
	)
	return cmd
}

// approvePause mirrors the split's pause-before-run: -y approves, no TTY refuses.
func approvePause(yes bool, prompt string) error {
	if yes {
		return nil
	}
	if !stdinIsTTY() {
		return fmt.Errorf("approval required: %s Re-run with -y to approve, or run the steps individually", prompt)
	}
	fmt.Fprintf(os.Stderr, "%s [y/N] ", prompt)
	var answer string
	_, _ = fmt.Fscanln(os.Stdin, &answer)
	if strings.ToLower(strings.TrimSpace(answer)) != "y" {
		return fmt.Errorf("aborted; nothing written")
	}
	return nil
}

// --- shared plumbing --------------------------------------------------------

// loadRunTransferMap loads the map and requires the code half to have run.
func loadRunTransferMap(rootDir string) (*transfer.Map, error) {
	m, err := transfer.LoadMap(rootDir)
	if err != nil {
		return nil, err
	}
	if !m.IsRun() {
		return nil, fmt.Errorf("the transfer map has not been run yet; run `demonolith transfer refactor` first")
	}
	return m, nil
}

// transferSlice is one root's view of the transfer: its map copy, the map
// hash, its role, and its workdir.
type transferSlice struct {
	dir  string
	m    *transfer.Map
	hash string
	role transfer.Role
	work string
}

func loadTransferSlice(rootDir string) (*transferSlice, error) {
	dir, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, err
	}
	m, err := loadRunTransferMap(dir)
	if err != nil {
		return nil, err
	}
	hash, err := transfer.MapHash(dir)
	if err != nil {
		return nil, err
	}
	role, err := transfer.RoleOf(m, dir)
	if err != nil {
		return nil, err
	}
	return &transferSlice{dir: dir, m: m, hash: hash, role: role, work: filepath.Join(dir, transfer.WorkDirName)}, nil
}

// transferAll is the --all view: every slice, anchored on the source root's
// sibling layout, in dependency order.
type transferAll struct {
	src   *transferSlice
	paths map[string]string
	order []string
}

func loadTransferAll(rootDir string) (*transferAll, error) {
	src, err := loadTransferSlice(rootDir)
	if err != nil {
		return nil, err
	}
	if src.role.Kind != "source" {
		return nil, fmt.Errorf("--all runs at the source root (%s); this is %s", src.m.SourceDir, src.role.Base)
	}
	paths, err := transfer.ResolveReceivers(src.m, src.dir)
	if err != nil {
		return nil, err
	}
	modules := []string{src.m.Remainder}
	modules = append(modules, src.m.ReceiverNames()...)
	order, err := proof.TopoOrder(modules, transfer.BoundaryFromMap(src.m))
	if err != nil {
		return nil, err
	}
	return &transferAll{src: src, paths: paths, order: order}, nil
}

// sliceFor builds a receiver's slice view from the source's map — --all needs
// no distributed copies to act, only to gate (refactor diff checks them).
func (a *transferAll) sliceFor(module string) *transferSlice {
	if module == a.src.m.Remainder {
		return a.src
	}
	dir := a.paths[module]
	return &transferSlice{
		dir:  dir,
		m:    a.src.m,
		hash: a.src.hash,
		role: transfer.Role{Kind: "receiver", Key: module, Base: filepath.Base(module)},
		work: filepath.Join(dir, transfer.WorkDirName),
	}
}

// sliceThreadVars assembles this slice's consumer input values from the
// producer outputs artifacts in its workdir.
func sliceThreadVars(s *transferSlice) (map[string]string, error) {
	vars := map[string]string{}
	mod := s.role.Module(s.m)
	for _, e := range s.m.CrossEdges {
		if e.Consumer != mod {
			continue
		}
		pbase := transfer.BaseForModule(s.m, e.Producer)
		oa, err := transfer.LoadOutputs(s.work, pbase)
		if err != nil {
			return nil, fmt.Errorf("input %q needs outputs-%s.yaml in %s — `demonolith transfer migrate prove` at %s writes it; bring it here", e.Input, pbase, transfer.WorkDirName, pbase)
		}
		if oa.MapHash != s.hash {
			return nil, fmt.Errorf("outputs-%s.yaml belongs to a different transfer (map hash mismatch); refresh it from %s", pbase, pbase)
		}
		v, ok := oa.Outputs[e.Output]
		if !ok {
			return nil, fmt.Errorf("outputs-%s.yaml lacks output %q; refresh it with `demonolith transfer migrate prove` at %s", pbase, e.Output, pbase)
		}
		vars[e.Input] = v
	}
	return vars, nil
}

func sliceIsProducer(s *transferSlice) bool {
	mod := s.role.Module(s.m)
	for _, e := range s.m.CrossEdges {
		if e.Producer == mod {
			return true
		}
	}
	return false
}

// loadSliceReadiness loads the slice's pin and ties it to this map generation.
func loadSliceReadiness(s *transferSlice) (*transfer.SlicePin, error) {
	pin, err := transfer.LoadSlicePin(s.dir)
	if err != nil {
		return nil, err
	}
	if pin.MapHash != s.hash {
		return nil, fmt.Errorf("%s in this root belongs to a different map generation; re-run `demonolith transfer migrate map` here", transfer.MigrateMapFile)
	}
	return pin, nil
}

func requireStateSlice(s *transferSlice) error {
	if s.role.Kind == "snapcd" {
		return fmt.Errorf("the Snap CD root carries wiring code, not a state slice; `transfer migrate` runs at the source root and each receiver")
	}
	return nil
}

func nowStamp() string { return time.Now().UTC().Format(time.RFC3339) }

// --- refactor steps ---------------------------------------------------------

func runTransferRefactorMap(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	plan, err := transfer.BuildPlan(rootDir)
	if err != nil {
		return verdictf("%v", err)
	}

	names := make([]string, 0, len(plan.Receivers))
	for n := range plan.Receivers {
		names = append(names, n)
	}
	sort.Strings(names)
	paths := map[string]string{}
	for _, name := range names {
		p, err := transfer.ResolveReceiverPath(rootDir, name)
		if err != nil {
			return verdictf("%v", err)
		}
		paths[name] = p
		pr := plan.Receivers[name]
		if err := transfer.ValidateReceiverDir(p, pr.Structural, pr.Inputs, pr.Outputs); err != nil {
			return verdictf("%v", err)
		}
	}
	srcInputs, srcOutputs := plan.SourceWiring()
	if err := transfer.ValidateSourceWiring(rootDir, srcInputs, srcOutputs); err != nil {
		return verdictf("%v", err)
	}

	m := &transfer.Map{Version: 1, Created: nowStamp(), Tool: toolString(), Remainder: plan.Remainder, SourceDir: filepath.Base(filepath.Clean(rootDir)), Receivers: map[string]transfer.Receiver{}}
	m.CrossEdges, m.OrderingEdges = plan.Edges()
	for _, name := range names {
		pr := plan.Receivers[name]
		m.Receivers[name] = transfer.Receiver{
			Blocks:    pr.Blocks,
			Moves:     pr.Moves,
			Variables: pr.Structural.VarNames,
			Locals:    pr.Structural.LocalNames,
			Inputs:    pr.Inputs,
			Outputs:   pr.Outputs,
		}
	}

	// Snap CD wiring: engaged when the transfer creates edges and a Snap CD
	// root is found (default: a sibling of the source root named "snapcd").
	snapcdNote := ""
	if len(m.CrossEdges)+len(m.OrderingEdges) > 0 {
		snapcdDir, err := transfer.ResolveSnapcdRoot(rootDir, f.snapcdRoot, f.snapcdSet)
		if err != nil {
			return verdictf("%v", err)
		}
		if snapcdDir == "" {
			snapcdNote = "No Snap CD root found (--snapcd-root); the cross-root wiring stays code-level only."
		} else {
			snapBase := filepath.Base(snapcdDir)
			if snapBase == m.SourceDir {
				return verdictf("the Snap CD root shares the directory name %q with the source root; slice roots are told apart by directory basename, so rename one directory first", snapBase)
			}
			for _, name := range names {
				if filepath.Base(name) == snapBase {
					return verdictf("the Snap CD root shares the directory name %q with receiver %q; slice roots are told apart by directory basename, so rename one directory first", snapBase, name)
				}
			}
			mods, err := transfer.MatchSnapcdModules(snapcdDir, rootDir, paths)
			if err != nil {
				return verdictf("%v", err)
			}
			m.Snapcd = &transfer.Snapcd{Dir: f.snapcdRoot, Modules: mods}
		}
	}
	if err := transfer.WriteMap(m, rootDir); err != nil {
		return err
	}

	outln(heading("Transfer plan:"))
	for _, name := range names {
		r := m.Receivers[name]
		outf("  -> %s %s\n", emphasis(name), dim("("+paths[name]+")"))
		for _, b := range r.Blocks {
			outf("       %s\n", b)
		}
		if len(r.Variables) > 0 {
			outf("       %s\n", dim("carries: variables "+strings.Join(r.Variables, ", ")))
		}
		if len(r.Locals) > 0 {
			outf("       %s\n", dim("carries: locals "+strings.Join(r.Locals, ", ")))
		}
	}
	if len(m.CrossEdges) > 0 || len(m.OrderingEdges) > 0 {
		outln("\n" + heading("Wiring across the boundary:"))
		for _, e := range m.CrossEdges {
			outf("  %s.%s <- %s.%s\n", emphasis(displayRoot(m, e.Consumer)), e.Input, emphasis(displayRoot(m, e.Producer)), e.Output)
		}
		for _, e := range m.OrderingEdges {
			outf("  %s depends on %s %s\n", emphasis(displayRoot(m, e.Consumer)), emphasis(displayRoot(m, e.Producer)), dim("(ordering only)"))
		}
		if m.Snapcd != nil {
			outf("\n  %s\n", dim("Snap CD root "+m.Snapcd.Dir+": wiring will be appended to its "+transfer.SnapcdTargetFile))
		} else if snapcdNote != "" {
			outf("\n  %s\n", dim(snapcdNote))
		}
	}
	outln("\n" + heading("Map written:"))
	outf("  %s\n", transfer.MapFile)
	return nil
}

// displayRoot renders a map module name for humans: the remainder is "source".
func displayRoot(m *transfer.Map, name string) string {
	if name == m.Remainder {
		return "source"
	}
	return name
}

func runTransferRefactorRun(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	m, err := transfer.LoadMap(rootDir)
	if err != nil {
		return err
	}
	paths, err := transfer.ResolveReceivers(m, rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	snapcdDirForMap := func() (string, error) {
		if m.Snapcd == nil {
			return "", nil
		}
		return transfer.ResolveSnapcdRoot(rootDir, m.Snapcd.Dir, true)
	}

	// Idempotent: if the code already moved and the map is finalized, report —
	// re-distributing the map copies, in case the crash fell between the two.
	if m.IsRun() {
		if err := transfer.CodeMoved(rootDir, m, paths); err == nil {
			dsts := make([]string, 0, len(paths)+1)
			for _, name := range m.ReceiverNames() {
				dsts = append(dsts, paths[name])
			}
			if m.Snapcd != nil {
				if d, err := snapcdDirForMap(); err == nil && d != "" {
					dsts = append(dsts, d)
				}
			}
			if err := transfer.DistributeMap(rootDir, dsts); err != nil {
				return err
			}
			outln("Code already moved; nothing to do (map copies refreshed).")
			return nil
		}
	}

	plan, err := transfer.BuildPlan(rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	if err := plan.MatchesMap(m); err != nil {
		return verdictf("%v", err)
	}

	// Validate and render everything before writing anything.
	contents := map[string]map[string]string{}
	for _, name := range m.ReceiverNames() {
		pr := plan.Receivers[name]
		if err := transfer.ValidateReceiverDir(paths[name], pr.Structural, pr.Inputs, pr.Outputs); err != nil {
			return verdictf("%v", err)
		}
		files, err := transfer.ReceiverFiles(rootDir, plan, name)
		if err != nil {
			return err
		}
		contents[name] = files
	}
	srcInputs, srcOutputs := plan.SourceWiring()
	if err := transfer.ValidateSourceWiring(rootDir, srcInputs, srcOutputs); err != nil {
		return verdictf("%v", err)
	}
	sourceFiles := transfer.SourceFiles(plan)
	snapcdHCL := transfer.SnapcdFileHCL(m)
	snapcdDir := ""
	if m.Snapcd != nil && snapcdHCL != "" {
		snapcdDir, err = snapcdDirForMap()
		if err != nil {
			return verdictf("%v", err)
		}
	}

	sortedFiles := func(files map[string]string) []string {
		names := make([]string, 0, len(files))
		for n := range files {
			names = append(names, n)
		}
		sort.Strings(names)
		return names
	}
	appendAll := func(dir string, files map[string]string) (map[string]string, error) {
		sums := map[string]string{}
		for _, fname := range sortedFiles(files) {
			if err := transfer.AppendToFile(dir, fname, files[fname]); err != nil {
				return nil, err
			}
			sum, err := transfer.FileSHA256(filepath.Join(dir, fname))
			if err != nil {
				return nil, err
			}
			sums[fname] = sum
		}
		return sums, nil
	}

	// Receivers first: a crash mid-way leaves duplicated code (harmless,
	// skipped on retry), never orphaned code. A receiver that already holds
	// every moved address is such a retry and is left alone.
	outln(heading("Code move:"))
	for _, name := range m.ReceiverNames() {
		have, err := transfer.DirAddrs(paths[name])
		if err != nil {
			return err
		}
		already := true
		for _, b := range m.Receivers[name].Blocks {
			if !have[b] {
				already = false
				break
			}
		}
		if already {
			outf("  %s: %s\n", emphasis(name), warn("already holds the moved blocks (skip)"))
			continue
		}
		sums, err := appendAll(paths[name], contents[name])
		if err != nil {
			return err
		}
		r := m.Receivers[name]
		r.FileChecksums = sums
		m.Receivers[name] = r
		outf("  %s: %s %s\n", emphasis(name), strings.Join(sortedFiles(contents[name]), ", "), success("appended"))
	}
	moved := map[string]bool{}
	receivers := map[string]bool{}
	for name, r := range m.Receivers {
		receivers[name] = true
		for _, b := range r.Blocks {
			moved[b] = true
		}
	}
	if snapcdDir != "" {
		snapAddrs, err := transfer.DirAddrs(snapcdDir)
		if err != nil {
			return err
		}
		snapDone := true
		for _, addr := range transfer.SnapcdWiringAddrs(m) {
			if !snapAddrs[addr] {
				snapDone = false
				break
			}
		}
		if snapDone {
			outf("  %s: %s\n", emphasis(m.Snapcd.Dir), warn("wiring already written (skip)"))
		} else {
			sums, err := appendAll(snapcdDir, map[string]string{transfer.SnapcdTargetFile: snapcdHCL})
			if err != nil {
				return err
			}
			m.Snapcd.FileChecksums = sums
			outf("  %s: %s %s %s\n", emphasis(m.Snapcd.Dir), transfer.SnapcdTargetFile, success("appended"), dim("(Snap CD wiring)"))
		}
	}
	if err := transfer.RemoveFromSource(rootDir, moved, receivers); err != nil {
		return err
	}
	changed, err := transfer.RewriteSourceRefs(rootDir, plan)
	if err != nil {
		return err
	}
	for _, path := range changed {
		outf("  %s: references rewritten to inputs %s\n", emphasis(displayPath(rootDir, path)), dim("(in place)"))
	}
	if len(sourceFiles) > 0 {
		sums, err := appendAll(rootDir, sourceFiles)
		if err != nil {
			return err
		}
		m.SourceFileChecksums = sums
		outf("  %s: %s %s %s\n", emphasis("source"), strings.Join(sortedFiles(sourceFiles), ", "), success("appended"), dim("(wiring declarations)"))
	}
	// Every source file the move touched is part of the diff-gated contract.
	for _, path := range changed {
		sum, err := transfer.FileSHA256(path)
		if err != nil {
			return err
		}
		if m.SourceFileChecksums == nil {
			m.SourceFileChecksums = map[string]string{}
		}
		m.SourceFileChecksums[filepath.Base(path)] = sum
	}
	if err := transfer.WriteMap(m, rootDir); err != nil {
		return err
	}
	// Distribute the finalized map into every touched root: each repo's change
	// carries the whole transfer, and the copies' shared hash is its identity.
	dsts := make([]string, 0, len(paths)+1)
	for _, name := range m.ReceiverNames() {
		dsts = append(dsts, paths[name])
	}
	if snapcdDir != "" {
		dsts = append(dsts, snapcdDir)
	}
	if err := transfer.DistributeMap(rootDir, dsts); err != nil {
		return err
	}
	outf("  %s: %d blocks %s\n", emphasis("source"), len(moved), success("removed"))
	outf("  %s\n\n", dim("map copy distributed to every touched root"))
	outf("Commit every touched root, then `demonolith transfer migrate --engine {terraform|tofu}` per slice (or --all from the source).\n%s\n", warn("All roots plan dirty until the state move completes — freeze their pipelines and keep the window short."))
	return nil
}

func runTransferRefactorDiff(ctx context.Context, f transferFlags) error {
	checkSums := func(label, dir string, sums map[string]string) error {
		for fname, want := range sums {
			got, err := transfer.FileSHA256(filepath.Join(dir, fname))
			if err != nil || got != want {
				return fmt.Errorf("%s: %s is missing or was edited after `transfer refactor run` (checksum mismatch); revert it or re-run the transfer refactor", label, fname)
			}
		}
		return nil
	}

	if !f.all {
		s, err := loadTransferSlice(f.rootDir)
		if err != nil {
			return verdictf("%v", err)
		}
		if err := transfer.CodeMovedSlice(s.dir, s.m, s.role); err != nil {
			return verdictf("%v", err)
		}
		var sums map[string]string
		switch s.role.Kind {
		case "source":
			sums = s.m.SourceFileChecksums
		case "receiver":
			sums = s.m.Receivers[s.role.Key].FileChecksums
		case "snapcd":
			sums = s.m.Snapcd.FileChecksums
		}
		if err := checkSums("this root", s.dir, sums); err != nil {
			return verdictf("%v", err)
		}
		outf("%s %s matches its side of the transfer map.\n", success("In sync:"), emphasis(s.role.Base))
		return nil
	}

	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	m, err := loadRunTransferMap(rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	paths, err := transfer.ResolveReceivers(m, rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	if err := transfer.CodeMoved(rootDir, m, paths); err != nil {
		return verdictf("%v", err)
	}
	for _, name := range m.ReceiverNames() {
		if err := checkSums("receiver "+name, paths[name], m.Receivers[name].FileChecksums); err != nil {
			return verdictf("%v", err)
		}
	}
	if err := checkSums("the source root", rootDir, m.SourceFileChecksums); err != nil {
		return verdictf("%v", err)
	}
	copies := map[string]string{}
	for _, name := range m.ReceiverNames() {
		copies["receiver "+name] = paths[name]
	}
	if m.Snapcd != nil {
		snapcdDir, err := transfer.ResolveSnapcdRoot(rootDir, m.Snapcd.Dir, true)
		if err != nil {
			return verdictf("%v", err)
		}
		if len(m.Snapcd.FileChecksums) > 0 {
			if err := checkSums("the Snap CD root", snapcdDir, m.Snapcd.FileChecksums); err != nil {
				return verdictf("%v", err)
			}
		}
		copies["the Snap CD root"] = snapcdDir
	}
	if err := transfer.MapCopiesIdentical(rootDir, copies); err != nil {
		return verdictf("%v", err)
	}
	outf("%s the code move on disk matches the transfer map, and every root carries an identical map copy.\n", success("In sync:"))
	return nil
}

// --- migrate steps ----------------------------------------------------------

func runTransferMigrateMap(ctx context.Context, f transferFlags) error {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	if f.all {
		a, err := loadTransferAll(f.rootDir)
		if err != nil {
			return verdictf("%v", err)
		}
		outln(heading("Pulling and pinning states (read-only):"))
		if err := migrateMapSlice(ctx, execPath, a.src, false); err != nil {
			return err
		}
		for _, name := range a.src.m.ReceiverNames() {
			s := a.sliceFor(name)
			if _, err := transfer.EnsureWorkDir(s.dir); err != nil {
				return err
			}
			base := s.role.Base
			if err := transfer.CopyFile(transfer.FragmentStateFile(a.src.work, base), transfer.FragmentStateFile(s.work, base)); err != nil {
				return err
			}
			if err := transfer.CopyFile(transfer.FragmentMetaFile(a.src.work, base), transfer.FragmentMetaFile(s.work, base)); err != nil {
				return err
			}
			if err := migrateMapSlice(ctx, execPath, s, false); err != nil {
				return err
			}
		}
		outln("\n" + heading("Receipts:"))
		outf("  %s %s\n", transfer.MigrateMapFile, dim("(in the source root and every receiver)"))
		return nil
	}
	s, err := loadTransferSlice(f.rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	return migrateMapSlice(ctx, execPath, s, true)
}

func migrateMapSlice(ctx context.Context, execPath string, s *transferSlice, showReceipts bool) error {
	if err := requireStateSlice(s); err != nil {
		return verdictf("%v", err)
	}
	if err := transfer.CodeMovedSlice(s.dir, s.m, s.role); err != nil {
		return verdictf("%v", err)
	}
	work, err := transfer.EnsureWorkDir(s.dir)
	if err != nil {
		return err
	}
	if showReceipts {
		outln(heading("Pulling and pinning state (read-only):"))
	}
	stateFile := transfer.SliceStateFile(work)
	outf("  %s ", emphasis(fmt.Sprintf("%-16s", s.role.Base)))
	if err := transfer.PullState(ctx, s.dir, stateFile, execPath); err != nil {
		return err
	}
	if err := transfer.CopyFile(stateFile, stateFile+".demono-backup"); err != nil {
		return err
	}
	meta, err := transfer.ReadMeta(stateFile)
	if err != nil {
		return err
	}
	pin := transfer.Pin{Lineage: meta.Lineage, Serial: meta.Serial}
	post := transfer.SlicePostFile(work)
	if err := transfer.CopyFile(stateFile, post); err != nil {
		return err
	}

	switch s.role.Kind {
	case "source":
		var frags []string
		for _, name := range s.m.ReceiverNames() {
			base := filepath.Base(name)
			fragState := transfer.FragmentStateFile(work, base)
			_ = os.Remove(fragState)
			if err := transfer.ApplyMoves(ctx, s.dir, post, fragState, s.m.Receivers[name].Moves, execPath); err != nil {
				return err
			}
			fm := &transfer.FragmentMeta{Version: 1, Created: nowStamp(), Tool: toolString(), MapHash: s.hash, Receiver: base, SourcePin: pin, Moves: s.m.Receivers[name].Moves}
			if err := transfer.WriteFragmentMeta(fm, work); err != nil {
				return err
			}
			frags = append(frags, base)
		}
		outf("%s%s\n", success("pulled + pinned"), dim(" (fragments written for "+strings.Join(frags, ", ")+")"))
	case "receiver":
		base := s.role.Base
		fm, err := transfer.LoadFragmentMeta(work, base)
		if err != nil {
			return verdictf("no state fragment for this receiver in %s — `demonolith transfer migrate map` at the source (%s) writes fragment-%s.tfstate and fragment-%s.yaml; bring both into this root's %s", transfer.WorkDirName, s.m.SourceDir, base, base, transfer.WorkDirName)
		}
		if fm.MapHash != s.hash {
			return verdictf("the fragment in %s belongs to a different transfer (map hash mismatch); refresh both fragment files from the source", transfer.WorkDirName)
		}
		inject := transfer.FragmentStateFile(work, base) + ".inject"
		if err := transfer.CopyFile(transfer.FragmentStateFile(work, base), inject); err != nil {
			return verdictf("the fragment meta is present but fragment-%s.tfstate is not; bring both files from the source", base)
		}
		if err := transfer.ApplyMoves(ctx, s.dir, inject, post, fm.Moves, execPath); err != nil {
			return err
		}
		_ = os.Remove(inject)
		outf("%s%s\n", success("pulled + pinned"), dim(" (fragment applied to the local copy)"))
	}
	if err := transfer.WriteSlicePin(&transfer.SlicePin{Version: 1, Created: nowStamp(), Tool: toolString(), MapHash: s.hash, Role: s.role.Base, Pin: pin}, s.dir); err != nil {
		return err
	}
	if showReceipts {
		outln("\n" + heading("Receipt:"))
		outf("  %s\n", transfer.MigrateMapFile)
	}
	return nil
}

func runTransferMigrateProve(ctx context.Context, f transferFlags) error {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	if f.all {
		a, err := loadTransferAll(f.rootDir)
		if err != nil {
			return verdictf("%v", err)
		}
		outln(heading("Proving (offline, against moved state copies, producer values threaded):"))
		for _, module := range a.order {
			s := a.sliceFor(module)
			if err := migrateProveSlice(ctx, execPath, s, false); err != nil {
				return err
			}
			if err := distributeOutputs(a, s); err != nil {
				return err
			}
		}
		outln("\n" + success("✓ source and receivers plan to zero changes with the states moved."))
		return nil
	}
	s, err := loadTransferSlice(f.rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	return migrateProveSlice(ctx, execPath, s, true)
}

// distributeOutputs copies a producer slice's outputs artifact into every
// other slice's workdir, the way an external orchestrator would.
func distributeOutputs(a *transferAll, from *transferSlice) error {
	src := transfer.OutputsFile(from.work, from.role.Base)
	if _, err := os.Stat(src); err != nil {
		return nil
	}
	for _, module := range a.order {
		s := a.sliceFor(module)
		if s.dir == from.dir {
			continue
		}
		if _, err := transfer.EnsureWorkDir(s.dir); err != nil {
			return err
		}
		if err := transfer.CopyFile(src, transfer.OutputsFile(s.work, from.role.Base)); err != nil {
			return err
		}
	}
	return nil
}

func migrateProveSlice(ctx context.Context, execPath string, s *transferSlice, showReceipts bool) error {
	if err := requireStateSlice(s); err != nil {
		return verdictf("%v", err)
	}
	if err := transfer.CodeMovedSlice(s.dir, s.m, s.role); err != nil {
		return verdictf("%v", err)
	}
	pin, err := loadSliceReadiness(s)
	if err != nil {
		return verdictf("%v", err)
	}
	post := transfer.SlicePostFile(s.work)
	if _, err := os.Stat(post); err != nil {
		return verdictf("no moved state copy in %s; run `demonolith transfer migrate map` here first", transfer.WorkDirName)
	}
	vars, err := sliceThreadVars(s)
	if err != nil {
		return verdictf("%v", err)
	}
	if showReceipts {
		outln(heading("Proving (offline, against the moved state copy, producer values threaded):"))
	}
	outf("  %s ", emphasis(fmt.Sprintf("%-16s", s.role.Base)))
	mp, outs, err := proof.PlanDir(ctx, s.dir, post, vars, proof.Options{ExecPath: execPath})
	if err != nil {
		outln(fail("plan FAILED"))
		return err
	}
	verdict := "zero changes"
	if !mp.ZeroDiff {
		verdict = fmt.Sprintf("CHANGES +%d ~%d -%d", mp.AddCount, mp.Change, mp.Destroy)
	}
	outln(colorVerdict(verdict))
	if sliceIsProducer(s) {
		oa := &transfer.OutputsArtifact{Version: 1, Created: nowStamp(), Tool: toolString(), MapHash: s.hash, Role: s.role.Base, Outputs: outs}
		if err := transfer.WriteOutputs(oa, s.work); err != nil {
			return err
		}
	}
	rec := &transfer.Receipt{Version: 1, Created: nowStamp(), Tool: toolString(), Step: "prove", MapHash: s.hash, Role: s.role.Base, Pin: pin.Pin, OK: mp.ZeroDiff, Roots: map[string]string{s.role.Base: verdict}}
	if err := transfer.WriteReceipt(rec, s.dir, transfer.ProveReceiptFile); err != nil {
		return err
	}
	if showReceipts {
		outln("\n" + heading("Receipt:"))
		outf("  %s\n", transfer.ProveReceiptFile)
	}
	if !mp.ZeroDiff {
		return verdictf("this slice does not prove clean; do not run the state move")
	}
	return nil
}

func runTransferMigrateRun(ctx context.Context, f transferFlags) error {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	if f.all {
		a, err := loadTransferAll(f.rootDir)
		if err != nil {
			return verdictf("%v", err)
		}
		outln(heading("Writing states (receivers first, source last):"))
		for _, name := range a.src.m.ReceiverNames() {
			s := a.sliceFor(name)
			if err := migrateRunSlice(ctx, execPath, s, false); err != nil {
				return err
			}
			if _, err := transfer.EnsureWorkDir(a.src.dir); err != nil {
				return err
			}
			if err := transfer.CopyFile(filepath.Join(s.dir, transfer.RunReceiptFile), transfer.RecvRunReceiptFile(a.src.work, s.role.Base)); err != nil {
				return err
			}
		}
		if err := migrateRunSlice(ctx, execPath, a.src, false); err != nil {
			return err
		}
		outln("\n" + heading("Receipts:"))
		outf("  %s %s\n", transfer.RunReceiptFile, dim("(in the source root and every receiver)"))
		return nil
	}
	s, err := loadTransferSlice(f.rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	return migrateRunSlice(ctx, execPath, s, true)
}

func migrateRunSlice(ctx context.Context, execPath string, s *transferSlice, showReceipts bool) error {
	if err := requireStateSlice(s); err != nil {
		return verdictf("%v", err)
	}
	pin, err := loadSliceReadiness(s)
	if err != nil {
		return verdictf("%v", err)
	}
	prove, err := transfer.LoadReceipt(s.dir, transfer.ProveReceiptFile)
	if err != nil || !prove.OK || prove.MapHash != s.hash || prove.Pin != pin.Pin {
		return verdictf("no clean proof for this pin generation; run `demonolith transfer migrate prove` here first")
	}
	// The source is stripped last: it demands every receiver's run receipt —
	// the claim that the moved addresses already live in their new homes.
	if s.role.Kind == "source" {
		for _, name := range s.m.ReceiverNames() {
			base := filepath.Base(name)
			rr, err := transfer.LoadReceipt(s.work, "run-"+base+".yaml")
			if err != nil {
				return verdictf("missing run receipt for receiver %s — the source's state is written last: run `demonolith transfer migrate run` at %s, then bring its %s into this root's %s as run-%s.yaml", base, base, transfer.RunReceiptFile, transfer.WorkDirName, base)
			}
			if rr.MapHash != s.hash || !rr.OK {
				return verdictf("the run receipt for receiver %s does not show a completed write for this transfer; re-run `demonolith transfer migrate run` there and refresh it", base)
			}
		}
	}
	if showReceipts {
		outln(heading("Re-pulling and writing state (guarded, never forced):"))
	}
	runState := transfer.SliceRunFile(s.work)
	if err := transfer.PullState(ctx, s.dir, runState, execPath); err != nil {
		return err
	}
	meta, err := transfer.ReadMeta(runState)
	if err != nil {
		return err
	}
	var moves []string
	if s.role.Kind == "source" {
		for _, name := range s.m.ReceiverNames() {
			moves = append(moves, s.m.Receivers[name].Moves...)
		}
	} else {
		moves = s.m.Receivers[s.role.Key].Moves
	}

	rec := &transfer.Receipt{Version: 1, Created: nowStamp(), Tool: toolString(), Step: "run", MapHash: s.hash, Role: s.role.Base, Pin: pin.Pin, OK: true, Roots: map[string]string{}}
	if meta.Lineage != pin.Pin.Lineage || meta.Serial != pin.Pin.Serial {
		present, absent, err := transfer.ContainsAddrs(runState, moves)
		if err != nil {
			return err
		}
		transferred := len(present) == 0
		if s.role.Kind == "receiver" {
			transferred = len(absent) == 0
		}
		if !transferred {
			return verdictf("this root's state changed since `transfer migrate map` (serial %d, pinned %d); re-run `demonolith transfer migrate map` and `prove` here", meta.Serial, pin.Pin.Serial)
		}
		rec.Roots[s.role.Base] = "skipped (already transferred)"
		if err := transfer.WriteReceipt(rec, s.dir, transfer.RunReceiptFile); err != nil {
			return err
		}
		outf("  %s %s\n", emphasis(fmt.Sprintf("%-16s", s.role.Base)), warn("skipped (already transferred)"))
		if showReceipts {
			outln("\n" + heading("Receipt:"))
			outf("  %s\n", transfer.RunReceiptFile)
		}
		return nil
	}

	push := transfer.SlicePushFile(s.work)
	if err := transfer.CopyFile(runState, push); err != nil {
		return err
	}
	switch s.role.Kind {
	case "receiver":
		inject := transfer.FragmentStateFile(s.work, s.role.Base) + ".inject"
		if err := transfer.CopyFile(transfer.FragmentStateFile(s.work, s.role.Base), inject); err != nil {
			return verdictf("no state fragment in %s; run `demonolith transfer migrate map` here again", transfer.WorkDirName)
		}
		if err := transfer.ApplyMoves(ctx, s.dir, inject, push, moves, execPath); err != nil {
			return err
		}
		_ = os.Remove(inject)
	case "source":
		for _, name := range s.m.ReceiverNames() {
			discard := filepath.Join(s.work, "discard-"+filepath.Base(name)+".tfstate")
			_ = os.Remove(discard)
			if err := transfer.ApplyMoves(ctx, s.dir, push, discard, s.m.Receivers[name].Moves, execPath); err != nil {
				return err
			}
		}
	}
	if err := transfer.BumpSerial(push, meta.Serial+1); err != nil {
		return err
	}
	outf("  %s pushing ... ", emphasis(fmt.Sprintf("%-16s", s.role.Base)))
	if err := transfer.PushState(ctx, s.dir, push, execPath); err != nil {
		outln(fail("FAILED"))
		rec.Roots[s.role.Base] = "failed"
		rec.OK = false
		_ = transfer.WriteReceipt(rec, s.dir, transfer.RunReceiptFile)
		return err
	}
	rec.Roots[s.role.Base] = "pushed"
	outln(success("pushed"))
	if err := transfer.WriteReceipt(rec, s.dir, transfer.RunReceiptFile); err != nil {
		return err
	}
	if showReceipts {
		outln("\n" + heading("Receipt:"))
		outf("  %s\n", transfer.RunReceiptFile)
	}
	return nil
}

func runTransferMigrateVerify(ctx context.Context, f transferFlags) error {
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	if f.all {
		a, err := loadTransferAll(f.rootDir)
		if err != nil {
			return verdictf("%v", err)
		}
		outln(heading("Verifying (against the real backends, producer values threaded):"))
		for _, module := range a.order {
			s := a.sliceFor(module)
			if err := migrateVerifySlice(ctx, execPath, s, false); err != nil {
				return err
			}
			if err := distributeOutputs(a, s); err != nil {
				return err
			}
		}
		outln("\n" + success("✓ transfer complete: source and receivers plan to zero changes against their real backends."))
		return nil
	}
	s, err := loadTransferSlice(f.rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	return migrateVerifySlice(ctx, execPath, s, true)
}

func migrateVerifySlice(ctx context.Context, execPath string, s *transferSlice, showReceipts bool) error {
	if err := requireStateSlice(s); err != nil {
		return verdictf("%v", err)
	}
	if err := transfer.CodeMovedSlice(s.dir, s.m, s.role); err != nil {
		return verdictf("%v", err)
	}
	vars, err := sliceThreadVars(s)
	if err != nil {
		return verdictf("%v", err)
	}
	if showReceipts {
		outln(heading("Verifying (against the real backend, producer values threaded):"))
	}
	outf("  %s ", emphasis(fmt.Sprintf("%-16s", s.role.Base)))
	mp, outs, err := proof.PlanDir(ctx, s.dir, "", vars, proof.Options{ExecPath: execPath, UseBackend: true})
	if err != nil {
		outln(fail("plan FAILED"))
		return err
	}
	verdict := "zero changes"
	if !mp.ZeroDiff {
		verdict = fmt.Sprintf("CHANGES +%d ~%d -%d", mp.AddCount, mp.Change, mp.Destroy)
	}
	outln(colorVerdict(verdict))
	if sliceIsProducer(s) {
		if _, err := transfer.EnsureWorkDir(s.dir); err != nil {
			return err
		}
		oa := &transfer.OutputsArtifact{Version: 1, Created: nowStamp(), Tool: toolString(), MapHash: s.hash, Role: s.role.Base, Outputs: outs}
		if err := transfer.WriteOutputs(oa, s.work); err != nil {
			return err
		}
	}
	pinned := transfer.Pin{}
	if pin, err := transfer.LoadSlicePin(s.dir); err == nil {
		pinned = pin.Pin
	}
	rec := &transfer.Receipt{Version: 1, Created: nowStamp(), Tool: toolString(), Step: "verify", MapHash: s.hash, Role: s.role.Base, Pin: pinned, OK: mp.ZeroDiff, Roots: map[string]string{s.role.Base: verdict}}
	if err := transfer.WriteReceipt(rec, s.dir, transfer.VerifyReceiptFile); err != nil {
		return err
	}
	if showReceipts {
		outln("\n" + heading("Receipt:"))
		outf("  %s\n", transfer.VerifyReceiptFile)
	}
	if !mp.ZeroDiff {
		return verdictf("verification found changes; inspect the plan above")
	}
	if showReceipts {
		outln("\n" + success("✓ this root plans to zero changes against its real backend."))
	}
	return nil
}
