// Package bootstrap emits the Snap CD bootstrap module: a Terraform root of
// snapcd_* resources that instructs Snap CD to deploy the carved modules — one
// snapcd_module per carved root, the manifest's cross edges realized as
// snapcd_module_input_from_output wirings, its ordering edges as
// snapcd_depends_on_module, and external inputs passed through as
// snapcd_module_input_from_literal bound to the bootstrap's own variables.
//
// It generates from the manifest alone: everything a control plane needs is in
// the public contract, which is the point.
package bootstrap

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hclwrite"

	"github.com/schrieksoft/demonolith/internal/emit"
	"github.com/schrieksoft/demonolith/internal/manifest"
)

// DirName is the bootstrap module's directory name under the output dir. The
// carved-module name "snapcd" is reserved for it.
const DirName = "snapcd"

// reservedVars are the bootstrap's own variables; an external monolith input
// with one of these names would collide and must be renamed first.
var reservedVars = []string{
	"client_id", "client_secret", "organization_id", "snapcd_server_url",
	"insecure_skip_verify", "stack_name", "runner_name", "namespace_name",
	"engine", "source_url", "source_revision", "source_subdirectory_prefix",
}

// Emit writes the bootstrap module under outDir and returns its dir.
func Emit(m *manifest.Manifest, rootDir, outDir string) (string, error) {
	dir := filepath.Join(outDir, DirName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}

	names := moduleNames(m)
	ext := externalInputs(m)
	reserved := map[string]bool{}
	for _, r := range reservedVars {
		reserved[r] = true
	}
	for _, name := range ext {
		if reserved[name] {
			return "", fmt.Errorf("external input %q collides with a bootstrap variable of the same name; rename the monolith variable before refactoring", name)
		}
	}

	mainTf := buildMain(m, rootDir, names)
	varsTf := buildVariables(m, ext)

	for name, content := range map[string]string{"main.tf": mainTf, "variables.tf": varsTf} {
		if err := os.WriteFile(filepath.Join(dir, name), hclwrite.Format([]byte(content)), 0o644); err != nil {
			return "", err
		}
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

func buildMain(m *manifest.Manifest, rootDir string, names []string) string {
	var b strings.Builder

	b.WriteString(`terraform {
  required_providers {
    snapcd = {
      source = "registry.terraform.io/schrieksoft/snapcd"
    }
  }
}

provider "snapcd" {
  client_id            = var.client_id
  client_secret        = var.client_secret
  organization_id      = var.organization_id
  url                  = var.snapcd_server_url
  insecure_skip_verify = var.insecure_skip_verify
}

data "snapcd_stack" "this" {
  name = var.stack_name
}

data "snapcd_runner" "this" {
  name = var.runner_name
}

resource "snapcd_namespace" "this" {
  name           = var.namespace_name
  stack_id       = data.snapcd_stack.this.id
  default_engine = var.engine
`)
	if m.Output.Monorepo {
		// Monorepo carve: modules should only redeploy when a commit touches
		// their own directory, not on every commit to the shared repo.
		b.WriteString("  default_trigger_path_filter_enabled = true\n")
	}
	b.WriteString("}\n\n")

	// One snapcd_module per carved root, subdirectory from the manifest.
	for _, name := range names {
		subdir := filepath.ToSlash(m.Modules[name].Dir)
		fmt.Fprintf(&b, `resource "snapcd_module" %q {
  name                = %q
  namespace_id        = snapcd_namespace.this.id
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

	// External inputs: passed through from the bootstrap's own variables.
	for _, name := range names {
		for _, in := range m.Modules[name].ExternalInputs {
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

func buildVariables(m *manifest.Manifest, ext []string) string {
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
  description = "Snap CD Server URL"
  type        = string
  default     = "http://localhost:5000"
}

variable "insecure_skip_verify" {
  description = "Skip TLS verification against the Snap CD Server"
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
  description = "Name of the Namespace this bootstrap creates"
  type        = string
  default     = "demonolith"
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
	for _, name := range ext {
		fmt.Fprintf(&b, `variable %q {
  description = "External input passed through to the consuming modules (was var.%s in the monolith)"
  type        = string
}

`, name, name)
	}
	return b.String()
}
