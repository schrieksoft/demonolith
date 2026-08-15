# Demonolith — CLI Redesign

> **Status:** implemented, with three exceptions — phase 3's `--push` (local-only remains the only mode), phase 5's `attach` (out of scope by design), and interactive cycle resolution (`refactor --interactive` reports a cycle rather than offering the moves that would break it).

The previous CLI was one `split` command staged by cumulative flags (`--state`, `--tfvars`, `--proof`, `--refresh`, `--state-file`, …). Every richer run re-executes the earlier stages, the flag combinations imply each other (`--proof` implies `--state`), and the code/state boundary — the most important operational line in the tool — is invisible in the command shape. This document redesigns the CLI around two commands split exactly at that line, connected by a manifest file.

```
demonolith refactor [--root-dir <dir>]   # code only: analyze + emit + write a manifest
demonolith migrate  [--root-dir <dir>]   # state only: execute the manifest's state moves
```

There are no positional arguments; every command takes `--root-dir`, defaulting to the current directory — matching the engines' own convention, so the normal invocation is bare, from inside the monolith root. The flag exists for CI jobs running from the repo root and for monorepos holding several roots.

`refactor` is pure and offline — no terraform/tofu binary, no state, safe to run repeatedly while iterating on decorators. `migrate` is the operational step — it touches state (local copies in v1) and needs an engine. The manifest is the contract between them: `refactor` computes *what* must move and *how* modules wire together; `migrate` replays that plan without re-deriving it.

---

## 1. `demonolith refactor`

Runs the existing front half (parse → decorators → placement → boundary → cycle gate) and emit, exactly as today: carved per-module roots with rewritten references, generated `variables.tf`/`outputs.tf`, propagated providers/locals/variable blocks, duplicated data sources, carved module calls. A detected cycle still refuses the run with the named path; the catchall and ordering-edge reports still print.

New behaviour: on success it writes a **manifest** into the root dir:

```
<root-dir>/demonolith-refactor-{datetime}.yaml
```

`{datetime}` is compact UTC (`20260815-143000`) so lexical order is date order — this is what lets `migrate` execute multiple manifests chronologically.

Flags (all code-side concerns move here):

| Flag | Meaning |
|---|---|
| `--root-dir` | the monolith root (default `.`) |
| `--out` | output directory for carved roots (default `<root-dir>/.demono/modules`) |
| `--remainder-module` | catchall module name (default `monolith`) |
| `--check` | drift gate: re-run and compare instead of writing (see below) |

### `refactor --check` — the drift gate

For workflows where the carved roots and manifest are committed and reviewed (§5.2, §5.3), `--check` re-runs the full analysis+emit **in memory** and compares the result against the committed output dir and the newest committed manifest. Nothing is written. Exit 0 means the committed plan is exactly what the committed source produces; exit 2 (see Machine interface, §2) means drift — the roots or manifest were hand-edited, or the source changed after the last `refactor` — with a per-file diff summary. This is the CI proof that a PR's migration plan is honest, and it is cheap: pure and offline like `refactor` itself.

### The manifest

The manifest carries everything the later steps need, so `migrate` (and future `verify`) never re-analyze the source:

```yaml
version: 1
created: "2026-08-15T14:30:00Z"
tool: demonolith 1.x
source:
  root: ./infra
  remainder_module: monolith
output:
  dir: .demono/modules
modules:
  networking:
    dir: .demono/modules/networking
    blocks: [random_uuid.vpc_id, random_uuid.private_subnet_id, data.random_id.shared_token]
  data:
    dir: .demono/modules/data
    blocks: [random_uuid.database_id, random_password.admin_password, data.random_id.shared_token]
  monolith:
    dir: .demono/modules/monolith
    blocks: [random_pet.environment, time_sleep.wait_10s]
catchall: [random_pet.environment, time_sleep.wait_10s]
duplicated_data:
  data.random_id.shared_token: [networking, data]
state_moves:                      # managed resources only; data sources carry no state
  - {address: random_uuid.vpc_id, module: networking}
  - {address: random_uuid.private_subnet_id, module: networking}
  - {address: random_uuid.database_id, module: data}
  - {address: random_password.admin_password, module: data}
  # remainder resources are absent: the carved-down monolith state becomes the remainder's state
cross_edges:                      # the wiring Snap CD needs at adoption time
  - {producer_module: networking, producer: random_uuid.private_subnet_id, attribute: result,
     output: random_uuid_private_subnet_id, consumer_module: data, consumer: random_uuid.database_id,
     input: random_uuid_private_subnet_id}
ordering_edges:
  - {consumer_module: networking, producer_module: monolith,
     consumer: random_uuid.vpc_id, producer: time_sleep.wait_10s}
emit_checksum: "sha256:…"         # hash over the emitted roots, for the staleness guard
```

The manifest doubles as the **adoption recipe**: `cross_edges` are the `snapcd_module_input_from_output` wirings and `ordering_edges` the dependency edges a control plane needs, machine-readable instead of scraped from CLI output.

### The manifest schema is a public, versioned API

The manifest is not an internal file format: PR reviewers read it, CI jobs parse it, and a control plane ingests it (§5.3). That makes `version:` load-bearing. Rules: the schema is documented alongside the tool (a published JSON Schema, so CI can validate a manifest without running demonolith); changes within a major version are **additive only** — new optional keys, never renamed or removed ones; a breaking change bumps `version`, and every consumer command (`migrate`, `verify`) refuses a manifest whose major version it doesn't know rather than guessing. External input *names* may appear in the manifest; external input *values* never do (§5.2's secret rule).

---

## 2. `demonolith migrate`

Executes the state moves recorded in the manifest(s). Discovery: all `demonolith-refactor-*.yaml` in the root dir, executed in filename date order; or exactly one via `--file`.

```bash
demonolith migrate --engine tofu
demonolith migrate --root-dir ./infra --engine terraform --file demonolith-refactor-20260815-143000.yaml
```

Per manifest, in order: obtain the monolith state (from `--state-file`, else a read-only `state pull` from the configured backend), back it up, then `state mv -state/-state-out` each entry in `state_moves` into its module's local state file. The remainder module inherits the carved-down leftover file, as today. Local copies only; the backend is never written.

Flags (all state-side concerns move here):

| Flag | Meaning |
|---|---|
| `--root-dir` | the monolith root (default `.`) |
| `--engine {terraform\|tofu}` | which binary performs the moves (required; no default, so the choice is explicit) |
| `--exec-path` | explicit binary path, overrides `--engine` resolution |
| `--file` | execute exactly this manifest instead of all, in-order |
| `--state-file` | carve this state snapshot instead of pulling; only needed for an uninitialized root, an exported snapshot, or a CI-pinned state artifact — the default read-only pull covers local backends too (it just reads `terraform.tfstate`) |
| `--output {text\|json}` | report format (see Machine interface below) |

### Guards

- **Staleness**: `migrate` recomputes the checksum over the emitted roots and refuses if it differs from `emit_checksum` — the roots were edited or re-emitted after the manifest was written; re-run `refactor` first.
- **Idempotency**: a manifest whose moves have already been applied (source address absent from monolith state, present in the module state) is skipped with a notice, so re-running `migrate` after a partial failure resumes rather than erroring.
- **Backup first**, always, as today; a failed run reports the backup path to restore from.
- **Execution record**: a successful (or partial) run writes a `demonolith-migrate-{datetime}.yaml` sidecar recording which manifest was applied, the per-move outcomes, the carved state file paths, and the backup path. The manifest is the plan; the sidecar is the receipt. The idempotency check consults it first (falling back to state inspection when it's absent), and `verify` uses it to find the carved states (phase 4).

### Machine interface (migrate, verify, refactor --check)

The CI and control-plane stories (§5.2, §5.3) gate on demonolith's results programmatically, so the machine-facing contract is part of the design, not an afterthought:

- **Exit codes**, uniform across commands: `0` success (moves applied / proof zero-diff / no drift); `2` **negative verdict** — the run worked but the answer is "no" (drift detected, a module plans changes, a stale or partially-inapplicable manifest refused); `1` operational error (bad flags, missing binary, engine failure). Pipelines can therefore distinguish "the split is wrong" from "the job broke".
- **`--output json`** on `migrate` and `verify` (and `refactor --check`): the human report is replaced by one JSON document on stdout — per-manifest move results for migrate, per-module plan counts and the overall verdict for verify, the drift file list for check. The schema is published and versioned alongside the manifest schema.
- **Non-interactive strictness**: without a TTY nothing ever prompts; anything that would have asked is an exit-1 error naming the missing flag. `--interactive` and `--output json` are mutually exclusive.

---

## 3. Interactive mode

Both commands gain `--interactive` / `-i`: a guided walkthrough of every step, prompting at each decision instead of requiring the flags and decorators to be right up front. Two ground rules keep it honest:

- **Interactive mode is a front-end, not a parallel channel.** Every choice made interactively resolves to something that already exists non-interactively — a decorator written into the source, a flag value, a manifest selection. A session leaves behind a state that reproduces the same result with a plain non-interactive run.
- **TTY required, safe defaults.** Without a TTY, `--interactive` is an error, never a silent fallback. Every prompt's default is the non-destructive answer (skip, keep, abort).

### `refactor --interactive`

An iterative loop around the analysis, in five steps:

1. **Analysis summary** — the placement table, duplicated data sources, and ordering edges, as today, then pause.
2. **Catchall triage** — for each unannotated block: assign it to an existing module, name a new module, or confirm it stays in the remainder. Data sources offer multi-select (duplication). The point is that defaulting stops being silent-but-reported and becomes a per-block decision.
3. **Write-back as decorators** — accepted assignments are written into the source files as `@demono:move` comments (shown as a diff, confirmed before writing). This is the load-bearing design choice: the source stays the single source of truth, the session's outcome is reviewable in git, and the next run — interactive or not — reproduces it. Interactive placement that lived only in the manifest would rot the moment the source changed.
4. **Cycle resolution** — if the gate refuses, show the named cycle and the specific crossing references per hop, and offer the moves that would break it (relocating either endpoint of a crossing). A chosen fix is applied as a decorator edit (step 3) and analysis re-runs in the loop until the gate passes or the user aborts.
5. **Emit confirmation** — per-module file listing (and per-file diff on request, when re-emitting over an existing `--out`), then one confirm to write the roots and the manifest. Aborting here leaves the source decorators in place but writes nothing else.

### `migrate --interactive`

A linear confirm-gated sequence — migrate has no iteration, only commitment points:

1. **Manifest selection** — list every `demonolith-refactor-*.yaml` found, newest last, each annotated with its staleness-check and idempotency status (applies cleanly / partially applied, will resume / stale, refuse). Multi-select, always executed in date order; `--file` skips this prompt.
2. **Engine confirmation** — the resolved binary path and its reported version, confirmed once. In interactive mode `--engine` may be omitted; the prompt supplies it.
3. **Plan preview** — the fully resolved operation list, exactly what `--dry-run` prints: every `state mv` with source and destination files, grouped by module, plus the backup path that will be written first.
4. **Execution** — one confirm for the whole run by default, or `step` mode to confirm module-by-module (each module's moves are one group; per-resource stepping is too fine to be anything but noise). Progress prints per move.
5. **On failure** — the error, the backup path, and a prompt: restore the backup now, or leave state as-is for inspection. Restore is the default.

`verify` (phase 4) gets the same treatment when it lands: confirm the tfvars values being written per module before the proof runs, since those values come from real state and end up on disk.

---

## 4. Next phases

### Phase 2 — `--dry-run` on migrate

Print the fully resolved operation list — every `state mv` with its source and destination files, per manifest, in execution order — without invoking the engine or touching any file. Since the plan comes from the manifest this needs no binary at all, making it a cheap review step (and a natural CI check: `refactor` + `migrate --dry-run` on every PR that touches decorators).

### Phase 3 — explicit local-only vs push modes

Today local-only is the *only* mode, implicitly. Make it explicit: `--local-only` is the default and keeps current behaviour (carved files under `.demono/modules/.state/` are the artifact); a future `--push` performs a guarded `state push` of each carved file to its module's configured new backend — refusing on serial/lineage conflicts, never `-force`, and only into empty/new backend locations. Splitting the modes now keeps the safety property visible in the command line when push eventually arrives. Push serves two personas equally: the CI merge job seeding per-module backends (§5.2) and the solo user whose monolith already lived in a remote backend (§5.1b).

### Phase 4 — `demonolith verify`: proving the split

A third command absorbing today's `--tfvars` and `--proof`:

```bash
demonolith verify [--root-dir <dir>] --engine tofu [--exec-path …] [--file …] [--state-file …] [--refresh] [--keep-tfvars] [--no-tfvars-file] [--var-file f] [--var k=v] [--output json]
```

Two sub-steps, both driven by the manifest:

1. **Extract tfvars** — resolve every cross-module input to its concrete value from the *applied* source state and write `generated.auto.tfvars` into each consumer module (today's statevars stage), so every carved root is plannable standalone.
2. **Zero-diff proof** — topo-order the modules over `cross_edges` + `ordering_edges`, plan each against a copy of its carved state with inputs threaded from upstream planned outputs (the role the control plane plays at runtime), and assert zero creates and zero destroys. `--refresh` gives the authoritative run against real provider APIs; off by default (fast, credential-free). Exit 2 if any module plans a create or destroy. **In-place updates do not fail the proof**: they are counted and reported per module (they can be legitimate pre-existing drift), so a pipeline gating on the JSON verdict must know that only create/destroy flips the exit code. The per-module plan files (`demono.tfplan`) are left in place as archivable evidence.

**Where verify gets its state.** Today's `split --proof` carves and proves in one process, handing the proof the carved state files and the pre-carve backup in memory; a standalone `verify` has neither — and in the CI story it runs during PR validation, *before* migrate has ever run. So verify sources state in two modes, chosen automatically:

- **Post-migrate**: if a migrate sidecar (§2, Guards) matching the manifest exists, verify uses the carved state files it records — proving the actual migration output.
- **Ephemeral**: otherwise, verify performs its own throwaway carve — obtain the monolith state (`--state-file`, else read-only pull), carve into a temp dir, prove, discard. Safe by construction since carving is local-only; this is what lets a credential-free PR job prove a split whose real migration hasn't happened yet. The pre-carve snapshot doubles as the applied state that sub-step 1 extracts values from.

**External inputs** (the monolith's own `var.*`) resolve exactly the way the monolith resolved them, in ascending precedence: the source root's `terraform.tfvars` and `*.auto.tfvars`, auto-loaded (the solo user, §5.1, passes nothing); then explicit `--var-file` files in the order given (for a monolith that was always applied with `-var-file=prod.tfvars` — a file terraform would not auto-load; unlike the auto-loaded set, a named file that is missing is an error); then `TF_VAR_*` environment variables; then explicit `--var k=v` flags (the CI user, §5.2, injects from the secret store). Injected values are used in memory only and appear in no artifact — the manifest and the verify sidecar record input *names*, never values. Cross-module inputs are never taken from any of these sources: the proof threads them from producer outputs itself, so a wrong wiring cannot be papered over with a hand-supplied value.

**`--no-tfvars-file`** suppresses sub-step 1's on-disk output entirely, threading every value in memory: required in credentialed CI runs where secret-bearing values must never land in a working tree, and the natural mode for §5.3 where the tfvars files would be dead weight anyway. `--keep-tfvars` is its opposite for the solo user, for whom the generated files are the permanent wiring.

The proof verdict should also be written back as a sidecar (`demonolith-verify-{datetime}.yaml`) recording per-module plan counts, so "this split was proven inert" is an artifact, not a terminal scrollback; `--output json` (see Machine interface, §2) emits the same verdict on stdout for pipelines.

### Phase 5 — attach (out of scope for the split, listed for completeness)

Generate the control-plane wiring from the manifest's `cross_edges`/`ordering_edges` — the step v1 deliberately leaves to the human. The manifest schema above is designed so this needs no re-analysis. Note that a control plane able to ingest the manifest directly (as Snap CD does in §5.3) makes this step unnecessary; attach exists for the ones that can't.

---

## 5. User stories

Three ways the same tool gets used. The spine is identical in all of them — decorate → `refactor` → review → validate (`migrate --dry-run` + `verify`) → `migrate` → adopt — and only two things vary: *who executes each step* and *where external input values come from*. Where the monolith's state lives is deliberately **not** an axis: `migrate` takes it either as a local file (`--state-file`) or by read-only pull from whatever backend the root configures, and every story works with either — §5.2 and §5.3 assume a remote backend as a matter of course, and §5.1 comes in both flavours. Everything a story needs that the tool doesn't yet do is called out inline and collected in §5.4.

### 5.1 Local solo user

One person, one machine, external inputs in the root's own `*.tfvars` files. In the base variant the monolith state is a local `terraform.tfstate`; §5.1b below is the same story against a remote backend.

Working from inside the root, so no dir argument anywhere:

1. Decorate the monolith (or skip straight to `demonolith refactor --interactive` and do the triage in the walkthrough; assignments are written back as decorators either way).
2. `demonolith refactor` — carved roots plus `demonolith-refactor-{datetime}.yaml`.
3. `demonolith migrate --engine tofu --dry-run` — read the plan; then the same command without `--dry-run`. No `--state-file` needed: the local backend is still a backend, and the default read-only pull just reads `terraform.tfstate`. Local-only is the default, so the carved per-module state files land under the output dir and the backup sits beside them.
4. `demonolith verify --engine tofu` — tfvars extracted from the applied state, zero-diff proof over the threaded graph.
5. Adopt by hand: move each carved root and its state file into place; the `generated.auto.tfvars` files are kept, because for a solo user with no control plane they *are* the wiring — each root plans standalone from its tfvars, and when an upstream output changes the user re-runs the extraction.

**§5.1b — solo with remote state.** The same person, but the monolith already uses a remote backend (s3, azurerm, …). The commands are *identical* — migrate's default pull reads from whatever backend the root configures, local or remote, so 1a and 1b differ only in where that pull happens to read from. The carve still happens on local copies, nothing is pushed. At adoption time this user has both options: keep the carved local state files (each root becomes a local-state root), or — once phase 3 lands — configure a backend per carved root and use guarded `--push`, which makes 1b the story that turns `--push` from a team feature into a general one.

**Needs from demonolith:** external root variables must resolve the way the monolith resolved them — `verify` (and the proof) must auto-load the source root's `terraform.tfvars` / `*.auto.tfvars` for external inputs rather than requiring them re-passed by hand. New capability: **source-tfvars passthrough**. (1b adds nothing further: read-only backend pull already exists, and `--push` is the same phase-3 capability §5.2 needs.)

### 5.2 Team using CI/CD

The developer refactors locally; the PR is the review gate; CI validates and, after merge, performs the migration. Secrets never touch a working tree. (This story keeps the explicit `--root-dir ./infra` throughout: CI jobs run from the repo root, which is exactly what the flag exists for.)

The division of labor across the three stages: **the dev authors the plan, PR CI proves the plan honest and inert, post-merge executes the reviewed plan verbatim.**

**Stage 1 — local dev (working in the branch).**

```bash
demonolith refactor --root-dir ./infra --interactive   # iterate: triage catchall, resolve cycles;
                                                       #   choices written back as @demono:move decorators
demonolith refactor --root-dir ./infra                 # final run: emits carved roots + the manifest
demonolith refactor --root-dir ./infra --check         # self-check before pushing
demonolith migrate  --root-dir ./infra --dry-run       # optional: eyeball the state-move list
```

The PR contains three things, all dev-authored: the decorator edits to the source, the carved roots, and the manifest. CI never generates or commits anything. The manifest in the PR is the reviewable migration plan — reviewers read `state_moves` and `cross_edges` as part of the diff.

**Stage 2 — PR validation (CI judges the PR; merges nothing, migrates nothing).** A credential-free job (backend *read* access only, no provider credentials) runs three gates:

```bash
demonolith refactor --root-dir ./infra --check --output json
    # gate 1: the plan is honest — committed source reproduces committed roots + manifest exactly; exit 2 fails the PR
demonolith migrate  --root-dir ./infra --dry-run --output json
    # gate 2 (visibility): the manifest rendered as the concrete `state mv` operation list, into the job log for reviewers
demonolith verify   --root-dir ./infra --engine tofu --no-tfvars-file --output json
    # gate 3: ephemeral mode — pull state read-only, carve a throwaway local copy, prove zero create/destroy, discard;
    #   exit 2 fails the PR
```

Optionally, a credentialed job adds the authoritative proof — external inputs injected from the secret store via `TF_VAR_*` env or `--var`, threaded in memory only, never materialized:

```bash
TF_VAR_db_password=… demonolith verify --root-dir ./infra --engine tofu --refresh --no-tfvars-file --var region=eu-west-1 --output json
```

Merge is approved on green gates. The manifest that was reviewed is byte-for-byte the manifest that gets executed.

**Stage 3 — post-merge execution (a CI job with backend credentials, inside an agreed change window so it is the only writer).**

```bash
demonolith migrate --root-dir ./infra --engine tofu --output json
    # the real carve: pull, back up, state-mv into per-module local files; writes the migrate receipt
demonolith migrate --root-dir ./infra --engine tofu --push --output json
    # phase 3: guarded push of each carved state into its module's new, empty backend; refuses conflicts, never -force
demonolith verify  --root-dir ./infra --engine tofu --no-tfvars-file --output json
    # final gate, post-migrate mode: proves the *actual* migration output via the receipt — not an ephemeral copy
```

The job archives the receipt and the verify sidecar as build artifacts — plan, receipt, proof is the audit trail. From here, pipelines plan/apply the per-module roots against their new backends, inputs still injected per-environment by CI.

**Needs from demonolith:** **`refactor --check`** (drift gate, exit-code contract); **injected external inputs** (`--var` and `TF_VAR_*` env, honored by `verify` and recorded as *names only* in any artifact); **`--no-tfvars-file`** so secret-bearing values never land on disk; the phase-3 **guarded `--push`**; and **CI ergonomics** — strict non-interactive behaviour and `--output json` on `migrate`/`verify` so pipelines can gate on structured verdicts rather than scraping text.

### 5.3 Team using CI for refactoring, Snap CD for the migration

Stages 1 and 2 are identical to §5.2 — same commands, same gates. The difference is stage 3: who executes the migration and who owns the wiring afterwards. Snap CD, driven by the manifest. The tfvars files disappear from the picture entirely (`--no-tfvars-file` throughout) — cross-module value passing is Snap CD's job at runtime.

**Stage 3b — post-merge, executed by Snap CD:**

1. **Ingest** *(Snap CD functionality, not a demonolith call)*: Snap CD is pointed at the merged manifest and creates the module definitions it describes — one module per carved root (remainder included), sourced from the repo paths in `modules`, with `cross_edges` realized as `snapcd_module_input_from_output` wirings and `ordering_edges` as dependency edges. External inputs become Snap CD input definitions bound to its own secret/variable sources.
2. **Migration job:** a one-time Snap CD migration job — approval-gated like any apply — runs, on its runner, the same three commands as §5.2's stage 3: `migrate` (the carve, writing the receipt), `migrate --push` into the backends Snap CD manages, and `verify` in post-migrate mode as the final gate.
3. **Cutover:** the monolith module is retired in Snap CD; from now on the control plane plans, orders, and threads values between the carved modules natively — the ordering edges the detached stories could only report are now enforced.

**Needs from demonolith: nothing beyond §5.2.** Snap CD consumes exactly the surface CI already depends on — the versioned manifest, the `--output json` verdicts, and the strict exit codes. (Manifest stability is not new here either: story 2's reviewers and CI jobs already parse it, so `version:` is load-bearing from §5.2 onward and changes must be additive.) Even the phase-5 `attach` step turns out to be unnecessary in this story: with Snap CD ingesting the manifest directly, there is nothing for demonolith to generate — attach remains relevant only for control planes that can't ingest the manifest themselves.

**Needs from Snap CD (the actual delta — new, explicit functionality):**

- A **manifest-ingest** endpoint or `terraform-provider-snapcd` resource that materializes module definitions and wirings from a demonolith manifest — modules from `modules`, `snapcd_module_input_from_output` from `cross_edges`, dependencies from `ordering_edges`, external inputs bound to Snap CD's own secret/variable sources.
- A first-class, approval-gated **migration job type** that executes the manifest's state moves against the monolith backend and seeds each new module's state in the backend Snap CD manages for it.
- The **demonolith binary in the runner image**, so the migration job has it available — a distribution task, not a demonolith capability.

### 5.4 Capability roll-up

Demonolith-side, there are only two requirement sets: the solo story's, and the team story's — §5.2 and §5.3 demand the identical set from demonolith, and §5.3's entire delta is Snap CD work. Every demonolith-side capability below is specced in the section its "Specced in" column names; only the Snap CD row is outside this document's scope.

| Capability | Needed by | Specced in |
|---|---|---|
| Source-tfvars passthrough for external inputs | §5.1 | phase 4, external inputs |
| `refactor --check` drift gate | §5.2 = §5.3 | §1 |
| `--var` / `TF_VAR_*` injection, `--no-tfvars-file` | §5.2 = §5.3 | phase 4, external inputs |
| Guarded `--push` to new backends | §5.1b, §5.2 = §5.3 | phase 3 |
| `--output json` + strict exit codes | §5.2 = §5.3 | §2, Machine interface |
| Versioned manifest schema as public API | §5.2 = §5.3 | §1, manifest schema |
| Manifest ingest + migration job type + binary in runner image | §5.3 only | Snap CD, not demonolith |

The variations stay small by design: §5.1 reads inputs from files, §5.2 injects them from CI, §5.3 hands them to the control plane — but `refactor`, the manifest, `--dry-run`, and the zero-diff proof are the same artifact and the same commands in all three.

---

## 6. Flag migration map

| Today (`split`) | Redesign |
|---|---|
| `--out` | `refactor --out` |
| `--remainder-module` | `refactor --remainder-module` |
| `--state` | gone — `migrate` *is* the state step |
| `--state-file` | `migrate --state-file` (and `verify --state-file` for the ephemeral carve, phase 4) |
| `--engine` / `--exec-path` | `migrate` (and `verify`) `--engine` / `--exec-path` |
| `--tfvars` | `verify` step 1 (phase 4) |
| `--proof` | `verify` step 2 (phase 4) |
| `--refresh` | `verify --refresh` (phase 4) |
| — | `refactor` writes the manifest (new) |
| — | `refactor --check` drift gate (new) |
| — | `migrate --file`, `--dry-run` (phase 2), `--local-only`/`--push` (phase 3) (new) |
| — | `verify --var` / `TF_VAR_*` / `--no-tfvars-file` external-input handling (phase 4) (new) |
| — | `--output json` + uniform exit codes on `migrate`/`verify`/`refactor --check` (new) |
| — | `--interactive` / `-i` on both commands (new) |

Everything in the current pipeline survives the redesign; nothing is dropped. What changes is the shape: one command per side of the code/state line, a manifest as the durable contract between them, and verification promoted from flags to a phase of its own.
