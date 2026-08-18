package cli

import (
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/schrieksoft/demonolith/internal/boundary"
	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
)

// migrateInputsWizard is the guided front half of the bare `migrate -i`: it
// walks every input the migration consumes — engine, state source, variable
// values with their provenance, backend config and credentials, ambient
// provider environment — pre-filling answers from any flags already given.
// Every accepted choice resolves back into flags on f, so the pipeline that
// follows behaves exactly like the equivalent non-interactive run (which the
// wizard prints before handing over).
func migrateInputsWizard(f *migrateFlags) (string, *manifest.Manifest, error) {
	outln("Interactive migration — Enter keeps the value in brackets.")

	rootIn, err := promptString("Monolith root", f.rootDir)
	if err != nil {
		return "", nil, err
	}
	f.rootDir = rootIn
	rootDir := resolveRoot(rootIn)
	m, err := loadRunManifest(rootDir)
	if err != nil {
		return "", nil, err
	}
	a, err := analyzeMatching(rootDir, m)
	if err != nil {
		return "", nil, err
	}

	if f.engine == "" && f.execPath == "" {
		engine, err := promptEngine()
		if err != nil {
			return "", nil, err
		}
		f.engine = engine
	} else if f.engine != "" {
		outf("Engine: %s (from --engine)\n", f.engine)
	}

	sf, err := promptString("Local .tfstate copy to split, if you have one (empty = pull the monolith's state from its backend)", f.stateFile)
	if err != nil {
		return "", nil, err
	}
	f.stateFile = sf

	if err := wizardVariables(rootDir, m, a.Boundary, f); err != nil {
		return "", nil, err
	}
	if err := wizardBackend(rootDir, m, f); err != nil {
		return "", nil, err
	}
	if err := wizardAmbient(rootDir); err != nil {
		return "", nil, err
	}

	outf("\n%s\n  %s\n", heading("Equivalent non-interactive run:"), emphasis(equivalentMigrateCommand(f)))
	return rootDir, m, nil
}

// wizardVariables checks every variable value the migration consumes — the
// root variables the carved modules declare (cross-module inputs excluded;
// those are threaded from producer plans) plus the boundary's external
// inputs — against the engine's precedence (TF_VAR_* env, the root's tfvars
// files, --var-file, --var). Only gaps are shown: required variables with no
// value anywhere, attributed to the modules declaring them. The loop accepts
// name=value (a --var) and @path (a --var-file) until the gaps are filled or
// the user explicitly continues.
func wizardVariables(rootDir string, m *manifest.Manifest, bound *boundary.Result, f *migrateFlags) error {
	needed := map[string]bool{}
	declaredBy := map[string]map[string]bool{}
	note := func(name, module string) {
		needed[name] = true
		if declaredBy[name] == nil {
			declaredBy[name] = map[string]bool{}
		}
		declaredBy[name][module] = true
	}
	cross := map[string]bool{}
	for _, b := range bound.Boundaries {
		for name, in := range b.Inputs {
			if in.External {
				note(name, b.Module)
			} else {
				cross[name] = true
			}
		}
	}
	// hasDefault[name] is true only when every module declaring the variable
	// gives it a default.
	hasDefault := map[string]bool{}
	for module, dir := range m.ModuleDirs(rootDir) {
		decls, err := moduleVarDecls(dir)
		if err != nil {
			return err
		}
		for name, def := range decls {
			if cross[name] {
				continue
			}
			if prev, seen := hasDefault[name]; seen {
				hasDefault[name] = prev && def
			} else {
				hasDefault[name] = def
			}
			note(name, module)
		}
	}

	for {
		rv, err := collectVarProvenance(rootDir, f.varFiles, f.vars, needed)
		if err != nil {
			return err
		}
		var gaps []string
		for n := range needed {
			if _, ok := rv[n]; !ok && !hasDefault[n] {
				gaps = append(gaps, n)
			}
		}
		if len(gaps) == 0 {
			outf("\n%s all %d resolved from tfvars, environment, and flags.\n", heading("Variable values:"), len(needed))
			return nil
		}
		sort.Strings(gaps)
		outf("\n%s %d of %d have no value and no default:\n", heading("Variable values:"), len(gaps), len(needed))
		for _, n := range gaps {
			mods := make([]string, 0, len(declaredBy[n]))
			for md := range declaredBy[n] {
				mods = append(mods, md)
			}
			sort.Strings(mods)
			outf("  %s %s\n", fail(fmt.Sprintf("%-32s", n)), dim("needed by: "+strings.Join(mods, ", ")))
		}
		in, err := promptLine("\nProvide a value (name=value), load a file (@path), or Enter to continue: ")
		if err != nil {
			return err
		}
		switch {
		case in == "":
			ok, err := promptYesNo(fmt.Sprintf("%d required value(s) still missing — the proof will fail without them; continue anyway?", len(gaps)), false)
			if err != nil {
				return err
			}
			if !ok {
				continue
			}
			return nil
		case strings.HasPrefix(in, "@"):
			path := strings.TrimPrefix(in, "@")
			if _, err := os.Stat(path); err != nil {
				outf("  %v\n", err)
				continue
			}
			f.varFiles = append(f.varFiles, path)
		case strings.Contains(in, "="):
			f.vars = append(f.vars, in)
		default:
			outln("  unrecognized; want name=value, @path, or Enter")
		}
	}
}

// wizardBackend previews the derived backend, names the credential attributes
// found in the init-time resolved config (values never shown), and loops
// accepting extra -backend-config values for init.
func wizardBackend(rootDir string, m *manifest.Manifest, f *migrateFlags) error {
	if m.Backend == nil {
		outln("\nBackend: none declared — each module gets a local terraform.tfstate.")
		return nil
	}
	outf("\n%s, derived per module:\n", heading(fmt.Sprintf("Backend (%s)", m.Backend.Type)))
	mods := make([]string, 0, len(m.Backend.Modules))
	for name := range m.Backend.Modules {
		mods = append(mods, name)
	}
	sort.Strings(mods)
	for _, name := range mods {
		outf("  %-16s %s\n", name, m.Backend.Modules[name])
	}
	block, err := emit.ParseBackend(rootDir)
	if err != nil {
		return err
	}
	if block != nil {
		if creds := block.CredentialEnv(); len(creds) > 0 {
			keys := make([]string, 0, len(creds))
			for k := range creds {
				keys = append(keys, k)
			}
			sort.Strings(keys)
			outln("\n" + heading("Backend credentials") + " (to per-module " + emit.EnvFileName + "):")
			for _, k := range keys {
				outf("  %s\n", k)
			}
		} else {
			outf("\nNo credentials in the init-time resolved config; the backend either needs none, carries them in HCL, or expects them from this shell.\n")
		}
	}
	if len(f.backendConfig) > 0 {
		outf("Extra -backend-config from flags: %s\n", strings.Join(f.backendConfig, ", "))
	}
	for {
		in, err := promptLine("\nAdditional -backend-config for init (key=value), or Enter to continue: ")
		if err != nil {
			return err
		}
		if in == "" {
			return nil
		}
		if !strings.Contains(in, "=") {
			outln("  want key=value")
			continue
		}
		f.backendConfig = append(f.backendConfig, in)
	}
}

// wizardAmbient states the ambient-credentials contract — provider
// credentials cannot be reliably detected and are never captured — and asks
// for one confirmation that this is the session the monolith works in.
func wizardAmbient(rootDir string) error {
	providers, err := emit.RequiredProviderNames(rootDir)
	if err != nil {
		return err
	}
	outf("\nProvider credentials (for %s) are inherited from this shell — demonolith cannot reliably detect them and never captures them.\n", strings.Join(providers, ", "))
	ok, err := promptYesNo("\nIs this the shell session in which the monolith inits and plans cleanly?", true)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("aborted: prepare the session (source credentials, init and plan the monolith) and re-run")
	}
	return nil
}

// equivalentMigrateCommand renders the flags the wizard's answers resolve to.
func equivalentMigrateCommand(f *migrateFlags) string {
	parts := []string{"demonolith migrate"}
	if f.engine != "" {
		parts = append(parts, "--engine "+f.engine)
	}
	if f.execPath != "" {
		parts = append(parts, "--exec-path "+f.execPath)
	}
	if f.rootDir != "" && f.rootDir != "." {
		parts = append(parts, "--root-dir "+f.rootDir)
	}
	if f.stateFile != "" {
		parts = append(parts, "--state-file "+f.stateFile)
	}
	for _, vf := range f.varFiles {
		parts = append(parts, "--var-file "+vf)
	}
	for _, v := range f.vars {
		parts = append(parts, fmt.Sprintf("--var %q", v))
	}
	for _, bc := range f.backendConfig {
		parts = append(parts, fmt.Sprintf("--backend-config %q", bc))
	}
	return strings.Join(parts, " ")
}
