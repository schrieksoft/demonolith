package hclgraph

// Provider is a parsed provider block: its local name, optional alias, and the
// addresses it references in its config body (var/local/resource/data/module).
// Providers are not graph nodes (their placement is usage-derived, not
// decorator-driven), but their references still cross module boundaries and must
// be wired, so they are collected separately here.
type Provider struct {
	Name  string // provider local name, e.g. "tls"
	Alias string // alias label, or "" for the default provider
	// Refs are the addresses referenced in the provider config body.
	Refs []Address
	// RefAttrs records, per referenced resource/data/module producer, every
	// attribute path used (e.g. "id"), so an emitted output/wiring exposes the
	// right attribute.
	RefAttrs map[string][]string
}

// Key is the provider's identity: "name" for the default, "name.alias" for an
// aliased instance. It matches the `provider = name.alias` meta-argument form.
func (p Provider) Key() string {
	if p.Alias == "" {
		return p.Name
	}
	return p.Name + "." + p.Alias
}

// Providers returns every provider block in the root, with alias and references
// resolved the same way node references are (AST traversal).
func (g *Graph) Providers() []Provider {
	var out []Provider
	for _, body := range g.Files {
		for _, blk := range body.Blocks {
			if blk.Type != "provider" || len(blk.Labels) != 1 {
				continue
			}
			p := Provider{Name: blk.Labels[0]}
			if attr, ok := blk.Body.Attributes["alias"]; ok {
				if v, diags := attr.Expr.Value(nil); !diags.HasErrors() && v.Type().FriendlyName() == "string" {
					p.Alias = v.AsString()
				}
			}
			c := newRefCollector()
			g.walkBody(blk.Body, c)
			// Drop the alias attribute's own (non-)refs; alias is a literal string.
			p.Refs = flatten(c.seen)
			p.RefAttrs = c.attrs
			out = append(out, p)
		}
	}
	return out
}
