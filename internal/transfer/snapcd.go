package transfer

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

// SnapcdTargetFile is the Snap CD root file the wiring is appended into.
const SnapcdTargetFile = "main.tf"

// ResolveSnapcdRoot resolves the Snap CD root directory: the flag value
// against the source root's parent (the sibling convention). Returns "" when
// the default location has no directory and the flag was not set explicitly.
func ResolveSnapcdRoot(rootDir, flag string, explicit bool) (string, error) {
	abs := flag
	if !filepath.IsAbs(flag) {
		abs = filepath.Clean(filepath.Join(filepath.Dir(rootDir), flag))
	}
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		if explicit {
			return "", fmt.Errorf("--snapcd-root: no directory at %s", abs)
		}
		return "", nil
	}
	return abs, nil
}

// MatchSnapcdModules parses the Snap CD root and maps each involved root
// (the source and every receiver, identified by directory basename) to the
// name of its `resource "snapcd_module"` block, matched on the trailing
// literal of the block's source_subdirectory. Every involved root must match
// exactly one module.
func MatchSnapcdModules(snapcdDir, rootDir string, receiverPaths map[string]string) (map[string]string, error) {
	type mod struct{ name, subdir string }
	var mods []mod
	entries, err := os.ReadDir(snapcdDir)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(snapcdDir, e.Name()))
		if err != nil {
			return nil, err
		}
		f, diags := hclwrite.ParseConfig(src, e.Name(), hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", filepath.Join(snapcdDir, e.Name()), diags.Error())
		}
		for _, blk := range f.Body().Blocks() {
			if blk.Type() != "resource" || len(blk.Labels()) != 2 || blk.Labels()[0] != "snapcd_module" {
				continue
			}
			attr := blk.Body().GetAttribute("source_subdirectory")
			if attr == nil {
				continue
			}
			mods = append(mods, mod{name: blk.Labels()[1], subdir: trailingLiteral(attr.Expr().BuildTokens(nil))})
		}
	}

	want := map[string]string{"source": filepath.Base(rootDir)}
	names := []string{"source"}
	for label, p := range receiverPaths {
		want[label] = filepath.Base(p)
		names = append(names, label)
	}
	sort.Strings(names)

	out := map[string]string{}
	for _, label := range names {
		base := want[label]
		var matches []string
		for _, m := range mods {
			if m.subdir == base || strings.HasSuffix(m.subdir, "/"+base) {
				matches = append(matches, m.name)
			}
		}
		switch len(matches) {
		case 1:
			out[label] = matches[0]
		case 0:
			return nil, fmt.Errorf("Snap CD root %s has no snapcd_module whose source_subdirectory ends in %q (for %s)", snapcdDir, base, label)
		default:
			sort.Strings(matches)
			return nil, fmt.Errorf("Snap CD root %s has several snapcd_module resources matching %q: %s", snapcdDir, base, strings.Join(matches, ", "))
		}
	}
	return out, nil
}

// trailingLiteral extracts the last quoted-literal run of an expression's
// tokens — the part of `"${var.prefix}roots/app"` or `"roots/app"` that names
// the directory.
func trailingLiteral(toks hclwrite.Tokens) string {
	var b strings.Builder
	for _, t := range toks {
		b.WriteString(string(t.Bytes))
	}
	s := b.String()
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, `"`)
	// Cut everything through the last interpolation, then take what remains.
	if i := strings.LastIndex(s, "}"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.LastIndex(s, `"`); i >= 0 {
		s = s[i+1:]
	}
	return s
}

// SnapcdFileHCL builds the wiring file for the Snap CD root: one
// snapcd_module_input_from_output per cross edge and one
// snapcd_depends_on_module per ordering edge, referencing the matched
// snapcd_module resources. Empty when the transfer creates no edges.
func SnapcdFileHCL(m *Map) string {
	if m.Snapcd == nil || (len(m.CrossEdges) == 0 && len(m.OrderingEdges) == 0) {
		return ""
	}
	modName := func(root string) string {
		if root == m.Remainder {
			return m.Snapcd.Modules["source"]
		}
		return m.Snapcd.Modules[root]
	}
	var b strings.Builder
	for _, e := range m.CrossEdges {
		fmt.Fprintf(&b, `resource "snapcd_module_input_from_output" "%s_%s" {
  input_kind       = "Param"
  module_id        = snapcd_module.%s.id
  name             = %q
  output_module_id = snapcd_module.%s.id
  output_name      = %q
}

`, modName(e.Consumer), e.Input, modName(e.Consumer), e.Input, modName(e.Producer), e.Output)
	}
	for _, e := range m.OrderingEdges {
		fmt.Fprintf(&b, `resource "snapcd_depends_on_module" "%s_on_%s" {
  module_id            = snapcd_module.%s.id
  depends_on_module_id = snapcd_module.%s.id
}

`, modName(e.Consumer), modName(e.Producer), modName(e.Consumer), modName(e.Producer))
	}
	return b.String()
}

// SnapcdWiringAddrs returns the addresses SnapcdFileHCL declares, for the
// already-appended (retry) check.
func SnapcdWiringAddrs(m *Map) []string {
	if m.Snapcd == nil {
		return nil
	}
	modName := func(root string) string {
		if root == m.Remainder {
			return m.Snapcd.Modules["source"]
		}
		return m.Snapcd.Modules[root]
	}
	var out []string
	for _, e := range m.CrossEdges {
		out = append(out, "snapcd_module_input_from_output."+modName(e.Consumer)+"_"+e.Input)
	}
	for _, e := range m.OrderingEdges {
		out = append(out, "snapcd_depends_on_module."+modName(e.Consumer)+"_on_"+modName(e.Producer))
	}
	return out
}
