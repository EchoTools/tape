package main

import (
	"errors"
	"fmt"
	"io"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

func newDiffCommand() *cobra.Command {
	var maxDiffs int

	cmd := &cobra.Command{
		Use:   "diff <file1.tape> <file2.tape>",
		Short: "Compare two tape files frame-by-frame",
		Long: `Compare two .tape files and report differences in frame count,
event counts per type, and field-level differences for individual frames.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runDiff(cmd, args[0], args[1], maxDiffs)
		},
	}

	cmd.Flags().IntVar(&maxDiffs, "max-diffs", 5, "maximum number of differing frames to show")
	addDictFlag(cmd)

	return cmd
}

// diffEventCounts holds event counts by type for a tape file.
type diffEventCounts map[string]int

func runDiff(cmd *cobra.Command, path1, path2 string, maxDiffs int) error {
	out := cmd.OutOrStdout()

	r1, err := openTape(cmd, path1)
	if err != nil {
		return fmt.Errorf("open %s: %w", path1, err)
	}
	defer r1.Close() //nolint:errcheck // best-effort cleanup

	r2, err := openTape(cmd, path2)
	if err != nil {
		return fmt.Errorf("open %s: %w", path2, err)
	}
	defer r2.Close() //nolint:errcheck // best-effort cleanup

	h1, err := r1.ReadHeader()
	if err != nil {
		return fmt.Errorf("read header %s: %w", path1, err)
	}

	h2, err := r2.ReadHeader()
	if err != nil {
		return fmt.Errorf("read header %s: %w", path2, err)
	}

	// Compare headers.
	if !proto.Equal(h1, h2) {
		printf(out, "headers differ\n")
		if h1.GetCaptureId() != h2.GetCaptureId() {
			printf(out, "  capture_id: %q vs %q\n", h1.GetCaptureId(), h2.GetCaptureId())
		}
		if h1.GetFormatVersion() != h2.GetFormatVersion() {
			printf(out, "  format_version: %d vs %d\n", h1.GetFormatVersion(), h2.GetFormatVersion())
		}
	} else {
		printf(out, "headers identical\n")
	}

	// Read all frames from both files.
	frames1, events1, err := readAllFrames(r1)
	if err != nil {
		return fmt.Errorf("read frames %s: %w", path1, err)
	}

	frames2, events2, err := readAllFrames(r2)
	if err != nil {
		return fmt.Errorf("read frames %s: %w", path2, err)
	}

	// Frame count comparison.
	printf(out, "\nframe count: %d vs %d", len(frames1), len(frames2))
	if len(frames1) == len(frames2) {
		printf(out, " (identical)\n")
	} else {
		printf(out, " (delta: %d)\n", len(frames2)-len(frames1))
	}

	// Event count comparison.
	printf(out, "\nevent counts:\n")
	allEventTypes := make(map[string]bool)
	for k := range events1 {
		allEventTypes[k] = true
	}
	for k := range events2 {
		allEventTypes[k] = true
	}

	hasDiffEvents := false
	for et := range allEventTypes {
		c1, c2 := events1[et], events2[et]
		if c1 != c2 {
			printf(out, "  %s: %d vs %d (delta: %d)\n", et, c1, c2, c2-c1)
			hasDiffEvents = true
		}
	}
	if !hasDiffEvents {
		printf(out, "  all event counts identical\n")
	}

	// Frame-by-frame comparison.
	minFrames := min(len(frames2), len(frames1))

	diffsFound := 0
	for i := range minFrames {
		if proto.Equal(frames1[i], frames2[i]) {
			continue
		}

		diffsFound++
		if diffsFound > maxDiffs {
			continue // keep counting but stop printing
		}

		printf(out, "\nframe %d differs:\n", i)
		diffFrameFields(out, frames1[i], frames2[i])
	}

	// Extra frames.
	extraFrames := 0
	if len(frames1) > minFrames {
		extraFrames = len(frames1) - minFrames
	} else if len(frames2) > minFrames {
		extraFrames = len(frames2) - minFrames
	}

	println(out)
	if diffsFound == 0 && extraFrames == 0 {
		printf(out, "files are identical (%d frames)\n", len(frames1))
	} else {
		printf(out, "total differing frames: %d (of %d compared)", diffsFound, minFrames)
		if extraFrames > 0 {
			printf(out, " + %d extra frames in longer file", extraFrames)
		}
		println(out)
		if diffsFound > maxDiffs {
			printf(out, "(showing first %d, use --max-diffs to see more)\n", maxDiffs)
		}
	}

	return nil
}

// readAllFrames reads every frame from a reader and collects event counts.
func readAllFrames(r *codec.Reader) ([]*capturepb.Frame, diffEventCounts, error) {
	var frames []*capturepb.Frame
	events := make(diffEventCounts)

	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, err
		}
		frames = append(frames, frame)

		if ea := frame.GetEchoArena(); ea != nil {
			for _, evt := range ea.GetEvents() {
				events[eventTypeName(evt)]++
			}
		}
	}

	return frames, events, nil
}

// eventTypeName returns a human-readable name for an event's oneof case.
func eventTypeName(evt *capturepb.EchoEvent) string {
	switch evt.GetEvent().(type) {
	case *capturepb.EchoEvent_RoundStarted:
		return "RoundStarted"
	case *capturepb.EchoEvent_RoundPaused:
		return "RoundPaused"
	case *capturepb.EchoEvent_RoundUnpaused:
		return "RoundUnpaused"
	case *capturepb.EchoEvent_RoundEnded:
		return "RoundEnded"
	case *capturepb.EchoEvent_MatchEnded:
		return "MatchEnded"
	case *capturepb.EchoEvent_ScoreboardUpdated:
		return "ScoreboardUpdated"
	case *capturepb.EchoEvent_MatchSettingsChanged:
		return "MatchSettingsChanged"
	case *capturepb.EchoEvent_PlayerJoined:
		return "PlayerJoined"
	case *capturepb.EchoEvent_PlayerLeft:
		return "PlayerLeft"
	case *capturepb.EchoEvent_PlayerSwitchedTeam:
		return "PlayerSwitchedTeam"
	case *capturepb.EchoEvent_EmotePlayed:
		return "EmotePlayed"
	case *capturepb.EchoEvent_DiscPossessionChanged:
		return "DiscPossessionChanged"
	case *capturepb.EchoEvent_DiscThrown:
		return "DiscThrown"
	case *capturepb.EchoEvent_DiscCaught:
		return "DiscCaught"
	case *capturepb.EchoEvent_GoalScored:
		return "GoalScored"
	case *capturepb.EchoEvent_PlayerGoal:
		return "PlayerGoal"
	case *capturepb.EchoEvent_PlayerSave:
		return "PlayerSave"
	case *capturepb.EchoEvent_PlayerStun:
		return "PlayerStun"
	case *capturepb.EchoEvent_PlayerPass:
		return "PlayerPass"
	case *capturepb.EchoEvent_PlayerSteal:
		return "PlayerSteal"
	case *capturepb.EchoEvent_PlayerBlock:
		return "PlayerBlock"
	case *capturepb.EchoEvent_PlayerInterception:
		return "PlayerInterception"
	case *capturepb.EchoEvent_PlayerAssist:
		return "PlayerAssist"
	case *capturepb.EchoEvent_PlayerShotTaken:
		return "PlayerShotTaken"
	case *capturepb.EchoEvent_GenericEvent:
		return "GenericEvent"
	default:
		return fmt.Sprintf("Unknown(%T)", evt.GetEvent())
	}
}

// diffFrameFields prints field-level differences between two frames.
func diffFrameFields(out io.Writer, f1, f2 *capturepb.Frame) {
	if f1.GetFrameIndex() != f2.GetFrameIndex() {
		printf(out, "  frame_index: %d vs %d\n", f1.GetFrameIndex(), f2.GetFrameIndex())
	}
	if f1.GetTimestampOffsetMs() != f2.GetTimestampOffsetMs() {
		printf(out, "  timestamp_offset_ms: %d vs %d\n", f1.GetTimestampOffsetMs(), f2.GetTimestampOffsetMs())
	}

	ea1 := f1.GetEchoArena()
	ea2 := f2.GetEchoArena()

	if ea1 == nil && ea2 == nil {
		return
	}
	if ea1 == nil {
		printf(out, "  echo_arena: absent vs present\n")
		return
	}
	if ea2 == nil {
		printf(out, "  echo_arena: present vs absent\n")
		return
	}

	if ea1.GetGameStatus() != ea2.GetGameStatus() {
		printf(out, "  game_status: %s vs %s\n", ea1.GetGameStatus(), ea2.GetGameStatus())
	}
	if ea1.GetGameClock() != ea2.GetGameClock() {
		printf(out, "  game_clock: %.1f vs %.1f\n", ea1.GetGameClock(), ea2.GetGameClock())
	}
	if ea1.GetBluePoints() != ea2.GetBluePoints() {
		printf(out, "  blue_points: %d vs %d\n", ea1.GetBluePoints(), ea2.GetBluePoints())
	}
	if ea1.GetOrangePoints() != ea2.GetOrangePoints() {
		printf(out, "  orange_points: %d vs %d\n", ea1.GetOrangePoints(), ea2.GetOrangePoints())
	}
	if ea1.GetRoundNumber() != ea2.GetRoundNumber() {
		printf(out, "  round_number: %d vs %d\n", ea1.GetRoundNumber(), ea2.GetRoundNumber())
	}
	if ea1.GetPauseState() != ea2.GetPauseState() {
		printf(out, "  pause_state: %s vs %s\n", ea1.GetPauseState(), ea2.GetPauseState())
	}
	if ea1.GetDiscHolderSlot() != ea2.GetDiscHolderSlot() {
		printf(out, "  disc_holder_slot: %d vs %d\n", ea1.GetDiscHolderSlot(), ea2.GetDiscHolderSlot())
	}
	if len(ea1.GetPlayers()) != len(ea2.GetPlayers()) {
		printf(out, "  player_count: %d vs %d\n", len(ea1.GetPlayers()), len(ea2.GetPlayers()))
	}
	if len(ea1.GetEvents()) != len(ea2.GetEvents()) {
		printf(out, "  event_count: %d vs %d\n", len(ea1.GetEvents()), len(ea2.GetEvents()))
	}

	// Disc state.
	if !proto.Equal(ea1.GetDisc(), ea2.GetDisc()) {
		printf(out, "  disc: differs\n")
	}
}
