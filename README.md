# demonolith

A Go CLI that refactors a monolithic Terraform/OpenTofu root into independent
per-module roots.

**v1 scope (one-shot splitter):** emits carved per-module roots (detached — no
Snap CD control-plane wiring yet), carves state into per-module local files
against local copies (never pushing to a real backend), and can prove the split
is operationally inert via a graph-threaded zero-diff plan bundle.

## Decorators

Placement is driven by strict, namespaced comments directly above a
`resource` / `module` / `data` block:

```hcl
# @demono:move networking
resource "random_uuid" "vpc_id" {}
```

- `resource` / `module` → exactly one target (a stateful singleton).
- `data` → one *or more* targets (duplicated into each; a stateless read).
- No decorator → the catchall remainder module (`--remainder-module`, default
  `monolith`).
- Anything that looks like a decorator but doesn't parse is a hard error.

## Usage

```bash
# Emit carved roots only (code, no state, no binary needed):
demonolith split ./infra

# Also carve state into per-module local files (needs terraform/tofu):
demonolith split ./infra --state

# Carve + prove every module plans to zero create/destroy with threaded inputs:
demonolith split ./infra --state --proof
```

Key flags: `--out`, `--remainder-module`, `--engine {terraform|tofu}`,
`--exec-path`, `--state`, `--state-file`, `--proof`, `--refresh`.

## What each stage does

1. **Parse** the root into a resource-level reference graph (via AST traversal,
   catching refs inside `templatefile`/`jsonencode`/index expressions).
2. **Placement** — resolve decorators into a total module assignment; fill the
   catchall; duplicate multi-target data sources.
3. **Boundary** — per module, references crossing *in* become `variable`s,
   crossing *out* become `output`s; cross-module `depends_on` becomes a
   whole-module ordering edge.
4. **Cycle gate** — contract each module to a node; refuse an impossible split
   with a named cycle path.
5. **Emit** — carve per-module roots via `hclwrite` (formatting preserved),
   generate variables/outputs, rewrite cross-module references to `var.<input>`,
   propagate providers, strip decorator comments.
6. **State carve** — `state mv -state/-state-out` over local copies; the
   carved-down monolith state becomes the remainder module's state. Backup first,
   never push.
7. **Proof** — walk modules in topo order, thread each producer's extracted
   outputs into its consumers' inputs (the role Snap CD plays at runtime), plan
   each against a copy of its carved state, and assert zero create/destroy.

## Development

```bash
go build ./...
go test ./...   # state/proof tests skip without a terraform/tofu binary
```
