# Tape Format — Design, Fidelity, and Reconstruction

The authoritative reference for how tape stores Echo VR telemetry, what it
keeps, what it drops, why, and how to get identity and lost data back. Read this
before touching the format, the converter, or the proto. It exists so nobody
re-derives this from scratch again. Every claim here is backed by a test in
`pkg/conversion/*_audit_test.go` / `*_probe_test.go` — run them on any recording
with `TAPE_AUDIT_FILE=/path go test ./pkg/conversion/ -run <Name> -v`.

---

## 1. The three formats and how they relate

| Format | What it is | Fidelity |
|---|---|---|
| **`.echoreplay`** | The game's own recording: ZIP of `timestamp\tsessionJSON[\tbonesJSON]` lines. `sessionJSON` is an `engine.v1.SessionResponse` — the `/session` API payload, the **complete** per-frame truth. | Full source of truth |
| **v1 (`telemetry.v1`)** | Proto mirror. `LobbySessionStateFrame` **embeds the whole `SessionResponse`** (field 4) + bones + events. The on-disk form is the **Legacy** codec (`.nevrcap`). | Full — nothing is lost at the v1 layer |
| **v2 (`telemetry.v2`)** | The current on-disk tape: zstd stream of `Envelope`s (`CaptureHeader` + `Frame` + `CaptureFooter` with seek indexes). `float32` spatial. Per-frame payload is `EchoArenaFrame`. | **Re-encoded, not curated** — see §2 and §4 |

**Conversion flow (`tapedeck convert`):** echoreplay/nevrcap → v1 `LobbySessionStateFrame`
→ `MapFrame` (`pkg/conversion/mapping.go`) → v2 `EchoArenaFrame` → `.tape`.
The loss happens **only** at the v1→v2 `MapFrame` step.

**The v2→echoreplay reverse path exists** — `ReconstructFile` /
`SessionReconstructor` (`pkg/conversion/reconstruct.go:148 / :636`), replaying the
header, frames and events through the `Session` state layer to rebuild each
per-frame `SessionResponse`. `TestRoundTripBAC` exercises the full
`echoreplay → v2 → echoreplay` cycle on every commit. (There is still no
v2→v1 path.)

`echoreplay ↔ echoreplay` through the v1 codec is **lossless** — proven
field-for-field on 1023 frames (`TestEchoReplayRoundTripFidelity`, 0 diffs).

**On "lossy by design".** This table used to call v2 *curated — lossy by design*,
and that phrasing has been read as licence to accept any missing field as
intentional. It is not what §2 says. v2 stores the same information in a
different **shape**: constants once in the header, discrete changes as events,
per-frame data per frame. A field that lives in the header or an event is not
lost — provided reconstruction re-materializes it. The genuinely-absent fields
are the short list in §4, and each is a gap to close, not a design decision.

## 2. The v2 design philosophy (do not violate this)

v2 is **not** "every field, every frame." It is a high-performance format for
60k+ frames at low allocation/size, organized by **how often data changes**:

- **Session-constant → the header.** Names, account numbers, jersey, level, role
  live in `EchoArenaHeader.initial_roster` (`PlayerInfo`), written once. Server
  IP lives in `EchoArenaHeader.session_ip`.
- **Discrete changes → events** (`EchoEvent`). Joins, leaves, goals, team
  switches, disc possession changes, loadout changes, grab changes, and match
  settings (`MatchSettingsChanged`: `rules_changed_by`, `rules_changed_at`,
  `total_round_count` — the header holds their capture-start values, and each
  later change carries all three; format 2.3.0, gh #59). Reconstruct
  per-frame state by replaying events (see §5).
- **Per-frame-varying → per-frame fields** on `EchoArenaFrame` / `PlayerState`.
  Poses, velocity, flags, ping, scores, clock, disc, analog shoulder input,
  packet loss. Frames are **self-contained for random access** (scores/round
  repeat every frame, ~6–9 bytes). proto3 writes **nothing for a zero value**,
  so an idle per-frame field costs 0 bytes on the wire.

**`float32` is correct, not a loss.** Coordinates never exceed ±40. A `float32`
ULP at magnitude 40 is 2^(5−23) ≈ **3.8 µm** — ~260× finer than a millimeter,
far below any real game resolution. `float64→float32` discards femtometer noise,
not signal. Orientation goes 9-float basis → quaternion (exact for a real
rotation) → `float32` (~1e-7 rad). **There is no meaningful precision loss in
v1→v2.** The loss is dropped *fields*, never resolution.

**The writer enforces the stream contract.** `Writer` (`pkg/codec/tape.go`)
rejects any call outside exactly-one-header → frames → one Close: a frame
before the header, a duplicate header, a call after Close, a nil or
payload-less frame are all errors (WRITER-ORDER-001). The footer's keyframe and
event indexes record each frame's own `frame_index` — the value serialized on
the wire — so they stay consistent even for legacy captures whose stored
indices are not sequential (frame_count is the count, not the last index).

## 2b. Container layout: one stream, or independent blocks

§2 is about what goes in a frame. This is about how frames are packed into a
file, which is a separate layer and was for a long time a separate problem.

**The whole-stream layout is one continuous Zstd frame** over the whole capture.
Under it, `CaptureFooter.keyframe_index` records byte offsets into the
*decompressed* stream. Nothing can seek to those offsets — reaching one means
decompressing everything before it. The index shipped; the property it exists
for did not. That layout is still reachable, by
`codec.WithWholeStreamCompression`.

**The per-block layout is where it can, and it is THE DEFAULT**
(`pkg/codec/tape.go`, `NewWriter`). Compression moves inside the container:

```
[zstd frame]  header block                 — the CaptureHeader alone
[zstd frame]  block 0                      — opens at a keyframe
[zstd frame]  block 1
...
[zstd frame]  footer block                 — the CaptureFooter alone
[skippable]   seek table, magic 0x184D2A5E — block sizes, locatable from EOF
```

Block boundaries fall on keyframes, so the keyframe interval is also the block
size: **one decision doing two jobs** rather than two numbers drifting apart.
Header and footer get their own blocks so either can be rewritten without
touching a frame byte — and so the footer, which carries the keyframe index a
seeking reader needs *first*, is readable in one block instead of by scanning
every frame.

Under this layout `KeyframeEntry.ByteOffset` is the block's offset in the
**compressed** file. `codec.OpenBlockIndex(path)` resolves it:
`ByteRange`, `ReadBlock`, `Footer`, `BlockForFrame`.

The index is the **zstd seekable format** verbatim (facebook/zstd,
`contrib/seekable_format/zstd_seekable_compression_format.md`), not a bespoke
trailer, so it rides in a skippable frame that decoders ignore rather than choke
on. An absent seek table is a legal state meaning *not seekable or not finished*
(`ErrNoSeekTable`); a table that is present and inconsistent is
`ErrSeekTableCorrupt`.

**It is the default as of v4.1.0, and opting OUT is what takes an argument.**
It shipped opt-in, on the reasoning that a format change must be asked for by
name. That reasoning was retired on 2026-09-05: every feature is on by default
and an argument opts out, and the format had not been released — v4.1.0 *is*
the release. The old reasoning was protecting a release that had not happened,
and the cost was real — the
seekability this layout exists for was off for every caller who did not know the
option's name, which was every caller, including `tapedeck trim` and
`conversion.ConvertFile`.

So `NewWriter`, `NewWriterWithKeyframeInterval` and `NewWriterWithOptions` with
no options all produce the per-block layout. `codec.WithWholeStreamCompression`
reproduces the whole-stream bytes exactly, and `TestBackwardCompatibility`
properties 3a/3b hold it to that byte for byte against every baseline.

**Measured** on `testdata/sample.echoreplay` (1023 frames), by
`pkg/conversion/shipped_layout_bench_test.go`:

| layout | B/frame | vs whole-stream | byte-rangeable | reach frame 920 |
|---|---|---|---|---|
| whole-stream (`WithWholeStreamCompression`) | 1583 | — | no | 5013 µs |
| per-block 1s (**default**) | 1626 | +2.7% | yes | 198 µs |
| per-block + dict 1s | 1503 | **−5.0%** | yes | 256 µs |
| per-block + dict 5s | 1495 | **−5.6%** | yes | 570 µs |

Every number here is from one 34-second, 1023-frame capture. Whole-stream seek
cost grows with capture length and per-block stays flat, so the gap should widen
on a full match — unmeasured. Encode cost of per-block vs whole-stream is also
unmeasured: `BenchmarkShippedLayoutSize`'s ns/op reports a cached size, not
encode time.

Byte-range access is **not a premium**: with a trained dictionary the seekable
layout is smaller than the one that cannot seek. Per-block alone costs +2.7%
because resetting the match window every block barely matters — whole-stream
compression was only achieving ratio 1.140 across frames to begin with — and a
dictionary more than recovers it.

**A dictionary is a permanent obligation.** A capture written with dictionary D
needs D forever. zstd records the dictionary id in every frame header, so a
reader without it fails with `ErrUnknownDictionary` rather than returning wrong
bytes, but the bytes must be stored somewhere reachable. `codec.TrainDictionary`
and `codec.TrainDictionaryFromCaptures` produce one; **who owns storage and
distribution is an open operational question, not a format one.**

That guarantee covers a *missing* dictionary only. Two different dictionaries
trained under the same id are indistinguishable to the reader; the damage
surfaces only as a checksum failure — within one block for per-block captures,
at end of file for whole-stream ones. The caller chooses the id, so **id
allocation is part of the storage obligation.** Storing a dictionary hash was
rejected (`pkg/codec/seekable.go`, the F3 comment in the block decoder).

## 3. What v2 keeps vs. drops (measured, not asserted)

Measured on a real arena recording (`TestFieldLossAudit`, 22,727 frames /
234,482 player-frames):

- **Kept — 100%:** head/body/both-hands poses + velocity + bones every frame;
  disc, scores, clock, round, game status; events. The full kinematic record.
- **Dropped but always-empty in arena (0%) — no real loss:** `weapon`,
  `ordnance`, `tac_mod`, payload fields. (Echo *Combat* fields.)
- ~~**Dropped and genuinely present:** grab state, `packet_loss_ratio`, analog
  shoulder~~ — **all three now have v2 homes and round-trip** (the §6 superset
  landed): `GrabChanged` event, `PlayerState.packet_loss_ratio`, and all four
  `EchoArenaFrame` shoulder fields including the `_2` variants. Loadout
  (`weapon`/`ordnance`/`tac_mod`) likewise, via `LoadoutChanged`.
- **Not actually lost (already represented):**
  - `has_possession` → `PlayerState.flags` bit 3, and `disc_holder_slot` (frame).
  - round scores → `ScoreboardUpdated` event (seeded at frame 0, so the opening
    scoreboard is recorded — `pkg/events/sensor_scoreboard.go`).
  - per-player names/account/jersey/level → `initial_roster` + `PlayerJoined`.
  - **`possession[]` (engine field 23) — fully redundant, intentionally not
    added.** It is `[team, in-team-player-index]`, proven on alienq to agree
    with `has_possession` 100% (0 frames where one knew the holder and the other
    didn't). See `TestPossessionProbe`. The team is derivable from the roster.
    Rebuilt by `reconstructPossession` (`reconstruct.go:565`).
  - **`Team.stats` — derived, not stored.** Summing the team's players
    reproduces it exactly: measured on 7,470 team-frames, all eleven integer
    counters exact, and `possession_time` exact in 4,269 with the remainder
    inside 7e-5 (float accumulation order). `sumTeamStats` in `reconstruct.go`.
  - **`game_clock_display` — derived, not stored.** It is a pure formatting of
    the per-frame `game_clock`: `trunc(clock*100)` centiseconds rendered
    `"%02d:%02d.%02d"` (the engine truncates, does not round; minutes are
    zero-padded). Exact on 13,840 real frames. It was previously read from the
    `ScoreboardUpdated` sample, which fires only on a score change, so it was
    stale between goals and empty before the first — wrong on 100% of frames in
    both available recordings. Same reasoning as `possession[]`: redundant and
    reconstructable, so reconstruct it. See CLOCK-001.

## 4. Round-trip

- **Lossless today:** `echoreplay ↔ echoreplay` via the v1 codec (full
  `SessionResponse`).
- **`echoreplay → v2 → echoreplay` runs today** and `tapedeck verify` checks it
  field for field (below). On the committed sample every one of 138
  `SessionResponse` fields is compared and only `last_throw` differs
  (`TestRoundTripBAC`). The reconstructor **is** built (§1).
- **What still does not round-trip** (measured; `TestRoundTripBAC` reports this as
  a finding):
  - `last_throw` — the engine reports only the **recording client's** own last
    throw, so it exists only in client-recorded captures. The archive's captures
    come from the game server, which never records it. **Out of scope, not a
    gap:** it is not stored and must never be reconstructed from `DiscThrown`
    (that fabricates data). gh #35.
  - `game_clock_display` — derived text, rebuilt by truncating the stored
    `float32` clock to centiseconds (`gameClockDisplay`,
    `pkg/conversion/reconstruct.go`). The engine formats it from clock bits the
    printed `game_clock` does not carry (`227.64` displayed as `03:47.63`), so
    no formula over the stored value is exact: measured 2026-09-21, 2 of 197,125
    real frames are one centisecond off, and the verifier accepts exactly that.
    `game_clock` itself is kept, and `game_status` (which drives
    `RoundEndSensor`) round-trips.
- **Byte-exact `.echoreplay` output is not a goal.** `.echoreplay` is being
  retired within months; nothing will read regenerated files, so what matters
  is that `.tape` holds the data. Measured 2026-09-21 on three real captures
  (10,581, 2,613 and 8,106 frames; 0 dropped), regenerated files differ in
  separators, key order, float printing, line endings and emitted empty keys —
  all irrelevant to the archive. Orientation basis differs by up to 2.93e-3 per
  component (≈0.17°): the engine's basis is not exactly orthonormal and the
  stored quaternion is, so this is engine rounding noise removed, not signal
  lost.
- **Goal:** v2 is a working superset for the recoverable lane (kinematics +
  identity + loadout + grab + disc + scores). `last_score` (LASTSCORE-001),
  per-frame `jersey_number` / `level` (ROSTER-001), and the scoreboard seed
  round-trip.
- **The deletion gate.** `.tape` exists so the `.echoreplay` archive can be
  deleted. A source file may be deleted only because *that file* passed
  `tapedeck verify <file.echoreplay>`, which exits non-zero otherwise. A sample
  is not enough; the run covers every recording (gh #57). The verifier
  (`conversion.VerifyEchoReplayRoundTrip`, comparison in `pkg/fidelity`)
  converts, reconstructs, and compares every field of every frame, walking the
  schema so a new proto field is compared without code changes. Its receipt
  gives source size and sha256, frame counts, key-scan coverage, per-message
  field coverage, and every differing path with a count and an example.
  - **Fails on:** a dropped or unparseable frame; a value outside tolerance on
    any field not excused in `conversion.KnownUnpreserved` (today: `last_throw`
    only, and a reconstruction that *sets* `last_throw` fails outright); a JSON
    key the proto cannot hold that carries a non-zero value anywhere; surplus
    tab-separated fields; a second zip member.
  - **Tolerances:** `float64` leaves 1e-3 (a `float32` narrowing); orientation
    basis 5e-3 per component (measured max 2.93e-3); `game_clock_display` one
    centisecond; everything else exact.
  - **Not failures:** formatting, key order and line endings (parsed records are
    compared, not bytes); unknown keys that are always null/false/0/empty
    (older recorders write the unused spelling of a field as null, e.g.
    `head.pos: null` beside `head.position`); fields absent from both sides on
    every frame (a server capture has no `last_throw`); an empty bones payload
    (`{"user_bones":[],"err_code":0}`), which carries nothing; event-check
    failures, which concern data derived from the frames and re-derivable.

## 5. Identity / roster reconstruction (the per-frame state is anonymized)

`PlayerState` carries **slot only** — no name/account. Identity comes entirely
from events:

- `PlayerJoined` carries slot + name + account + role + jersey + level.
  `previousPlayers` starts empty, so **every player present at frame 0 emits a
  `PlayerJoined`** — the initial roster's names are in the tape.
- `PlayerLeft` carries slot + name → handles slot reuse (someone leaves slot 3,
  someone else takes it).
- Replaying joins/leaves rebuilds slot→name for any frame. Proven 100% on alienq
  (`TestRosterRebuildAudit`: 12 joins + 7 leaves recover all 234,482
  player-frames, 0 wrong, 0 missing). This is what GH #34 (`Session.RosterAt`)
  formalizes.
- **Risk (addressed):** GH #18 (silent event loss on a full channel) is FIXED
  (EVENTDROP-001) — the detector's drop counters are surfaced in `ConvertResult`
  and warned on by `tapedeck convert`, and conversion drains the events channel
  every frame, so a join cannot be dropped. Roster reconstruction is safe.

## 6. The superset plan (proto change in `nevr-proto/telemetry/v2/echo_arena.proto`)

Placed by §2's rule — nothing constant or rare gets propagated per-frame.

**SHIPPED** — all of the following are in the proto and wired end to end:

- **Per-frame:** `packet_loss_ratio` on `PlayerState`; the capture client's analog
  shoulder input (`left/right_shoulder_pressed` + alternate) on `EchoArenaFrame`
  alongside `vr_root` — it's the recording client's own input, not per-player.
- **Events** (rare changes; seed at frame 0, reconstruct via Session):
  `LoadoutChanged{weapon,ordnance,tac_mod}`, `GrabChanged{left_holding,right_holding}`.
- **Combat-only sub-message** (absent in arena): `EchoArenaFrame.payload`
  (`PayloadState`).
- **Header** (constant): `EchoArenaHeader.session_ip`.
- **Not added:** `possession[]` (redundant — §3).

**Also SHIPPED (2.1.0)** — placement measured on the dal1 corpus rather than
assumed, per §2's rule:

| addition | where | why there |
|---|---|---|
| `rules_changed_by`, `rules_changed_at` | header | invariant across every frame of 15 sampled captures |
| `team_names` | header | constant per capture (25/25); NOT derivable from `Role` — custom names exist |
| `err_code`, `blue/orange_team_restart_request` | frame | measured 0 everywhere, but transient flags; proto3 makes a zero field free |
| `PauseDetail` (`unpaused_team`, `paused_requested_team`, both timers) | frame | nil outside an active pause |
| `PAUSE_STATE_NONE`, `PAUSE_STATE_PAUSED_REQUESTED` | enum | "none" and "paused_requested" were narrowed away and unrecoverable |
| `PlayerState.possession_time` | frame | 29.4 changes per 100 frames |
| `PlayerStatsUpdated` (11 counters) | event | 0.6 changes per 100 frames |
| `EVENT_TYPE_LOADOUT_CHANGED` / `_GRAB_CHANGED` / `_PLAYER_STATS_UPDATED` | enum | without them those events never reached the footer index |

**Still needed:** nothing for the fields above. What remains is `last_throw`
(gh #35, CANONICAL-001 §2 — local-player-only). `last_score` (LASTSCORE-001),
per-frame `jersey_number` / `level` (ROSTER-001), and the scoreboard seed
(CLOCK-001) all ship.

Ship path: edit proto → CI `buf push` on merge to `nevr-proto` main publishes
the BSR module → `go get` the new version in tape (or `buf generate` + a local
`replace` for dev) → wire `MapFrame` to populate it → wire `reconstruct.go` to
read it back → round-trip test. **Both directions in the same change** — a field
written but never read is the failure mode this repo keeps repeating.

## 7. Open work (also tracked as GitHub issues)

- ~~Build the v2→echoreplay reconstructor~~ — **done** (`reconstruct.go`, §1).
- ~~Build GH #34 `Session`~~ — **done**: `OpenSession` / `RosterAt` / `LoadoutAt` /
  `GrabAt` / `ScoreAt` / `StatsAt` / `LastGoalAt` in `pkg/conversion/session.go`;
  `replay()` handles join/leave/switch/score/loadout/grab/player-info/stats
  variants. `player.stats` / `team.stats` reconstruct from the engine's
  `PlayerStatsUpdated` events (STATS-001).
- ~~Wire `last_score`~~ — **done** (LASTSCORE-001).
- `last_throw` — remaining; local-player-only (CANONICAL-001 §2).
- ~~Fix GH #18 (silent event loss)~~ — **done**: drop counters surfaced on
  `ConvertResult` (EVENTDROP-001) and `Processor` (PROCDROP-001), warned on by
  `tapedeck convert`.
- Deprecate v1 as a runtime dep once v2 is a superset; add a v1→v2 importer.

## 8. The audit tools (reusable receipts)

**None of these run in CI** — every one skips unless `TAPE_AUDIT_FILE` is set,
so the numbers quoted throughout this document come from tests that only ran
when someone ran them by hand. Re-measure before relying on any of them.

In `pkg/conversion/`, taking `TAPE_AUDIT_FILE` and skipping when absent:

- `field_loss_audit_test.go` — what's dropped + how often it carries data
- `roster_rebuild_audit_test.go` — identity recovery from events
- `possession_probe_test.go` — `possession[]` redundancy
- `roundtrip_v2_test.go` `TestRoundTripBACAudit` — the full
  `echoreplay → v2 → echoreplay` verification on an external capture (the same
  check as `tapedeck verify <file>`)

`TAPE_AUDIT_FILE` must be an **absolute** path: these run with the package
directory as cwd, and a relative path silently takes the skip branch.

`roundtrip_baseline_test.go` (`TestEchoReplayRoundTripFidelity`) does **not**
take `TAPE_AUDIT_FILE` — it is hardcoded to `testdata/sample.echoreplay` and so
runs in CI. This document previously listed it among the audit tools; it is not
one.

Run them on a new recording before assuming anything about it.
