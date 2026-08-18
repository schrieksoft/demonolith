package emit

import (
	"encoding/json"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

// BackendBlock is the monolith's parsed backend configuration: the type label
// and the raw attribute expressions of the block body. Only attributes present
// in HCL are carried — resolved config (which can hold credentials supplied
// out-of-band at init) is never read into it.
type BackendBlock struct {
	Type string
	// block is the original hclwrite block, cloned per module at emit time.
	block *hclwrite.Block
	// resolved is the root's init-time backend config (scalars), used as the
	// fallback for location attributes absent from HCL and as the source of
	// credential values for per-module demono.env files. Never emitted into HCL.
	resolved map[string]string
}

// EnvFileName is the per-module backend-credentials file written by the
// migrate steps and sourced around each module's backend operations.
const EnvFileName = "demono.env"

// locationAttrs names, per backend type, the attributes that distinguish one
// state location from another — every built-in backend type of the
// open-source lineage. A dotted name ("workspaces.name") addresses an
// attribute inside a nested block.
var locationAttrs = map[string][]string{
	"local":      {"path"},
	"s3":         {"key"},
	"azurerm":    {"key"},
	"gcs":        {"prefix"},
	"consul":     {"path"},
	"http":       {"address", "lock_address", "unlock_address"},
	"cos":        {"key"},
	"oss":        {"key"},
	"kubernetes": {"secret_suffix"},
	"pg":         {"schema_name"},
	"remote":     {"workspaces.name"},
}

// SupportedBackend reports whether derivation knows the given backend type.
func SupportedBackend(t string) bool {
	_, ok := locationAttrs[t]
	return ok
}

// supportedTypesList renders the supported backend types for error messages.
func supportedTypesList() string {
	names := make([]string, 0, len(locationAttrs))
	for t := range locationAttrs {
		names = append(names, t)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// ParseBackend finds the monolith's `terraform { backend "<type>" {...} }`
// block. Returns nil when the root declares no backend.
func ParseBackend(srcDir string) (*BackendBlock, error) {
	files, err := tfFiles(srcDir)
	if err != nil {
		return nil, err
	}
	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		f, diags := hclwrite.ParseConfig(src, path, hcl.Pos{Line: 1, Column: 1})
		if diags.HasErrors() {
			return nil, diags
		}
		for _, blk := range f.Body().Blocks() {
			if blk.Type() != "terraform" {
				continue
			}
			for _, inner := range blk.Body().Blocks() {
				if inner.Type() == "backend" && len(inner.Labels()) == 1 {
					t := inner.Labels()[0]
					return &BackendBlock{Type: t, block: inner, resolved: resolvedBackendConfig(srcDir, t)}, nil
				}
				// A cloud block is not a backend, but treating it as "no
				// backend" would silently local-seed a workspace-managed
				// monolith; surface it so derivation can refuse loudly.
				if inner.Type() == "cloud" {
					return &BackendBlock{Type: "cloud", block: inner}, nil
				}
			}
		}
	}
	return nil, nil
}

// DeriveLocation postfixes a location value with the module name: inserted
// before the filename extension when the last path segment has one
// ("prod/terraform.tfstate" -> "prod/terraform-networking.tfstate"), appended
// otherwise ("envs/prod" -> "envs/prod-networking"). A trailing "/lock" or
// "/unlock" verb segment (the http backend's lock endpoints) is preserved:
// the postfix lands on the state-name segment before it.
func DeriveLocation(value, module string) string {
	for _, verb := range []string{"/lock", "/unlock"} {
		if strings.HasSuffix(value, verb) {
			return DeriveLocation(strings.TrimSuffix(value, verb), module) + verb
		}
	}
	dir, last := path.Split(value)
	if ext := path.Ext(last); ext != "" {
		return dir + strings.TrimSuffix(last, ext) + "-" + module + ext
	}
	return dir + last + "-" + module
}

// DerivedLocations validates the block and returns each module's derived
// primary location (the first location attribute). Every location attribute
// must be present in HCL as a plain string literal — a location supplied only
// via -backend-config cannot be derived and is a refusal, not a guess.
func (b *BackendBlock) DerivedLocations(modules []string) (monolith string, byModule map[string]string, err error) {
	if b.Type == "cloud" {
		return "", nil, fmt.Errorf("the cloud block drives workspaces on a remote platform, not a derivable state location; pass --no-backend to carve without backend blocks and create per-module workspaces in the platform")
	}
	attrs, ok := locationAttrs[b.Type]
	if !ok {
		return "", nil, fmt.Errorf("backend type %q is not supported for state-location derivation; supported: %s — pass --no-backend to carve without backend blocks", b.Type, supportedTypesList())
	}
	if b.Type == "remote" {
		if _, named := b.locationValue("workspaces.name"); !named {
			if _, prefixed := b.locationValue("workspaces.prefix"); prefixed {
				return "", nil, fmt.Errorf("remote backend in workspaces.prefix mode maps CLI workspaces onto many remote workspaces; only workspaces.name mode is derivable — pass --no-backend to carve without backend blocks")
			}
		}
	}
	for _, name := range attrs {
		if _, ok := b.locationValue(name); !ok {
			return "", nil, fmt.Errorf("backend %q attribute %q is neither in the HCL block nor in the root's init-time resolved config; init the root (so -backend-config values are resolved) or pass --no-backend", b.Type, name)
		}
	}
	primary, _ := b.locationValue(attrs[0])
	byModule = map[string]string{}
	for _, m := range modules {
		byModule[m] = DeriveLocation(primary, m)
	}
	return primary, byModule, nil
}

// BackendHCL builds the module's backend block for its root.tf: the
// monolith's block with every location attribute rewritten to the module's
// derived value.
func (b *BackendBlock) BackendHCL(module string) (*hclwrite.Block, error) {
	// Built fresh rather than cloned: a single-line source block (e.g. an
	// empty `backend "http" {}`) cloned and mutated yields invalid HCL.
	out := hclwrite.NewBlock("backend", []string{b.Type})
	locs := map[string]bool{}
	for _, name := range locationAttrs[b.Type] {
		locs[name] = true
	}
	hclAttrs := b.block.Body().Attributes()
	hclNames := make([]string, 0, len(hclAttrs))
	for name := range hclAttrs {
		if !locs[name] {
			hclNames = append(hclNames, name)
		}
	}
	sort.Strings(hclNames)
	for _, name := range hclNames {
		out.Body().SetAttributeRaw(name, hclAttrs[name].Expr().BuildTokens(nil))
	}
	// Non-secret settings that were supplied out-of-band (lock methods, TLS
	// toggles, regions...) must survive into the carved roots too. Everything
	// credential-shaped is excluded here — it goes to demono.env instead.
	creds := envMapping[b.Type]
	extras := make([]string, 0, len(b.resolved))
	for name := range b.resolved {
		// Dotted keys are flattened nested-block values; they are handled by
		// the location loop below and are never valid flat attribute names.
		if locs[name] || strings.Contains(name, ".") {
			continue
		}
		if _, isCred := creds[name]; isCred {
			continue
		}
		if _, inHCL := b.block.Body().Attributes()[name]; inHCL {
			continue
		}
		if b.resolved[name] == "" {
			continue
		}
		extras = append(extras, name)
	}
	sort.Strings(extras)
	for _, name := range extras {
		out.Body().SetAttributeValue(name, cty.StringVal(b.resolved[name]))
	}
	for _, name := range locationAttrs[b.Type] {
		val, _ := b.locationValue(name)
		derived := DeriveLocation(val, module)
		if blkName, attr, nested := strings.Cut(name, "."); nested {
			nb := out.Body().FirstMatchingBlock(blkName, nil)
			if nb == nil {
				nb = out.Body().AppendNewBlock(blkName, nil)
			}
			nb.Body().SetAttributeValue(attr, cty.StringVal(derived))
			continue
		}
		out.Body().SetAttributeValue(name, cty.StringVal(derived))
	}
	return out, nil
}

// StripBackend removes backend blocks from every terraform{} block in src,
// returning the stripped bytes and whether anything was removed — the offline
// proof's way of un-declaring the backend while keeping required_providers.
// Unparseable input is returned unchanged.
func StripBackend(src []byte) ([]byte, bool) {
	f, diags := hclwrite.ParseConfig(src, "root.tf", hcl.Pos{Line: 1, Column: 1})
	if diags.HasErrors() {
		return src, false
	}
	changed := false
	for _, blk := range f.Body().Blocks() {
		if blk.Type() != "terraform" {
			continue
		}
		for _, inner := range blk.Body().Blocks() {
			if inner.Type() == "backend" {
				blk.Body().RemoveBlock(inner)
				changed = true
			}
		}
	}
	if !changed {
		return src, false
	}
	return hclwrite.Format(f.Bytes()), true
}

// literalAttr reads a plain double-quoted string attribute, rejecting
// interpolations.
func literalAttr(blk *hclwrite.Block, name string) (string, bool) {
	s, ok := stringAttr(blk, name)
	if !ok || strings.Contains(s, "${") {
		return "", false
	}
	return s, true
}

// envMapping names, per backend type, the official engine environment variable
// for each credential-bearing config attribute. Secrets are persisted only as
// gitignored per-module demono.env files in these variables — never into HCL.
var envMapping = map[string]map[string]string{
	"http": {
		"username": "TF_HTTP_USERNAME",
		"password": "TF_HTTP_PASSWORD",
	},
	"s3": {
		"access_key": "AWS_ACCESS_KEY_ID",
		"secret_key": "AWS_SECRET_ACCESS_KEY",
		"token":      "AWS_SESSION_TOKEN",
	},
	"azurerm": {
		"access_key":      "ARM_ACCESS_KEY",
		"sas_token":       "ARM_SAS_TOKEN",
		"client_id":       "ARM_CLIENT_ID",
		"client_secret":   "ARM_CLIENT_SECRET",
		"tenant_id":       "ARM_TENANT_ID",
		"subscription_id": "ARM_SUBSCRIPTION_ID",
	},
	"gcs": {
		"credentials":  "GOOGLE_BACKEND_CREDENTIALS",
		"access_token": "GOOGLE_OAUTH_ACCESS_TOKEN",
	},
	"consul": {
		"access_token": "CONSUL_HTTP_TOKEN",
	},
	"cos": {
		"secret_id":      "TENCENTCLOUD_SECRET_ID",
		"secret_key":     "TENCENTCLOUD_SECRET_KEY",
		"security_token": "TENCENTCLOUD_SECURITY_TOKEN",
	},
	"oss": {
		"access_key":     "ALICLOUD_ACCESS_KEY",
		"secret_key":     "ALICLOUD_SECRET_KEY",
		"security_token": "ALICLOUD_SECURITY_TOKEN",
	},
	"kubernetes": {
		"token":           "KUBE_TOKEN",
		"password":        "KUBE_PASSWORD",
		"client_key_data": "KUBE_CLIENT_KEY_DATA",
	},
	"pg": {
		// conn_str is both address and credential; the DSN carries the
		// password, so the whole string stays out of HCL.
		"conn_str": "PG_CONN_STR",
	},
	// remote's token env var is host-dependent (TF_TOKEN_<hostname>); the
	// empty mapping keeps the attribute out of emitted HCL, and CredentialEnv
	// derives the variable name from the hostname.
	"remote": {
		"token": "",
	},
}

// resolvedBackendConfig reads the root's initialized backend configuration
// from .terraform/terraform.tfstate — the values init resolved, including
// -backend-config flags the HCL never saw. Scalars only; absent init or a
// type mismatch yields nil.
func resolvedBackendConfig(srcDir, backendType string) map[string]string {
	b, err := os.ReadFile(filepath.Join(srcDir, ".terraform", "terraform.tfstate"))
	if err != nil {
		return nil
	}
	var doc struct {
		Backend struct {
			Type   string         `json:"type"`
			Config map[string]any `json:"config"`
		} `json:"backend"`
	}
	if err := json.Unmarshal(b, &doc); err != nil || doc.Backend.Type != backendType {
		return nil
	}
	out := map[string]string{}
	for k, v := range doc.Backend.Config {
		switch t := v.(type) {
		case string:
			out[k] = t
		case bool:
			out[k] = fmt.Sprintf("%t", t)
		case float64:
			if t == float64(int64(t)) {
				out[k] = fmt.Sprintf("%d", int64(t))
			} else {
				out[k] = fmt.Sprintf("%g", t)
			}
		case map[string]any:
			// One level of nesting, flattened to dotted keys — the remote
			// backend's workspaces block.
			for k2, v2 := range t {
				if s, ok := v2.(string); ok && s != "" {
					out[k+"."+k2] = s
				}
			}
		}
	}
	return out
}

// locationValue resolves a location attribute: the HCL literal, else the
// root's resolved (init-time) value. A dotted name reads an attribute inside
// a nested block ("workspaces.name").
func (b *BackendBlock) locationValue(name string) (string, bool) {
	if blkName, attr, nested := strings.Cut(name, "."); nested {
		for _, nb := range b.block.Body().Blocks() {
			if nb.Type() == blkName {
				if v, ok := literalAttr(nb, attr); ok {
					return v, true
				}
			}
		}
		v, ok := b.resolved[name]
		return v, ok && v != ""
	}
	if v, ok := literalAttr(b.block, name); ok {
		return v, true
	}
	v, ok := b.resolved[name]
	return v, ok && v != ""
}

// CredentialEnv maps the resolved credential attributes that are absent from
// HCL to their engine environment variables — the content of a module's demono.env.
func (b *BackendBlock) CredentialEnv() map[string]string {
	out := map[string]string{}
	for attr, envVar := range envMapping[b.Type] {
		if envVar == "" {
			continue
		}
		if _, inHCL := literalAttr(b.block, attr); inHCL {
			// An HCL-present credential is committed intent (e.g. seeded dev
			// defaults) and travels in the emitted root.tf clone as-is.
			continue
		}
		if v, ok := b.resolved[attr]; ok && v != "" {
			out[envVar] = v
		}
	}
	if b.Type == "remote" {
		if _, inHCL := literalAttr(b.block, "token"); !inHCL {
			if v := b.resolved["token"]; v != "" {
				host := "app.terraform.io"
				if h, ok := b.locationValue("hostname"); ok {
					host = h
				}
				// The engines' credential variable encodes the hostname:
				// dots become _, dashes become __.
				out["TF_TOKEN_"+strings.NewReplacer(".", "_", "-", "__").Replace(host)] = v
			}
		}
	}
	return out
}

// WriteEnvFile writes a module's gitignored .env holding the backend
// credentials as engine environment variables. No file when there is nothing
// to write. Mode 0600: this file holds secrets.
func WriteEnvFile(moduleDir string, env map[string]string) (bool, error) {
	if len(env) == 0 {
		return false, nil
	}
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var sb strings.Builder
	sb.WriteString("# Backend credentials inherited from the monolith's init-time configuration.\n")
	sb.WriteString("# This file holds secrets: it must stay gitignored.\n")
	sb.WriteString("# Shell-sourceable: `source " + EnvFileName + "` exports every value.\n")
	for _, k := range keys {
		// Single-quoted so credentials with shell metacharacters source
		// verbatim; embedded quotes use the standard '\'' escape.
		fmt.Fprintf(&sb, "export %s='%s'\n", k, strings.ReplaceAll(env[k], `'`, `'\''`))
	}
	return true, os.WriteFile(filepath.Join(moduleDir, EnvFileName), []byte(sb.String()), 0o600)
}
