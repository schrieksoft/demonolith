# demonolith

A Go CLI that refactors a monolithic Terraform/OpenTofu root into independent
per-module roots — code, state, and control-plane wiring — in two command
families split at the code/state line, connected by a manifest:

```
demonolith refactor            # plan → run → verify        (the code carve)
  refactor plan                #   analyze → write the manifest (the review artifact)
  refactor run                 #   execute the manifest: emit roots + backends + bootstrap
  refactor verify              #   gate: committed output ≡ committed source

demonolith migrate             # plan → prove → run → verify (the state migration)
  migrate plan                 #   pull read-only, back up, carve local state copies
  migrate prove                #   threaded zero-diff proof over plan's exact artifacts
  migrate run                  #   seed each root's derived backend (guarded, never forced)
  migrate verify               #   judge the result against the real backends
```

The bare family commands run their steps in order, pausing for approval
before the run step — refactor after showing the plan, migrate after the
proof — with `-y`/`--yes` approving automatically; each subcommand stands
alone. `refactor plan/run/verify` are pure and offline. The migrate family
needs an engine (`--engine terraform` or `--engine tofu` — no default, the
choice is explicit). Exit codes are uniform: `0` success, `1` operational
error, `2` negative verdict (a difference, a failed proof, a stale manifest).

## Decorators

Placement is driven by strict, namespaced comments directly above a
`resource` / `module` block:

```hcl
# @demono:move networking
resource "random_uuid" "vpc_id" {}
```

- `resource` / `module` → exactly one target (a stateful singleton).
- No decorator → the catchall remainder module (`--remainder-module`, default
  `monolith`).
- `data` blocks are **never decorated** (a hard error): like locals and
  variables, a data source follows its consumers automatically — copied into
  every module that references it and re-read there.
- Anything that looks like a decorator but doesn't parse is a hard error.

`refactor plan --interactive` (`-i`) prompts for the run's parameters, triages
unannotated blocks in a guided loop, and writes the accepted assignments back
into the source as decorators, so the session's outcome is reviewable in git
and reproducible non-interactively.

## Usage

From inside the monolith root (every command also takes `--root-dir <dir>`):

```bash
# Carve the code. Roots land in modules/ by default (--out to change):
demonolith refactor                      # or: refactor plan && refactor run

# Migrate the state, end to end:
demonolith migrate --engine tofu         # plan → prove → run → verify
```

The manifest is the contract: one per root, `demonolith-refactor.yaml`. A
*planned* manifest carries the full plan (placement, state moves, wiring
edges, derived backend locations) for review; `refactor run` executes it
verbatim — refusing if the source changed since — and finalizes the
`emit_checksum` that ties every later step to this exact generation. The
audit trail is four fixed-name sidecars, overwritten per execution —
`demonolith-migrate-plan.yaml`, `demonolith-migrate-run.yaml`,
`demonolith-prove.yaml`, `demonolith-verify.yaml` — each carrying its
`created` datetime and the generation's `manifest_checksum` inside the
document, so an external system can tell what ran, when, and for which plan;
history lives in version control. Undo on the code side is git: everything
demonolith writes is ordinary committed files.

**Backends** are derived, not hand-written: the monolith's `backend` block is
carried into every carved root with its state location postfixed per module
(`prod/terraform.tfstate` → `prod/terraform-networking.tfstate`; supported
types: local, s3, azurerm, gcs, consul, http). A backend configured partly or
wholly via `-backend-config` flags works too: locations fall back to the
init-time resolved config, other non-secret settings persist into each
`backend.tf`, and credentials are materialized by **`migrate run`/`verify`** (refactor
deals with code only) as **gitignored per-module `.env` files** (0600) in the
engines' official variables (`TF_HTTP_USERNAME`, `AWS_ACCESS_KEY_ID`,
`ARM_ACCESS_KEY`, …) — never into HCL — and sourced automatically around each
module's init. Every emitted root (bootstrap included) gets its own
`.gitignore` covering the local artifacts these steps leave behind
(`.terraform/`, `*.tfstate`, `.env`, `generated.auto.tfvars`, …), so a carved
root is safe to commit or ship to its own repo as-is. `migrate run` seeds each
derived location from the carved state — the target must be empty, a push is
never forced, and the monolith's own state is never written. Retiring the
monolith (its pipelines and its old state) is deliberately a human cutover
step. A monolith with no backend gets local seeding: each root receives its
`terraform.tfstate` in place.

**The Snap CD bootstrap** (`<out>/snapcd`, `--no-bootstrap` to skip) is
generated from the manifest alone: one `snapcd_module` per carved root, every
cross edge as `snapcd_module_input_from_output`, ordering edges as
`snapcd_depends_on_module`, external inputs as literals bound to the
bootstrap's variables. Applying it against a Snap CD server is the adoption
step.

**Variable values** transfer in the engine's own precedence — `TF_VAR_*` env,
then the root's `terraform.tfvars`/`*.auto.tfvars`, then `--var-file` files,
then `--var` flags. `migrate prove`/`run`/`verify` materialize each module's
resolved values into its `generated.auto.tfvars`: the root variable values
the module declares in one section (declared defaults already travel in the
carved code, so only resolved values get an entry), and with
`--create-tfvars` the cross-module input values from the applied state in
another — the standalone wiring for detached use. Cross-module inputs are
never user-supplied: the proof threads them from producer outputs itself, and
`.env` stays backend-credentials-only. A value that only ever existed as a
`-var` flag on the original apply is not recoverable — state does not record
inputs — so pass it again as `--var`. `migrate run` requires a passing prove
verdict no older than the plan receipt (`--unproven` is the explicit
override).

**Prerequisite — one shell session.** The monolith root must `init` and
`plan` cleanly before you start, and every demonolith command should run in
that same shell session. Provider and init-time environment (`AWS_PROFILE`,
`ARM_*`, plugin mirrors, …) is not captured: demonolith materializes only
backend credentials (`.env`) and variable values (`generated.auto.tfvars`);
everything else the migrate steps inherit from whatever the session has set.

Key flags: `--out`, `--remainder-module`, `--monorepo` (link in-repo child
modules instead of copying), `--no-bootstrap`, `--no-backend` (refactor plan);
`--quiet`/`--silent`, `--validate` (refactor verify: engine-validate the
committed roots, still credential-free); `--engine`, `--exec-path`,
`--state-file` (migrate plan); `--refresh`, `--create-tfvars`, `--var-file`,
`--var` (migrate prove);
`--backend-config`, `--unproven` (migrate run); `--output {text|json}` and
`--interactive`/`-i` where applicable.

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
5. **Emit** (`refactor run`) — carve per-module roots via `hclwrite`
   (formatting preserved), generate variables/outputs, rewrite cross-module
   references to `var.<input>`, propagate providers, derive backends, strip
   decorator comments, generate the bootstrap; record it all in the manifest.
6. **State carve** (`migrate plan`) — `state mv -state/-state-out` over local
   copies. Backup first; a receipt records what happened and where.
7. **Proof** (`migrate prove`) — walk modules in topo order, thread each
   producer's extracted outputs into its consumers' inputs (the role Snap CD
   plays at runtime), plan each against its carved state copy, and assert zero
   create/destroy (in-place updates are reported but don't fail).
8. **Migration** (`migrate run`) — seed each root's derived backend from the
   carved state, guarded; then **judgment** (`migrate verify`) — the threaded
   proof re-run against the real backends.

See `DESIGN.md` for the pipeline concepts, the CLI and manifest design, and
usage patterns; `LIMITATIONS.md` lists the carve's known limits and how to
handle them manually.

## The manual approach

Everything demonolith does can be done by hand — it is exactly the procedure
you would follow without it, minus the guardrails. For reference, splitting a
monolith manually — after the session setup, steps 2–5 are the refactor
family's work (code only) and steps 6–10 the migrate family's (state and
ambient inputs):

**1. Establish the working session** (the prerequisite demonolith documents,
plus what `migrate plan` reads from the init-time resolved config). In one
shell, `tofu init` the monolith — with every `-backend-config` flag it needs —
and `tofu plan` it to a clean, zero-surprise run. Write down everything that
made that work: the backend flags and their values, every `TF_VAR_*` set in
the environment, every `-var`/`-var-file` argument, and the provider
credentials the session carries (`AWS_PROFILE`, `ARM_*`, …). Every later step
runs in this same shell; the notes are the only record of the ambient inputs,
and a `-var` that appears nowhere else lives only in those notes — state does
not store inputs.

**2. Find the seams** (what `refactor plan`'s analysis does). Read every
`*.tf` file and decide, per resource, which future module owns it — a useful
test: two resources that would never be changed in the same PR by the same
person probably belong in different states. Then find every reference that
will cross a boundary: grep for each resource address
among its consumers, remembering the ones hiding inside `templatefile(...)`,
`jsonencode(...)`, index expressions, `depends_on` lists, provider configs,
and locals.

**3. Carve the code** (what `refactor run` does). Manually move the blocks
into new root directories, along with everything they require: the
`required_providers` block, the `provider` configs they use, every `variable`
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
the files and are handled in step 7. Give every root a `.gitignore` covering
`.terraform/`, `*.tfstate*`, `.env`, and the tfvars file from step 7, so
nothing local can be committed by accident.

**5. Gate the carve** (what `refactor verify` does): redo steps 3–4 from
scratch and compare the results file by file — in practice, a careful code
review of every carved file against the monolith source, repeated after every
source change.

**6. Carve the state** (what `migrate plan` does). On local copies, never
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

**7. Copy the credentials and input values over** (the `.env` and
`generated.auto.tfvars` that the migrate steps materialize). Per root, put the
secret-shaped backend settings from your step-1 notes into a `.env` (chmod
600) in the engine's official variables (`TF_HTTP_USERNAME`,
`AWS_ACCESS_KEY_ID`, `ARM_ACCESS_KEY`, …), to be sourced before each init.
Then list the variables the root's moved blocks declare and reproduce the
value the monolith actually resolved for each by walking the engine's
precedence from the bottom: `TF_VAR_*` env, overridden by `terraform.tfvars`,
then `*.auto.tfvars` in lexical order, then `-var-file` files in argument
order, then `-var` flags — all read off the step-1 notes. Write the winners
into a per-root `*.auto.tfvars` so every later plan loads them with no flags.
Declared defaults moved with the code in step 3; only values the monolith
resolved from outside need an entry.

**8. Prove the split** (what `migrate prove` does). Order the roots so every
producer comes before its consumers, then per root, in that order:

```bash
cd networking
mv backend.tf backend.tf.hold  # offline proof: the engine refuses to plan a
                               # declared-but-uninitialized backend, so hold
                               # it aside and let the local state copy rule
cp ../networking.tfstate terraform.tfstate
tofu init -backend=false
tofu plan -out demo.tfplan     # root values load from the step-7 tfvars;
                               # pass -var only for cross-root inputs, with
                               # values read out of the producers' plans
tofu show -json demo.tfplan | jq '[.resource_changes[].change.actions[]] | unique'
# the only acceptable answer contains no "create" and no "delete"
mv backend.tf.hold backend.tf; rm terraform.tfstate
```

After each plan, extract the root's planned output values (`tofu show -json
demo.tfplan | jq '.planned_values.outputs'`) and hand them to its consumers
as the `-var` values — you are playing the control plane's role by hand.

**9. Execute** (what `migrate run` does). Per root, source its `.env`, `tofu
init` against its new backend, confirm the target is **empty**, and `tofu
state push` its carved state — never forced. The monolith's own state is never
touched.

**10. Adopt** (what `migrate verify` does). Per root, prove the migration
against reality: a fresh `tofu init` must find the pushed state in the new
backend, and a refresh-on plan must show zero create/destroy — a plan that
wants to create everything means the init did not find the state you pushed.
From then on, every producer output must reach its consumers somehow:
re-extract values by hand whenever an upstream changes, or let a control plane
ingest the manifest's wiring and do it at runtime. Retire the monolith — its
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
