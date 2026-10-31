package conversion

import (
	"path/filepath"
	"testing"
	"time"

	spatialv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/spatial/v1"
	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// combatFixturePose is a pose with a position and an identity orientation.
func combatFixturePose(x, y, z float32) *spatialv1.Pose {
	return &spatialv1.Pose{
		Position:    &spatialv1.Vec3{X: x, Y: y, Z: z},
		Orientation: &spatialv1.Quat{W: 1},
	}
}

// combatFixturePlayer is one synthetic combat player. Team index lives in
// flags bits 5-6 (0 blue, 1 orange); no real names or identifiers are used.
func combatFixturePlayer(slot int32, team uint32, n float32) *capturepb.PlayerState {
	return &capturepb.PlayerState{
		Slot:            slot,
		Flags:           team << teamIndexShift,
		Ping:            40,
		Head:            combatFixturePose(n, 1.6, 0),
		Body:            combatFixturePose(n, 1.0, 0),
		LeftHand:        combatFixturePose(n-0.3, 1.2, 0.2),
		RightHand:       combatFixturePose(n+0.3, 1.2, 0.2),
		Velocity:        &spatialv1.Vec3{X: 0.5, Y: 0, Z: -0.5},
		PacketLossRatio: 0.01,
	}
}

// writeCombatFixture writes a three-frame Echo Combat tape built from the
// measured combat key tree (types and structure only, no real data) into dir
// and returns its path. The payload is present so the payload keys are
// exercised; the match header carries the engine's combat match_type.
func writeCombatFixture(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "combat.tape")
	w, err := codec.NewWriterWithOptions(path)
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	header := &capturepb.CaptureHeader{
		CaptureId:     "combat-fixture",
		CreatedAt:     timestamppb.New(time.Unix(1756600000, 0).UTC()),
		FormatVersion: 2,
		GameType:      "Echo_Combat",
		GameHeader: &capturepb.CaptureHeader_EchoArena{
			EchoArena: &capturepb.EchoArenaHeader{
				SessionId:       "00000000-0000-4000-8000-000000000001",
				MapName:         "mpl_combat_fission",
				ClientName:      "fixture-client",
				SessionIp:       "192.0.2.1",
				TotalRoundCount: 3,
				InitialRoster: []*capturepb.PlayerInfo{
					{Slot: 0, AccountNumber: 1, DisplayName: "fixture-blue-1", Role: capturepb.Role_ROLE_BLUE_TEAM, JerseyNumber: 1, Level: 10},
					{Slot: 1, AccountNumber: 2, DisplayName: "fixture-blue-2", Role: capturepb.Role_ROLE_BLUE_TEAM, JerseyNumber: 2, Level: 11},
					{Slot: 2, AccountNumber: 3, DisplayName: "fixture-orange-1", Role: capturepb.Role_ROLE_ORANGE_TEAM, JerseyNumber: 3, Level: 12},
					{Slot: 3, AccountNumber: 4, DisplayName: "fixture-orange-2", Role: capturepb.Role_ROLE_ORANGE_TEAM, JerseyNumber: 4, Level: 13},
				},
			},
		},
	}
	if err := w.WriteHeader(header); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for i := range 3 {
		n := float32(i)
		frame := &capturepb.Frame{
			FrameIndex:        uint32(i),
			TimestampOffsetMs: uint32(i * 33),
			Payload: &capturepb.Frame_EchoArena{EchoArena: &capturepb.EchoArenaFrame{
				GameClock:  300 - n,
				BluePoints: 0,
				Players: []*capturepb.PlayerState{
					combatFixturePlayer(0, 0, n),
					combatFixturePlayer(1, 0, n+1),
					combatFixturePlayer(2, 1, n+2),
					combatFixturePlayer(3, 1, n+3),
				},
				VrRoot:  combatFixturePose(0, 0, 0),
				Payload: &capturepb.PayloadState{Multiplier: 1, Checkpoint: 2, Distance: 12.5 + n, Defenders: 1, Speed: 3},
			}},
		}
		if err := w.WriteFrame(frame); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}
