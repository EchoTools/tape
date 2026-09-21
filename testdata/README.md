# testdata

## `sample.echoreplay`

A real EchoReplay capture (ZIP-wrapped `timestamp\tsessionJSON[\tbonesJSON]`,
~2.1 MB on disk / 8.76 MB uncompressed, 1023 frames, 36 detected events). It is
the same real capture the codec benchmark already uses as its "real file"
(`pkg/codec/echoreplay_bench_test.go:104`), copied here so the conversion tests
that were skipping now have an input to run against.

Tests unblocked by this file:

- `TestConvertFile_EchoReplay` (`pkg/conversion/convert_test.go:68`) — converts
  the capture and verifies the output is a valid tape file with a non-zero,
  consistent frame count. **Deterministic; runs and passes.**
- `TestGoldenConvert` (`pkg/conversion/golden_test.go:19`) — converts the
  capture and compares the tape bytes against the committed
  `sample.tape.golden`. **Byte-exact fidelity gate; deterministic, passes
  repeatably (`go test -count=10`).**

## `sample.tape.golden` is committed and trustworthy

Conversion is byte-deterministic: converting `sample.echoreplay` always produces
the same tape bytes. The golden is committed and tracked (its
`/testdata/*.golden` gitignore entry was removed), so `TestGoldenConvert` is a
real byte-level fidelity gate, not just a smoke test.

**Current golden** (measured 2026-09-21):

    sha256:a4ba97682a96a53eb4b517b40f37e28a822f2811485fc6fef5735a230ed72f46
    1642841 bytes, 1023 frames, 208 events

**THIS BLOCK WAS STALE AGAIN AND THAT IS RECORDED RATHER THAN QUIETLY FIXED.**
Until 2026-09-07 it read `sha256:9e51e60d…, 1618955 bytes` — the pre-`e27a62a`
values. `e27a62a` (per-block compression becomes the default) changed the golden
to `sha256:32263cc7…, 1642825 bytes` and did not update these three numbers,
which is the same drift the paragraph below warns about and the second time it
has happened to this file. Measured before regenerating: the committed artifact
hashed `32263cc7…` at 1642825 bytes while this block claimed `9e51e60d…` at
1618955. The record was wrong about the artifact by 23,870 bytes.

Regenerate by deleting `sample.tape.golden` and running `TestGoldenConvert`,
which recreates it when absent. **Update the three values above in the same
commit** — they drifted before (the file was regenerated at some point while
this block still recorded `cc060af5…`, 1625143 bytes and 36 events, none of
which matched the committed artifact), which makes the record useless for
telling whether the golden is the artifact it should be.

Changing the golden means changing the converter's output. Say what changed it:

- **2026-09-21** — `format_minor` 2 → 3 for `MatchSettingsChanged` (gh #59).
  The sample's match settings never change, so it carries no such event:
  frames and events unchanged at 1023 / 208, size unchanged at 1642841 bytes
  (single-byte varint), sha `8d8a0b4d…` → `a4ba9768…`. Predicted before
  running, all three held.
- **2026-09-07** — `format_minor` 1 → 2. The batch that removed
  `EchoArenaHeader.match_type` / `.private_match` / `.tournament_match` is not an
  additive minor, so it does not get to keep 2.1.0 — captures carrying
  `format_minor: 1` already exist on disk and reusing the number would leave two
  byte-incompatible schemas indistinguishable. **Size is unchanged at 1642841
  bytes**: the value is a single-byte varint either way, so only the sha moves
  (`3728a209…` → `8d8a0b4d…`). Frames and events unchanged at 1023 / 208.
  Predicted before running, and the prediction is the interesting part: the
  compat baselines were expected NOT to move, because `writeCompatCapture` and
  the compat harness build their headers directly and never set `format_minor`
  (`grep -rn FormatMinor pkg cmd` has exactly one non-test writer). They did not.
- **2026-09-07** — `EchoArenaHeader.match_type` / `.private_match` /
  `.tournament_match` are removed from the proto and replaced by a single
  `CaptureHeader.game_type` string holding the engine's symbol verbatim
  (`pkg/conversion/gametype.go` derives the axes back out of it). The header
  gains a `game_type` field and the game band loses one enum and two booleans:
  1642825 → 1642841 bytes, +16. Frames and events unchanged at 1023 / 208,
  because nothing per-frame moved. Verified on this artifact:
  `tapedeck show` reports
  `game_type: Echo_Arena_Private (mode=echo_arena private=true tournament=false)`,
  so the real capture's symbol carries its own privacy and the derived axes agree
  with what the removed booleans said.
- **2026-09-07 (recorded late)** — `e27a62a` made per-block compression the
  default, changing the golden to `sha256:32263cc7…` at 1642825 bytes. That
  commit did not add an entry here; this line is the back-fill, written when the
  drift was found rather than left implied by the next entry.
- **2026-07-27** — the 2.1.0 superset fields are wired: `rules_changed_by`/`_at`
  and `team_names` in the header, `err_code` / both restart requests /
  `pause_detail` per frame, `possession_time` on `PlayerState`, and a new
  `PlayerStatsUpdated` event carrying the engine's eleven integer stat counters.
  Events 200 → 208 (8 stat seeds, one per player per team occupancy).
  EventType also gained values for LoadoutChanged, GrabChanged and
  PlayerStatsUpdated, so those three finally reach CaptureFooter.event_index —
  a larger footer, hence 1620524 → 1618955 bytes rather than a pure shrink.
- **2026-07-27** — `ScoreboardSensor` now emits a seed event on the first frame
  (previously it recorded state and returned nil, so the opening scoreboard was
  never captured). Events 199 → 200. The uncompressed stream grew 21 bytes
  (1862615 → 1862636); the compressed file *shrank* 10661 bytes, because the
  seed at frame 0 gives zstd a better early match reference. Verified the
  payload grew and only the compression ratio moved.

### The non-determinism that used to block it (now fixed)

Three player sensors built their per-frame event queue by iterating a Go map,
whose iteration order is randomized:

- `PlayerJoinSensor` (`pkg/events/sensor_player.go:39`)
- `PlayerLeaveSensor` (`pkg/events/sensor_player.go:101`)
- `PlayerTeamSwitchSensor` (`pkg/events/sensor_player.go:163`)

The first frame of a real capture has every player join at once, so the
`PlayerJoined` events serialized in a random order and the tape bytes differed
run to run. The fix collects the affected player slots, sorts them ascending
(slot is the player's stable identity), and emits in that order — the same set
of events, a deterministic order, and no allocation or sort on the common
no-event frame. Verified by `TestConvertDeterministic`
(`pkg/conversion/determinism_test.go`) and the per-sensor ordering tests in
`pkg/events/sensor_player_order_test.go`.

The other event sensors were already deterministic: they emit while iterating
the session's team/player **slices** (fixed order), not maps.

### The dropped-batch risk (investigated, not a loss)

The synchronous detector drops an event batch if its events channel is full
(`pkg/events/events.go:257-263`). This does **not** lose events during
conversion: the pipeline drains the channel fully after every frame
(`pkg/conversion/convert.go:232,292`), so the channel never holds more than
one batch and the drop branch is unreachable on this path. Verified by
`TestSyncDetector_ConversionPatternNoEventLoss`
(`pkg/events/sync_detector_drop_test.go`).
