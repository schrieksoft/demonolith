// Package boundary computes, per module, the inputs and outputs implied by
// references that cross a module boundary after placement.
//
// An edge is consumer -> producer (the consumer block references the producer).
// After placement:
//   - both endpoints in the same module: internal, relocates unchanged.
//   - endpoints in different modules: the producer module must expose an
//     `output` and the consumer module must declare a `variable`; a boundary
//     CrossEdge records the (producer output -> consumer input) wiring.
//   - a reference to a `var` becomes an external/root input on the consumer
//     module (it was a monolith root variable; each module that uses it gets its
//     own input).
//   - a reference to a `local` is resolved by pulling the local's own upstream
//     references into the consuming module (v1: treated like the resources it
//     depends on; a cross-module local surfaces as boundary edges to those
//     upstream producers).
package boundary

import (
	"fmt"
	"sort"

	"github.com/schrieksoft/demonolith/internal/hclgraph"
	"github.com/schrieksoft/demonolith/internal/placement"
)

// CrossEdge is a single boundary-crossing reference that requires wiring: the
// Producer module exposes OutputName, threaded into the Consumer module's
// InputName. For v1 the output/input names are derived from the producer
// address, but they are independent by construction.
type CrossEdge struct {
	ProducerModule string
	ConsumerModule string
	Producer       hclgraph.Address // the referenced node
	Consumer       hclgraph.Address // the referencing node
	Attr           string           // referenced attribute path; "" = whole object
	OutputName     string           // output declared in producer module
	InputName      string           // variable declared in consumer module
}

// ModuleBoundary is the wiring surface of a single module.
type ModuleBoundary struct {
	Module string
	// Inputs are variables this module must declare: cross-module inputs
	// (sourced from another module's output) plus external/root inputs (former
	// monolith var.* values). Keyed by input name.
	Inputs map[string]Input
	// Outputs are output names this module must expose because a node in
	// another module references one of its nodes.
	Outputs map[string]Output
}

// Input is a variable a module must declare.
type Input struct {
	Name string
	// FromModule is the producing module for an upstream-sourced input, or ""
	// for an external/root input.
	FromModule string
	// FromOutput is the producing output name for an upstream-sourced input.
	FromOutput string
	// External is true for a former root variable (region, env, tags...).
	External bool
	// SourceVar is the original var.<name> for an external input.
	SourceVar string
}

// Output is an output a module must expose.
type Output struct {
	Name string
	// Node is the address whose value the output exposes.
	Node hclgraph.Address
	// Attr is the referenced attribute path (e.g. "result"); empty means the
	// whole object. A producer referenced through several attributes exposes
	// one output per attribute, each with its own name.
	Attr string
}

// OrderingEdge is a whole-module ordering dependency induced by a cross-module
// depends_on. It requires no variable/output — only that the producer module is
// applied before the consumer module. In detached v1 this is reported so the
// operator can enforce ordering (Snap CD's graph would carry it natively).
type OrderingEdge struct {
	ConsumerModule string
	ProducerModule string
	Consumer       hclgraph.Address
	Producer       hclgraph.Address
}

// Result is the full boundary computation across all modules.
type Result struct {
	Boundaries    map[string]*ModuleBoundary
	CrossEdges    []CrossEdge
	OrderingEdges []OrderingEdge
	// multiAttr marks producers referenced through more than one distinct
	// attribute anywhere in the root; their outputs/inputs get attr-scoped
	// names so each attribute carries its own value.
	multiAttr map[string]bool
}

// Compute derives the boundary surface for every module in the placement.
func Compute(g *hclgraph.Graph, p *placement.Placement) (*Result, error) {
	res := &Result{Boundaries: map[string]*ModuleBoundary{}, multiAttr: multiAttrProducers(g)}
	for _, m := range p.ModuleNames() {
		res.Boundaries[m] = &ModuleBoundary{
			Module:  m,
			Inputs:  map[string]Input{},
			Outputs: map[string]Output{},
		}
	}

	// Walk every placed consumer node and inspect its references.
	for _, consumer := range g.SortedNodes() {
		if !isPlaced(consumer.Addr.Kind) {
			continue
		}
		consumerMods := p.ModulesOf(consumer.Addr)
		if len(consumerMods) == 0 {
			continue
		}
		// Ordering-only (depends_on) refs across a boundary become module
		// ordering edges: no value wiring, only apply-order.
		for _, dep := range consumer.DependsOnOnly {
			for _, cm := range consumerMods {
				res.addOrdering(p, consumer.Addr, dep, cm)
			}
		}

		for _, ref := range consumer.Refs {
			switch ref.Kind {
			case hclgraph.KindResource, hclgraph.KindData, hclgraph.KindModule:
				for _, attr := range attrsOf(consumer.RefAttrs, ref) {
					for _, cm := range consumerMods {
						if err := res.wireProducer(p, consumer.Addr, ref, cm, attr); err != nil {
							return nil, err
						}
					}
				}
			case hclgraph.KindVariable:
				// A variable the root declares travels with the module as its
				// own original `variable` block (carved by emit), so it needs no
				// boundary input. Only an undeclared var would be a true external
				// input.
				if g.Node(ref) != nil {
					break
				}
				for _, cm := range consumerMods {
					b := res.Boundaries[cm]
					name := ref.Name
					b.Inputs[name] = Input{Name: name, External: true, SourceVar: ref.Name}
				}
			case hclgraph.KindLocal:
				// v1: a local's cross-module use is resolved by following the
				// local's own upstream refs. Handled by resolveLocal.
				for _, cm := range consumerMods {
					if err := res.resolveLocal(g, p, ref, cm); err != nil {
						return nil, err
					}
				}
			}
		}
	}

	// Provider config bodies also cross module boundaries: a provider used by a
	// module may reference a producer that lives elsewhere, and (unlike a
	// resource body) it might be the SOLE consumer of that producer. Wire those.
	if err := res.wireProviders(g, p); err != nil {
		return nil, err
	}

	sort.Slice(res.CrossEdges, func(i, j int) bool {
		a, b := res.CrossEdges[i], res.CrossEdges[j]
		if a.ConsumerModule != b.ConsumerModule {
			return a.ConsumerModule < b.ConsumerModule
		}
		return a.InputName < b.InputName
	})
	return res, nil
}

// wireProviders determines, per module, which providers it uses (by resource
// type + `provider = name.alias` meta-arg) and wires any cross-module producer
// referenced in each used provider's config — creating the CrossEdge/variable
// even when the provider is the only thing referencing that producer.
func (res *Result) wireProviders(g *hclgraph.Graph, p *placement.Placement) error {
	providers := indexProviders(g.Providers())

	// moduleUses[module][providerKey] = true
	moduleUses := map[string]map[string]bool{}
	for _, node := range g.SortedNodes() {
		if node.Addr.Kind != hclgraph.KindResource && node.Addr.Kind != hclgraph.KindData {
			continue
		}
		key := node.ProviderKey
		if key == "" {
			key = providerName(node.Addr.Type)
		}
		for _, m := range p.ModulesOf(node.Addr) {
			if moduleUses[m] == nil {
				moduleUses[m] = map[string]bool{}
			}
			moduleUses[m][key] = true
		}
	}

	// For each (module, providerKey) used, wire the provider's cross-module refs.
	for module, keys := range moduleUses {
		for key := range keys {
			prov, ok := providers[key]
			if !ok {
				continue // implicit/default provider with no config block
			}
			for _, ref := range prov.Refs {
				switch ref.Kind {
				case hclgraph.KindResource, hclgraph.KindData, hclgraph.KindModule:
					for _, attr := range attrsOf(prov.RefAttrs, ref) {
						if err := res.wireProducer(p, providerConsumer(prov), ref, module, attr); err != nil {
							return err
						}
					}
				case hclgraph.KindVariable, hclgraph.KindLocal:
					// var/local carried into the module by emit's structural pass.
				}
			}
		}
	}
	return nil
}

// indexProviders keys providers by their Key() ("name" or "name.alias").
func indexProviders(ps []hclgraph.Provider) map[string]hclgraph.Provider {
	out := map[string]hclgraph.Provider{}
	for _, p := range ps {
		out[p.Key()] = p
	}
	return out
}

// providerConsumer synthesizes a consumer address for a provider so a CrossEdge
// has a meaningful "consumer" for diagnostics. It is not a real graph node.
func providerConsumer(p hclgraph.Provider) hclgraph.Address {
	return hclgraph.Address{Kind: hclgraph.KindProvider, Name: p.Key()}
}

// providerName derives the provider local name from a resource/data type: the
// segment before the first underscore.
func providerName(resourceType string) string {
	for i := 0; i < len(resourceType); i++ {
		if resourceType[i] == '_' {
			return resourceType[:i]
		}
	}
	return resourceType
}

// wireProducer records the wiring for a consumer in module cm referencing a
// resource/data producer. Same-module -> nothing. Cross-module -> output on the
// producer module, variable on cm, and a CrossEdge.
func (res *Result) wireProducer(p *placement.Placement, consumer, producer hclgraph.Address, cm, attr string) error {
	prodMods := p.ModulesOf(producer)
	if len(prodMods) == 0 {
		return fmt.Errorf("reference to unplaced node %s", producer)
	}
	// If the producer is duplicated and one of its copies lives in cm, the
	// reference is satisfied locally.
	for _, pm := range prodMods {
		if pm == cm {
			return nil
		}
	}
	// Producer lives elsewhere. If duplicated across several other modules,
	// pick a deterministic one (sorted first) as the source.
	sorted := append([]string(nil), prodMods...)
	sort.Strings(sorted)
	pm := sorted[0]

	outName := res.edgeName(producer, attr)
	inName := outName

	res.Boundaries[pm].Outputs[outName] = Output{Name: outName, Node: producer, Attr: attr}
	res.Boundaries[cm].Inputs[inName] = Input{Name: inName, FromModule: pm, FromOutput: outName}
	for _, e := range res.CrossEdges {
		if e.ConsumerModule == cm && e.InputName == inName && e.Consumer == consumer {
			return nil
		}
	}
	res.CrossEdges = append(res.CrossEdges, CrossEdge{
		ProducerModule: pm,
		ConsumerModule: cm,
		Producer:       producer,
		Consumer:       consumer,
		Attr:           attr,
		OutputName:     outName,
		InputName:      inName,
	})
	return nil
}

// edgeName derives the output/input name for a producer reference. A producer
// referenced through a single attribute everywhere keeps the plain
// address-derived name; one referenced through several attributes gets one
// name per attribute, so each carries its own value.
func (res *Result) edgeName(producer hclgraph.Address, attr string) string {
	base := outputName(producer)
	if attr == "" || !res.multiAttr[producer.String()] {
		return base
	}
	return base + "_" + sanitizeAttr(attr)
}

// sanitizeAttr turns an attribute path into a name fragment.
func sanitizeAttr(attr string) string {
	out := make([]byte, len(attr))
	for i := 0; i < len(attr); i++ {
		c := attr[i]
		if c == '.' || c == '-' {
			c = '_'
		}
		out[i] = c
	}
	return string(out)
}

// attrsOf returns the recorded attribute paths for a producer reference, or a
// single whole-object entry when none were recorded.
func attrsOf(refAttrs map[string][]string, ref hclgraph.Address) []string {
	attrs := refAttrs[ref.String()]
	if len(attrs) == 0 {
		return []string{""}
	}
	return attrs
}

// multiAttrProducers finds producers referenced through more than one distinct
// non-empty attribute path anywhere in the root — including from provider
// config bodies — so their edges can be attr-scoped consistently everywhere.
func multiAttrProducers(g *hclgraph.Graph) map[string]bool {
	byProducer := map[string]map[string]bool{}
	record := func(refAttrs map[string][]string) {
		for producer, attrs := range refAttrs {
			if byProducer[producer] == nil {
				byProducer[producer] = map[string]bool{}
			}
			for _, a := range attrs {
				if a != "" {
					byProducer[producer][a] = true
				}
			}
		}
	}
	for _, n := range g.SortedNodes() {
		record(n.RefAttrs)
	}
	for _, prov := range g.Providers() {
		record(prov.RefAttrs)
	}
	out := map[string]bool{}
	for producer, attrs := range byProducer {
		if len(attrs) > 1 {
			out[producer] = true
		}
	}
	return out
}

// addOrdering records a whole-module ordering edge for a cross-module
// depends_on. Same-module or a producer duplicated into cm needs nothing.
func (res *Result) addOrdering(p *placement.Placement, consumer, producer hclgraph.Address, cm string) {
	prodMods := p.ModulesOf(producer)
	for _, pm := range prodMods {
		if pm == cm {
			return
		}
	}
	if len(prodMods) == 0 {
		return
	}
	sorted := append([]string(nil), prodMods...)
	sort.Strings(sorted)
	res.OrderingEdges = append(res.OrderingEdges, OrderingEdge{
		ConsumerModule: cm,
		ProducerModule: sorted[0],
		Consumer:       consumer,
		Producer:       producer,
	})
}

// resolveLocal follows a local's own references so a cross-module local use
// materializes as boundary edges to the local's upstream producers.
func (res *Result) resolveLocal(g *hclgraph.Graph, p *placement.Placement, local hclgraph.Address, cm string) error {
	ln := g.Node(local)
	if ln == nil {
		return nil
	}
	for _, ref := range ln.Refs {
		switch ref.Kind {
		case hclgraph.KindResource, hclgraph.KindData, hclgraph.KindModule:
			for _, attr := range attrsOf(ln.RefAttrs, ref) {
				if err := res.wireProducer(p, local, ref, cm, attr); err != nil {
					return err
				}
			}
		case hclgraph.KindVariable:
			// Declared root variables travel with the module (carved by emit);
			// only undeclared vars become external inputs.
			if g.Node(ref) != nil {
				continue
			}
			b := res.Boundaries[cm]
			b.Inputs[ref.Name] = Input{Name: ref.Name, External: true, SourceVar: ref.Name}
		case hclgraph.KindLocal:
			if err := res.resolveLocal(g, p, ref, cm); err != nil {
				return err
			}
		}
	}
	return nil
}

func isPlaced(k hclgraph.Kind) bool {
	return k == hclgraph.KindResource || k == hclgraph.KindData || k == hclgraph.KindModule
}

// outputName derives a stable output name for a producer node. Resource
// type+name keeps it unique within the producing module.
func outputName(a hclgraph.Address) string {
	switch a.Kind {
	case hclgraph.KindResource:
		return a.Type + "_" + a.Name
	case hclgraph.KindData:
		return "data_" + a.Type + "_" + a.Name
	case hclgraph.KindModule:
		return "module_" + a.Name
	default:
		return a.Name
	}
}
