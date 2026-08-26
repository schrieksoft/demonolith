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
}

func transferCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "transfer",
		Short: "Move decorated blocks into pre-existing roots: refactor (the code move), then migrate (the state move) (experimental)",
		Long: `Move decorated blocks (# @demono:move ../some-root) from the source root into
pre-existing receiver roots. Both sides keep living: the source's state and every
receiver's state are rewritten. Experimental: the selection must be
self-contained — references across the transfer boundary are refused.

Each decorator names its destination as a directory relative to the source
root; the directory must already exist. Two halves, same grammar as the split:
  transfer refactor   map → run → diff       the code move (commit and review this)
  transfer migrate    map → prove → run → verify   the state move (post-merge, once)

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

func transferCommonFlags(cmd *cobra.Command, f *transferFlags, withEngine bool) {
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the source root")
	flags.StringVar(&f.snapcdRoot, "snapcd-root", "snapcd", "the Snap CD root holding the roots' snapcd_module resources, relative to the source root's parent; when present, the transfer writes the cross-root wiring (snapcd_module_input_from_output, snapcd_depends_on_module) into it")
	if withEngine {
		flags.StringVar(&f.engine, "engine", "", "state engine: terraform or tofu (required)")
		flags.StringVar(&f.execPath, "exec-path", "", "explicit terraform/tofu binary path (overrides --engine)")
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
		Short: "The code move: map → run → diff",
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
			return runTransferRefactorDiff(ctx, f)
		},
	}
	transferCommonFlags(cmd, &f, false)
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "approve the code move automatically instead of pausing after the map")

	mapCmd := &cobra.Command{
		Use:   "map",
		Short: "Analyze the source and write the transfer map (pure, offline)",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorMap(cmd.Context(), f) },
	}
	runCmd := &cobra.Command{
		Use:   "run",
		Short: "Execute the map: append the moved code into each receiver's own files, remove the blocks from the source",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorRun(cmd.Context(), f) },
	}
	diffCmd := &cobra.Command{
		Use:   "diff",
		Short: "Gate: the code on disk (source and receivers) still matches the map",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, args []string) error { return runTransferRefactorDiff(cmd.Context(), f) },
	}
	for _, c := range []*cobra.Command{mapCmd, runCmd, diffCmd} {
		var cf transferFlags
		transferCommonFlags(c, &cf, false)
		c.RunE = wrapTransferRunE(c, &cf)
	}
	cmd.AddCommand(mapCmd, runCmd, diffCmd)
	return cmd
}

// wrapTransferRunE rebinds a subcommand to its own flag struct.
func wrapTransferRunE(c *cobra.Command, cf *transferFlags) func(*cobra.Command, []string) error {
	switch c.Use {
	case "map":
		return func(cmd *cobra.Command, args []string) error { return runTransferRefactorMap(cmd.Context(), *cf) }
	case "run":
		return func(cmd *cobra.Command, args []string) error { return runTransferRefactorRun(cmd.Context(), *cf) }
	case "diff":
		return func(cmd *cobra.Command, args []string) error { return runTransferRefactorDiff(cmd.Context(), *cf) }
	}
	return c.RunE
}

// --- transfer migrate: the state half ---------------------------------------

func transferMigrateCmd() *cobra.Command {
	var f transferFlags
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "The state move: map → prove → run → verify",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
			outf("%s\n\n", banner("── transfer migrate map ──"))
			if err := runTransferMigrateMap(ctx, f); err != nil {
				return err
			}
			outf("\n%s\n\n", banner("── transfer migrate prove ──"))
			if err := runTransferMigrateProve(ctx, f); err != nil {
				return err
			}
			if err := approvePause(f.yes, "Proceed with the state move? Both sides' states are rewritten."); err != nil {
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
	transferCommonFlags(cmd, &f, true)
	cmd.Flags().BoolVarP(&f.yes, "yes", "y", false, "approve the state move automatically instead of pausing after the proof")

	mkStep := func(use, short string, fn func(context.Context, transferFlags) error) *cobra.Command {
		var cf transferFlags
		c := &cobra.Command{
			Use:   use,
			Short: short,
			Args:  cobra.NoArgs,
			RunE:  func(cmd *cobra.Command, args []string) error { return fn(cmd.Context(), cf) },
		}
		transferCommonFlags(c, &cf, true)
		return c
	}
	cmd.AddCommand(
		mkStep("map", "Pull source and receiver states read-only, back them up, pin them, apply the moves to local copies", runTransferMigrateMap),
		mkStep("prove", "Gate: source and receivers plan to zero changes against the moved local copies", runTransferMigrateProve),
		mkStep("run", "Execute the state move: rewrite every receiver's state, then the source's (guarded, never forced)", runTransferMigrateRun),
		mkStep("verify", "Gate: replan source and receivers against their real backends", runTransferMigrateVerify),
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

func transferWorkFiles(workDir, name string) (pulled, post string) {
	if name == "" {
		return filepath.Join(workDir, "source.tfstate"), filepath.Join(workDir, "source-post.tfstate")
	}
	slug := transfer.Slug(name)
	return filepath.Join(workDir, "recv-"+slug+".tfstate"), filepath.Join(workDir, "recv-"+slug+"-post.tfstate")
}

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

	m := &transfer.Map{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Tool: toolString(), Remainder: plan.Remainder, Receivers: map[string]transfer.Receiver{}}
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

	// Idempotent: if the code already moved and the map is finalized, report.
	if m.IsRun() {
		if err := transfer.CodeMoved(rootDir, m, paths); err == nil {
			outln("Code already moved; nothing to do.")
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
		snapcdDir, err = transfer.ResolveSnapcdRoot(rootDir, m.Snapcd.Dir, true)
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
	outf("  %s: %d blocks %s\n\n", emphasis("source"), len(moved), success("removed"))
	outf("Commit both sides, then `demonolith transfer migrate --engine {terraform|tofu}`.\n%s\n", warn("Both roots plan dirty until the state move completes — freeze their pipelines and keep the window short."))
	return nil
}

func runTransferRefactorDiff(ctx context.Context, f transferFlags) error {
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
	checkSums := func(label, dir string, sums map[string]string) error {
		for fname, want := range sums {
			got, err := transfer.FileSHA256(filepath.Join(dir, fname))
			if err != nil || got != want {
				return fmt.Errorf("%s: %s is missing or was edited after `transfer refactor run` (checksum mismatch); revert it or re-run the transfer refactor", label, fname)
			}
		}
		return nil
	}
	for _, name := range m.ReceiverNames() {
		if err := checkSums("receiver "+name, paths[name], m.Receivers[name].FileChecksums); err != nil {
			return verdictf("%v", err)
		}
	}
	if err := checkSums("the source root", rootDir, m.SourceFileChecksums); err != nil {
		return verdictf("%v", err)
	}
	if m.Snapcd != nil && len(m.Snapcd.FileChecksums) > 0 {
		snapcdDir, err := transfer.ResolveSnapcdRoot(rootDir, m.Snapcd.Dir, true)
		if err != nil {
			return verdictf("%v", err)
		}
		if err := checkSums("the Snap CD root", snapcdDir, m.Snapcd.FileChecksums); err != nil {
			return verdictf("%v", err)
		}
	}
	outf("%s the code move on disk matches the transfer map.\n", success("In sync:"))
	return nil
}

// --- migrate steps ----------------------------------------------------------

func runTransferMigrateMap(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
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

	workDir, err := transfer.EnsureWorkDir(rootDir)
	if err != nil {
		return err
	}
	pins := &transfer.Pins{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Tool: toolString(), Receivers: map[string]transfer.Pin{}}

	outln(heading("Pulling and pinning states (read-only):"))
	sourceState, sourcePost := transferWorkFiles(workDir, "")
	outf("  %s ", emphasis(fmt.Sprintf("%-16s", "source")))
	if err := transfer.PullState(ctx, rootDir, sourceState, execPath); err != nil {
		return err
	}
	if err := transfer.CopyFile(sourceState, sourceState+".demono-backup"); err != nil {
		return err
	}
	dm, err := transfer.ReadMeta(sourceState)
	if err != nil {
		return err
	}
	pins.Source = transfer.Pin{Lineage: dm.Lineage, Serial: dm.Serial}
	outln(success("pulled"))

	if err := transfer.CopyFile(sourceState, sourcePost); err != nil {
		return err
	}
	for _, name := range m.ReceiverNames() {
		recvState, recvPost := transferWorkFiles(workDir, name)
		outf("  %s ", emphasis(fmt.Sprintf("%-16s", name)))
		if err := transfer.PullState(ctx, paths[name], recvState, execPath); err != nil {
			return err
		}
		if err := transfer.CopyFile(recvState, recvState+".demono-backup"); err != nil {
			return err
		}
		rm, err := transfer.ReadMeta(recvState)
		if err != nil {
			return err
		}
		pins.Receivers[name] = transfer.Pin{Lineage: rm.Lineage, Serial: rm.Serial}
		if err := transfer.CopyFile(recvState, recvPost); err != nil {
			return err
		}
		if err := transfer.ApplyMoves(ctx, rootDir, sourcePost, recvPost, m.Receivers[name].Moves, execPath); err != nil {
			return err
		}
		outf("%s%s\n", success("pulled"), dim(" + moves applied to the local copy"))
	}
	if err := transfer.WritePins(pins, rootDir); err != nil {
		return err
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", transfer.MigrateMapFile)
	return nil
}

func runTransferMigrateProve(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	m, err := loadRunTransferMap(rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	pins, err := transfer.LoadPins(rootDir)
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

	workDir := filepath.Join(rootDir, transfer.WorkDirName)
	_, sourcePost := transferWorkFiles(workDir, "")
	if _, err := os.Stat(sourcePost); err != nil {
		return verdictf("no moved state copies in %s; run `demonolith transfer migrate map` first", transfer.WorkDirName)
	}

	outln(heading("Proving (offline, against moved state copies, producer values threaded):"))
	rec := &transfer.Receipt{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Tool: toolString(), Step: "prove", Source: pins.Source, OK: true, Roots: map[string]string{}}
	moduleDirs := map[string]string{m.Remainder: rootDir}
	moduleStates := map[string]string{m.Remainder: sourcePost}
	for _, name := range m.ReceiverNames() {
		_, recvPost := transferWorkFiles(workDir, name)
		moduleDirs[name] = paths[name]
		moduleStates[name] = recvPost
	}
	bound := transfer.BoundaryFromMap(m)
	res, err := proof.Run(ctx, moduleDirs, moduleStates, bound, proof.Options{
		ExecPath:    execPath,
		OnPlanStart: func(module string) { outf("  %s ", emphasis(fmt.Sprintf("%-16s", displayRoot(m, module)))) },
		OnPlanDone:  func(module, verdict string) { outln(colorVerdict(verdict)) },
	})
	if err != nil {
		return err
	}
	for name, mp := range res.Modules {
		verdict := "zero changes"
		if !mp.ZeroDiff {
			verdict = fmt.Sprintf("CHANGES +%d ~%d -%d", mp.AddCount, mp.Change, mp.Destroy)
		}
		rec.Roots[displayRoot(m, name)] = verdict
	}
	rec.OK = res.OK
	if err := transfer.WriteReceipt(rec, rootDir, transfer.ProveReceiptFile); err != nil {
		return err
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", transfer.ProveReceiptFile)
	if !rec.OK {
		return verdictf("the transfer does not prove clean; do not run it")
	}
	outln("\n" + success("✓ source and receivers plan to zero changes with the states moved."))
	return nil
}

func runTransferMigrateRun(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
	if err != nil {
		return err
	}
	m, err := loadRunTransferMap(rootDir)
	if err != nil {
		return verdictf("%v", err)
	}
	pins, err := transfer.LoadPins(rootDir)
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
	prove, err := transfer.LoadReceipt(rootDir, transfer.ProveReceiptFile)
	if err != nil || !prove.OK || prove.Source != pins.Source {
		return verdictf("no clean proof for this pin generation; run `demonolith transfer migrate prove` first")
	}

	workDir := filepath.Join(rootDir, transfer.WorkDirName)
	allMoves := []string{}
	for _, name := range m.ReceiverNames() {
		allMoves = append(allMoves, m.Receivers[name].Moves...)
	}

	outln(heading("Re-pulling and checking states:"))
	sourceRun := filepath.Join(workDir, "source-run.tfstate")
	if err := transfer.PullState(ctx, rootDir, sourceRun, execPath); err != nil {
		return err
	}
	dm, err := transfer.ReadMeta(sourceRun)
	if err != nil {
		return err
	}
	sourcePending := false
	switch {
	case dm.Lineage == pins.Source.Lineage && dm.Serial == pins.Source.Serial:
		sourcePending = true
		outf("  %s: %s\n", emphasis("source"), success("matches the pinned state"))
	default:
		present, _, err := transfer.ContainsAddrs(sourceRun, allMoves)
		if err != nil {
			return err
		}
		if len(present) > 0 {
			return verdictf("the source state changed since `transfer migrate map` (serial %d, pinned %d); re-run `demonolith transfer migrate`", dm.Serial, pins.Source.Serial)
		}
		outf("  %s: %s\n", emphasis("source"), warn("already transferred (skip)"))
	}

	recvPending := map[string]bool{}
	recvRun := map[string]string{}
	recvMeta := map[string]transfer.Meta{}
	for _, name := range m.ReceiverNames() {
		r := m.Receivers[name]
		pin := pins.Receivers[name]
		path := filepath.Join(workDir, "recv-"+transfer.Slug(name)+"-run.tfstate")
		recvRun[name] = path
		if err := transfer.PullState(ctx, paths[name], path, execPath); err != nil {
			return err
		}
		rm, err := transfer.ReadMeta(path)
		if err != nil {
			return err
		}
		recvMeta[name] = rm
		switch {
		case rm.Lineage == pin.Lineage && rm.Serial == pin.Serial:
			recvPending[name] = true
			outf("  %s: %s\n", emphasis(name), success("matches the pinned state"))
		default:
			_, absent, err := transfer.ContainsAddrs(path, r.Moves)
			if err != nil {
				return err
			}
			if len(absent) > 0 {
				return verdictf("receiver %q state changed since `transfer migrate map` (serial %d, pinned %d); re-run `demonolith transfer migrate`", name, rm.Serial, pin.Serial)
			}
			outf("  %s: %s\n", emphasis(name), warn("already transferred (skip)"))
		}
	}

	// Rebuild the post-move states from the fresh pulls. Every receiver's
	// moves leave the source copy, whether or not that receiver still pushes.
	sourcePush := filepath.Join(workDir, "source-push.tfstate")
	if err := transfer.CopyFile(sourceRun, sourcePush); err != nil {
		return err
	}
	recvPush := map[string]string{}
	for _, name := range m.ReceiverNames() {
		dest := filepath.Join(workDir, "recv-"+transfer.Slug(name)+"-discard.tfstate")
		if recvPending[name] {
			dest = filepath.Join(workDir, "recv-"+transfer.Slug(name)+"-push.tfstate")
			if err := transfer.CopyFile(recvRun[name], dest); err != nil {
				return err
			}
			recvPush[name] = dest
		} else {
			_ = os.Remove(dest)
		}
		if sourcePending || recvPending[name] {
			if err := transfer.ApplyMoves(ctx, rootDir, sourcePush, dest, m.Receivers[name].Moves, execPath); err != nil {
				return err
			}
		}
	}

	outln("\n" + heading("Writing states (receivers first, source last):"))
	rec := &transfer.Receipt{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Tool: toolString(), Step: "run", Source: pins.Source, OK: true, Roots: map[string]string{}}
	for _, name := range m.ReceiverNames() {
		if !recvPending[name] {
			rec.Roots[name] = "skipped (already transferred)"
			outf("  %s: %s\n", emphasis(name), warn("skipped (already transferred)"))
			continue
		}
		if err := transfer.BumpSerial(recvPush[name], recvMeta[name].Serial+1); err != nil {
			return err
		}
		outf("  %s: pushing ... ", emphasis(name))
		if err := transfer.PushState(ctx, paths[name], recvPush[name], execPath); err != nil {
			outln(fail("FAILED"))
			rec.Roots[name] = "failed"
			rec.OK = false
			_ = transfer.WriteReceipt(rec, rootDir, transfer.RunReceiptFile)
			return err
		}
		rec.Roots[name] = "pushed"
		outln(success("pushed"))
	}
	if sourcePending {
		if err := transfer.BumpSerial(sourcePush, dm.Serial+1); err != nil {
			return err
		}
		outf("  %s: pushing ... ", emphasis("source"))
		if err := transfer.PushState(ctx, rootDir, sourcePush, execPath); err != nil {
			outln(fail("FAILED"))
			rec.Roots["source"] = "failed"
			rec.OK = false
			_ = transfer.WriteReceipt(rec, rootDir, transfer.RunReceiptFile)
			return err
		}
		rec.Roots["source"] = "pushed"
		outln(success("pushed"))
	} else {
		rec.Roots["source"] = "skipped (already transferred)"
	}
	if err := transfer.WriteReceipt(rec, rootDir, transfer.RunReceiptFile); err != nil {
		return err
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", transfer.RunReceiptFile)
	return nil
}

func runTransferMigrateVerify(ctx context.Context, f transferFlags) error {
	rootDir, err := filepath.Abs(f.rootDir)
	if err != nil {
		return err
	}
	execPath, err := engineExecPath(f.engine, f.execPath)
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

	outln(heading("Verifying (against the real backends, producer values threaded):"))
	sourcePin := transfer.Pin{}
	if pins, err := transfer.LoadPins(rootDir); err == nil {
		sourcePin = pins.Source
	}
	rec := &transfer.Receipt{Version: 1, Created: time.Now().UTC().Format(time.RFC3339), Tool: toolString(), Step: "verify", Source: sourcePin, OK: true, Roots: map[string]string{}}
	moduleDirs := map[string]string{m.Remainder: rootDir}
	for _, name := range m.ReceiverNames() {
		moduleDirs[name] = paths[name]
	}
	bound := transfer.BoundaryFromMap(m)
	res, err := proof.Run(ctx, moduleDirs, map[string]string{}, bound, proof.Options{
		ExecPath:    execPath,
		UseBackend:  true,
		OnPlanStart: func(module string) { outf("  %s ", emphasis(fmt.Sprintf("%-16s", displayRoot(m, module)))) },
		OnPlanDone:  func(module, verdict string) { outln(colorVerdict(verdict)) },
	})
	if err != nil {
		return err
	}
	for name, mp := range res.Modules {
		verdict := "zero changes"
		if !mp.ZeroDiff {
			verdict = fmt.Sprintf("CHANGES +%d ~%d -%d", mp.AddCount, mp.Change, mp.Destroy)
		}
		rec.Roots[displayRoot(m, name)] = verdict
	}
	rec.OK = res.OK
	if err := transfer.WriteReceipt(rec, rootDir, transfer.VerifyReceiptFile); err != nil {
		return err
	}
	outln("\n" + heading("Receipt:"))
	outf("  %s\n", transfer.VerifyReceiptFile)
	if !rec.OK {
		return verdictf("verification found changes; inspect the plans above")
	}
	outln("\n" + success("✓ transfer complete: source and receivers plan to zero changes against their real backends."))
	return nil
}
