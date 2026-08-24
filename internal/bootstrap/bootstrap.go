// Package bootstrap emits the Snap CD bootstrap module: a Terraform root of
// snapcd_* resources that instructs Snap CD to deploy the carved modules — one
// snapcd_module per carved root, the manifest's cross edges realized as
// snapcd_module_input_from_output wirings, its ordering edges as
// snapcd_depends_on_module, and external inputs passed through as
// snapcd_module_input_from_literal bound to the bootstrap's own variables.
//
// It generates from the manifest alone: everything a control plane needs is in
// the public contract, which is the point. The one exception is the README,
// which carries clone-local git values as apply-time hints and is excluded
// from the emit checksum for that reason.
package bootstrap

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
)

// DirName is the bootstrap module's directory name under the output dir. The
// carved-module name "snapcd" is reserved for it.
const DirName = "snapcd"

// ProviderVersion is the schrieksoft/snapcd provider release the emitted
// bootstrap is generated against.
const ProviderVersion = "1.5.0"

// reservedVars are the bootstrap's own variables; an external monolith input
// with one of these names would collide and must be renamed first.
var reservedVars = []string{
	"client_id", "client_secret", "organization_id", "snapcd_server_url",
	"insecure_skip_verify", "create_stack", "create_namespace", "stack_name",
	"runner_name", "namespace_name", "engine", "source_url", "source_revision",
	"source_subdirectory_prefix",
}

// Emit writes the bootstrap module under outDir and returns its dir. backend,
// when set, derives the bootstrap's own backend block the same way every
// carved module gets one, postfixed with the bootstrap's name.
func Emit(m *manifest.Manifest, rootDir, outDir string, backend *emit.BackendBlock) (string, error) {
	dir := filepath.Join(outDir, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	names := moduleNames(m)
	ext := externalInputs(m)
	rootIns := rootInputs(m)
	reserved := map[string]bool{}
	for _, r := range reservedVars {
		reserved[r] = true
	}
	for _, name := range append(append([]string{}, ext...), rootIns...) {
		if reserved[name] {
			return "", fmt.Errorf("external input %q collides with a bootstrap variable of the same name; rename the monolith variable before refactoring", name)
		}
	}
	rootDecls, err := emit.SourceVariableBlocks(rootDir, rootIns)
	if err != nil {
		return "", err
	}

	rootTf, err := buildRoot(backend)
	if err != nil {
		return "", err
	}
	files := map[string]string{
		"root.tf":      rootTf,
		"providers.tf": buildProviders(),
		"main.tf":      buildMain(m, names),
		"variables.tf": buildVariables(ext, rootIns, rootDecls),
	}
	for name, content := range files {
		if err := os.WriteFile(filepath.Join(dir, name), hclwrite.Format([]byte(content)), 0o644); err != nil {
			return "", err
		}
	}
	readme := buildReadme(rootDir, backend != nil, len(ext)+len(rootIns) > 0)
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0o644); err != nil {
		return "", err
	}
	if err := emit.WriteGitignore(dir); err != nil {
		return "", err
	}
	return dir, nil
}

func moduleNames(m *manifest.Manifest) []string {
	names := make([]string, 0, len(m.Modules))
	for name := range m.Modules {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// externalInputs is the sorted union of all modules' external input names.
func externalInputs(m *manifest.Manifest) []string {
	set := map[string]bool{}
	for _, mod := range m.Modules {
		for _, n := range mod.ExternalInputs {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// rootInputs is the sorted union of all modules' carried root variable names.
func rootInputs(m *manifest.Manifest) []string {
	set := map[string]bool{}
	for _, mod := range m.Modules {
		for _, n := range mod.RootInputs {
			set[n] = true
		}
	}
	out := make([]string, 0, len(set))
	for n := range set {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// buildRoot builds root.tf: the terraform{} block pinning the snapcd provider,
// plus the bootstrap's own derived backend when the monolith has one.
func buildRoot(backend *emit.BackendBlock) (string, error) {
	f := hclwrite.NewEmptyFile()
	tfb := f.Body().AppendNewBlock("terraform", nil)
	rp := tfb.Body().AppendNewBlock("required_providers", nil)
	rp.Body().SetAttributeValue("snapcd", cty.ObjectVal(map[string]cty.Value{
		"source":  cty.StringVal("registry.terraform.io/schrieksoft/snapcd"),
		"version": cty.StringVal(ProviderVersion),
	}))
	if backend != nil {
		bb, err := backend.BackendHCL(DirName)
		if err != nil {
			return "", err
		}
		tfb.Body().AppendNewline()
		tfb.Body().AppendBlock(bb)
	}
	return string(f.Bytes()), nil
}

func buildProviders() string {
	return `provider "snapcd" {
  client_id            = var.client_id
  client_secret        = var.client_secret
  organization_id      = var.organization_id
  url                  = var.snapcd_server_url
  insecure_skip_verify = var.insecure_skip_verify
}
`
}

// buildReadme builds the bootstrap's README. The source_url/source_revision
// exports carry the clone's actual git remote and branch as apply-time hints;
// the README is excluded from the emit checksum so they never affect it.
func buildReadme(rootDir string, hasBackend, hasExt bool) string {
	url, branch := gitGuess(rootDir)
	var b strings.Builder
	b.WriteString("# How to run\n\n```\n")
	if hasBackend {
		b.WriteString("source " + emit.EnvFileName + "\n")
	}
	b.WriteString("tofu init\n")
	fmt.Fprintf(&b, "export TF_VAR_source_url=%q\n", url)
	fmt.Fprintf(&b, "export TF_VAR_source_revision=%q\n", branch)
	cmd := "tofu apply"
	if hasExt {
		cmd += " --var-file demono.root.tfvars"
	}
	b.WriteString(cmd + "\n```\n")
	if hasBackend || hasExt {
		b.WriteString("\nThe demono.* files are written by the `demonolith migrate` pipeline.\n")
	}
	return b.String()
}

// gitGuess reads the root repo's origin URL and current branch for the README
// hints, falling back to placeholders outside a usable git checkout.
func gitGuess(rootDir string) (url, branch string) {
	url, branch = "<your-repo-url>", "main"
	if out, err := exec.Command("git", "-C", rootDir, "remote", "get-url", "origin").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" {
			url = s
		}
	}
	if out, err := exec.Command("git", "-C", rootDir, "rev-parse", "--abbrev-ref", "HEAD").Output(); err == nil {
		if s := strings.TrimSpace(string(out)); s != "" && s != "HEAD" {
			branch = s
		}
	}
	return url, branch
}

func buildMain(m *manifest.Manifest, names []string) string {
	var b strings.Builder

	b.WriteString(`resource "snapcd_stack" "this" {
  count = var.create_stack ? 1 : 0
  name  = var.stack_name
}

data "snapcd_stack" "this" {
  count = var.create_stack ? 0 : 1
  name  = var.stack_name
}

resource "snapcd_namespace" "this" {
  count          = var.create_namespace ? 1 : 0
  name           = var.namespace_name
  stack_id       = local.stack_id
  default_engine = var.engine
`)
	if m.Output.Monorepo {
		// Monorepo carve: modules should only redeploy when a commit touches
		// their own directory, not on every commit to the shared repo.
		b.WriteString("  default_trigger_path_filter_enabled = true\n")
	}
	b.WriteString(`}

data "snapcd_namespace" "this" {
  count    = var.create_namespace ? 0 : 1
  name     = var.namespace_name
  stack_id = local.stack_id
}

locals {
  stack_id     = one(concat(snapcd_stack.this[*].id, data.snapcd_stack.this[*].id))
  namespace_id = one(concat(snapcd_namespace.this[*].id, data.snapcd_namespace.this[*].id))
}

data "snapcd_runner" "this" {
  name = var.runner_name
}

`)

	// One snapcd_module per carved root, subdirectory from the manifest.
	for _, name := range names {
		subdir := filepath.ToSlash(m.Modules[name].Dir)
		fmt.Fprintf(&b, `resource "snapcd_module" %q {
  name                = %q
  namespace_id        = local.namespace_id
  source_url          = var.source_url
  source_revision     = var.source_revision
  source_subdirectory = "${var.source_subdirectory_prefix}%s"
  runner_id           = data.snapcd_runner.this.id
  engine              = var.engine
}

`, name, name, subdir)
	}

	// Cross edges: producer output threaded into consumer input, deduplicated
	// per (consumer, input) — several consumer blocks can share one wiring.
	seen := map[string]bool{}
	for _, e := range m.CrossEdges {
		key := e.ConsumerModule + "\x00" + e.Input
		if seen[key] {
			continue
		}
		seen[key] = true
		fmt.Fprintf(&b, `resource "snapcd_module_input_from_output" "%s_%s" {
  input_kind       = "Param"
  module_id        = snapcd_module.%s.id
  name             = %q
  output_module_id = snapcd_module.%s.id
  output_name      = %q
}

`, e.ConsumerModule, e.Input, e.ConsumerModule, e.Input, e.ProducerModule, e.Output)
	}

	// Ordering edges: whole-module dependencies with no value, deduplicated
	// per module pair.
	seenDep := map[string]bool{}
	for _, e := range m.OrderingEdges {
		key := e.ConsumerModule + "\x00" + e.ProducerModule
		if seenDep[key] {
			continue
		}
		seenDep[key] = true
		fmt.Fprintf(&b, `resource "snapcd_depends_on_module" "%s_on_%s" {
  module_id            = snapcd_module.%s.id
  depends_on_module_id = snapcd_module.%s.id
}

`, e.ConsumerModule, e.ProducerModule, e.ConsumerModule, e.ProducerModule)
	}

	// Root-variable inputs: every value a module's carved code takes from the
	// monolith's root — declared variables it carries and undeclared external
	// inputs alike — passed through from the bootstrap's own variables.
	for _, name := range names {
		inputs := append(append([]string{}, m.Modules[name].RootInputs...), m.Modules[name].ExternalInputs...)
		sort.Strings(inputs)
		for _, in := range inputs {
			fmt.Fprintf(&b, `resource "snapcd_module_input_from_literal" "%s_%s" {
  input_kind    = "Param"
  module_id     = snapcd_module.%s.id
  name          = %q
  literal_value = var.%s
  type          = "String"
}

`, name, in, name, in, in)
		}
	}

	return b.String()
}

func buildVariables(ext, rootIns []string, rootDecls map[string]string) string {
	var b strings.Builder
	b.WriteString(`variable "client_id" {
  description = "Client ID for Snap CD authentication"
  type        = string
  default     = "default"
}

variable "client_secret" {
  description = "Client Secret for Snap CD authentication"
  type        = string
  sensitive   = true
  default     = "default"
}

variable "organization_id" {
  description = "Snap CD Organization ID"
  type        = string
  default     = "10000000-0000-0000-0000-000000000000"
}

variable "snapcd_server_url" {
  description = "Snap CD Server URL, reachable both from where this root is applied and from inside the Runner"
  type        = string
  default     = "http://localhost:5000"
}

variable "insecure_skip_verify" {
  description = "Skip TLS verification against the Snap CD Server"
  type        = bool
  default     = true
}

variable "create_stack" {
  description = "Create the Stack instead of reusing an existing one by name"
  type        = bool
  default     = false
}

variable "create_namespace" {
  description = "Create the Namespace instead of reusing an existing one by name"
  type        = bool
  default     = true
}

variable "stack_name" {
  description = "Name of the Stack to deploy into"
  type        = string
  default     = "default"
}

variable "runner_name" {
  description = "Name of the registered Runner that executes the modules"
  type        = string
  default     = "default"
}

variable "namespace_name" {
  description = "Name of the Namespace the modules deploy into"
  type        = string
  default     = "demonolith-bootstrap"
}

variable "engine" {
  description = "Engine the modules run with"
  type        = string
  default     = "OpenTofu"
}

variable "source_url" {
  description = "Git URL of the repository holding the new module directories"
  type        = string
}

variable "source_revision" {
  description = "Git revision (branch or tag) of the new module directories"
  type        = string
  default     = "main"
}

variable "source_subdirectory_prefix" {
  description = "Path from the repository root to the monolith root, with a trailing slash; empty when the monolith is the repository root"
  type        = string
  default     = ""
}

`)
	// Carried root variables keep the monolith's own declarations, so type,
	// default, description and sensitivity survive into the bootstrap.
	for _, name := range rootIns {
		if decl, ok := rootDecls[name]; ok {
			b.WriteString(decl)
			b.WriteString("\n")
		}
	}
	for _, name := range ext {
		fmt.Fprintf(&b, `variable %q {
  description = "External input passed through to the consuming modules (was var.%s in the monolith)"
  type        = string
}

`, name, name)
	}
	return b.String()
}
