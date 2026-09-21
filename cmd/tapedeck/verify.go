package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/echotools/tape/v4/pkg/conversion"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

func newVerifyCommand() *cobra.Command {
	var verbose bool

	cmd := &cobra.Command{
		Use:   "verify <file.echoreplay>",
		Short: "Verify that a recording survives echoreplay -> tape -> echoreplay, field for field",
		Long: `Convert an .echoreplay to .tape in a temp directory, reconstruct the
.echoreplay back out of it, and compare the reconstruction against the original
EXHAUSTIVELY: every frame, every field of every message, no sampling.

This is the question you ask before deleting an irreplaceable recording, so the
answer is a receipt: the source's size and SHA-256, the frame counts on both
sides, the raw-bytes key scan, per-message schema coverage, and every field path
that differed with a count and an example. Exit status is non-zero unless the
recording round-tripped completely.

Differences on paths documented in conversion.KnownUnpreserved (see
docs/format-design.md §4) are printed as "allowed" and do not fail. Every other
difference fails, as does anything the comparison never saw: an unparseable
frame, a JSON key the proto cannot represent, a surplus tab-separated payload,
or a second member inside the zip.

Secondary, and reported separately: the event-enrichment checks over the tape
this verification already produced (throw/score events, player join-leave
balance, round monotonicity). Those cover data derived from the frames, not file
content, so they are printed but do not change the verdict or the exit status.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVerify(cmd, args[0], verbose)
		},
	}

	cmd.Flags().BoolVarP(&verbose, "verbose", "v", false, "show detailed check output")
	addDictFlag(cmd)

	return cmd
}

// verifyResult tracks all checks and their outcomes.
type verifyResult struct {
	checks  []verifyCheck
	passed  int
	failed  int
	warned  int
	verbose bool
}

type checkStatus int

const (
	statusPass checkStatus = iota
	statusFail
	statusWarn
)

type verifyCheck struct {
	name    string
	details string
	status  checkStatus
}

func (r *verifyResult) pass(name, details string) {
	r.checks = append(r.checks, verifyCheck{name: name, status: statusPass, details: details})
	r.passed++
}

func (r *verifyResult) fail(name, details string) {
	r.checks = append(r.checks, verifyCheck{name: name, status: statusFail, details: details})
	r.failed++
}

func (r *verifyResult) warn(name, details string) {
	r.checks = append(r.checks, verifyCheck{name: name, status: statusWarn, details: details})
	r.warned++
}

// runVerify is the production caller of the round-trip verification.
//
// It used to BE a verification: a hand-written list of event-shape checks that
// printed "VERDICT: PASS" for the committed sample — a file that provably loses
// client_name and the whole pause sub-state on every one of its 1023 frames.
// Checks nobody runs are decoration; a check that runs and cannot see the loss
// is worse, because it issues a receipt. The field-for-field verification is
// now the verdict, and the event checks are a clearly subordinate section.
func runVerify(cmd *cobra.Command, inputPath string, verbose bool) error {
	out := cmd.OutOrStdout()

	if filepath.Ext(inputPath) != ".echoreplay" {
		return fmt.Errorf("expected .echoreplay file, got %s", filepath.Ext(inputPath))
	}

	tmpDir, err := os.MkdirTemp("", "tapedeck-verify-*")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer os.RemoveAll(tmpDir) //nolint:errcheck // best-effort cleanup

	// --- the verdict: every frame, every field ---
	printf(out, "verifying %s\n", inputPath)
	printf(out, "  echoreplay -> tape -> echoreplay, every frame, every field of every message\n\n")

	verdict, err := conversion.VerifyEchoReplayRoundTrip(inputPath, conversion.VerifyOptions{
		Allowlist: conversion.KnownUnpreserved,
		WorkDir:   tmpDir,
	})
	if err != nil {
		return fmt.Errorf("verify %s: %w", inputPath, err)
	}
	printf(out, "%s", verdict.String())

	reasons := verdict.FailureReasons()
	if len(reasons) > 0 {
		printf(out, "\n%d reason(s) this recording did NOT survive the round trip:\n", len(reasons))
		for _, why := range reasons {
			printf(out, "  - %s\n", why)
		}
	}

	// --- secondary: the derived events, over the tape already written ---
	dict, err := dictBytes(cmd)
	if err != nil {
		return err
	}
	result := &verifyResult{verbose: verbose}
	if evErr := runEventChecks(out, inputPath, filepath.Join(tmpDir, conversion.VerifyTapeName), dict, result); evErr != nil {
		return evErr
	}

	println(out)
	printf(out, "event checks: passed %d, failed %d, warned %d\n", result.passed, result.failed, result.warned)

	if !verdict.Pass() {
		printf(out, "\nVERDICT: FIDELITY FAIL — %d reason(s). Do NOT delete the original.\n", len(reasons))
		return fmt.Errorf("%s did not round-trip: %d reason(s)", filepath.Base(inputPath), len(reasons))
	}
	// Event checks cover data the converter DERIVES from the frames. The tape
	// keeps the frames, so a detection defect loses nothing from the recording
	// and can be re-derived later; it is reported, and it does not decide
	// whether the original may be deleted.
	if result.failed > 0 {
		printf(out, "\nnote: %d event check(s) failed — derived data, re-derivable from the frames; "+
			"does not affect the verdict\n", result.failed)
	}
	printf(out, "\nVERDICT: FIDELITY PASS — %d frames, every schema field compared, "+
		"only documented holes differ (docs/format-design.md §4).\n", verdict.FramesOriginal)
	return nil
}

// runEventChecks reports on data pkg/events DERIVES during conversion. It is
// not part of the round trip: events are not present in the .echoreplay, so
// comparing them would measure this repository's own enrichment rather than the
// recording (see conversion.NotFileContent).
func runEventChecks(out io.Writer, inputPath, tapePath string, dict []byte, result *verifyResult) error {
	truth, err := collectGroundTruth(inputPath)
	if err != nil {
		return fmt.Errorf("read source: %w", err)
	}
	tapeData, err := collectTapeData(tapePath, dict)
	if err != nil {
		return fmt.Errorf("read tape: %w", err)
	}

	verifyFrameCount(result, truth.frameCount, tapeData.frameCount)
	verifyThrowEvents(result, truth, tapeData)
	verifyScoreEvents(result, truth, tapeData)
	verifyPlayerConsistency(result, tapeData)
	verifyRoundMonotonicity(result, tapeData)
	verifyNoEmptyEvents(result, tapeData)

	println(out)
	printf(out, "event enrichment checks (derived data — NOT part of the round trip):\n")
	for _, check := range result.checks {
		var marker string
		switch check.status {
		case statusPass:
			marker = "PASS"
		case statusFail:
			marker = "FAIL"
		case statusWarn:
			marker = "WARN"
		}
		printf(out, "  [%s] %s", marker, check.name)
		if check.details != "" && (result.verbose || check.status != statusPass) {
			printf(out, ": %s", check.details)
		}
		println(out)
	}
	return nil
}

// --- Ground truth collection from .echoreplay ---

type groundTruth struct {
	throwChanges []throwChange // frames where last_throw changed
	scoreChanges []scoreChange // frames where last_score changed
	frameCount   uint32
}

type throwChange struct {
	frameIndex uint32
	armSpeed   float64
	totalSpeed float64
}

type scoreChange struct {
	team         string
	personScored string
	discSpeed    float64
	frameIndex   uint32
	pointAmount  int32
}

func collectGroundTruth(inputPath string) (*groundTruth, error) {
	reader, err := codec.NewEchoReplayReader(inputPath)
	if err != nil {
		return nil, err
	}
	defer reader.Close() //nolint:errcheck // best-effort cleanup

	truth := &groundTruth{}
	var prevLastThrow *enginev1.LastThrowInfo
	var prevLastScore *enginev1.LastScore

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read frame %d: %w", truth.frameCount, err)
		}

		session := frame.GetSession()
		if session != nil {
			// Track last_throw changes
			lt := session.GetLastThrow()
			if lt != nil && (prevLastThrow == nil || !proto.Equal(prevLastThrow, lt)) {
				truth.throwChanges = append(truth.throwChanges, throwChange{
					frameIndex: truth.frameCount,
					armSpeed:   lt.GetArmSpeed(),
					totalSpeed: lt.GetTotalSpeed(),
				})
				prevLastThrow = proto.Clone(lt).(*enginev1.LastThrowInfo)
			} else if lt == nil {
				prevLastThrow = nil
			}

			// Track last_score changes
			ls := session.GetLastScore()
			if ls != nil && (prevLastScore == nil || !proto.Equal(prevLastScore, ls)) {
				truth.scoreChanges = append(truth.scoreChanges, scoreChange{
					frameIndex:   truth.frameCount,
					discSpeed:    ls.GetDiscSpeed(),
					team:         ls.GetTeam(),
					pointAmount:  ls.GetPointAmount(),
					personScored: ls.GetPersonScored(),
				})
				prevLastScore = proto.Clone(ls).(*enginev1.LastScore)
			} else if ls == nil {
				prevLastScore = nil
			}
		}

		truth.frameCount++
	}

	return truth, nil
}

// --- Tape data collection ---

type tapeData struct {
	throwEvents  []tapeThrowEvent
	scoreEvents  []tapeScoreEvent
	joinEvents   []tapeJoinEvent
	leaveEvents  []tapeLeaveEvent
	roundNumbers []roundSighting
	allEvents    []tapeEventInfo // for empty-data check
	finalPlayers int             // player count on last frame
	frameCount   uint32
}

type tapeThrowEvent struct {
	frameIndex uint32
	playerSlot int32
	hasDetails bool
	armSpeed   float32
	totalSpeed float32
}

type tapeScoreEvent struct {
	frameIndex  uint32
	discSpeed   float32
	team        capturepb.Role
	pointAmount int32
	goalType    capturepb.GoalType
}

type tapeJoinEvent struct {
	displayName string
	frameIndex  uint32
	slot        int32
}

type tapeLeaveEvent struct {
	displayName string
	frameIndex  uint32
	slot        int32
}

type roundSighting struct {
	frameIndex  uint32
	roundNumber int32
}

type tapeEventInfo struct {
	eventType  string
	detail     string
	frameIndex uint32
	isEmpty    bool
}

// collectTapeData takes the dictionary as bytes rather than reaching for the
// command, because it is called from inside the comparison rather than from a
// RunE and there is no cobra command in scope there.
func collectTapeData(tapePath string, dict []byte) (*tapeData, error) {
	reader, err := codec.NewReaderWithDictionary(tapePath, dict)
	if err != nil {
		return nil, err
	}
	defer reader.Close() //nolint:errcheck // best-effort cleanup

	if _, err := reader.ReadHeader(); err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	data := &tapeData{}
	var lastRoundNumber int32

	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, fmt.Errorf("read frame %d: %w", data.frameCount, err)
		}

		ea := frame.GetEchoArena()
		if ea != nil {
			// Track round number from frame state
			if ea.GetRoundNumber() != lastRoundNumber || data.frameCount == 0 {
				data.roundNumbers = append(data.roundNumbers, roundSighting{
					frameIndex:  data.frameCount,
					roundNumber: ea.GetRoundNumber(),
				})
				lastRoundNumber = ea.GetRoundNumber()
			}

			// Track final player count
			data.finalPlayers = len(ea.GetPlayers())

			// Process events
			for _, evt := range ea.GetEvents() {
				switch e := evt.GetEvent().(type) {
				case *capturepb.EchoEvent_DiscThrown:
					dt := e.DiscThrown
					te := tapeThrowEvent{
						frameIndex: data.frameCount,
						playerSlot: dt.GetPlayerSlot(),
						hasDetails: dt.GetThrowDetails() != nil,
					}
					if dt.GetThrowDetails() != nil {
						te.armSpeed = dt.GetThrowDetails().GetArmSpeed()
						te.totalSpeed = dt.GetThrowDetails().GetTotalSpeed()
					}
					data.throwEvents = append(data.throwEvents, te)

					empty, detail := isEmptyDiscThrown(dt)
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "discThrown",
						isEmpty:    empty,
						detail:     detail,
					})

				case *capturepb.EchoEvent_GoalScored:
					gs := e.GoalScored
					data.scoreEvents = append(data.scoreEvents, tapeScoreEvent{
						frameIndex:  data.frameCount,
						discSpeed:   gs.GetDiscSpeed(),
						team:        gs.GetTeam(),
						pointAmount: gs.GetPointAmount(),
						goalType:    gs.GetGoalType(),
					})

					empty, detail := isEmptyGoalScored(gs)
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "goalScored",
						isEmpty:    empty,
						detail:     detail,
					})

				case *capturepb.EchoEvent_PlayerJoined:
					pj := e.PlayerJoined
					data.joinEvents = append(data.joinEvents, tapeJoinEvent{
						frameIndex:  data.frameCount,
						slot:        pj.GetSlot(),
						displayName: pj.GetDisplayName(),
					})
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "playerJoined",
						isEmpty:    pj.GetDisplayName() == "" && pj.GetAccountNumber() == 0,
						detail:     fmt.Sprintf("slot=%d name=%q", pj.GetSlot(), pj.GetDisplayName()),
					})

				case *capturepb.EchoEvent_PlayerLeft:
					pl := e.PlayerLeft
					data.leaveEvents = append(data.leaveEvents, tapeLeaveEvent{
						frameIndex:  data.frameCount,
						slot:        pl.GetPlayerSlot(),
						displayName: pl.GetDisplayName(),
					})
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "playerLeft",
						isEmpty:    false,
						detail:     fmt.Sprintf("slot=%d name=%q", pl.GetPlayerSlot(), pl.GetDisplayName()),
					})

				case *capturepb.EchoEvent_RoundStarted:
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "roundStarted",
						isEmpty:    false,
						detail:     fmt.Sprintf("round=%d", e.RoundStarted.GetRoundNumber()),
					})

				case *capturepb.EchoEvent_RoundEnded:
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  "roundEnded",
						isEmpty:    false,
						detail:     fmt.Sprintf("round=%d", e.RoundEnded.GetRoundNumber()),
					})

				default:
					data.allEvents = append(data.allEvents, tapeEventInfo{
						frameIndex: data.frameCount,
						eventType:  fmt.Sprintf("%T", evt.GetEvent()),
						isEmpty:    false,
					})
				}
			}
		}

		data.frameCount++
	}

	return data, nil
}

// --- Verification checks ---

func verifyFrameCount(r *verifyResult, sourceFrames, tapeFrames uint32) {
	if sourceFrames == tapeFrames {
		r.pass("frame count", fmt.Sprintf("%d frames match", sourceFrames))
	} else {
		r.fail("frame count", fmt.Sprintf("source=%d tape=%d (delta=%d)",
			sourceFrames, tapeFrames, int64(sourceFrames)-int64(tapeFrames)))
	}
}

func verifyThrowEvents(r *verifyResult, truth *groundTruth, tape *tapeData) {
	if len(truth.throwChanges) == 0 {
		r.pass("throw events", "no throws in source")
		return
	}

	if len(tape.throwEvents) == 0 && len(truth.throwChanges) > 0 {
		r.fail("throw events", fmt.Sprintf("source has %d throw changes but tape has 0 discThrown events",
			len(truth.throwChanges)))
		return
	}

	// Count matching: each source throw change should have a corresponding tape event.
	if len(tape.throwEvents) != len(truth.throwChanges) {
		r.fail("throw event count", fmt.Sprintf("source=%d tape=%d",
			len(truth.throwChanges), len(tape.throwEvents)))
	} else {
		r.pass("throw event count", fmt.Sprintf("%d throws match", len(tape.throwEvents)))
	}

	// Check that all throw events have populated ThrowDetails.
	missingDetails := 0
	for _, te := range tape.throwEvents {
		if !te.hasDetails {
			missingDetails++
		}
	}
	if missingDetails > 0 {
		r.fail("throw details populated", fmt.Sprintf("%d/%d discThrown events missing ThrowDetails",
			missingDetails, len(tape.throwEvents)))
	} else {
		r.pass("throw details populated", fmt.Sprintf("all %d have ThrowDetails", len(tape.throwEvents)))
	}

	// Spot-check: compare arm speed / total speed of matched throws.
	// Match by order since both are collected sequentially.
	matched := min(len(truth.throwChanges), len(tape.throwEvents))
	speedMismatches := 0
	for i := range matched {
		src := truth.throwChanges[i]
		dst := tape.throwEvents[i]
		// Compare float64 -> float32 with tolerance for precision loss.
		if !floatCloseEnough(src.armSpeed, float64(dst.armSpeed)) ||
			!floatCloseEnough(src.totalSpeed, float64(dst.totalSpeed)) {
			speedMismatches++
			if r.verbose {
				r.warn("throw speed mismatch",
					fmt.Sprintf("throw %d: src arm=%.6f total=%.6f, tape arm=%.6f total=%.6f",
						i, src.armSpeed, src.totalSpeed, dst.armSpeed, dst.totalSpeed))
			}
		}
	}
	if speedMismatches > 0 {
		r.fail("throw speed values", fmt.Sprintf("%d/%d throws have speed mismatches beyond precision tolerance",
			speedMismatches, matched))
	} else if matched > 0 {
		r.pass("throw speed values", fmt.Sprintf("%d throws verified", matched))
	}
}

func verifyScoreEvents(r *verifyResult, truth *groundTruth, tape *tapeData) {
	if len(truth.scoreChanges) == 0 {
		r.pass("score events", "no scores in source")
		return
	}

	if len(tape.scoreEvents) == 0 && len(truth.scoreChanges) > 0 {
		r.fail("score events", fmt.Sprintf("source has %d score changes but tape has 0 goalScored events",
			len(truth.scoreChanges)))
		return
	}

	if len(tape.scoreEvents) != len(truth.scoreChanges) {
		r.fail("score event count", fmt.Sprintf("source=%d tape=%d",
			len(truth.scoreChanges), len(tape.scoreEvents)))
	} else {
		r.pass("score event count", fmt.Sprintf("%d scores match", len(tape.scoreEvents)))
	}

	// Check that all goal events have populated fields.
	emptyGoals := 0
	for _, se := range tape.scoreEvents {
		if se.team == capturepb.Role_ROLE_UNSPECIFIED && se.pointAmount == 0 && se.discSpeed == 0 {
			emptyGoals++
		}
	}
	if emptyGoals > 0 {
		r.fail("score details populated", fmt.Sprintf("%d/%d goalScored events have all-zero fields",
			emptyGoals, len(tape.scoreEvents)))
	} else {
		r.pass("score details populated", fmt.Sprintf("all %d have data", len(tape.scoreEvents)))
	}

	// Spot-check: compare disc speed and point amount.
	matched := min(len(truth.scoreChanges), len(tape.scoreEvents))
	speedMismatches := 0
	for i := range matched {
		src := truth.scoreChanges[i]
		dst := tape.scoreEvents[i]
		if !floatCloseEnough(src.discSpeed, float64(dst.discSpeed)) {
			speedMismatches++
		}
		if src.pointAmount != dst.pointAmount {
			r.fail("score point amount",
				fmt.Sprintf("score %d: src=%d tape=%d", i, src.pointAmount, dst.pointAmount))
		}
	}
	if speedMismatches > 0 {
		r.fail("score disc speed", fmt.Sprintf("%d/%d scores have disc speed mismatches",
			speedMismatches, matched))
	} else if matched > 0 {
		r.pass("score disc speed", fmt.Sprintf("%d scores verified", matched))
	}
}

func verifyPlayerConsistency(r *verifyResult, tape *tapeData) {
	joins := len(tape.joinEvents)
	leaves := len(tape.leaveEvents)
	netPlayers := joins - leaves

	if joins == 0 && leaves == 0 {
		r.pass("player join/leave", "no player events (pre-populated lobby)")
		return
	}

	// Net joins should be >= 0 (can't have more leaves than joins).
	if netPlayers < 0 {
		r.fail("player join/leave balance",
			fmt.Sprintf("more leaves (%d) than joins (%d), net=%d", leaves, joins, netPlayers))
	} else {
		r.pass("player join/leave balance",
			fmt.Sprintf("joins=%d leaves=%d net=%d final_players=%d", joins, leaves, netPlayers, tape.finalPlayers))
	}

	// The net player count from events should match or be close to the final
	// player count in the tape. They may not match exactly because players
	// can be present in the first frame before any join event fires.
	if tape.finalPlayers > 0 && netPlayers > tape.finalPlayers*2 {
		r.warn("player count plausibility",
			fmt.Sprintf("net joins (%d) >> final players (%d), possible duplicate join events",
				netPlayers, tape.finalPlayers))
	}
}

func verifyRoundMonotonicity(r *verifyResult, tape *tapeData) {
	if len(tape.roundNumbers) == 0 {
		r.pass("round monotonicity", "no rounds observed")
		return
	}

	violations := 0
	var prevRound int32
	for i, rs := range tape.roundNumbers {
		if i > 0 && rs.roundNumber < prevRound {
			violations++
			if r.verbose {
				r.warn("round regression",
					fmt.Sprintf("frame %d: round went from %d to %d",
						rs.frameIndex, prevRound, rs.roundNumber))
			}
		}
		prevRound = rs.roundNumber
	}

	if violations > 0 {
		r.fail("round monotonicity",
			fmt.Sprintf("%d round number regressions", violations))
	} else {
		maxRound := tape.roundNumbers[len(tape.roundNumbers)-1].roundNumber
		r.pass("round monotonicity",
			fmt.Sprintf("rounds 0..%d, %d transitions, no regressions", maxRound, len(tape.roundNumbers)))
	}
}

func verifyNoEmptyEvents(r *verifyResult, tape *tapeData) {
	emptyCount := 0
	var details []string

	for _, evt := range tape.allEvents {
		if evt.isEmpty {
			emptyCount++
			details = append(details,
				fmt.Sprintf("frame %d: %s has empty/zeroed data (%s)",
					evt.frameIndex, evt.eventType, evt.detail))
		}
	}

	if emptyCount > 0 {
		r.fail("no empty events",
			fmt.Sprintf("%d/%d events have empty data that should be populated",
				emptyCount, len(tape.allEvents)))
		if r.verbose {
			for _, d := range details {
				r.warn("empty event detail", d)
			}
		}
	} else {
		r.pass("no empty events",
			fmt.Sprintf("all %d events have data", len(tape.allEvents)))
	}
}

// --- Helpers ---

func isEmptyDiscThrown(dt *capturepb.DiscThrown) (bool, string) {
	if dt.GetThrowDetails() == nil {
		return true, "missing ThrowDetails"
	}
	td := dt.GetThrowDetails()
	if td.GetArmSpeed() == 0 && td.GetTotalSpeed() == 0 && td.GetRotPerSec() == 0 {
		return true, "all speed fields are zero"
	}
	return false, ""
}

func isEmptyGoalScored(gs *capturepb.GoalScored) (bool, string) {
	if gs.GetTeam() == capturepb.Role_ROLE_UNSPECIFIED &&
		gs.GetPointAmount() == 0 &&
		gs.GetDiscSpeed() == 0 &&
		gs.GetGoalType() == capturepb.GoalType_GOAL_TYPE_UNSPECIFIED {
		return true, "all fields empty/zero"
	}
	return false, ""
}

// floatCloseEnough compares a float64 source value against a float32-converted
// tape value. The conversion from f64 to f32 loses precision, so we tolerate
// a relative error of 1e-5 (0.001%) or an absolute difference under 1e-6.
func floatCloseEnough(src float64, tape float64) bool {
	diff := src - tape
	if diff < 0 {
		diff = -diff
	}
	if diff < 1e-6 {
		return true
	}
	denom := src
	if denom < 0 {
		denom = -denom
	}
	if denom == 0 {
		return diff < 1e-6
	}
	return diff/denom < 1e-5
}
