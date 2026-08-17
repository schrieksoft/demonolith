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

What a user runs is four commands, two doing and two judging, connected by the
manifest:

```
   decorate sources
        │
   ┌────▼─────┐  emits carved roots            ┌──────────┐
   │ refactor │ ────────────────────────────►  │   diff   │  does the committed
   │  (code)  │  writes the manifest           │ (judge)  │  plan match the
   └────┬─────┘                                └──────────┘  committed source?
        │
        │   demonolith-refactor.yaml — the reviewable contract
        │
   ┌────▼─────┐  carves state (local copies)   ┌──────────┐
   │ migrate  │ ────────────────────────────►  │  prove   │  zero-diff proof: does
   │ (state)  │  writes a receipt              │ (judge)  │  every root plan inert?
   └──────────┘                                └──────────┘  writes a verdict
```

Underneath, the code side is one shared analysis pipeline, `pipeline.Analyze` —
pure, offline, deterministic — run by `refactor` (to emit), by `diff` (to
re-derive and compare), and by `prove` (to recover the boundary the proof
threads values over):

```
Parse       *.tf → reference graph                     [hclgraph]
Decorators  # @demono:move <module> comments           [decorator]
Placement   resources/modules by decorator;            [placement]
            data sources follow their consumers
Boundary    cross-module refs → input/output wiring    [boundary]
Cycle gate  refuse impossible splits                   [cycle]
```

The stages that do I/O sit behind the commands: Emit (`refactor`) writes the
carved roots via `hclwrite`; StateCarve (`migrate`) and Proof (`prove`) shell
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
   - `data` → **no decorator, ever** (a hard error). A data source is a
     stateless read and follows its consumers automatically (§5) — duplicated
     into every module that references it, like locals and variables. There is
     nothing for a human to decide, so a decorator would only mislead.

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

Two pipeline commands split exactly at the code/state line, each with a gate
command that judges its output, connected by the manifest (§12):

```bash
demonolith refactor                  # code only: analyze + emit + write the manifest (offline)
demonolith diff                      # gate: committed roots + manifest match the committed source? (offline)
demonolith migrate --engine tofu     # state only: execute the manifest's state moves (local copies)
demonolith prove   --engine tofu     # gate: zero-diff proof — graph-threaded plans + verdict sidecar
```

There are no positional arguments; every command takes `--root-dir`, defaulting
to the current directory — matching the engines' own convention, so the normal
invocation is bare from inside the monolith root, and CI jobs running from a
repo root pass the flag. `--engine {terraform|tofu}` has **no default**: the
choice of binary is always explicit (`--exec-path` overrides resolution).

- **`refactor`** runs `pipeline.Analyze` (which includes the cycle gate), emits
  the carved roots (default `<root>/.demono/modules`), and writes the manifest.
  Pure and offline — no engine, no state, safe to re-run while iterating on
  decorators.
- **`diff`** re-runs analysis+emit **in memory** and compares against the
  committed roots and manifest. It writes nothing and takes no placement flags —
  the remainder-module name comes from the committed manifest, so nothing can
  skew the comparison. Exit 0 means the committed plan is exactly what the
  committed source produces; exit 2 means they differ, with a per-file summary
  of what changed. This is the CI proof that a PR's migration plan is honest.
- **`migrate`** executes the manifest's state moves (§9 mechanics) against
  local copies, backup first, and writes a receipt. `--dry-run` prints the
  resolved `state mv` operation list without an engine or any file touched.
- **`prove`** runs the proof oracle (§10) as a standalone command, writing a
  verdict sidecar. It sources carved state in two modes, chosen automatically:
  **post-migrate** (from an intact receipt — proving the actual migration
  output) or **ephemeral** (its own throwaway carve into a temp dir, discarded
  after — what lets a credential-free PR job prove a split whose real migration
  hasn't happened yet; safe by construction since carving is local-only).

**External inputs** (the monolith's own `var.*`) resolve for the proof the way
the monolith resolved them, in ascending precedence: the root's auto-loaded
`terraform.tfvars` / `*.auto.tfvars`(.json), explicit `--var-file` files in the
order given (a named file that is missing is an error, unlike the auto-loaded
set), `TF_VAR_*` environment variables, `--var k=v` flags. Only names the
boundary declares as external are collected; cross-module inputs are **never**
user-suppliable — the proof threads them from producer outputs itself, so a
wrong wiring cannot be papered over with a hand-supplied value. `--no-tfvars-file`
keeps every value in memory (for credentialed CI where secrets must not land in
a working tree); `--keep-tfvars` retains the `generated.auto.tfvars` files as
the permanent wiring for detached roots.

**Machine interface.** Exit codes are uniform: `0` success, `1` operational
error (bad flags, missing binary, engine failure), `2` **negative verdict** —
the run worked but the answer is "no" (the committed output differs from the
source, a module plans a create/destroy, a stale or inapplicable manifest). Pipelines can therefore distinguish "the
split is wrong" from "the job broke". `--output json` on `diff`/`migrate`/`prove`
replaces the human report with one JSON document on stdout. Without a TTY
nothing ever prompts; `--interactive` is an error rather than a silent
fallback.

**Interactive mode** (`refactor --interactive`, `migrate --interactive`) is a
front-end, not a parallel channel: every choice resolves to something that
exists non-interactively. Refactor's guided loop triages the catchall and
writes accepted assignments **back into the source as decorators** — the source
stays the single source of truth, the session's outcome is reviewable in git,
and the next run reproduces it. Migrate previews the resolved move plan and
confirms once before executing. Every prompt defaults to the non-destructive
answer.

Not yet built, by design: `migrate --push` (guarded `state push` into new,
empty backends — local-only remains the only mode, per §9), `attach`
(unnecessary for a control plane that ingests the manifest directly), and
interactive cycle resolution (a cycle is reported, the breaking moves are not
yet offered).

---

## 12. The manifest and its sidecars  `[internal/manifest]`

The manifest is the durable contract between the code side and the state side:
`refactor` computes *what* must move and *how* modules wire together;
`migrate` and `prove` replay that plan without re-deriving it. That indirection
is what lets a different actor — CI, a control plane — execute a plan someone
else computed and reviewed.

**One manifest per root.** `refactor` writes `demonolith-refactor.yaml` into
the root dir, overwriting it each run. The monolith root is the single refactor
source: the plan is always re-derived from it in full, so there is no
meaningful sequence of manifests — history lives in version control, and
re-refactoring an already-carved root (decorating inside emitted output) is
deliberately out of scope.

**Contents:** module assignments (`modules`, with root-relative dirs), the
`catchall` list, `duplicated_data`, `state_moves` (managed addresses only;
remainder resources are absent because the carved-down monolith state *becomes*
the remainder's state), `cross_edges` (the value wiring a control plane needs
at adoption time, attribute included), `ordering_edges`, and `emit_checksum` —
a hash over the emitted roots that excludes later-stage artifacts (lock files,
state files, plan files, generated tfvars), so running the pipeline never
invalidates its own manifest.

**The schema is a public, versioned API.** PR reviewers read it, CI parses it,
a control plane may ingest it. Changes within a major version are additive
only — new optional keys, never renamed or removed ones; a breaking change
bumps `version`, and every consumer refuses a manifest whose major version it
doesn't know rather than guessing. External input *names* may appear in
manifest or sidecars; input *values* never do.

**Sidecars** record execution, timestamped (`{datetime}` is compact UTC so
lexical order is date order):

- **Receipt** (`demonolith-migrate-{datetime}.yaml`) — which moves ran and with
  what outcome, the carved state paths, the backup path, and the executed
  manifest's `emit_checksum`. The checksum is what ties a receipt to one
  manifest *generation* (the filename is constant), so re-running `refactor`
  invalidates old receipts. The idempotency check consults the receipt first; a
  complete receipt for the current generation skips the run. On resume after a
  partial failure, migrate reuses the working monolith state (never re-pulls —
  that would corrupt a partial carve), classifies each move as pending or
  already-applied by inspecting state addresses, and refuses a manifest whose
  moves match neither side.
- **Verdict** (`demonolith-prove-{datetime}.yaml`) — per-module
  create/destroy/update counts, the proof order, and external input names.
  "This split was proven inert" is an artifact, not terminal scrollback.

Plan, receipt, proof: the migration's audit trail is three files.

---

## 13. Usage patterns

The spine is the same everywhere — decorate → `refactor` → review → validate
(`diff` + `migrate --dry-run` + `prove`) → `migrate` → adopt — and only two
things vary: who executes each step, and where external input values come from.
Where the monolith's state lives is deliberately not an axis: migrate takes a
local `--state-file` or pulls read-only from whatever backend the root
configures.

- **Solo.** One person runs the whole spine from inside the root; external
  inputs come from the root's own tfvars files (auto-loaded, nothing passed by
  hand); `--keep-tfvars` makes the generated wiring files the permanent
  value-passing mechanism for the detached roots.
- **Team CI.** The dev authors the plan (`refactor`, decorators, committed
  roots + manifest in the PR); PR CI judges it credential-free — `diff` gates
  honesty, `--dry-run` renders the move list into the job log, `prove` in
  ephemeral mode gates inertness — with external inputs injected via `TF_VAR_*`
  or `--var` and `--no-tfvars-file` keeping secrets off disk; a post-merge job
  executes the reviewed manifest verbatim (`migrate`, then `prove` in
  post-migrate mode) and archives the receipt and verdict.
- **Control plane.** Same as team CI through the merge; the migration itself is
  executed by a control plane that ingests the manifest — `cross_edges` become
  input-from-output wirings, `ordering_edges` become dependency edges, and the
  ordering the detached stories could only report is enforced natively. Needs
  nothing from demonolith beyond the versioned manifest, the JSON verdicts, and
  the exit-code contract.

---

## 14. Testing & fixtures

Tests live beside each package (`internal/*/*_test.go`). Four self-contained
fixtures under `testdata/` drive them, built on credential-free providers so
the entire pipeline — **including the proof oracle** — runs **offline with zero
credentials**:

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

The pure stages (parse, decorator, placement, boundary, cycle, emit) run
anywhere. The `statemove` and `proof` tests need a real `terraform`/`tofu`
binary and **self-skip** via `testsupport.RequireEngine(t)` when none is on PATH
(overridable with `DEMO_TF_EXEC`).

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```

---

## 15. Design principles, distilled

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
- **The manifest is the contract.** The plan is computed once, reviewed as an
  artifact, and replayed verbatim — never silently recomputed by the actor
  executing it; guards (checksums, versioning) make a mismatch a refusal, not a
  guess.
