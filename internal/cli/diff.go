package cli

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/spf13/cobra"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
	"github.com/schrieksoft/demonolith/internal/pipeline"
)

type diffFlags struct {
	rootDir string
	output  string
}

func diffCmd() *cobra.Command {
	var f diffFlags
	cmd := &cobra.Command{
		Use:   "diff",
		Short: "Compare the committed roots and manifest against what the committed source produces",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			mode, err := parseOutput(f.output)
			if err != nil {
				return err
			}
			return runDiff(resolveRoot(f.rootDir), mode)
		},
	}
	flags := cmd.Flags()
	flags.StringVar(&f.rootDir, "root-dir", ".", "the monolith root")
	flags.StringVar(&f.output, "output", "text", "report format: text or json")
	return cmd
}

// diffReport is the machine-facing result of diff.
type diffReport struct {
	Differs      bool     `json:"differs"`
	Manifest     string   `json:"manifest,omitempty"`
	ChangedFiles []string `json:"changed_files,omitempty"`
	Reasons      []string `json:"reasons,omitempty"`
}

// runDiff re-runs analysis+emit into a temp dir and compares against the
// committed roots and manifest. The remainder-module name comes from the
// committed manifest, so no placement flag can skew the comparison. Nothing is
// written to the root or output dirs; any mismatch is a negative verdict.
func runDiff(rootDir string, mode outputMode) error {
	rep := diffReport{}
	path := manifest.Path(rootDir)
	if _, err := os.Stat(path); err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "no committed "+manifest.FileName+" found; run `demonolith refactor` and commit its output")
		return reportDiff(rep, mode)
	}
	rep.Manifest = manifest.FileName
	committed, err := manifest.Load(path)
	if err != nil {
		return err
	}

	a, err := pipeline.Analyze(rootDir, pipeline.Options{Remainder: committed.Source.RemainderModule})
	if err != nil {
		return err
	}
	tmp, err := os.MkdirTemp("", "demono-diff-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.RemoveAll(tmp) }()

	e := &emit.Emitter{SrcDir: rootDir, OutDir: tmp, Graph: a.Graph, Place: a.Placement, Bound: a.Boundary}
	ems, err := e.Emit()
	if err != nil {
		return err
	}
	freshDirs := map[string]string{}
	for _, em := range ems {
		freshDirs[em.Module] = em.Dir
	}
	fresh, err := manifest.Build(a, rootDir, tmp, ems, time.Now(), toolString())
	if err != nil {
		return err
	}

	committedHashes, err := manifest.FileHashes(committed.ModuleDirs(rootDir))
	if err != nil {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, fmt.Sprintf("committed roots unreadable: %v", err))
		return reportDiff(rep, mode)
	}
	freshHashes, err := manifest.FileHashes(freshDirs)
	if err != nil {
		return err
	}

	rep.ChangedFiles = diffHashes(freshHashes, committedHashes)
	if len(rep.ChangedFiles) > 0 {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "committed roots differ from what the committed source produces")
	}
	if committed.EmitChecksum != manifest.ChecksumOf(committedHashes) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "committed roots were edited after the manifest was written (emit_checksum mismatch)")
	}
	if !manifest.SemanticEqual(fresh, committed) {
		rep.Differs = true
		rep.Reasons = append(rep.Reasons, "manifest content differs from what the committed source produces")
	}
	return reportDiff(rep, mode)
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

func reportDiff(rep diffReport, mode outputMode) error {
	if mode == outputJSON {
		if err := printJSON(rep); err != nil {
			return err
		}
	} else if rep.Differs {
		outln("Committed output differs from the committed source:")
		for _, r := range rep.Reasons {
			outf("  - %s\n", r)
		}
		for _, f := range rep.ChangedFiles {
			outf("    %s\n", f)
		}
	} else {
		outf("In sync: committed roots and manifest %s match the committed source.\n", rep.Manifest)
	}
	if rep.Differs {
		return verdictf("committed output differs from the committed source; re-run `demonolith refactor` and commit its output")
	}
	return nil
}
