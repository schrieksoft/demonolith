package emit

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
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
}

// locationAttrs names, per backend type, the attributes that distinguish one
// state location from another. Types absent here are unsupported for
// derivation.
var locationAttrs = map[string][]string{
	"local":   {"path"},
	"s3":      {"key"},
	"azurerm": {"key"},
	"gcs":     {"prefix"},
	"consul":  {"path"},
	"http":    {"address", "lock_address", "unlock_address"},
}

// SupportedBackend reports whether derivation knows the given backend type.
func SupportedBackend(t string) bool {
	_, ok := locationAttrs[t]
	return ok
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
					return &BackendBlock{Type: inner.Labels()[0], block: inner}, nil
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
	attrs, ok := locationAttrs[b.Type]
	if !ok {
		return "", nil, fmt.Errorf("backend type %q is not supported for state-location derivation; supported: local, s3, azurerm, gcs, consul, http — pass --no-backend to carve without backend blocks", b.Type)
	}
	for _, name := range attrs {
		if _, ok := literalAttr(b.block, name); !ok {
			return "", nil, fmt.Errorf("backend %q attribute %q is not a plain string in the HCL block (supplied via -backend-config?); the location must be present in HCL to derive per-module locations, or pass --no-backend", b.Type, name)
		}
	}
	primary, _ := literalAttr(b.block, attrs[0])
	byModule = map[string]string{}
	for _, m := range modules {
		byModule[m] = DeriveLocation(primary, m)
	}
	return primary, byModule, nil
}

// WriteBackendTF writes the module's backend.tf: the monolith's block with
// every location attribute rewritten to the module's derived value.
func (b *BackendBlock) WriteBackendTF(moduleDir, module string) error {
	f := hclwrite.NewEmptyFile()
	tfBlk := f.Body().AppendNewBlock("terraform", nil)
	clone := cloneBlock(b.block)
	for _, name := range locationAttrs[b.Type] {
		val, _ := literalAttr(clone, name)
		clone.Body().SetAttributeValue(name, cty.StringVal(DeriveLocation(val, module)))
	}
	tfBlk.Body().AppendBlock(clone)
	return os.WriteFile(filepath.Join(moduleDir, "backend.tf"), hclwrite.Format(f.Bytes()), 0o644)
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
