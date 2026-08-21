# Demonolith — Design

This document explains the concepts Demonolith works with and how its stages
fit together — the design behind the tool, for people working on it. The
user surface — commands, flags, workflows, limitations — is the `README.md`'s
and is not repeated here; the package doc-comments carry the detail.

---

## 1. The problem

A Terraform/OpenTofu **monolith** is a single root directory whose `*.tf` files
declare everything — networking, database, shared bits — managed as one state
and applied as one unit. Monoliths get slow, risky, and coupled: a one-line
change re-plans the whole world, and one broken resource can block unrelated
ones.

The goal is to split that monolith into **independent per-module roots**, each a
self-contained Terraform root with its own state, that can be planned and applied
on its own. In production these roots are orchestrated by **Snap CD**: where the
monolith passed a value from resource A to resource B implicitly (same graph,
same state), the split roots pass it explicitly — module A publishes an `output`,
module B declares a `variable`, and Snap CD wires one to the other at runtime.

The hard requirement is that the split be **operationally inert**: after
split, planning each new root against its share of the old state must show
**zero changes** — no creates, no destroys, no in-place updates. Nothing is
rebuilt or altered; only the *organization*
of the code and state changes. Demonolith's job is to perform that split and to
*prove* the inertness.

### v1 scope

Demonolith is a **one-shot splitter**. One deliberate cut from the fuller
design: **string-typed inputs** — every generated `variable` is
`type = string`, matching Snap CD's stringified value-passing; richer type
coercion is deferred.

State handling is staged, never destructive: `migrate map` splits **local
copies** (the monolith's backend is only ever read), the proof consumes those
copies, and `migrate run` seeds each module's **new, empty** backend location
from them — the monolith's own state is never written, and retiring it is a
deliberate human cutover.

The module directories themselves are plain Terraform with `variable`/`output`
boundaries — usable detached, with no control plane at all. The wiring into
Snap CD ships alongside them: by default `refactor` also emits a **bootstrap
module** (§12a) of `snapcd_*` resources that instructs Snap CD to deploy the
split-out modules, generated entirely from the map.

What it keeps — and what makes it more than a code generator — is the **proof
oracle** (§10): a graph-threaded, zero-diff plan bundle run against real
`terraform`/`tofu`.

---

## 2. The pipeline at a glance

The user-facing surface — two command families, `refactor map → run →
validate → diff` and `migrate map → prove → run → verify`, each pausing for
approval before its run step — is the README's. What matters here is what
sits underneath: the code side is one shared analysis pipeline,
`pipeline.Analyze` — pure, offline, deterministic — run by `refactor map`
(to compute), `refactor run` and `refactor diff` (to re-derive and compare),
and the migrate proofs (to recover the boundary they thread values over):

```
Parse       *.tf → reference graph                     [hclgraph]
Decorators  # @demono:move <module> comments           [decorator]
Placement   resources/modules by decorator;            [placement]
            data sources follow their consumers
Boundary    cross-module refs → input/output wiring    [boundary]
Cycle gate  refuse impossible splits                   [cycle]
```

The stages that do I/O sit behind the commands: Emit (`refactor run`) writes
the module directories via `hclwrite`; the state split (`migrate map`), the
proofs (`migrate prove`/`verify`), and the push (`migrate run`) shell out to
a real `terraform`/`tofu` binary via `terraform-exec`.

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

- **`RefAttrs`** — for each referenced resource/data/module producer, every
  *distinct attribute path* used (`result` in `random_uuid.x.result`). This is
  what lets an emitted `output` expose `random_uuid.x.result` rather than the
  whole object — and lets a producer referenced through several attributes
  expose one output per attribute.
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
   - `data` → **no decorator, ever** (a hard error). A data source is a
     stateless read and follows its consumers automatically (§5) — duplicated
     into every module that references it, like locals and variables. There is
     nothing for a human to decide, so a decorator would only mislead.

3. **Attaches to an address, not a line.** A decorator binds to the resolved
   block address (matching `hclgraph.Address.String()`), so downstream stages
   join on identity, not position.

4. **Total assignment, always.** A block with no decorator is not an error — it
   falls to the **catchall / remainder** module (`--remainder-module`, default
   `legacy`). Every resource and data source therefore has a home; nothing is
   ever left unplaced.

An orphan decorator (a valid `@demono:` comment not adjacent to any decoratable
block) is also a hard error — it means the author expected placement that won't
happen.

---

## 5. Core concept: placement  `[internal/placement]`

**Placement** turns the graph + decorators into a *total assignment* of nodes
to modules, in two passes. **Resources and module calls** are placed by
decorator (or fall to the catchall). **Data sources** are then placed
automatically: a data source is copied into every module that references it —
directly, or transitively through locals and other data sources — computed by
walking the reference graph backwards from the data node to its placed
consumers. One consuming module → single home; several → duplicated into each;
none → the remainder, reported. `variable`, `local`, and `output` nodes are
*structural*: a variable/local is materialized wherever its consumers land, an
output is generated at a boundary.

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
| to a resource producer **in another module** P | P gets an `output`; C gets a `variable`; a **CrossEdge** records the wiring |
| to a `var.*` (a former monolith root variable) | C gets an **external** input (a root variable each module re-declares) |
| to a `local.*` | resolved by following the local's *own* upstream refs into C (a local is inlined, so its dependencies become C's boundary edges) |
| a cross-module **`depends_on`** (from `DependsOnOnly`) | an **OrderingEdge** — whole-module apply-order, no value wiring |

The key output types:

- **`CrossEdge`** — a value-carrying boundary crossing, per referenced
  *attribute*: *producer module exposes `OutputName`, threaded into consumer
  module's `InputName`.* Output/input names derive from the producer address
  (`<type>_<name>`, or `data_<type>_<name>`); a producer referenced through
  several distinct attributes gets attr-scoped names (`module_subnet_subnet_id`,
  `module_subnet_cidr_block`) so each attribute carries its own value, while a
  single-attribute producer keeps the plain name. The two names are independent
  by construction; v1 keeps them aligned for readability.
- **`OrderingEdge`** — a whole-module ordering dependency with **no**
  variable/output. It exists only to say "apply P before C." In detached v1 this
  is *reported* so the operator enforces ordering; Snap CD's graph would carry it
  natively.
- **`ModuleBoundary`** — per module, the full set of `Inputs` (upstream +
  external) and `Outputs` it must declare. This is the module's wiring surface,
  consumed directly by Emit.

Duplication interacts cleanly here — and, since data sources follow their
consumers (§5), by construction: every consumer of a data source has a copy in
its own module, so **a data-source result never crosses a boundary** and never
generates wiring. What a data source *can* contribute cross-module is its
argument side: a copy whose argument reads a resource in another module gets an
input for it like any other consumer.

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

## 8. Emit — splitting the code  `[internal/emit]`

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
  monolith root into every module directory, so each root can resolve its providers
  independently.

**Structural blocks** (`structural.go`) — beyond resource/data, three kinds of
block travel with the module that uses them, duplicated in the same way
`required_providers` is (never split, since they carry no state):

- **`provider` blocks** — copied into every module that uses that provider
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
(arity 1, a stateful singleton), `movedBlocks` moves the block, its local
`source = "./..."` directory is copied into the owning root, its input refs to
cross-module producers rewrite to `var.<input>`, and a `module.<name>.<output>`
consumed elsewhere becomes a CrossEdge (producer root re-exposes it as an
`output`). State moves the whole `module.<name>.*` subtree (§9). In
**monorepo mode** (`--monorepo`) local child-module dirs are not copied:
the call's `source` is rewritten to the relative path from the module directory back
to the original in-repo dir. The default writes fully standalone roots,
shippable to separate repos.

Decorator comments are stripped from moved blocks (they've served their purpose
and would be dead noise in the output). Everything is run through
`hclwrite.Format` before writing.

**The module directories stay plain Terraform** — no `snapcd_*` blocks inside them,
so they are valid standalone roots. The Snap CD wiring lives in the separate
bootstrap module (§12a), generated from the map's CrossEdges and
OrderingEdges.

---

## 9. State split — splitting the state  `[internal/statemove]`

Code without state would re-create everything. The state split relocates each
resource's state entry into its module's own state file, so the new roots adopt
the *existing* infrastructure rather than rebuilding it.

The operation is deliberately **local-only and reversible**:

1. **Obtain the monolith state as a local file** — either a provided
   `--state-file`, or pulled once via `terraform state pull` from the configured
   backend. (Pull reads; it does not lock or write the backend.)
2. **Back it up** before any mutation, so a mid-run failure recovers to the
   pre-run snapshot.
3. **Split** with `state mv -state=<monolith> -state-out=<module> src dst`
   (`tfexec.StateMv` with `State`/`StateOut`), which reads one local file and
   writes another — the backend is never touched.
4. **The remainder module inherits the leftovers.** Its resources are *not*
   moved (moving a resource to itself within one file is an error). After every
   other module is moved out, the monolith state contains exactly the
   remainder's resources, so that whittled-down file simply *becomes* the
   remainder's state. This subtlety — separating "move these out" from "keep the
   rest" — was a real bug fix, not incidental.

Only **managed resources** carry state, so `BuildPlan` moves resources and skips
data sources entirely. A duplicated data source contributes **no** move — its
copies are re-read in each module at plan time.

**The split itself pushes nothing.** The guarded push into each module's
derived backend is `migrate run`'s job (§11); the monolith's own backend is
only ever read.

The `SourceAddr`/`DestAddr` split in `Move` is identical for flat roots today but
exists so nested re-addressing can be added without an interface change.

---

## 10. Proof — proving the split is inert  `[internal/proof]`

This is the most ambitious piece and the reason Demonolith is a migration tool,
not just a generator.

**The problem it solves:** a split-out module planned *in isolation* has its
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
3. **Plans** the module directory against a *copy* of its split state, with those
   inputs supplied as `-var`, and refresh **always off**: the proofs judge the
   migration's fidelity to the pulled state. Drift is out of scope by design —
   ruled out by the prerequisite clean monolith plan, and watched by whatever
   plans the modules after adoption.
4. **Asserts zero-diff**: counts create/destroy/update from the plan JSON;
   `ZeroDiff` requires **zero changes of any kind** — an in-place update fails
   too, because a wrong input value that does not force a replacement still
   surfaces as an update.
5. **Extracts this module's output values** from `planned_values.outputs`,
   stringifies them (matching Snap CD's string-passing coercion — `stringify.go`
   renders scalars bare, composites as compact JSON, integers without `.0`), and
   makes them available to downstream consumers.

In the refactoring case the infrastructure already exists, so every output value
is known at plan time and no apply is ever needed — the whole proof is
plan-only.

**The proof** is the bundle of per-module plans, each showing zero changes
against split state with correctly threaded inputs. `Result.OK` is true iff
every module is zero-diff. That's the evidence the split changes nothing
operationally *and* that the computed wiring is correct — because a wrong
`CrossEdge` would thread a wrong value and surface as a diff.

---

## 11. The CLI  `[internal/cli]`

(What each command does, its flags, and the workflows around them are the
README's; this section is the design behind that surface.)

**One meaning per verb.** `map` produces the reviewable plan, `run` executes
it with guards, `validate` asks the engine, `diff` compares the code split
on disk against its source, `prove` rehearses, `verify` judges the outcome.
The bare family commands run their steps in order and **pause for approval
before the run step**; without a TTY the pause is a refusal naming `-y`,
never a silent proceed, and `--interactive` is likewise an error rather
than a fallback. Every interactive choice resolves to a flag value or a
decorator, so a guided session is reproducible non-interactively.

**Every guard has one home.** Map-time checks — backend-type support, the
reserved `snapcd` module name, `--out` inside the root — live in
`refactor map` and are recorded into the map. Whether the target
directories may exist is `refactor run`'s own gate: they must be gone, or
`--overwrite` deletes them whole; run also refuses a source that no longer
matches the map (semantic staleness) and is the step that finalizes the
`emit_checksum`. On the state side the coupling is receipts: `migrate
prove` requires the generation's migrate-map receipt with intact
artifacts; `migrate run` requires a passing prove receipt **no older than
the map receipt** (`--unproven` is the explicit override — run never
proves on its own); `migrate verify` requires the run receipt. The push
guard is run's alone: a destination must be empty or already hold this
module's state — same lineage, or identical content from a fresh split,
the idempotent skip that makes a crashed run retryable — and `--force` is
the one explicit, loudly-warned exception (`state push -force`). The
monolith's own state is never written.

**Artifacts are materialized by the step that owns them.** Run and verify
write each module's gitignored `demono.env` (0600) from the root's
init-time resolved backend config, in the engine's official environment
variables. Prove (and verify) write `demono.root.tfvars` from the engine's
own variable precedence. Run writes `demono.graph.tfvars` from the applied
state, with expression-valued cross-module outputs filled from the proof's
planned values, which prove records in a gitignored workdir sidecar.
Cross-module inputs are never user-suppliable — the proof threads them
itself, because a hand-supplied value would prove nothing.

**Machine interface.** Exit codes are uniform: `0` success, `1` operational
error, `2` **negative verdict** — the run worked but the answer is "no" (the
split on disk differs from the source, a module plans a create/destroy, a stale or
inapplicable map). Beyond the exit code, the machine-readable record of what
happened is the receipts (§12) — stdout is for people.

Not yet built, by design: interactive cycle resolution (a cycle is reported,
the breaking moves are not yet offered). Pushes are exercised end to end on
local-type backends in the test suite; the cloud backend types (s3,
azurerm, gcs, consul, cos, oss, kubernetes, pg, remote) are unit-tested at
the derivation level, with the runnable sample exercising the s3 derivation
against a real S3-compatible store.

---

## 12. Receipts  `[internal/manifest]`

Every command writes a receipt; the map receipt ("the map") is the durable
contract between the code side and the state side:
`refactor` computes *what* must move and *how* modules wire together;
`migrate` and `prove` replay that plan without re-deriving it. That indirection
is what lets a different actor — CI, a control plane — execute a plan someone
else computed and reviewed.

**One map per root, two states.** `refactor map` writes
`demonolith-refactor-map.yaml` into the root dir, overwriting it each run; a
freshly written map has no `emit_checksum` yet, and every downstream command
refuses it until `refactor run` executes the plan and finalizes the checksum. The monolith root is the single refactor
source: the plan is always re-derived from it in full, so there is no
meaningful sequence of maps — history lives in version control, and
re-refactoring an already-module directory (decorating inside emitted output) is
deliberately out of scope.

**Contents:** module assignments (`modules`, with root-relative dirs and each
module's external input *names*), the `catchall` list, `duplicated_data`,
`state_moves` (managed addresses only; remainder resources are absent because
the whittled-down monolith state *becomes* the remainder's state), `cross_edges`
(the value wiring a control plane needs at adoption time, attribute included),
`ordering_edges`, the output mode (`monorepo`, `bootstrap`, `bootstrap_dir`),
the `backend` section — the monolith's backend type and **each module's
derived state location**, putting where every state will live into the
reviewed plan — and `emit_checksum` — a hash over all generated output
(module directories plus the bootstrap module) that excludes later-stage artifacts
(lock files, state files, plan files, generated tfvars), so running the
pipeline never invalidates its own map.

**The schema is a public, versioned API.** PR reviewers read it, CI parses it,
a control plane may ingest it. Changes within a major version are additive
only — new optional keys, never renamed or removed ones; a breaking change
bumps `version`, and every consumer refuses a map whose major version it
doesn't know rather than guessing. External input *names* may appear in
map or sidecars; input *values* never do.

**Sidecars** record execution in fixed-name files, like the map itself:
the datetime lives *inside* each document (`created`), alongside the
generation tie (`map_checksum`), so an external system can tell what
ran, when, and for which plan without parsing filenames; history lives in
version control:

- **Receipts** (`demonolith-migrate-map.yaml`, `demonolith-migrate-run.yaml`)
  — one canonical file per migrate step, overwritten per execution: the
  action-"map" receipt records which moves ran, the
  split state paths, and the backup path; the action-"run" receipt records where
  each module's state was pushed. Both carry the executed map's
  `emit_checksum`, which ties them to one map *generation* (the filename
  is constant), so re-running `refactor` invalidates old receipts. A re-run
  never trusts leftovers: `migrate map` pulls fresh and redoes the whole
  split, comparing each new carve against any previous one, and `migrate
  run` re-derives idempotency from the destinations themselves (lineage or
  identical content), so a crashed run is retried by just re-running.
- **Verdicts** — mode-"prove" (`demonolith-migrate-prove.yaml`) judges the split
  artifacts before the push; mode-"final" (`demonolith-migrate-verify.yaml`) judges
  the pushed states against the real backends. Both carry per-module create/destroy/update counts, the
  proof order, external input *names*, and the generation checksum — which is
  what lets `migrate run` demand a prove receipt for exactly this map.

Map and receipts: the migration's audit trail is files, not terminal
scrollback.

## 12a. The Snap CD bootstrap module  `[internal/bootstrap]`

By default `refactor` emits one more root, `<out>/snapcd` (the module
name `snapcd` is reserved for it): a Terraform root of `snapcd_*` resources
that instructs Snap CD to deploy the split-out modules. It is generated **from
the map alone** — proof that the public contract carries everything a
control plane needs:

- a `snapcd_namespace` (into a stack/runner looked up by name), and one
  `snapcd_module` per module directory, its `source_subdirectory` derived from the
  map's module dir (prefixed by `var.source_subdirectory_prefix` for
  monoliths that don't sit at the repo root);
- every cross edge realized as `snapcd_module_input_from_output` — the
  input↔output threading the proof performed locally, now performed by Snap CD
  at runtime;
- every ordering edge realized as `snapcd_depends_on_module` — the dependency
  the detached stories could only report, now enforced;
- every external input passed through as `snapcd_module_input_from_literal`
  bound to a variable of the bootstrap module itself, so per-environment
  values are supplied where the bootstrap is applied (an external input whose
  name collides with a bootstrap variable is a refusal, not a rename).

The bootstrap is covered by the emit checksum (so `refactor diff` and the
staleness guards treat it as generated output) but is **not** a placement
module: it
appears in no state move and is never planned by `prove` — it needs a Snap CD
server, and applying it is the adoption step, not part of the split.
`--no-bootstrap` skips it entirely.

---

## 13. Testing & fixtures

Tests live beside each package (`internal/*/*_test.go`). Five self-contained
fixtures under `testdata/` drive them, built on credential-free providers so
the entire pipeline — **including the proof oracle** — runs **offline with zero
credentials**. Each fixture keeps its committed inputs (source and seed
states) in `in/`; tests copy them into a gitignored `out/` scratch tree via
`testsupport.OutDir`, so a run never mutates a fixture:

- **`monolith/`** — the full-featured fixture: cross-module value edge, a
  cross-module `depends_on`, catchall resources, and a data source duplicated
  into two modules by its consumers.
- **`statefix/`** — a fast `random`-only fixture (producer module `a` → consumer
  module `b`, plus a catchall), used by the end-to-end CLI and state+proof tests.
- **`e2e-split/`** — the full cross-reference matrix: every producer kind
  (resource, module output, data result) consumed from every consumer position
  (resource body, module input, provider config, local, data argument), applied
  and proven zero-diff.
- **`cyclic/`** — two mutually-referencing resources in different modules; proves
  the cycle gate refuses.
- **`sample/`** — the realistic showcase monolith (local and GitHub child
  modules, multi-consumer data sources), driven through both full pipelines
  end to end.

The pure stages (parse, decorator, placement, boundary, cycle, emit) run
anywhere. The `statemove` and `proof` tests need a real `terraform`/`tofu`
binary and **self-skip** via `testsupport.RequireEngine(t)` when none is on PATH
(overridable with `DEMO_TF_EXEC`).

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```

---

## 14. Design principles, distilled

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
- **The monolith's backend is read-only.** Pull, split local copies, back up
  before mutating; only the modules' new destinations are ever written, and
  only through `migrate run`'s guards.
- **Prove, don't assert.** Inertness is demonstrated by a real graph-threaded
  plan bundle against real state, not asserted from the model — which is what
  makes the tool trustworthy for a production migration.
- **The map is the contract.** The plan is computed once, reviewed as an
  artifact, and replayed verbatim — never silently recomputed by the actor
  executing it; guards (checksums, versioning) make a mismatch a refusal, not a
  guess.
