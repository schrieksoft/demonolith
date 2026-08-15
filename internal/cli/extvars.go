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
// inputs the way the monolith resolved them, in ascending precedence: the
// source root's terraform.tfvars and *.auto.tfvars (plus their .json forms),
// then explicit --var-file files in the order given, then TF_VAR_* environment
// variables, then explicit --var flags. Only names the boundary actually
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

	vals := map[string]string{}

	files, err := tfvarsFiles(rootDir)
	if err != nil {
		return nil, nil, err
	}
	// Auto-loaded files may be absent; an explicit --var-file must exist.
	for _, vf := range varFiles {
		if _, err := os.Stat(vf); err != nil {
			return nil, nil, fmt.Errorf("--var-file %s: %w", vf, err)
		}
	}
	files = append(files, varFiles...)
	for _, f := range files {
		fileVals, err := readTfvars(f)
		if err != nil {
			return nil, nil, err
		}
		for k, v := range fileVals {
			if needed[k] {
				vals[k] = v
			}
		}
	}

	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, "TF_VAR_") {
			continue
		}
		eq := strings.Index(kv, "=")
		name := strings.TrimPrefix(kv[:eq], "TF_VAR_")
		if needed[name] {
			vals[name] = kv[eq+1:]
		}
	}

	for _, kv := range varFlags {
		eq := strings.Index(kv, "=")
		if eq < 1 {
			return nil, nil, fmt.Errorf("invalid --var %q (want name=value)", kv)
		}
		name := kv[:eq]
		if needed[name] {
			vals[name] = kv[eq+1:]
		}
	}

	names := make([]string, 0, len(vals))
	for k := range vals {
		names = append(names, k)
	}
	sort.Strings(names)
	return vals, names, nil
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
