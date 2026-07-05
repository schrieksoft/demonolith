// Package hclgraph parses a Terraform/OpenTofu root into a resource-level
// reference graph using hclsyntax. Nodes are the top-level configurable objects
// (managed resources, data sources, variables, locals, outputs, module calls)
// and edges are references discovered by traversing expressions — not by regex,
// so references hidden inside templatefile/jsonencode/dynamic blocks are caught.
package hclgraph

import (
	"fmt"
	"strings"
)

// Kind enumerates the top-level block kinds Demonolith tracks as graph nodes.
type Kind int

const (
	KindResource Kind = iota // managed resource
	KindData                 // data source
	KindVariable             // input variable
	KindLocal                // a single named local value
	KindOutput               // output value
	KindModule               // module call
	KindProvider             // provider block (not a graph node; used for edge diagnostics)
)

func (k Kind) String() string {
	switch k {
	case KindResource:
		return "resource"
	case KindData:
		return "data"
	case KindVariable:
		return "var"
	case KindLocal:
		return "local"
	case KindOutput:
		return "output"
	case KindModule:
		return "module"
	case KindProvider:
		return "provider"
	default:
		return "unknown"
	}
}

// Address is the canonical, dedup-safe identity of a graph node. It is the
// address as written in configuration (no count/for_each instance key — a
// decorator attaches to the whole block, not an instance).
type Address struct {
	Kind Kind
	// Type is the resource/data type (e.g. "random_uuid"); empty for
	// var/local/output/module.
	Type string
	// Name is the local name (resource/data second label, or the single
	// label for var/local/output/module).
	Name string
}

// String renders the address the way it appears in Terraform references, so it
// can be used as a stable map key and in diagnostics.
func (a Address) String() string {
	switch a.Kind {
	case KindResource:
		return a.Type + "." + a.Name
	case KindData:
		return "data." + a.Type + "." + a.Name
	case KindVariable:
		return "var." + a.Name
	case KindLocal:
		return "local." + a.Name
	case KindOutput:
		return "output." + a.Name
	case KindModule:
		return "module." + a.Name
	case KindProvider:
		return "provider." + a.Name
	default:
		return fmt.Sprintf("<%s.%s>", a.Kind, a.Name)
	}
}

// refPrefix returns the leading traversal segments a reference to this node
// begins with, so edge resolution can match a traversal against known nodes.
func (a Address) refPrefix() []string {
	switch a.Kind {
	case KindResource:
		return []string{a.Type, a.Name}
	case KindData:
		return []string{"data", a.Type, a.Name}
	case KindVariable:
		return []string{"var", a.Name}
	case KindLocal:
		return []string{"local", a.Name}
	case KindModule:
		return []string{"module", a.Name}
	default:
		// outputs are never referenced from within the same root
		return nil
	}
}

// ParseRefRoot inspects the leading segments of a traversal and returns the
// Address it refers to (Type/Name populated, no instance key). ok is false for
// traversals that do not name a trackable node (e.g. count.index, path.module,
// terraform.workspace, each.key).
func ParseRefRoot(segments []string) (Address, bool) {
	if len(segments) == 0 {
		return Address{}, false
	}
	switch segments[0] {
	case "var":
		if len(segments) >= 2 {
			return Address{Kind: KindVariable, Name: segments[1]}, true
		}
	case "local":
		if len(segments) >= 2 {
			return Address{Kind: KindLocal, Name: segments[1]}, true
		}
	case "module":
		if len(segments) >= 2 {
			return Address{Kind: KindModule, Name: segments[1]}, true
		}
	case "data":
		if len(segments) >= 3 {
			return Address{Kind: KindData, Type: segments[1], Name: segments[2]}, true
		}
	case "count", "each", "self", "path", "terraform":
		// meta-references, not graph nodes
		return Address{}, false
	default:
		// managed resource: <type>.<name>...
		if len(segments) >= 2 && looksLikeResourceType(segments[0]) {
			return Address{Kind: KindResource, Type: segments[0], Name: segments[1]}, true
		}
	}
	return Address{}, false
}

// looksLikeResourceType is a heuristic: resource types contain an underscore
// (provider_thing). This avoids treating bare identifiers as resources. It is
// only used as a tiebreaker; known-node matching in the graph is authoritative.
func looksLikeResourceType(s string) bool {
	return strings.Contains(s, "_")
}
