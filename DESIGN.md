# Demonolith — Design

This document explains what Demonolith is, the concepts it works with, and how
its stages fit together into one pipeline. It is the conceptual companion to the
`README.md` (which is task-oriented) and the package doc-comments (which are
detail-oriented).

---

## 1. The problem

A Terraform/OpenTofu **monolith** is a single root directory whose `*.tf` files
declare everything — networking, database, shared bits — managed as one state
and applied as one unit. Monoliths get slow, risky, and coupled: a one-line
change re-plans the whole world, and one broken resource can block unrelated
ones.

The goal is to carve that monolith into **independent per-module roots**, each a
self-contained Terraform root with its own state, that can be planned and applied
on its own. In production these roots are orchestrated by **Snap CD**: where the
monolith passed a value from resource A to resource B implicitly (same graph,
same state), the split roots pass it explicitly — module A publishes an `output`,
module B declares a `variable`, and Snap CD wires one to the other at runtime.

The hard requirement is that the split be **operationally inert**: after
carving, planning each new root against its share of the old state must show
**zero creates and zero destroys**. Nothing is rebuilt; only the *organization*
of the code and state changes. Demonolith's job is to perform that carve and to
*prove* the inertness.

### v1 scope

Demonolith v1 is a **one-shot splitter**. Three deliberate cuts from the fuller
design:

1. **Detached roots.** It emits plain Terraform roots with `variable`/`output`
   boundaries. It does **not** generate `snapcd_*` control-plane resources yet —
   the human (or a later tool) wires the roots into Snap CD. The cross-module
   edges it computes are exactly the wiring Snap CD would need.
2. **Local state only.** It carves state into per-module **local files** and
   **never pushes** to a real backend. The carved files are both the deliverable
   and the input the proof consumes.
3. **String-typed inputs.** Every generated `variable` is `type = string`,
   matching Snap CD's stringified value-passing. Richer type coercion is
   deferred.

What it keeps — and what makes it more than a code generator — is the **proof
oracle** (§10): a graph-threaded, zero-diff plan bundle run against real
`terraform`/`tofu`.

---

## 2. The pipeline at a glance

```
                 ┌──────────┐
   monolith/  →  │  Parse   │  *.tf → reference graph            [hclgraph]
                 └────┬─────┘
                      │  Graph{Nodes, Refs, DependsOnOnly, RefAttrs}
                 ┌────▼─────┐
                 │Decorators│  # @demono:move <module> comments    [decorator]
                 └────┬─────┘
                      │  []BlockDecorators
                 ┌────▼─────┐
                 │Placement │  every resource/data → one module  [placement]
                 └────┬─────┘
                      │  Placement{Modules, Owner, Duplicated, Catchall}
                 ┌────▼─────┐
                 │ Boundary │  cross-module refs → input/output  [boundary]
                 └────┬─────┘
                      │  Result{CrossEdges, OrderingEdges, Boundaries}
                 ┌────▼─────┐
                 │Cycle gate│  refuse impossible splits          [cycle]
                 └────┬─────┘
      ════════════════╪════════════════  (pipeline.Analyze ends here)
                      │
        ┌─────────────┼──────────────┐
   ┌────▼────┐   ┌────▼─────┐    ┌────▼────┐
   │  Emit   │   │StateCarve│    │  Proof  │
   │  [emit] │   │[statemove]│   │ [proof] │
   └─────────┘   └──────────┘    └─────────┘
    per-module     per-module      per-module zero-diff
    roots (code)   state files     plan bundle
```

The front half (Parse → Cycle gate) is pure, offline, and deterministic; it is
packaged as `pipeline.Analyze` and shared by the CLI and tests. The back half
(Emit / Carve / Proof) does I/O — writing files, and (for Carve/Proof) shelling
out to a real `terraform`/`tofu` binary via `terraform-exec`.

Module layout mirrors these stages one-package-per-stage under `internal/`.

---

## 3. Core concept: the reference graph  `[internal/hclgraph]`

Everything downstream reasons over a **resource-level reference graph** of the
monolith, not raw text.

### Nodes and addresses

A **Node** is one top-level configurable object: a managed `resource`, a `data`
source, a `variable`, an `output`, a `module` call, or a single named value
inside a `locals` block (each local attribute is its own node). Each node has an
**Address** — its canonical identity as written in Terraform references
(`random_uuid.vpc_id`, `data.aws_ami.ubuntu`, `var.region`, `local.tags`,
`module.net`). The address deliberately carries **no `count`/`for_each` instance
key**: a decorator and a state move attach to the whole block, not an instance.
`Address.String()` is the stable map key used across every package.

### Edges by AST traversal, not regex

An **edge** is a reference: node A's body mentions node B. Demonolith extracts
edges by **walking the parsed expression AST** (`hclsyntax.Walk`), collecting
every `ScopeTraversalExpr`/`RelativeTraversalExpr`, and resolving each
traversal's leading segments to a known node (`ParseRefRoot`).

This is a specific, deliberate choice over two easier options:

- **Not regex** — a textual scan can't tell a real reference from one inside a
  string or comment, and would miss refs inside `templatefile(...)`,
  `jsonencode(...)`, and `dynamic` blocks.
- **Not `Expression.Variables()`** — HCL's built-in helper is known to *miss*
  traversals that appear as the target of an index expression, e.g.
  `foo.x[count.index].name`. Walking the AST and pulling from every traversal
  node closes that gap. This is the single most important correctness property
  of the parser.

Meta-references (`count.index`, `each.key`, `path.module`, `self`,
`terraform.workspace`) resolve to no node and are dropped. Resource/data edges
are only recorded when the target actually exists in the root, so the
"contains-underscore" resource-type heuristic in `ParseRefRoot` is a tiebreaker,
never authoritative.

### Two annotations that shape the whole split

While collecting refs, each node records two extra things that later stages
depend on:

- **`RefAttrs`** — for each referenced resource/data producer, the *attribute
  path* used at the first crossing (`result` in `random_uuid.x.result`). This is
  what lets an emitted `output` expose `random_uuid.x.result` rather than the
  whole object.
- **`DependsOnOnly`** — producers referenced **solely** from a `depends_on`
  meta-argument, and never for their value. `walkBody` intercepts the
  `depends_on` attribute and routes its refs into a separate bucket; a producer
  that appears both in `depends_on` and in a real value expression counts as a
  value ref. This split is what lets an ordering-only dependency become a
  *whole-module ordering edge* instead of a spurious `variable`/`output`
  (§6, §8).

---

## 4. Core concept: decorators  `[internal/decorator]`

Placement is driven by **decorators** — strict, namespaced comments directly
above (or immediately below) a decoratable block:

```hcl
# @demono:move networking
resource "random_uuid" "vpc_id" {}
```

Grammar: `@demono:<verb> <target...>`. v1 supports one verb, `move`, whose target
is one or more module names.

Four design commitments:

1. **Namespaced and strict.** `@demono:` prevents collisions with ordinary
   comments. The scanner is deliberately *fail-loud*: a comment that *looks* like
   a decorator (matches `@demono\b`) but doesn't parse strictly is a **hard
   error**, not a silent skip. Comments are invisible to `terraform validate`, so
   a silent typo would misplace a resource with no other warning.

2. **Arity encodes state semantics.**
   - `resource` / `module` → **exactly one** target. A managed resource is a
     stateful singleton; it cannot live in two roots.
   - `data` → **one or more** targets. A data source is a stateless read; it is
     *duplicated* into each target and re-read there.

3. **Attaches to an address, not a line.** A decorator binds to the resolved
   block address (matching `hclgraph.Address.String()`), so downstream stages
   join on identity, not position.

4. **Total assignment, always.** A block with no decorator is not an error — it
   falls to the **catchall / remainder** module (`--remainder-module`, default
   `monolith`). Every resource and data source therefore has a home; nothing is
   ever left unplaced.

An orphan decorator (a valid `@demono:` comment not adjacent to any decoratable
block) is also a hard error — it means the author expected placement that won't
happen.

---

## 5. Core concept: placement  `[internal/placement]`

**Placement** turns the graph + decorators into a *total assignment* of nodes to
modules. Only **resource** and **data** nodes are placed directly — `variable`,
`local`, `output`, and `module`-call nodes are *structural*: a variable/local is
materialized wherever its consumers land, an output is generated at a boundary.

The resulting `Placement` carries:

- `Modules` — module name → the addresses assigned to it.
- `Owner` — address → its single owning module (for non-duplicated nodes).
- `Duplicated` — a data-source address → the ≥2 modules it was copied into (such
  a node has no single owner).
- `Catchall` — the unannotated addresses that fell to the remainder module,
  reported every run so the operator sees exactly what defaulted.

The `ModuleOf` / `ModulesOf` accessors express the core asymmetry: a normal node
has one home; a duplicated data source has several. Everything downstream calls
`ModulesOf` so duplication is handled uniformly.

---

## 6. Core concept: boundaries  `[internal/boundary]`

Once every node has a home, references that **cross** a module boundary must be
turned into explicit wiring. `boundary.Compute` walks every placed consumer and
classifies each of its references:

| Reference from consumer in module C… | Result |
|---|---|
| to a producer **in the same module** | internal — relocates unchanged, no wiring |
| to a resource/data producer **in another module** P | P gets an `output`; C gets a `variable`; a **CrossEdge** records the wiring |
| to a `var.*` (a former monolith root variable) | C gets an **external** input (a root variable each module re-declares) |
| to a `local.*` | resolved by following the local's *own* upstream refs into C (a local is inlined, so its dependencies become C's boundary edges) |
| a cross-module **`depends_on`** (from `DependsOnOnly`) | an **OrderingEdge** — whole-module apply-order, no value wiring |

The key output types:

- **`CrossEdge`** — a value-carrying boundary crossing: *producer module exposes
  `OutputName`, threaded into consumer module's `InputName`.* Output/input names
  derive from the producer address (`<type>_<name>`, or `data_<type>_<name>`) so
  they're unique within a module. The two names are independent by construction;
  v1 keeps them aligned for readability.
- **`OrderingEdge`** — a whole-module ordering dependency with **no**
  variable/output. It exists only to say "apply P before C." In detached v1 this
  is *reported* so the operator enforces ordering; Snap CD's graph would carry it
  natively.
- **`ModuleBoundary`** — per module, the full set of `Inputs` (upstream +
  external) and `Outputs` it must declare. This is the module's wiring surface,
  consumed directly by Emit.

Duplication interacts cleanly here: if a producer is a duplicated data source and
one of its copies already lives in the consumer's module, the reference is
satisfied **locally** and induces no edge; only genuinely-remote producers wire
across.

The separation of `CrossEdge` (value) from `OrderingEdge` (ordering) is why a
`depends_on` never produces a meaningless "pass this value" input — a subtle but
important correctness property that came out of an early bug.

---

## 7. Core concept: the cycle gate  `[internal/cycle]`

Not every split is possible. If module A needs an output of module B **and** B
needs an output of A, no apply order exists — it's illegal in Terraform and
unresolvable in Snap CD's DAG. `cycle.Check` is the gate that refuses such
splits *before* any file is written.

Mechanism — **module contraction**: contract each module to a single node, lift
every `CrossEdge` and `OrderingEdge` to a module-level dependency edge (consumer
→ producer), and DFS with white/gray/black coloring. A back edge to a gray node
is a cycle. The gate reconstructs the full **named path**
(`networking → compute → networking`) and, for each hop, the specific crossing
reference responsible (`net.a → db.b via ...`), so the operator can see exactly
which references to break. Node and successor ordering are sorted, so the
reported cycle is deterministic.

The gate is the last step of `pipeline.Analyze`: a detected cycle is returned as
an error and the pipeline never reaches emission.

---

## 8. Emit — carving the code  `[internal/emit]`

Emit writes one subdirectory per module, each a complete Terraform root:

- **`main.tf`** — the module's assigned `resource`/`data` blocks, moved
  **verbatim** via `hclwrite` (comments and formatting preserved), with two
  transforms applied:
  - **Cross-module refs rewritten to `var.<input>`** (`rewrite.go`). This is
    token-level surgery, not string replacement: it walks the attribute token
    stream, greedily matches the longest known cross-module producer prefix
    (3 segments for `data.t.n`, 2 for `t.n`), and replaces the whole reference —
    including any trailing `.attr` and `[idx]` steps — with the input variable's
    tokens. Operating on tokens preserves surrounding formatting and handles refs
    nested arbitrarily deep in expressions.
  - **Cross-module `depends_on` entries dropped.** The dependency is now carried
    by the input variable (or an OrderingEdge); leaving a `depends_on` pointing at
    a resource that no longer exists in this root would be a parse error. If a
    `depends_on` list empties out, the attribute is removed entirely.
- **`variables.tf`** — a `variable "<name>" { type = string }` per boundary
  input, with a description recording its origin (external `var.*` vs. upstream
  `module→output`). Written empty-skipped.
- **`outputs.tf`** — an `output "<name>" { value = <addr>.<attr> }` per boundary
  output, exposing exactly the attribute recorded in `RefAttrs` (so
  `.result`, not the whole object). Written empty-skipped.
- **A `terraform{}` block** carrying `required_providers` propagated from the
  monolith root into every carved root, so each root can resolve its providers
  independently.

**Structural blocks** (`structural.go`) — beyond resource/data, three kinds of
block travel with the module that uses them, duplicated in the same way
`required_providers` is (never split, since they carry no state):

- **`provider` blocks** — carved into every module that uses that provider
  (derived from its resource/data types). A provider's *own body* is walked for
  references too: a `var`/`local` used only in provider config (e.g. a
  region/endpoint) is pulled into the module, and a cross-module producer
  referenced in provider config is rewritten to `var.<input>` like any resource
  body.
- **`locals`** — every local a module references (followed transitively: a local
  that uses another local or a variable pulls those in), emitted as one `locals`
  block with cross-module value refs rewritten to `var.<input>`.
- **original `variable` blocks** — the monolith's own `variable` declarations,
  carried verbatim (preserving `type`/`default`/validation) into each consuming
  module. Because a declared root variable travels with the module, boundary does
  *not* treat it as an external input — only an *undeclared* `var.*` becomes one.

**Module calls** are first-class: a `@demono:move` on a `module` block places it
(arity 1, a stateful singleton), `movedBlocks` carves the block, its local
`source = "./..."` directory is copied into the owning root, its input refs to
cross-module producers rewrite to `var.<input>`, and a `module.<name>.<output>`
consumed elsewhere becomes a CrossEdge (producer root re-exposes it as an
`output`). State moves the whole `module.<name>.*` subtree (§9).

Decorator comments are stripped from moved blocks (they've served their purpose
and would be dead noise in the output). Everything is run through
`hclwrite.Format` before writing.

**Detached, by design:** no `snapcd_*` blocks are generated. The carved roots are
valid standalone Terraform; the CrossEdges/OrderingEdges are the recipe for
wiring them into Snap CD later.

---

## 9. State carve — carving the state  `[internal/statemove]`

Code without state would re-create everything. State carving relocates each
resource's state entry into its module's own state file, so the new roots adopt
the *existing* infrastructure rather than rebuilding it.

The operation is deliberately **local-only and reversible**:

1. **Obtain the monolith state as a local file** — either a provided
   `--state-file`, or pulled once via `terraform state pull` from the configured
   backend. (Pull reads; it does not lock or write the backend.)
2. **Back it up** before any mutation, so a mid-run failure recovers to the
   pre-run snapshot.
3. **Carve** with `state mv -state=<monolith> -state-out=<module> src dst`
   (`tfexec.StateMv` with `State`/`StateOut`), which reads one local file and
   writes another — the backend is never touched.
4. **The remainder module inherits the leftovers.** Its resources are *not*
   moved (moving a resource to itself within one file is an error). After every
   other module is carved out, the monolith state contains exactly the
   remainder's resources, so that carved-down file simply *becomes* the
   remainder's state. This subtlety — separating "move these out" from "keep the
   rest" — was a real bug fix, not incidental.

Only **managed resources** carry state, so `BuildPlan` moves resources and skips
data sources entirely. A duplicated data source contributes **no** move — its
copies are re-read in each module at plan time.

**Nothing is pushed.** In v1 the carved per-module state files are the artifact;
guarded push to new/empty backend locations (with serial/lineage conflicts as
surfaced errors, never `-force`) is a later feature.

The `SourceAddr`/`DestAddr` split in `Move` is identical for flat roots today but
exists so nested re-addressing can be added without an interface change.

---

## 10. Proof — proving the split is inert  `[internal/proof]`

This is the most ambitious piece and the reason Demonolith is a migration tool,
not just a generator.

**The problem it solves:** a carved module planned *in isolation* has its
upstream-sourced inputs **unset**, because the input↔output wiring
(`snapcd_module_input_from_output` in production) is runtime metadata Terraform
never sees. Plan it naively and Terraform either errors on a missing variable or
plans a spurious diff. So a naive per-module plan can't prove inertness.

**The oracle plays Snap CD's runtime role locally.** `proof.Run`:

1. **Topo-orders** the modules over the CrossEdges + OrderingEdges (Kahn's
   algorithm, deterministic — `topo.go`), so every producer is planned before its
   consumers.
2. For each module in order, **assembles its input values**: external inputs from
   `--var`-style options; upstream inputs **threaded** from the *already-extracted
   output values* of the producing module. (A missing upstream value at this
   point is a topo/threading bug and is surfaced as an error, not defaulted.)
3. **Plans** the carved root against a *copy* of its carved state, with those
   inputs supplied as `-var`, and refresh **off** by default (fast,
   credential-free, state-only; `--refresh` gives the authoritative run).
4. **Asserts zero-diff**: counts create/destroy/update from the plan JSON;
   `ZeroDiff` requires **zero creates and zero destroys** (in-place updates are
   counted and reported but don't fail the proof).
5. **Extracts this module's output values** from `planned_values.outputs`,
   stringifies them (matching Snap CD's string-passing coercion — `stringify.go`
   renders scalars bare, composites as compact JSON, integers without `.0`), and
   makes them available to downstream consumers.

In the refactoring case the infrastructure already exists, so every output value
is known at plan time and no apply is ever needed — the whole proof is
plan-only.

**The proof** is the bundle of per-module plans, each showing zero create/destroy
against carved state with correctly threaded inputs. `Result.OK` is true iff
every module is zero-diff. That's the evidence the split changes nothing
operationally *and* that the computed wiring is correct — because a wrong
`CrossEdge` would thread a wrong value and surface as a diff.

---

## 11. The CLI  `[internal/cli]`

Three commands split at the code/state line, connected by a versioned manifest
(`internal/manifest`); the full design and its user stories are in
`REDESIGN.md`:

```bash
demonolith refactor                  # analyze + emit code + write the manifest (offline)
demonolith migrate --engine tofu     # execute the manifest's state moves (local copies)
demonolith verify  --engine tofu     # graph-threaded zero-diff proof + verdict sidecar
```

`refactor` runs `pipeline.Analyze` (which includes the cycle gate), emits, and
writes `demonolith-refactor-{datetime}.yaml`; `--check` is the CI drift gate.
`migrate` replays the manifest's moves without re-analyzing, guarded by a
staleness check and an idempotent resume, and writes a receipt sidecar.
`verify` re-analyzes, threads inputs, and proves zero create/destroy — against
the receipt's carved states when a migration has run, or an ephemeral throwaway
carve when not. Exit codes: 0 success, 1 operational error, 2 negative verdict;
`--output json` emits machine-readable reports.

---

## 12. Testing & fixtures

Tests live beside each package (`internal/*/*_test.go`). Three self-contained
fixtures under `testdata/` drive them, all built on the `random`/`time`
providers so the entire pipeline — **including the proof oracle** — runs
**offline with zero credentials**:

- **`monolith/`** — the full-featured fixture: cross-module value edge, a
  cross-module `depends_on`, catchall resources, and a multi-target data source.
- **`statefix/`** — a fast `random`-only fixture (producer module `a` → consumer
  module `b`, plus a catchall), used by the end-to-end state+proof test.
- **`cyclic/`** — two mutually-referencing resources in different modules; proves
  the cycle gate refuses.

The pure stages (parse, decorator, placement, boundary, cycle, emit) run
anywhere. The `statemove` and `proof` tests need a real `terraform`/`tofu`
binary and **self-skip** via `testsupport.RequireEngine(t)` when none is on PATH
(overridable with `DEMO_TF_EXEC`).

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```

---

## 13. Design principles, distilled

- **Reason over a graph, never over text.** Every decision joins on
  `Address.String()`; edges come from AST traversal so nothing hidden in a
  function call or index is missed.
- **Total, explicit assignment.** Nothing is unplaced; the catchall guarantees
  it, and it's reported so no defaulting is silent.
- **Fail loud on ambiguity.** A malformed/orphan decorator, an impossible cycle,
  a missing upstream value — all hard errors with source positions, never a
  silent guess.
- **Value edges and ordering edges are different things.** Conflating them
  produces spurious wiring; keeping them separate (`CrossEdge` vs.
  `OrderingEdge`, `Refs` vs. `DependsOnOnly`) is a load-bearing distinction.
- **Never touch the real backend in v1.** Pull (read-only) and carve local
  copies; back up before mutating; the carved files are the artifact.
- **Prove, don't assert.** Inertness is demonstrated by a real graph-threaded
  plan bundle against real state, not asserted from the model — which is what
  makes the tool trustworthy for a production migration.
```
