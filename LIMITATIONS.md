# Limitations

Inherent limits of the split — things demonolith cannot make true on its own, because the information isn't in the graph or the fix would require judgment. Each entry says what breaks, how it shows up, and how to handle it manually. The proofs (`migrate prove` and `migrate verify`) are the net under all of them: a split that hits one of these and isn't handled surfaces as a plan error or a non-zero diff, not as silent corruption.

## Path-relative references

**What:** `path.module` / `path.root`, relative paths in `file()` / `templatefile()`, and file-reading data sources (`local_file`) resolve against the root that evaluates them. The new module directories live in a different place, and demonolith neither copies referenced files nor rewrites paths.

**Shows up as:** `migrate prove` (or any plan of a module directory) failing with file-not-found. Because data sources follow their consumers automatically, one file-reading data source can land in several module directories — every one of them needs the file.

**Handle it:** the durable fix is restructuring the monolith before refactoring to pass file *content* through a variable (`var.environment_json` + `jsondecode`) so nothing path-relative crosses the split. Copying the referenced files into the module directories also works, but interacts with the guards: files added after `refactor run` invalidate the emit checksum, so the migrate family refuses. The sequence that works is copy the files into the module directories, then `refactor run` **again** so the checksum includes them — migrate then passes, but `refactor diff` still refuses (a fresh run does not produce the copies), so a team flow gated on diff needs the restructure, not the copy.

## Sensitive values crossing a boundary

**What:** cross-module wiring is generated `output` / `variable` pairs, and generated outputs are plain — no `sensitive = true`. A provider-marked sensitive value (a private key, a password) that crosses a boundary makes the engine refuse the producer root ("Output refers to sensitive values"). The same applies when a data source's copies read a sensitive value from another module.

**Shows up as:** the producer module erroring at plan time during `migrate prove`.

**Handle it:** prefer placement that keeps the sensitive edge inside one module (decorate producer and consumer into the same module — for a data source, that means keeping its consumers with the resource it reads). Hand-editing `sensitive = true` into a module directory works mechanically but makes `refactor diff` and the staleness checksum refuse by design; if you must, do it as a documented post-adoption edit, after `migrate` and `prove` have run.

## Backend derivation covers common types only

**What:** the monolith's `backend` block is carried into every module directory with the state location postfixed per module — for every built-in backend type (`local`, `s3`, `azurerm`, `gcs`, `consul`, `http`, `cos`, `oss`, `kubernetes`, `pg`, `remote` in workspace-name mode). Config supplied via `-backend-config` is handled through the init-time resolved config: locations derive from it, non-secret settings persist into each root.tf, and credential-shaped attributes land in gitignored per-module `demono.env` files that migrate sources automatically. Workspace-driven configurations (a `cloud` block, `remote` in workspaces.prefix mode) are refused at plan time, and nested backend config (e.g. s3 `assume_role`) does not survive the flag path.

**Shows up as:** `refactor map` refusing with the backend type or attribute named (for a flags-configured backend, the fix it names is initializing the root first, so the resolved config exists).

**Handle it:** pass `--no-backend` to write the modules without backend blocks and wire the backends by hand (`tofu init -migrate-state`, or a careful `state push` into an **empty** location — never `-force`), or move the missing attribute into HCL. Either way the monolith's own state is never written by demonolith; retiring it after every module proves clean is the human cutover step.

## Provider environment is not captured

**What:** demonolith writes exactly two ambient inputs into the module directories: backend credentials (per-module `demono.env`) and variable values (per-module `demono.root.tfvars`/`demono.graph.tfvars`). Everything else a plan may depend on — provider credentials (`AWS_PROFILE`, `ARM_CLIENT_SECRET`, `GOOGLE_APPLICATION_CREDENTIALS`), plugin mirrors, proxy settings — is inherited from the calling environment, never recorded.

**Shows up as:** migrate steps failing at init or plan with provider authentication errors, in a session where the monolith itself was never init'd or the environment differs from the one that could.

**Handle it:** treat a clean `init` + `plan` of the monolith root as the entry ticket, and run every demonolith command in that same shell session. A value passed as `-var` on the original apply is likewise not recoverable from state; re-supply it as `--var`.

## An occupied destination only skips when its state matches

**What:** `migrate run`'s empty-destination guard treats existing state as an idempotent skip when it matches this migration — same lineage, or identical content modulo the identity fields a fresh split regenerates (lineage, serial, engine version). A crashed run is therefore retried by just re-running: already-pushed modules skip, the rest push, and a lost workdir is re-split automatically at the next `migrate map`. What still refuses is a destination whose state *genuinely differs* — for example pushed by an earlier migration of a since-changed monolith.

**Shows up as:** `migrate run` refusing with "target … already holds state that does not match this migration" on a location an earlier, different migration attempt wrote to.

**Handle it:** inspect the existing state (`state pull` in the module dir) and decide which side is right. If it is a stale artifact of an abandoned attempt, empty or delete that remote state and re-run — or re-run with `--overwrite` to force-push over it (the existing state is lost; the run warns loudly). The per-module state files are reproducible and the monolith's own state is never touched.

## The prove receipt ages between prove and run

**What:** `migrate prove` judges the split against the state as it was when `migrate map` pulled it; `migrate run` pushes at a later moment. Real infrastructure drifting in between is invisible to the proof — the same plan/apply gap every plan-then-execute system has.

**Shows up as:** `migrate verify` (which plans against the real backends, refresh on) reporting diffs that prove did not.

**Handle it:** keep the plan→prove→run window short (the bare `demonolith migrate` pipeline makes it seconds), run the migration inside a change freeze for the monolith, and treat `migrate verify` as the final authority.

## Whole-block placement only

**What:** decorators, state moves, and the map all address whole blocks. A `count`/`for_each` resource's instances cannot be split across modules, and there is no per-instance re-addressing.

**Handle it:** split the block in the monolith first (separate resources, or two blocks with partitioned `for_each` sets, using `moved` blocks to keep state), apply that as an ordinary monolith change, then refactor.

## `moved`, `import`, and `check` blocks are not carried

**What:** these root-level blocks are not graph nodes and are not carried into any module directory.

**Shows up as:** silently absent from the module directories.

**Handle it:** they are usually historical by the time of a split. If one is still load-bearing (an unapplied `import`, a pending `moved`), apply it in the monolith *before* refactoring so the state reflects it, and the split then needs nothing.

## Workspace-dependent monoliths

**What:** `terraform.workspace` is a meta-reference that resolves to no node; each module directory plans in its own (default) workspace, so the value can silently change across the split.

**Handle it:** replace `terraform.workspace` with an explicit variable in the monolith before refactoring.

## Data sources referenced only from provider config

**What:** provider blocks are not graph nodes, so a data source consumed *only* inside a `provider` block is invisible to automatic data placement and falls to the remainder; its value reaches the provider config as a wired-in variable.

**Shows up as:** a provider config in one module depending on a remainder-module output — correct, but it makes the remainder a producer you may not have expected.

**Handle it:** usually acceptable. If the coupling is wrong, give the data source a same-module consumer (reference it from a resource in the module whose provider needs it) so it lands there.

## Ordering edges are only reported without a control plane

**What:** a cross-module `depends_on` becomes an ordering edge — recorded in the map, enforced by nothing in detached use. Value edges self-enforce (a consumer can't plan without its input); ordering edges don't.

**Handle it:** apply the roots in the map's order (the proofs print the topo order), wire the ordering into your pipeline, or adopt into a control plane whose dependency graph carries it natively.

## Cross-module values are strings

**What:** every generated variable is `type = string`, matching stringified value-passing: scalars arrive bare, composites as compact JSON.

**Shows up as:** a consumer that indexes into a composite (`producer.list[0]`) receiving a JSON string instead — a plan error or a diff at proof time.

**Handle it:** keep composite-shaped edges inside one module, or adapt the consumer to `jsondecode(var.<input>)` in the monolith before refactoring so the same expression survives on both sides of the split.

## Expression-valued module outputs need the proof to fill them in

**What:** the generated `demono.graph.tfvars` files resolve cross-module input values from the *applied* state, and child-module outputs are not stored in state. `migrate run` fills those from the producer values the proof computed, so after a normal map → prove → run the file is complete — but a run with `--unproven` (or with the workdir's proof sidecar gone) has nothing to fill from.

**Shows up as:** an `--unproven` `migrate run` listing the input under "Cross-module inputs not in the graph tfvars" and leaving it out of the written `demono.graph.tfvars`. The proofs are unaffected — they evaluate *planned* values in memory, expressions included.

**Handle it:** run `migrate prove` before `migrate run` (the default pipeline order) so the fill happens; otherwise supply the listed inputs by hand at plan time, or let a control plane pass them at runtime, which is not subject to this at all.

## Data sources are re-read in every module that holds a copy

**What:** a data source follows its consumers, so it can be duplicated into several roots; each re-reads it at plan time. A read that is expensive, quota-limited, or time-varying multiplies accordingly, and a time-varying result can differ between copies.

**Handle it:** for volatile or expensive reads, arrange a single consumer module (so the data source has one home) and pass the *derived resource values* across the boundary instead of having every module re-read.
