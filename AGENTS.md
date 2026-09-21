# AGENTS.md — agent governance for the tape repository

These rules govern every agent that touches this repo. They are enforced by git
hooks (see `scripts/hooks/`) and CI (see `.github/workflows/`). A violation
means a rejected commit, a blocked push, or a failing PR check.

This is the mechanical contract — not advice, not aspiration. Read it before
touching any file, then read `CLAUDE.md`, then read `docs/format-design.md` if
the change touches format, conversion, or fidelity.

Orientation chain: **AGENTS.md → CLAUDE.md → format-design.md → GitHub issues**

Bugs and open work live in the **GitHub issue tracker** (`gh issue list`), not a
repo ledger. The root `BUGS.md` was retired 2026-08-02; resolved-bug
identifiers in docs (CLOCK-001, ROSTER-001, CANONICAL-001, …) refer to fixes
preserved in git history.

---

## 1. Commit hygiene

- **Conventional-commit subjects.** Permitted prefixes: `feat`, `fix`, `docs`,
  `test`, `refactor`, `chore`, `build`, `style`, `perf`. The commit-msg hook
  rejects subjects without one.
- **Commit each logical piece separately.** Every commit must independently
  pass `just`. Do not batch unrelated changes.
- **Never leave uncommitted code at the end of a turn.** Not staged, not "for
  review," not "let me know." Committed.
- **Never say "I'll let you decide if it should be committed."** You decide,
  commit, and report what you committed.
- **Rebase on pull, don't merge.** `git pull --rebase`.

## 2. Branch discipline

- Work lands on `main` behind `just`. No long-lived topic branches; a branch
  found lying around is merged and deleted in the same pass.
- Before deleting a branch, check `git rev-list --count main..<branch>`. Commits
  that cannot be merged cheaply are preserved first as a pushed annotated tag —
  `git tag -a archive/<what>-<YYYY-MM-DD> <tip> -m "Archived: <what> (<N>
  commits)"`, `git push origin archive/...` — with a GitHub issue saying what
  it holds, and the branch is deleted only after the tag is on origin.
  Precedent: `archive/fidelity-batch-2026-07-25` / gh #43.
- The pre-push hook blocks pushing to `main` from any other branch.

## 3. Closed-loop gate

```sh
just
```

This is `gofmt`, `goimports`, `go vet`, `golangci-lint run ./...`,
`go test -race -count=1 ./...`. It must exit 0 before work is done.

- The pre-commit hook runs lint and format checks.
- The pre-push hook runs the full test suite with the race detector.
- CI (`pr.yml`, `main.yml`) runs the same gates on every push.

Do not fake green. If `just` is red for a reason you did not introduce, file it
as a GitHub issue and keep the suite green. A previous effort landed 27
deliberate failures "pending a ruling" and was reverted wholesale.

## 4. Code standards

The Go addendum governs. Read `~/src/metis-core/GO-ADDENDUM-GENERIC.md` if
present, or the repo copy at `.claude/GO-ADDENDUM-GENERIC.md`.

| Rule | Enforcement |
|---|---|
| No `interface{}` | golangci-lint |
| `%w` wrapping with a function-scoped prefix on every error boundary | review |
| `//nolint` requires an inline reason | golangci-lint |
| Exported symbols carry doc comments | review (revive's `exported` rule is disabled in `.golangci.yml`) |
| `b.Loop()` in benchmarks | go vet |
| `go fix` / `go mod tidy` produce no changes | pre-commit hook |

Additional rules specific to this codec library:

- **Never silently drop input.** A skipped line, a rejected frame, an unmapped
  enum must be counted and the counter must have a consumer. An uncalled method
  is a defect, not instrumentation.
- **Both directions or neither.** A field added to the forward mapper
  (`pkg/conversion/mapping.go`) gets its reverse in `pkg/conversion/reconstruct.go`
  in the same commit. A complete write path against a partial read path is the
  recurring defect in this repo.
- **Switches over proto oneofs are exhaustive.** If you cannot handle a case,
  name it in a `default:` that states so. `Session.replay()` once handled 6 of
  26 event kinds silently — this is the canonical failure mode.
- **Success derives from the artifact, never a proxy.** `go build` passing is
  not a test passing. A test passing is not a test that ran (check for
  `t.Skip`). A subagent's summary is not a measurement.

## 5. Test requirements

- **The failing test exists before the implementation.** For a fidelity change,
  the test is RED for the field in question. If you cannot make it fail first,
  you do not yet know what you are fixing.
- **The comparison must see the loss.** A round-trip test that reads both sides
  through the same reader cannot detect anything that reader drops. Compare
  against the source line count, not the parsed frame count.
- **Golden file.** `TestGoldenConvert` is a byte-for-byte gate against
  `testdata/sample.tape.golden`. When the golden changes, update
  `testdata/README.md` (SHA256, frame count, event count) in the same commit
  and state what changed it.
- **Never commit a red suite.** A finding that cannot be fixed in the same
  change goes in a GitHub issue.

## 6. Evidence

- **Every factual claim carries its source.** A `file:line` with the quoted
  line, a proto field path, or a command and its output. An unsourced claim is
  written `[UNVERIFIED]` or not written.
- **Numbers are measured, never inherited.** Do not cite a previous agent's
  number. Run the command yourself and quote its output.
- **A test that skips is not evidence.** Check whether the test you are citing
  actually ran (no `t.Skip`, no env-var gate that is not set).

## 7. What you never do

- Never leave uncommitted code at the end of a turn.
- Never say "I'll let you decide if it should be committed."
- Never push untested code.
- Never use a Python heredoc when a reusable Go tool fits — justify in writing
  why the heredoc instead.
- Never work on a repo you have not oriented to.
- Never fake green.
- Never drop a field in one direction while wiring the other.
- Never commit as someone else's identity.
