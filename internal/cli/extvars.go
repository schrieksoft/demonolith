package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"

	"github.com/schrieksoft/demonolith/internal/boundary"
)

// collectExternalInputs resolves the modules' external (former root var.*)
// inputs the way the monolith resolved them. Only names the boundary actually
// declares as external are collected; values stay in memory. Returns the
// values and the sorted names that were resolved.
func collectExternalInputs(rootDir string, bound *boundary.Result, varFiles, varFlags []string) (map[string]string, []string, error) {
	needed := map[string]bool{}
	for _, b := range bound.Boundaries {
		for name, in := range b.Inputs {
			if in.External {
				needed[name] = true
			}
		}
	}
	vals, err := collectVarValues(rootDir, varFiles, varFlags, needed)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	return vals, names, nil
}

// resolvedVar is a variable value plus where the engine's precedence found
// it — the provenance the interactive walkthrough displays.
type resolvedVar struct {
	Value  string
	Source string
}

// collectVarProvenance gathers variable values in the engine's own
// (ascending) precedence — TF_VAR_* environment first, then the root's
// terraform.tfvars and *.auto.tfvars (plus their .json forms) in load order,
// then explicit --var-file files in the order given, then --var flags — and
// records, per name, which source won. A nil needed set collects every name
// found; otherwise only the named variables are kept.
func collectVarProvenance(rootDir string, varFiles, varFlags []string, needed map[string]bool) (map[string]resolvedVar, error) {
	keep := func(name string) bool { return needed == nil || needed[name] }
	vals := map[string]resolvedVar{}

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_VAR_") {
			continue
		}
		eq := strings.Index(kv, "=")
		name := strings.TrimPrefix(kv[:eq], "TF_VAR_")
		if keep(name) {
			vals[name] = resolvedVar{Value: kv[eq+1:], Source: "TF_VAR_" + name + " (environment)"}
		}
	}

	files, err := tfvarsFiles(rootDir)
	if err != nil {
		return nil, err
	}
	// Auto-loaded files may be absent; an explicit --var-file must exist.
	for _, vf := range varFiles {
		if _, err := os.Stat(vf); err != nil {
			return nil, fmt.Errorf("--var-file %s: %w", vf, err)
		}
	}
	sources := make([]string, 0, len(files)+len(varFiles))
	for _, f := range files {
		sources = append(sources, filepath.Base(f))
	}
	for _, vf := range varFiles {
		files = append(files, vf)
		sources = append(sources, "--var-file "+vf)
	}
	for i, f := range files {
		fileVals, err := readTfvars(f)
		if err != nil {
			return nil, err
		}
		for k, v := range fileVals {
			if keep(k) {
				vals[k] = resolvedVar{Value: v, Source: sources[i]}
			}
		}
	}

	for _, kv := range varFlags {
		eq := strings.Index(kv, "=")
		if eq < 1 {
			return nil, fmt.Errorf("invalid --var %q (want name=value)", kv)
		}
		name := kv[:eq]
		if keep(name) {
			vals[name] = resolvedVar{Value: kv[eq+1:], Source: "--var"}
		}
	}
	return vals, nil
}

// collectVarValues is collectVarProvenance without the provenance.
func collectVarValues(rootDir string, varFiles, varFlags []string, needed map[string]bool) (map[string]string, error) {
	rv, err := collectVarProvenance(rootDir, varFiles, varFlags, needed)
	if err != nil {
		return nil, err
	}
	vals := make(map[string]string, len(rv))
	for k, v := range rv {
		vals[k] = v.Value
	}
	return vals, nil
}

// moduleVarDecls parses a carved root's *.tf files and returns the variables
// it declares — the duplicated root declarations plus the generated boundary
// inputs — mapped to whether each carries a default value.
func moduleVarDecls(dir string) (map[string]bool, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tf") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, diags := hclsyntax.ParseConfig(b, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, fmt.Errorf("parse %s: %s", path, diags.Error())
		}
		for _, blk := range f.Body.(*hclsyntax.Body).Blocks {
			if blk.Type == "variable" && len(blk.Labels) == 1 {
				_, hasDefault := blk.Body.Attributes["default"]
				out[blk.Labels[0]] = hasDefault
			}
		}
	}
	return out, nil
}

// moduleVarNames is moduleVarDecls reduced to a name set.
func moduleVarNames(dir string) (map[string]bool, error) {
	decls, err := moduleVarDecls(dir)
	if err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(decls))
	for name := range decls {
		out[name] = true
	}
	return out, nil
}

// tfvarsFiles lists the root's variable files in terraform's own load order:
// terraform.tfvars(.json) first, then *.auto.tfvars(.json) lexically.
func tfvarsFiles(rootDir string) ([]string, error) {
	entries, err := os.ReadDir(rootDir)
	if err != nil {
		return nil, err
	}
	var base, auto []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		switch {
		case name == "terraform.tfvars" || name == "terraform.tfvars.json":
			base = append(base, filepath.Join(rootDir, name))
		case strings.HasSuffix(name, ".auto.tfvars") || strings.HasSuffix(name, ".auto.tfvars.json"):
			auto = append(auto, filepath.Join(rootDir, name))
		}
	}
	sort.Strings(base)
	sort.Strings(auto)
	return append(base, auto...), nil
}

// readTfvars evaluates a tfvars file's literal assignments to strings. An
// expression that needs context (references, functions) cannot be resolved
// offline and is skipped rather than guessed.
func readTfvars(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	out := map[string]string{}

	if strings.HasSuffix(path, ".json") {
		var raw map[string]any
		if err := json.Unmarshal(b, &raw); err != nil {
			return nil, fmt.Errorf("parse %s: %w", path, err)
		}
		for k, v := range raw {
			out[k] = jsonValueString(v)
		}
		return out, nil
	}

	f, diags := hclsyntax.ParseConfig(b, path, hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse %s: %s", path, diags.Error())
	}
	body := f.Body.(*hclsyntax.Body)
	for name, attr := range body.Attributes {
		v, vdiags := attr.Expr.Value(nil)
		if vdiags.HasErrors() {
			continue
		}
		if s, ok := ctyValueString(v); ok {
			out[name] = s
		}
	}
	return out, nil
}

// ctyValueString renders a cty value the way values are passed downstream:
// scalars bare, composites as compact JSON.
func ctyValueString(v cty.Value) (string, bool) {
	if v.IsNull() || !v.IsWhollyKnown() {
		return "", false
	}
	switch v.Type() {
	case cty.String:
		return v.AsString(), true
	case cty.Number:
		return v.AsBigFloat().Text('f', -1), true
	case cty.Bool:
		if v.True() {
			return "true", true
		}
		return "false", true
	}
	b, err := ctyjson.Marshal(v, v.Type())
	if err != nil {
		return "", false
	}
	return string(b), true
}

// jsonValueString mirrors ctyValueString for JSON-decoded values.
func jsonValueString(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case bool:
		if t {
			return "true"
		}
		return "false"
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("%d", int64(t))
		}
		return fmt.Sprintf("%g", t)
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return fmt.Sprintf("%v", t)
		}
		return string(b)
	}
}
