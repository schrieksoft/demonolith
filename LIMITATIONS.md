# Limitations

Inherent limits of the carve — things demonolith cannot make true on its own, because the information isn't in the graph or the fix would require judgment. Each entry says what breaks, how it shows up, and how to handle it manually. The proofs (`migrate prove` and `migrate verify`) are the net under all of them: a split that hits one of these and isn't handled surfaces as a plan error or a non-zero diff, not as silent corruption.

## Path-relative references

**What:** `path.module` / `path.root`, relative paths in `file()` / `templatefile()`, and file-reading data sources (`local_file`) resolve against the root that evaluates them. Carved roots live in a different directory, and demonolith neither copies referenced files nor rewrites paths.

**Shows up as:** `migrate prove` (or any plan of a carved root) failing with file-not-found. Because data sources follow their consumers automatically, one file-reading data source can land in several carved roots — every one of them needs the file.

**Handle it:** the durable fix is restructuring the monolith before refactoring to pass file *content* through a variable (`var.environment_json` + `jsondecode`) so nothing path-relative crosses the carve. Copying the referenced files into the carved roots also works, but interacts with the guards: files added after `refactor run` invalidate the emit checksum, so the migrate family refuses. The sequence that works is copy the files into the emitted roots, then `refactor run` **again** so the checksum includes them — migrate then passes, but `refactor verify` still refuses (a fresh emit does not produce the copies), so a team flow gated on verify needs the restructure, not the copy.

## Sensitive values crossing a boundary

**What:** cross-module wiring is generated `output` / `variable` pairs, and generated outputs are plain — no `sensitive = true`. A provider-marked sensitive value (a private key, a password) that crosses a boundary makes the engine refuse the producer root ("Output refers to sensitive values"). The same applies when a data source's copies read a sensitive value from another module.

**Shows up as:** the producer module erroring at plan time during `migrate prove`.

**Handle it:** prefer placement that keeps the sensitive edge inside one module (decorate producer and consumer into the same module — for a data source, that means keeping its consumers with the resource it reads). Hand-editing `sensitive = true` into an emitted root works mechanically but makes `refactor verify` and the staleness checksum refuse by design; if you must, do it as a documented post-adoption edit, after `migrate` and `prove` have run.

## Backend derivation covers common types only

**What:** the monolith's `backend` block is carried into every carved root with the state location postfixed per module — for `local`, `s3`, `azurerm`, `gcs`, `consul`, and `http`. Config supplied via `-backend-config` is handled through the init-time resolved config: locations derive from it, non-secret settings persist into each `backend.tf`, and credential-shaped attributes land in gitignored per-module `.env` files that migrate sources automatically. Other backend types are refused at plan time; a `cloud` block is not handled at all, and nested backend config (e.g. s3 `assume_role`) does not survive the flag path.

**Shows up as:** `refactor plan` refusing with the backend type or attribute named (for a flags-configured backend, the fix it names is initializing the root first, so the resolved config exists).

**Handle it:** pass `--no-backend` to carve without backend blocks and wire the backends by hand (`tofu init -migrate-state`, or a careful `state push` into an **empty** location — never `-force`), or move the missing attribute into HCL. Either way the monolith's own state is never written by demonolith; retiring it after every root proves clean is the human cutover step.

## A re-carve cannot re-push over its own earlier push

**What:** every `migrate plan` carve mints a fresh state lineage per module file. `migrate run`'s empty-target guard treats an identical-lineage occupant as an idempotent skip — which works as long as the carve workdir (`<out>/.state/`) that produced the pushed states still exists. Lose the workdir after a successful run, re-carve, and the new files carry new lineages: the push now refuses against your own earlier (semantically identical) push as "unrelated state".

**Shows up as:** `migrate run` refusing with "target … already holds unrelated state" on locations this migration itself seeded.

**Handle it:** keep the workdir until the cutover is done — the receipts point at it. If it is gone and the seeded states are confirmed to be this migration's output, delete those target states (they are reproducible from the carve; the monolith backup still exists) and re-run the pipeline, which re-carves and re-pushes cleanly.

## The prove verdict ages between prove and run

**What:** `migrate prove` judges the carved artifacts against the state as it was when `migrate plan` pulled it; `migrate run` pushes at a later moment. Real infrastructure drifting in between is invisible to the verdict — the same plan/apply gap every plan-then-execute system has.

**Shows up as:** `migrate verify` (which plans against the real backends, refresh on) reporting diffs that prove did not.

**Handle it:** keep the plan→prove→run window short (the bare `demonolith migrate` pipeline makes it seconds), run the migration inside a change freeze for the monolith, and treat `migrate verify` as the final authority.

## Whole-block placement only

**What:** decorators, state moves, and the manifest all address whole blocks. A `count`/`for_each` resource's instances cannot be split across modules, and there is no per-instance re-addressing.

**Handle it:** split the block in the monolith first (separate resources, or two blocks with partitioned `for_each` sets, using `moved` blocks to keep state), apply that as an ordinary monolith change, then refactor.

## `moved`, `import`, and `check` blocks are not carried

**What:** these root-level blocks are not graph nodes and are not emitted into any carved root.

**Shows up as:** silently absent from the carved roots.

**Handle it:** they are usually historical by the time of a split. If one is still load-bearing (an unapplied `import`, a pending `moved`), apply it in the monolith *before* refactoring so the state reflects it, and the carve then needs nothing.

## Workspace-dependent monoliths

**What:** `terraform.workspace` is a meta-reference that resolves to no node; each carved root plans in its own (default) workspace, so the value can silently change across the carve.

**Handle it:** replace `terraform.workspace` with an explicit variable in the monolith before refactoring.

## Data sources referenced only from provider config

**What:** provider blocks are not graph nodes, so a data source consumed *only* inside a `provider` block is invisible to automatic data placement and falls to the remainder; its value reaches the provider config as a wired-in variable.

**Shows up as:** a provider config in one module depending on a remainder-module output — correct, but it makes the remainder a producer you may not have expected.

**Handle it:** usually acceptable. If the coupling is wrong, give the data source a same-module consumer (reference it from a resource in the module whose provider needs it) so it lands there.

## Ordering edges are only reported without a control plane

**What:** a cross-module `depends_on` becomes an ordering edge — recorded in the manifest, enforced by nothing in detached use. Value edges self-enforce (a consumer can't plan without its input); ordering edges don't.

**Handle it:** apply the roots in the manifest's order (the proofs print the topo order), wire the ordering into your pipeline, or adopt into a control plane whose dependency graph carries it natively.

## Cross-module values are strings

**What:** every generated variable is `type = string`, matching stringified value-passing: scalars arrive bare, composites as compact JSON.

**Shows up as:** a consumer that indexes into a composite (`producer.list[0]`) receiving a JSON string instead — a plan error or a diff at proof time.

**Handle it:** keep composite-shaped edges inside one module, or adapt the consumer to `jsondecode(var.<input>)` in the monolith before refactoring so the same expression survives on both sides of the carve.

## Expression-valued module outputs cannot be materialized from state

**What:** the generated `generated.auto.tfvars` files resolve cross-module input values from the *applied* state. Child-module outputs are not stored in state, so a module-call output can only be resolved when it is a bare passthrough of a resource attribute; an output built from an expression (`"https://${random_uuid.x.result}..."`) cannot.

**Shows up as:** `migrate prove --create-tfvars` erroring with "cannot resolve module.<name>.<output> from state". The default in-memory threading is unaffected — it evaluates *planned* values, expressions included.

**Handle it:** skip `--create-tfvars` (the default proof works); for detached adoption, where the tfvars files were going to be the permanent wiring, supply the affected inputs by hand — or let a control plane thread them at runtime, which is not subject to this at all.

## Data sources are re-read in every module that holds a copy

**What:** a data source follows its consumers, so it can be duplicated into several roots; each re-reads it at plan time. A read that is expensive, quota-limited, or time-varying multiplies accordingly, and a time-varying result can differ between copies.

**Handle it:** for volatile or expensive reads, arrange a single consumer module (so the data source has one home) and pass the *derived resource values* across the boundary instead of having every module re-read.
