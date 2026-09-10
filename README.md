# demonolith


## TL;DR

Restructure Terraform/OpenTofu projects without changing any infrastructure: split a monolithic root into independent per-module projects, or transfer resources between existing ones - code, state, and the wiring between them - with the result proven to change nothing.

Standalone companion to the [Snap CD](https://github.com/schrieksoft/snapcd) composition and orchestration system.


## Get Going Quickly

```bash
go install github.com/schrieksoft/demonolith@latest
```

(or grab a prebuilt binary from the [releases page](https://github.com/schrieksoft/demonolith/releases)).

demonolith does two kinds of restructuring - **split** a monolithic root into new per-module roots, or **transfer** blocks between roots that already exist - and each has a guided tour of runnable scripts, no cloud account needed:

- [sample-deployment-demonolith](https://github.com/snapcd-samples/sample-deployment-demonolith) - the split: a deliberately knotted monolith (remote state, shared data sources, cross-cutting references), split, migrated, and verified end to end.
- [sample-deployment-demonolith-transfer](https://github.com/snapcd-samples/sample-deployment-demonolith-transfer) - the transfer: the landscape that split left behind, with resources moved between its living roots.

> 📺 **Watch it run:** [Monolith begone! How to split a Terraform state with demonolith](https://youtu.be/ewV55RndPf0) - the tool in action. For the full line-by-line walkthrough against the sample above, see [Splitting a Terraform Monolith, Line by Line](https://youtu.be/AbpQfjxH1BY).

[![Monolith begone! How to split a Terraform state with demonolith](https://img.youtube.com/vi/ewV55RndPf0/maxresdefault.jpg)](https://youtu.be/ewV55RndPf0)

## Two modes

- **Split** - the bare `refactor` and `migrate` commands: carve one monolithic root into new per-module roots that demonolith generates and owns outright.
- **Transfer** - the `transfer refactor` an `transfer migrate` commands (experimental): move selected blocks between roots that already exist, are hand-maintained, and keep living after the move.

## Mode 1: split

```
demonolith refactor            # map → run → validate → diff   (the code split)
  refactor map                 #   analyze → write the map (the review artifact)
  refactor run                 #   execute the map: write the new module directories (they must not exist yet)
  refactor validate            #   gate: ask the engine whether it accepts what was written
  refactor diff                #   gate: the map and module directories on disk still match the source

demonolith migrate             # map → prove → run → verify (the state migration)
  migrate map                  #   pull read-only, back up, split into local state copies
  migrate prove                #   gate: prove the split changes nothing (plans over the local copies)
  migrate run                  #   push each module's state to its new backend (guarded, never forced)
  migrate verify               #   gate: judge the result against the real migrated backends
```

Mark each stateful block with the module it belongs to; anything unmarked lands in the catchall remainder (`--remainder-module`, default `legacy`). `data` blocks are never decorated - a data source follows its consumers into every module that reads it:

```hcl
# @demono:move networking
resource "random_uuid" "vpc_id" {}
```

Then, from inside the monolith root (every command also takes `--root-dir <dir>`):

```bash
demonolith refactor                      # the code split: new roots land in roots/ (--out to change)
demonolith migrate --engine tofu         # the state migration, end to end
```

`refactor map` writes the map (`demonolith-refactor-map.yaml`) - placement, state moves, wiring, and each module's derived state location, reviewable like any other diff - and `refactor run` executes it verbatim, refusing if the source changed since. 

Backends are derived from the monolith's own backend block (state locations postfixed per module); backend credentials land in gitignored per-module `demono.env` files, never in HCL, and each module's resolved variable values in per-module tfvars files. `refactor map -i` triages unmarked blocks interactively and writes the answers back as decorators; `migrate -i` walks every input the migration consumes before anything runs.

**The Snap CD bootstrap** (`<out>/snapcd`, `--no-bootstrap` to skip) is an apply-ready root wiring every module into Snap CD: one `snapcd_module` per module, the cross-module references as `snapcd_module_input_from_output`, every input bound to a variable. Applying it against a Snap CD server is the adoption step.

## Mode 2: transfer (experimental)

```
demonolith transfer            # move blocks between pre-existing roots (experimental)
  transfer refactor            # map → run → diff   (the code move; run at the source root)
  transfer migrate             # map → prove → run → verify   (the state move; one root at a time, or --all)
```

Here the decorator names its destination as a directory relative to the source root - an existing root, in the same repo or a neighboring checkout:

```hcl
# @demono:move ../network
resource "random_uuid" "vpc_id" {}
```

`transfer refactor` moves the code: blocks are appended into the destination's own `main.tf`/`variables.tf`/`outputs.tf` (created only when missing; a name the destination already declares is refused), references between the roots are updated on both sides - the source consumes the moved value as a variable, the destination exposes it as an output.

A Snap CD root wiring the involved roots (`--snapcd-root`, default: a sibling named `snapcd`) gets the matching `snapcd_module_input_from_output` / `snapcd_depends_on_module`. A copy of the transfer map is written into every touched root, so `transfer refactor diff` run in any one of them checks that root against its own copy - in a multi-repo setup, each repo's own merge gate.

`transfer migrate` moves the state, one root at a time: each step needs only that root's checkout and credentials, so roots on different machines each run their own side. What has to pass between roots travels as files in each root's `.demono-transfer/` - the moved resources' state, output values, and per-root receipts - and the source's state is only stripped once every destination has confirmed its write. When all roots share one filesystem, `--all` from the source root runs every root's part in order.

Between the code move landing and the state move completing, source and destinations show pending changes: pause their pipelines until `transfer migrate verify` comes back clean, and keep that gap short. Not supported: a data source consumed on both sides, `count`/`for_each` instance moves, and transfers between different backend types.

The runnable example is [sample-deployment-demonolith-transfer](https://github.com/snapcd-samples/sample-deployment-demonolith-transfer) - the landscape a split left behind, a transfer through it, and the CI lanes a team would use.

## In a team

The near-truth is "a developer runs the code half, CI runs the state half" - but the halves split differently across people and pipelines, because one is reversible and one is not:

- **A developer runs `refactor` locally**, then `refactor validate`, and opens a PR. The decorators, the map, and the written directories are ordinary files, so the PR *is* the review - and rejecting it undoes everything.
- **CI runs the diff gate on every PR and push** - the committed output still matches the source. Offline, no credentials, seconds.
- **CI can rehearse the migration on the PR**: `migrate map` pulls state read-only, `migrate prove` plans every root to zero changes. Nothing is pushed. This lane needs the working-session inputs as CI secrets.
- **The state half is not a PR job.** Merge first, then run it once - a manually triggered pipeline or a person at a terminal - inside a change freeze. A crashed run is retried by just re-running.
- **Adoption and retirement stay human**: apply the bootstrap, watch every module verify clean, then retire what the move emptied.

Both samples carry this as runnable GitHub Actions workflows - the diff gate and a read-only rehearsal on every PR, the migration behind a manual `workflow_dispatch`.

## More

`DESIGN.md` has the concepts and internals; `LIMITATIONS.md` the known limits and how to handle them manually.

## Development

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```

Pushes are exercised end to end on local-type backends in the test suite; the cloud backend types are unit-tested at the derivation level, with the runnable sample exercising the s3 derivation against a real S3-compatible store.
