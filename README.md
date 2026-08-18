# demonolith

A Go CLI that splits a monolithic Terraform/OpenTofu root into independent
per-module projects — code, state, and control-plane wiring — in two command
families split at the code/state line, connected by a map:

```
demonolith refactor            # map → run → verify        (the code split)
  refactor map                 #   analyze → write the map (the review artifact)
  refactor run                 #   execute the map: write the new module directories
  refactor verify              #   gate: committed output ≡ committed source

demonolith migrate             # map → prove → run → verify (the state migration)
  migrate map                  #   pull read-only, back up, split into local state copies
  migrate prove                #   prove the split changes nothing (plans over the local copies)
  migrate run                  #   push each module's state to its new backend (guarded, never forced)
  migrate verify               #   judge the result against the real backends
```

The bare family commands run their steps in order, pausing for approval
before the run step — refactor after showing the map, migrate after the
proof — with `-y`/`--yes` approving automatically; each subcommand stands
alone. `refactor map/run/verify` are pure and offline. The migrate family
needs an engine (`--engine terraform` or `--engine tofu` — no default, the
choice is explicit). Exit codes are uniform: `0` success, `1` operational
error, `2` negative verdict (a difference, a failed proof, a stale map).

## Decorators

Placement is driven by strict, namespaced comments directly above a
`resource` / `module` block:

```hcl
# @demono:move networking
resource "random_uuid" "vpc_id" {}
```

- `resource` / `module` → exactly one target (a stateful singleton).
- No decorator → the catchall remainder module (`--remainder-module`, default
  `legacy`).
- `data` blocks are **never decorated** (a hard error): like locals and
  variables, a data source follows its consumers automatically — copied into
  every module that references it and re-read there.
- Anything that looks like a decorator but doesn't parse is a hard error.

`refactor map --interactive` (`-i`) prompts for the run's parameters, triages
unannotated blocks in a guided loop, and writes the accepted assignments back
into the source as decorators, so the session's outcome is reviewable in git
and reproducible non-interactively.

## Usage

From inside the monolith root (every command also takes `--root-dir <dir>`):

```bash
# Split the code. The new module directories land in modules/ by default (--out to change):
demonolith refactor                      # or: refactor map && refactor run

# Migrate the state, end to end:
demonolith migrate --engine tofu         # map → prove → run → verify
```

Every command writes a receipt, and the audit trail is five fixed-name
receipt files, overwritten per execution. The map receipt
(`demonolith-refactor-map.yaml`, or just "the map") is the contract: it
carries the full plan (placement, state moves, wiring edges, derived backend
locations) for review; `refactor run` executes it verbatim — refusing if the
source changed since — and finalizes the `emit_checksum` that ties every
later step to this exact generation. The migrate steps write theirs —
`demonolith-migrate-map.yaml`, `demonolith-migrate-run.yaml`,
`demonolith-migrate-prove.yaml`, `demonolith-migrate-verify.yaml` — each
carrying its `created` datetime and the generation's `map_checksum`, so an
external system can tell what ran, when, and for which map; history lives in
version control. Undo on the code side is git: everything demonolith writes
is ordinary committed files.

**Backends** are derived, not hand-written: the monolith's `backend` block is
carried into every new module directory with its state location postfixed per module
(`prod/terraform.tfstate` → `prod/terraform-networking.tfstate`; every
built-in backend type is supported — local, s3, azurerm, gcs, consul, http,
cos, oss, kubernetes, pg, and remote in workspace-name mode. A `cloud` block
and remote's workspace-prefix mode are workspace-driven rather than
location-driven and refuse with `--no-backend` as the way out). A backend
configured partly or
wholly via `-backend-config` flags works too: locations fall back to the
init-time resolved config, other non-secret settings persist into each
root's `root.tf`, and credentials are written by **`migrate run`/`verify`** (refactor
deals with code only) as **gitignored per-module `demono.env` files** (0600) in the
engines' official variables (`TF_HTTP_USERNAME`, `AWS_ACCESS_KEY_ID`,
`ARM_ACCESS_KEY`, …) — never into HCL — and sourced automatically around each
module's init. Every module directory (bootstrap included) gets its own
`.gitignore` covering the local artifacts these steps leave behind
(`.terraform/`, `*.tfstate`, `demono.env`, `demono.root.tfvars`,
`demono.graph.tfvars`, …), so a module directory is safe to commit or ship to its own repo as-is. `migrate run` pushes each
module's state to its derived location — the destination must be empty or already
hold this module's state (an idempotent skip), a push is never forced, and
the monolith's own state is never written. Retiring the monolith (its
pipelines and its old state) is deliberately a human cutover step. A monolith
with no backend stays local: each module directory receives its
`terraform.tfstate` in place.

**Crashes and retries.** Every migrate step is safe to just re-run. Prove,
run, and verify print one line per module as they work, and a failed run
leaves a partial (non-complete) run receipt recording how far it got, so the
crash point is never a mystery. On the retry, `migrate map` skips while its
split state copies still exist and redoes the split if they were lost, and
`migrate run` skips every destination that already holds this module's state —
matched by lineage or, after a fresh split, by identical content — and pushes
the rest. The one thing that stops a retry is a destination holding state that
genuinely does not match this migration (typically an earlier migration of a
since-changed monolith): inspect it with `state pull` in the module dir and
empty that remote state before re-running — or pass `--overwrite` to
force-push over it, which loses the existing state and is warned about
loudly. Nothing else is ever forced.

**The Snap CD bootstrap** (`<out>/snapcd`, `--no-bootstrap` to skip) is
generated from the map alone: one `snapcd_module` per module directory, every
cross edge as `snapcd_module_input_from_output`, ordering edges as
`snapcd_depends_on_module`, external inputs as literals bound to the
bootstrap's variables. A `--monorepo` split additionally sets
`default_trigger_path_filter_enabled = true` on the generated namespace, so
each module only redeploys when a commit touches its own directory. Applying
it against a Snap CD server is the adoption step.

**Variable values** transfer in the engine's own precedence — `TF_VAR_*` env,
then the root's `terraform.tfvars`/`*.auto.tfvars`, then `--var-file` files,
then `--var` flags. `migrate prove` writes each module's resolved root
variable values into `demono.root.tfvars` (declared defaults already travel
with the code, so only resolved values get an entry), and `migrate run`
writes the cross-module input values from the applied state into
`demono.graph.tfvars` — together, everything a module needs to plan on its own.
The files are deliberately not `.auto.tfvars`: the proofs load them
explicitly with -var-file, and so does anyone planning a module on its own. A
cross value state cannot resolve (an expression-valued child-module output)
is filled from the producer values the proof computed, so the
graph file is normally complete; only a run with `--unproven` and no prior
proof leaves gaps, which run reports. `--no-tfvars` (for tests) writes
nothing and passes the values in memory instead. Cross-module inputs are
never user-supplied: the proof feeds them from producer outputs itself,
and `demono.env` stays backend-credentials-only. A value that only ever existed as a
`-var` flag on the original apply is not recoverable — state does not record
inputs — so pass it again as `--var`. `migrate run` requires a passing prove
receipt no older than the map receipt (`--unproven` is the explicit
override).

**Prerequisite — one shell session.** The monolith root must `init` and
`plan` cleanly before you start, and every demonolith command should run in
that same shell session. Provider and init-time environment (`AWS_PROFILE`,
`ARM_*`, plugin mirrors, …) is not captured: demonolith writes only
backend credentials (`demono.env`) and variable values (`demono.root.tfvars`,
`demono.graph.tfvars`);
everything else the migrate steps inherit from whatever the session has set.

**Interactive migration.** `demonolith migrate -i` walks every input the
migration consumes before anything runs: engine and state source; each
external variable with the value and the source the engine's precedence
resolved it from (answer `name=value` or `@file` to supply more); the derived
backend with the credential attributes headed for `demono.env` and a prompt
for extra `-backend-config` values; and an advisory listing of the ambient
provider environment this shell carries for each declared provider — ending
with the equivalent non-interactive command, since every answer resolves to
a flag. Flags passed alongside `-i` pre-fill the answers.

Key flags: `--out`, `--remainder-module`, `--monorepo` (link in-repo child
modules instead of copying), `--no-bootstrap`, `--no-backend` (refactor map);
`--quiet`/`--silent`, `--validate` (refactor verify: run the engine's validate on each
module directory, still credential-free); `--engine`, `--exec-path`,
`--state-file` (migrate map); `--refresh`, `--no-tfvars`, `--var-file`,
`--var` (migrate prove);
`--backend-config`, `--unproven`, `--overwrite` (migrate run); `--no-color`
(any command); `--output
{text|json}` and `--interactive`/`-i` where applicable.

## What each stage does

1. **Parse** the root into a resource-level reference graph (via AST traversal,
   catching refs inside `templatefile`/`jsonencode`/index expressions).
2. **Placement** — resolve decorators into a total module assignment; fill the
   catchall; copy each data source into every module that references it.
3. **Boundary** — per module, references crossing *in* become `variable`s,
   crossing *out* become `output`s; cross-module `depends_on` becomes a
   whole-module ordering edge.
4. **Cycle gate** — contract each module to a node; refuse an impossible split
   with a named cycle path.
5. **Write** (`refactor run`) — write the per-module directories via `hclwrite`
   (formatting preserved), generate variables/outputs, rewrite cross-module
   references to `var.<input>`, propagate providers, derive backends, strip
   decorator comments, generate the bootstrap; record it all in the map.
6. **State split** (`migrate map`) — `state mv -state/-state-out` over local
   copies. Backup first; a receipt records what happened and where.
7. **Proof** (`migrate prove`) — walk modules in dependency order, feed each
   producer's extracted outputs into its consumers' inputs (the role Snap CD
   plays at runtime), plan each against its local state copy, and assert zero
   changes — create, destroy, and in-place update all fail the proof.
8. **Migration** (`migrate run`) — push each module's state to its derived
   backend, guarded; then **judgment** (`migrate verify`) — the same
   proof re-run against the real backends.

See `DESIGN.md` for the pipeline concepts, the CLI and map design, and
usage patterns; `LIMITATIONS.md` lists the split's known limits and how to
handle them manually.

## In a team

The near-truth is "a developer runs `refactor`, CI runs `migrate`" — but the two halves split differently across people and pipelines, because one is reversible and one is not:

- **A developer runs `refactor` locally** and opens a PR. Decorators, the map, and the new module directories are ordinary files, so the PR *is* the review: teammates read the map (placement, state moves, wiring, state locations) like any other diff, and rejecting the PR undoes everything.
- **CI runs `refactor verify` on every PR and push** — the standing gate that the committed module directories still match the source. Offline, no engine, no credentials, seconds. This is the piece that keeps the split honest while the PR is iterated on, and forever after.
- **CI can rehearse the migration on the PR**: `migrate map` pulls the monolith's state read-only and splits local copies; `migrate prove` plans every module to zero changes. Nothing is pushed, so a rejected PR leaves the world untouched. This lane needs the working-session inputs as CI secrets — backend credentials, `TF_VAR_*` values, and any `-var`-only value passed as `--var`.
- **`migrate run` is not a PR job.** Pushing state from an unmerged branch means the world has already changed when the PR is rejected. Merge first, then run `demonolith migrate --engine …` once — a manually triggered pipeline or a person at a terminal — inside a change freeze on the monolith, keeping the prove-to-run window short. A crashed run is retried by just re-running, and the receipts record what happened for anyone who looks later.
- **Adoption and retirement stay human**: apply the bootstrap, watch every module verify clean, then retire the monolith's pipelines and old state.

[`sample-deployment-demonolith`](https://github.com/snapcd-samples/sample-deployment-demonolith) carries this as a runnable GitHub Actions workflow — verify on every PR, a read-only rehearsal on every PR, and the migration behind a manual `workflow_dispatch`.

## Running a module by hand

After the migration, each module directory is a plain Terraform root that plans and
applies without demonolith or a control plane. Everything demonolith
wrote for it lives inside the module's own directory: `demono.env`
(backend credentials, shell-sourceable), `demono.root.tfvars` (its root
variable values), and `demono.graph.tfvars` (its cross-module input values).
Only provider environment (`AWS_PROFILE`, plugin mirrors, …) stays outside
by design — ambient, per the one-shell-session prerequisite:

```bash
cd modules/app
source ../../.env    # ambient provider credentials from the source project —
                     # demonolith cannot reliably detect these, so source them
                     # yourself
source demono.env    # backend credentials, exported into this shell
tofu init
tofu plan \
  -var-file=demono.root.tfvars \
  -var-file=demono.graph.tfvars \
  -out app.tfplan
tofu apply app.tfplan
```

The tfvars files are deliberately not `.auto.tfvars`, so loading them is
always explicit. If run executed with `--unproven` and listed inputs it could
not fill, pass each as a `-var` read from the producer's applied output
(`-var "x=$(tofu -chdir=../cluster output -raw x)"` — producers must be
applied first, in dependency order; those first applies plan zero changes
and exist to record output values). From here you are playing the control
plane's role by hand: the written values freeze this moment, so
refresh them whenever an upstream changes — or adopt the bootstrap and let
Snap CD do it at runtime.

**With `--no-tfvars`** none of these files are written, and the same run needs every
value passed explicitly: your own `-var`/`-var-file` for the root variable
values, a `-var` per cross-module input (all of them, not only the
offline-unresolvable ones), and the backend credentials exported by hand.
The migrate steps themselves are unaffected — they pass everything in
memory — so this variant is for test pipelines that must leave nothing on
disk.

## The manual approach

Everything demonolith does can be done by hand — it is exactly the procedure
you would follow without it, minus the guardrails. For reference, splitting a
monolith manually — after the session setup, steps 2–5 are the refactor
family's work (code only) and steps 6–10 the migrate family's (state and
ambient inputs):

**1. Establish the working session** (the prerequisite demonolith documents,
plus what `migrate map` reads from the init-time resolved config). In one
shell, `tofu init` the monolith — with every `-backend-config` flag it needs —
and `tofu plan` it to a clean, zero-surprise run. Write down everything that
made that work: the backend flags and their values, every `TF_VAR_*` set in
the environment, every `-var`/`-var-file` argument, and the provider
credentials the session carries (`AWS_PROFILE`, `ARM_*`, …). Every later step
runs in this same shell; the notes are the only record of the ambient inputs,
and a `-var` that appears nowhere else lives only in those notes — state does
not store inputs.

**2. Find the seams** (what `refactor map`'s analysis does). Read every
`*.tf` file and decide, per resource, which future module owns it — a useful
test: two resources that would never be changed in the same PR by the same
person probably belong in different states. Then find every reference that
will cross a boundary: grep for each resource address
among its consumers, remembering the ones hiding inside `templatefile(...)`,
`jsonencode(...)`, index expressions, `depends_on` lists, provider configs,
and locals.

**3. Split the code** (what `refactor run` does). Manually move the blocks
into new root directories, along with everything they require: the
`required_providers` block (conventionally into each root's `root.tf`), the
`provider` configs they use, every `variable`
and `local` the moved blocks reference (following local-to-local chains), and
the source directories of local child modules. Then wire up outputs with
inputs: for every reference that now crosses roots, declare an `output` on the
producer, a `variable` on the consumer, rewrite the reference to `var.<name>`,
and delete `depends_on` entries pointing at blocks that left. Copy each data
source into every root that reads it. Finally, convince yourself no dependency
cycle exists between the new roots — a cycle means no apply order exists, and
you find out late.

**4. Copy the backend over** (the derivation `refactor run` does). Write each
root a backend config with its own state location — the monolith's location
postfixed per module — carrying every non-secret setting the monolith's init
resolved, whether it came from HCL or a `-backend-config` flag in your step-1
notes; secret-shaped settings (usernames, passwords, access keys) stay out of
the files and are handled in step 7. By hand, keep the backend block in its
own `backend.tf` rather than in `root.tf` — step 8 needs to set it aside
whole, and separating it spares you surgery on the terraform block. Give
every root a `.gitignore` covering `.terraform/`, `*.tfstate*`, `.env`, and
the tfvars file from step 7, so nothing local can be committed by accident.

**5. Gate the split** (what `refactor verify` does): redo steps 3–4 from
scratch and compare the results file by file — in practice, a careful code
review of every new file against the monolith source, repeated after every
source change.

**6. Split the state** (what `migrate map` does). On local copies, never
against the backend:

```bash
tofu state pull > monolith.tfstate            # a local working copy
cp monolith.tfstate monolith.backup.tfstate   # backup before any surgery

# one mv per managed resource, into its module's state file
# (a module.<name> address moves the whole subtree; data sources carry no state):
tofu state mv -state=monolith.tfstate -state-out=networking.tfstate \
    random_uuid.vpc_id random_uuid.vpc_id
tofu state mv -state=monolith.tfstate -state-out=networking.tfstate \
    module.private_subnet module.private_subnet
# ... repeat for every resource of every module ...

# whatever is left in monolith.tfstate IS the remainder module's state
```

Keep notes of every address you moved and where — that is the receipt.

**7. Copy the credentials and input values over** (the `demono.env` and
`demono.*.tfvars` files that the migrate steps write). Per root, put the
secret-shaped backend settings from your step-1 notes into a `.env` (chmod
600) in the engine's official variables (`TF_HTTP_USERNAME`,
`AWS_ACCESS_KEY_ID`, `ARM_ACCESS_KEY`, …), to be sourced before each init.
Then list the variables the root's moved blocks declare and reproduce the
value the monolith actually resolved for each by walking the engine's
precedence from the bottom: `TF_VAR_*` env, overridden by `terraform.tfvars`,
then `*.auto.tfvars` in lexical order, then `-var-file` files in argument
order, then `-var` flags — all read off the step-1 notes. Write the winners
into a per-root `*.auto.tfvars` so every later plan loads them with no flags
(demonolith instead writes plain `.tfvars` files and passes them explicitly
with -var-file). Declared defaults moved with the code in step 3; only values
the monolith resolved from outside need an entry.

**8. Prove the split** (what `migrate prove` does). Order the roots so every
producer comes before its consumers, then per root, in that order:

```bash
cd networking
mv backend.tf backend.tf.hold  # offline proof: the engine refuses to plan a
                               # declared-but-uninitialized backend, so hold
                               # it aside and let the local state copy rule
cp ../networking.tfstate terraform.tfstate
tofu init -backend=false
tofu plan -out demono.tfplan     # root values load from the step-7 tfvars;
                               # pass -var only for cross-root inputs, with
                               # values read out of the producers' plans
tofu show -json demono.tfplan | jq '[.resource_changes[].change.actions[]] | unique'
# the only acceptable answer contains no "create", no "delete" — and no
# "update": a changed value that forces no replacement is still a wrong value
mv backend.tf.hold backend.tf; rm terraform.tfstate
```

After each plan, extract the root's planned output values (`tofu show -json
demono.tfplan | jq '.planned_values.outputs'`) and hand them to its consumers
as the `-var` values — you are playing the control plane's role by hand.

**9. Execute** (what `migrate run` does). Per root, source its `.env`, `tofu
init` against its new backend, confirm the destination is **empty**, and `tofu
state push` its state file from step 6 — never forced. The monolith's own state is never
touched.

**10. Adopt** (what `migrate verify` does). Per root, prove the migration
against reality: a fresh `tofu init` must find the pushed state in the new
backend, and a refresh-on plan must show zero changes — a plan that
wants to create everything means the init did not find the state you pushed.
From then on, every producer output must reach its consumers somehow:
re-extract values by hand whenever an upstream changes, or let a control plane
ingest the map's wiring and do it at runtime. Retire the monolith — its
pipelines and its old state — only after every root proves clean.

Every step is mechanical; none of it is hard — but step 3 is a hundred small
edits where any missed reference is a broken root, step 6 is one mistyped
address away from a bad day, step 7 is an exercise in reconstructing what the
engine did silently, and step 8 is the only proof you did it right, run once,
by hand, on the day you migrated. That gap — mechanical but unguarded — is the
reason demonolith exists.

## Development

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```
