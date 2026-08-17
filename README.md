# demonolith

A Go CLI that refactors a monolithic Terraform/OpenTofu root into independent
per-module roots, in two halves connected by a manifest — each half with a gate
command that judges its output:

```
demonolith refactor   # code only: analyze + emit carved roots + write the manifest
demonolith diff       # gate: do the committed roots + manifest match the committed source?
demonolith migrate    # state only: execute the manifest's state moves (local copies)
demonolith prove      # gate: prove the split inert — per-module zero-diff plans, threaded inputs
```

`refactor` and `diff` are pure and offline. `migrate` needs an engine
(`--engine terraform` or `--engine tofu` — no default, the choice is explicit)
and only ever touches local state copies; nothing is pushed. `prove` plays the
control plane's role locally: it threads producer outputs into consumer inputs
and asserts every module plans to zero create/destroy. Exit codes are uniform:
`0` success, `1` operational error, `2` negative verdict (the committed output
differs from the source, a failed proof, a stale manifest).

## Decorators

Placement is driven by strict, namespaced comments directly above a
`resource` / `module` / `data` block:

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

`refactor --interactive` triages unannotated blocks in a guided loop and writes
the accepted assignments back into the source as decorators, so the session's
outcome is reviewable in git and reproducible non-interactively.

## Usage

From inside the monolith root (every command also takes `--root-dir <dir>`):

```bash
# Carve the code; writes demonolith-refactor.yaml (offline, no binary):
demonolith refactor

# Preview the state-move operation list (no engine, nothing touched):
demonolith migrate --dry-run

# Execute the moves against local state copies; writes a receipt sidecar:
demonolith migrate --engine tofu

# Prove zero-diff; writes generated.auto.tfvars per consumer and a verdict sidecar:
demonolith prove --engine tofu --keep-tfvars
```

The manifest is the contract: `migrate` and `prove` replay it without
re-analyzing. There is exactly one per root, `demonolith-refactor.yaml`,
regenerated in place by every `refactor` run; sidecars are tied to a manifest
generation by its emit checksum. CI runs `demonolith diff` as a gate (exit 2
if the committed roots or manifest don't match the committed source), and
`--output json` on `diff`/`migrate`/`prove` emits machine-readable verdicts.

`prove` resolves external inputs the way the monolith did — the root's own
`terraform.tfvars`/`*.auto.tfvars`, then `--var-file` files, then `TF_VAR_*`
env, then `--var` flags — and `--no-tfvars-file` keeps every value in memory
for credentialed CI runs. Cross-module inputs are never user-supplied: the
proof threads them from producer outputs itself.
Before a migration has run it carves an ephemeral throwaway copy of the state;
afterwards it proves the real carved states via the migrate receipt.

Key flags: `--root-dir`, `--out`, `--remainder-module` (refactor);
`--engine`, `--exec-path`, `--state-file`, `--dry-run` (migrate);
`--refresh`, `--keep-tfvars`, `--no-tfvars-file`, `--var-file`, `--var` (prove);
`--output {text|json}` and `--interactive`/`-i` where applicable.

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
5. **Emit** — carve per-module roots via `hclwrite` (formatting preserved),
   generate variables/outputs, rewrite cross-module references to `var.<input>`,
   propagate providers, strip decorator comments; record it all in the manifest.
6. **State carve** (`migrate`) — `state mv -state/-state-out` over local copies;
   the carved-down monolith state becomes the remainder module's state. Backup
   first, never push; a receipt records what happened and where.
7. **Proof** (`prove`) — walk modules in topo order, thread each producer's
   extracted outputs into its consumers' inputs (the role Snap CD plays at
   runtime), plan each against a copy of its carved state, and assert zero
   create/destroy (in-place updates are reported but don't fail).

See `DESIGN.md` for the pipeline concepts, the CLI and manifest design, and
usage patterns; `LIMITATIONS.md` lists the carve's known limits and how to
handle them manually.

## Development

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```
