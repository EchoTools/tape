---
name: tape-work
description: Execution harness for any implementation, investigation, or format task in the tape repo (Go 1.26 codec library + tapedeck CLI for .tape/.echoreplay telemetry captures). Walks the repo's discipline gate by gate — GO-addendum pre-read, orient on the format reference, measure-before-assert against protos and real captures, decision gates on the fidelity invariants, spec-before-code, `just` as the closed-loop gate, and the GitHub issue tracker for bugs — with a verification action and an abort condition at every gate. Invoke BEFORE starting any task in this repository.
---

# /tape-work — the execution harness for this repository

The Go analogue of `/nevr-work`, adapted to a codec library whose entire value is
that a byte you put in comes back out. It executes this repo's `CLAUDE.md` +
`docs/format-design.md` + `~/src/metis-core/GO-ADDENDUM-GENERIC.md`. The rules
live there; this skill forces you to walk them in order and produce proof at each
gate before passing it.

## This IS

- A gate sequence. Every gate has a **verification action** (something you run or
  read) and an **abort condition** (when you stop and escalate). Passing a gate
  without its verification action is skipping the gate.
- Bound to the closed-loop gate `just` — `fmt vet lint test`, where `test` is
  `go test -race -count=1 ./...`. Every "done/verified" claim resolves against
  that, not against `go build` and not against a subagent's summary.

## This is NOT

- A replacement for `CLAUDE.md`, `docs/format-design.md`, or the GO addendum. It
  CITES them; it never overrides them. On conflict THOSE WIN — note the conflict,
  file it as a GitHub issue, continue under their reading.
- Optional for "small" tasks. A one-line reconstructor omission is how
  `client_name` survived conversion and vanished on the way out for months. A
  silent `continue` on a parse error is how a 450-line capture round-trips to
  zero frames and reports PASS. Small is where fidelity decays.

## Gate 0 — GO addendum pre-read (before touching any .go file)

**Do:** read `~/src/metis-core/GO-ADDENDUM-GENERIC.md` IN FULL before editing any
`.go`, `go.mod`, `justfile`, or `.golangci.yml`. Its "Never Use" and "Code Review
Hard Stops" are enforced, not advisory. The repo's `AGENTS.md` applies on top of
the addendum.

**Verification action:** your plan names the Hard Stops in scope for this change
(typically: `%w` wrapping with a function-scoped prefix on every boundary, no
`interface{}`, `//nolint` only with an inline reason, exported functions carry doc
comments, `b.Loop()` in benchmarks, `go fix`/`go mod tidy` clean).

**Abort if:** the change cannot be made without violating a Hard Stop — stop and
hand back a Decision-line. Do not relax the constraint "just here."

## Gate 1 — Orient

**Do:** read, in this order:

1. `CLAUDE.md` — build/test commands, architecture, conventions.
2. **`docs/format-design.md`** — mandated by `CLAUDE.md` before touching the
   format, the converter, or any fidelity question. Read it knowing it has drifted
   before (see Gate 2).
3. `gh issue list --state open` — the repo's bug + work tracker. The root
   `BUGS.md` ledger was retired 2026-08-02; bugs and open work live here.
4. `git log --oneline -20`, `git status`, `git branch -a`.

**Verification action:** your plan names the files you oriented on and states, in
one line, the current state you took from them (what builds, what is red, which
ledger entries are open).

**Abort if:** you cannot establish repo state or the task's subsystem — escalate
rather than explore blind.

## Gate 2 — Measure before you assert

**Rule:** before asserting ANY fact — which fields a proto carries, what a
reconstructor writes, whether a test runs in CI, how fast or how large something
is — measure it FIRST. Not to confirm a memory; to find out.

This gate exists because this repo has repeatedly punished the alternative:

- **The protos outrank every `.md` in the repo.** `docs/format-design.md` has
  claimed a reconstructor was "not built yet" while `ReconstructFile` ran in a
  passing test, and named three "real gaps" that had already shipped in
  `telemetry/v2`. Read
  `/home/andrew/src/nevr-proto/telemetry/v2/echo_arena.proto` and
  `engine/v1/engine_http.proto` directly. A doc is a claim; a proto is a fact.
- **`grep -c` proves a symbol is unused, not that a concept is unhandled.** Say
  which you measured.
- **Numbers get measured, never inherited.** A prior effort reported 11.9 GB RSS
  and 7m15s for one capture; measured, `convert` is ~8.4 MB/s per core with a
  ~17 MB base plus ~15 MB per in-flight file. Quote your own command and its
  output.
- **A test that skips is not evidence.** Four audit tests are gated on
  `TAPE_AUDIT_FILE` and never run in CI, yet `docs/format-design.md` cites their
  numbers as proof. Check whether the test you are citing actually ran.
- For engine behavior that is not in the protos (e.g. what the game counts as a
  throw), measure the binary via the ReVault MCP server. If ReVault lacks it, SAY
  SO — do not guess.

**Verification action:** every factual claim in your plan or report carries its
source — a `file.go:LINE` with the quoted line, a proto line, or a command and its
output. An unsourced claim is written `[UNVERIFIED]` or not written.

**Abort if:** a subagent hands you a number with a story — re-verify the story
independently before building on it. A correct number wearing a false story is the
most expensive artifact an agent can hand back.

## Gate 3 — Decision gates on the invariants

**Do:** enumerate what the task touches against these:

- **Round-trip fidelity.** `decode(encode(x)) == x` for the tape format. Any codec
  change must preserve it. Repaired inputs normalize to the **canonical**
  (May-2026 client) form — the loss must be real, never cosmetic.
- **The v2 delta model** [`format-design.md` §2, "do not violate this"]:
  session-constant → header, discrete change → event, per-frame-varying → frame.
  A field living in the header or an event is not a loss, provided reconstruction
  re-materializes it per frame. Do not "fix" fidelity by making v2 dense.
- **EchoReplay byte compatibility.** The writer applies
  `FixProtojsonUint64Encoding` and `FixExponentNotation` to match engine output
  exactly. Third-party parsers depend on those bytes.
- **Determinism.** `TestGoldenConvert` is a byte-for-byte gate. If the golden
  changes, regenerate it deliberately, say what changed it, and update
  `testdata/README.md`'s size and sha256 in the same commit.
- **Reader budgets.** SEC-001/SEC-002 caps (`pkg/codec/limits.go`) stay on every
  reader and every accumulation site. Never add a read path that bypasses
  `newBudgetReader`.
- **Proto changes are not made here.** `telemetry/v2` lives in `nevr-proto` and
  ships via the BSR. Never edit generated Go.
- **`main`'s `go.mod` never carries the dev-local `replace`** (RELEASE-001).
- **File extensions:** `.tape` is canonical; `.nevrcap` is accepted legacy input
  and is read-only (the v1 writer was deliberately deleted in `ea20c26`).

**Verification action:** your plan states in one line which invariants the task
touches, or "touches no invariant" with the reasoning.

**Abort if:** unsure whether it's gated — then it is. Write the Decision-line
(closed question + options + recommendation) and STOP. Do not build "pending
approval."

## Gate 4 — Spec the behavior before the code

**Do:**

1. Plan before non-trivial code, with **at least two self-review passes** and a
   stated testing strategy.
2. **TDD: the failing test exists before the implementation.** For a fidelity
   change that means a round-trip assertion that is RED for the field in question.
   If you cannot make it fail first, you do not yet know what you are fixing.
3. **The comparison must be able to see the loss.** A round-trip test that reads
   both sides through the same reader cannot detect anything that reader drops:
   skipped lines are absent from both sides and score as a match. Compare against
   the **source line count**, not the parsed frame count.
4. **The wiring rule:** every CLI flag, config field, and exported option you add
   must have a proven consumer — grep shows it read outside its declaring file,
   and a test exercises the behavior. `--help` output IS the interface contract.
   A field written but never read (see `PayloadState`) is a defect, not a stub.

**Abort if:** you cannot write the failing test or state the observable — return
to Gate 3.

## Gate 5 — Implement inside the negations

- **Both directions or neither.** Every field added to the forward mapper gets its
  reverse in `reconstruct.go` (or an explicit ledger entry saying why not) in the
  same change. The recurring defect in this repo is a complete write path against
  a partial read path.
- **Switches over proto oneofs are exhaustive or explicitly not.** `Session.replay()`
  handled 6 of 26 `EchoEvent` kinds and `classifyEvent` 24 of 26, both silently.
  If you cannot handle a case, name it in a `default:` that says so.
- **Never silently drop input.** A skipped line, a rejected frame, an unmapped
  enum: count it, expose it, and make some caller check it. `SkippedFrames()`
  existed for months with no production consumer.
- **Success derives from the artifact, never a proxy.** `go build` passing is not
  a test passing; a test passing is not a test that ran (check for `t.Skip`).
- Build and run the affected package's tests after each logical step, not only at
  the end.

## Gate 6 — Verify it landed (close the loop)

**Do:**

1. **`just`** — `fmt vet lint test`. It must exit 0. Quote its tail. Known
   pre-existing failures are cited by ledger entry, never waved through.
2. `gofmt -l .` and `go vet ./...` zero output; `golangci-lint run` zero output;
   `go fix ./...` and `go mod tidy` zero changes; `govulncheck ./...` clean.
3. For a codec or conversion change, additionally run the fidelity lane on real
   data: `TAPE_AUDIT_FILE=/path/to.echoreplay go test ./pkg/conversion/ -run
   'Audit|Fidelity|RoundTrip' -v`. The committed sample is 1023 frames of one
   private match; it does not exercise payload, spectators, or multi-round play.
4. **Never commit a red suite.** A previous effort landed 27 deliberate failures
   "pending a ruling" and was reverted wholesale. If a finding is out of scope, it
   goes in a GitHub issue and the suite stays green.

**Abort if:** `just` is red for a reason you did not introduce — report it as a
finding with its measurement and a Decision-line; do NOT fake green.

## Gate 7 — Record and commit

- **Tracker:** record findings as GitHub issues. Shape: title = severity + What;
  body = What (measured) → Where (`file:line`) → Evidence → Impact → Fix
  direction → Status. When a fix lands, close the issue with a comment citing the
  commit hash and the test names that pin it.
- **Docs ship with the code.** If behavior changes, `docs/format-design.md`
  changes in the **same commit**. Doc drift here is not cosmetic — it is what
  caused a 4,545-line effort to be built on a schema that no longer existed, and
  reverted.
- **Commit identity:** author name *and* email both `agents@sprock.io`,
  `--no-gpg-sign`, trailer `Co-authored-by: Andrew Bates <a@sprock.io>`;
  conventional prefix (`feat:`/`fix:`/`docs:`/`test:`/`build:`/`style:`). Commit
  each logical piece separately. **Verify after each commit:**
  `git log --format='%h %G? %an %ae' -1` — expect `N` for the signature and never
  the owner's name/email.

  (Measured from `nevr-runtime` `git log`, 5/5 recent commits. Note `/nevr-work`'s
  written Gate 7 specifies a second `Co-authored-by: Metis Sprock` trailer that
  the repo does not actually use; observed practice wins per Gate 2.)
- **Scratch** lives under `$CLAUDE_JOB_DIR/tmp` or `/var/tmp/work-tape/`, never
  `/tmp`, never the repo. `_local/` is gitignored developer scratch and at least
  one benchmark silently falls back when it is absent.

## Report — the standing sibling lines

Before declaring done, produce the unit report: deliverable + every
"verified/green" claim with its quoted post-hoc measurement, plus when applicable:

- **CANDIDATE line** — any integrity check you invented ad-hoc: the predicate it
  tests + yes/no on graduation into a persistent checker.
- **USAGE line** — any `--help`/recipe-doc gap you touched.
- **DOC-DRIFT line** — any stale claim you found in `format-design.md`,
  `testdata/README.md`, or `docs/browser-integration.md`, whether or not you
  fixed it.

**Same failure twice → STOP and hand back a Decision-line.** Do not grind; do not
fake a green.

## Common mistakes

- Citing `docs/format-design.md` as authority on what v2 carries. It has been
  wrong about the reconstructor's existence and about three "real gaps" that had
  already shipped. Read the proto.
- Reading a `--- PASS` as proof of fidelity when the test compares survivor set to
  survivor set, or when it was gated on an env var and skipped.
- Treating "v2 is lossy by design" as license to allowlist a field. Check whether
  the proto has a home for it first — most of the historical allowlist did.
- Reconstructing a field from an event that fires for every player when the source
  field is local-player-only (`last_throw`). Fabricated data passes a round-trip
  and is worse than an absence.
- Adding a new package for what an existing audit test already covers.
  `pkg/conversion/` already has field-loss, mapping, roster-rebuild, possession,
  superset-forward, and round-trip audits.
