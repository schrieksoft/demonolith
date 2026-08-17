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
# Carve the code. Roots land in .demono/modules by default (scratch); the
# canonical home for committed roots is --out modules:
demonolith refactor --out modules        # or: refactor plan && refactor run

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
module's init. `migrate run` seeds each
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

`migrate prove` resolves external inputs the way the monolith did — the
root's own `terraform.tfvars`/`*.auto.tfvars`, then `--var-file` files, then
`TF_VAR_*` env, then `--var` flags — threading every value in memory by
default; `--create-tfvars` materializes `generated.auto.tfvars` per consumer
root as the standalone wiring for detached use. Cross-module inputs are never
user-supplied: the proof threads them from producer outputs itself.
`migrate run` requires a passing prove verdict no older than the plan receipt
(`--unproven` is the explicit override).

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
monolith manually:

**1. Find the seams** (what `refactor plan`'s analysis does). Read every
`*.tf` file and decide, per resource, which future module owns it — then find
every reference that will cross a boundary: grep for each resource address
among its consumers, remembering the ones hiding inside `templatefile(...)`,
`jsonencode(...)`, index expressions, `depends_on` lists, provider configs,
and locals.

**2. Carve the code** (what `refactor run` does). Manually move the blocks
into new root directories, along with everything they require: the
`required_providers` block, the `provider` configs they use, every `variable`
and `local` the moved blocks reference (following local-to-local chains), and
the source directories of local child modules. Then wire up outputs with
inputs: for every reference that now crosses roots, declare an `output` on the
producer, a `variable` on the consumer, rewrite the reference to `var.<name>`,
and delete `depends_on` entries pointing at blocks that left. Copy each data
source into every root that reads it. Write each root a backend config with
its own state location. Finally, convince yourself no dependency cycle exists
between the new roots — a cycle means no apply order exists, and you find out
late.

**3. Gate the carve** (what `refactor verify` does): redo step 2 from scratch
and compare the results file by file — in practice, a careful code review of
every carved file against the monolith source, repeated after every source
change.

**4. Carve the state** (what `migrate plan` does). On local copies, never
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

**5. Prove the split** (what `migrate prove` does). Order the roots so every
producer comes before its consumers, then per root, in that order:

```bash
cd networking
cp ../networking.tfstate terraform.tfstate
tofu init
tofu plan -out demo.tfplan     # pass -var for every cross-root input, with
                               # values read out of the producers' plans
tofu show -json demo.tfplan | jq '[.resource_changes[].change.actions[]] | unique'
# the only acceptable answer contains no "create" and no "delete"
```

After each plan, extract the root's planned output values (`tofu show -json`
again) and hand them to its consumers as the `-var` values — you are playing
the control plane's role by hand.

**6. Execute and adopt** (what `migrate run` and `migrate verify` do). Per
root: `tofu init` against its new backend, confirm the target is empty, and
`tofu state push` its carved state — never forced; then plan every root
against its real backend and demand zero create/destroy. From then on, every
producer output must reach its consumers somehow: re-extract values by hand
whenever an upstream changes, or let a control plane ingest the manifest's
wiring and do it at runtime. Retire the monolith — its pipelines and its old
state — only after every root proves clean.

Every step is mechanical; none of it is hard — but step 2 is a hundred small
edits where any missed reference is a broken root, step 4 is one mistyped
address away from a bad day, and step 5 is the only proof you did it right,
run once, by hand, on the day you migrated. That gap — mechanical but
unguarded — is the reason demonolith exists.

## Development

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```
