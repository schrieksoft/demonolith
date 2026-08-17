# Limitations

Inherent limits of the carve — things demonolith cannot make true on its own, because the information isn't in the graph or the fix would require judgment. Each entry says what breaks, how it shows up, and how to handle it manually. The proof (`demonolith prove`) is the net under all of them: a split that hits one of these and isn't handled surfaces as a plan error or a non-zero diff, not as silent corruption.

## Path-relative references

**What:** `path.module` / `path.root`, relative paths in `file()` / `templatefile()`, and file-reading data sources (`local_file`) resolve against the root that evaluates them. Carved roots live in a different directory, and demonolith neither copies referenced files nor rewrites paths.

**Shows up as:** `prove` (or any plan of a carved root) failing with file-not-found. Because data sources follow their consumers automatically, one file-reading data source can land in several carved roots — every one of them needs the file.

**Handle it:** before refactoring, either restructure the monolith to pass file *content* through a variable (`var.environment_json` + `jsondecode`) so nothing path-relative crosses the carve, or after refactoring copy the referenced files into each carved root that holds a copy of the reading block. The restructure is the durable fix; the copy has to be repeated after every re-emit.

## Sensitive values crossing a boundary

**What:** cross-module wiring is generated `output` / `variable` pairs, and generated outputs are plain — no `sensitive = true`. A provider-marked sensitive value (a private key, a password) that crosses a boundary makes the engine refuse the producer root ("Output refers to sensitive values"). The same applies when a data source's copies read a sensitive value from another module.

**Shows up as:** the producer module erroring at plan time during `prove`.

**Handle it:** prefer placement that keeps the sensitive edge inside one module (decorate producer and consumer into the same module — for a data source, that means keeping its consumers with the resource it reads). Hand-editing `sensitive = true` into an emitted root works mechanically but makes `diff` and the staleness checksum refuse by design; if you must, do it as a documented post-adoption edit, after `migrate` and `prove` have run.

## No backend configuration is carried

**What:** carved roots get no `backend`/`cloud` block; `migrate` writes local state files and never pushes.

**Shows up as:** carved roots operating on local state until you say otherwise.

**Handle it:** after `prove`, add a backend config to each root, then move its carved state in with `tofu init -migrate-state` (or a careful `state push` into an **empty** backend location — never `-force`). Do the monolith's backend teardown last, after every root is proven against its new home.

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

**Handle it:** apply the roots in the manifest's order (`prove` prints the topo order), wire the ordering into your pipeline, or adopt into a control plane whose dependency graph carries it natively.

## Cross-module values are strings

**What:** every generated variable is `type = string`, matching stringified value-passing: scalars arrive bare, composites as compact JSON.

**Shows up as:** a consumer that indexes into a composite (`producer.list[0]`) receiving a JSON string instead — a plan error or a diff at `prove` time.

**Handle it:** keep composite-shaped edges inside one module, or adapt the consumer to `jsondecode(var.<input>)` in the monolith before refactoring so the same expression survives on both sides of the carve.

## Data sources are re-read in every module that holds a copy

**What:** a data source follows its consumers, so it can be duplicated into several roots; each re-reads it at plan time. A read that is expensive, quota-limited, or time-varying multiplies accordingly, and a time-varying result can differ between copies.

**Handle it:** for volatile or expensive reads, arrange a single consumer module (so the data source has one home) and pass the *derived resource values* across the boundary instead of having every module re-read.
