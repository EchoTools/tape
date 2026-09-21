package conversion

import (
	"encoding/binary"
	"math"
	"testing"
	"time"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	telemetryv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func TestMapHeader_Basic(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Millisecond)
	v1h := &telemetryv1.TelemetryHeader{
		CaptureId: "test-capture-123",
		CreatedAt: timestamppb.New(now),
		Metadata:  map[string]string{"version": "1.0"},
	}

	got := MapHeader(v1h)
	if got == nil {
		t.Fatal("MapHeader returned nil")
	}
	if got.CaptureId != "test-capture-123" {
		t.Errorf("CaptureId = %q, want %q", got.CaptureId, "test-capture-123")
	}
	if got.FormatVersion != 2 {
		t.Errorf("FormatVersion = %d, want 2", got.FormatVersion)
	}
	if got.CreatedAt.AsTime() != now {
		t.Errorf("CreatedAt = %v, want %v", got.CreatedAt.AsTime(), now)
	}
	if got.Metadata["version"] != "1.0" {
		t.Errorf("Metadata[version] = %q, want %q", got.Metadata["version"], "1.0")
	}
	// No session_id in metadata, so no EchoArenaHeader.
	if got.GetEchoArena() != nil {
		t.Error("expected no EchoArenaHeader without session_id metadata")
	}
}

func TestMapHeader_Nil(t *testing.T) {
	if got := MapHeader(nil); got != nil {
		t.Errorf("MapHeader(nil) = %v, want nil", got)
	}
}

func TestMapHeader_WithSessionID(t *testing.T) {
	v1h := &telemetryv1.TelemetryHeader{
		CaptureId: "cap-1",
		CreatedAt: timestamppb.Now(),
		Metadata: map[string]string{
			"session_id":  "sess-abc",
			"map_name":    "mpl_arena_a",
			"match_type":  "Echo_Arena",
			"client_name": "TestClient",
		},
	}

	got := MapHeader(v1h)
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaHeader")
	}
	if ea.SessionId != "sess-abc" {
		t.Errorf("SessionId = %q, want %q", ea.SessionId, "sess-abc")
	}
	if ea.MapName != "mpl_arena_a" {
		t.Errorf("MapName = %q, want %q", ea.MapName, "mpl_arena_a")
	}
	if gt := got.GetGameType(); gt != "Echo_Arena" {
		t.Errorf("GameType = %q, want %q (verbatim, not narrowed)", gt, "Echo_Arena")
	}
	if ea.ClientName != "TestClient" {
		t.Errorf("ClientName = %q, want %q", ea.ClientName, "TestClient")
	}
}

func TestMapFrame_Basic(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	frameTime := baseTime.Add(500 * time.Millisecond)

	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 42,
		Timestamp:  timestamppb.New(frameTime),
		Session: &enginev1.SessionResponse{
			GameStatus:   "playing",
			GameClock:    120.5,
			BluePoints:   3,
			OrangePoints: 2,
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	if got == nil {
		t.Fatal("MapFrame returned nil")
	}
	if got.FrameIndex != 42 {
		t.Errorf("FrameIndex = %d, want 42", got.FrameIndex)
	}
	if got.TimestampOffsetMs != 500 {
		t.Errorf("TimestampOffsetMs = %d, want 500", got.TimestampOffsetMs)
	}

	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}
	if ea.GameStatus != capturepb.GameStatus_GAME_STATUS_PLAYING {
		t.Errorf("GameStatus = %v, want GAME_STATUS_PLAYING", ea.GameStatus)
	}
	if ea.GameClock != 120.5 {
		t.Errorf("GameClock = %f, want 120.5", ea.GameClock)
	}
	if ea.BluePoints != 3 {
		t.Errorf("BluePoints = %d, want 3", ea.BluePoints)
	}
	if ea.OrangePoints != 2 {
		t.Errorf("OrangePoints = %d, want 2", ea.OrangePoints)
	}
}

func TestMapFrame_Nil(t *testing.T) {
	if got, err := MapFrame(nil, time.Now()); err != nil || got != nil {
		t.Errorf("MapFrame(nil) = (%v, %v), want (nil, nil)", got, err)
	}
}

func TestMapFrame_NilSession(t *testing.T) {
	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.Now(),
	}
	got, err := MapFrame(v1f, time.Now())
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	if got == nil {
		t.Fatal("MapFrame returned nil for nil session")
	}
	if got.GetEchoArena() != nil {
		t.Error("expected no EchoArenaFrame for nil session")
	}
}

func TestMapFrame_WithPlayers(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.New(baseTime),
		Session: &enginev1.SessionResponse{
			GameStatus: "playing",
			Teams: []*enginev1.Team{
				{
					TeamName: "BLUE TEAM",
					Players: []*enginev1.TeamMember{
						{
							SlotNumber:    0,
							AccountNumber: 12345,
							DisplayName:   "Player1",
							Level:         50,
							JerseyNumber:  7,
							Ping:          30,
							IsStunned:     true,
							HasPossession: true,
							Head: &enginev1.BodyPart{
								Position: []float64{1.0, 2.0, 3.0},
								Forward:  []float64{0.0, 0.0, 1.0},
								Left:     []float64{1.0, 0.0, 0.0},
								Up:       []float64{0.0, 1.0, 0.0},
							},
							Body: &enginev1.BodyPart{
								Position: []float64{1.0, 1.0, 3.0},
								Forward:  []float64{0.0, 0.0, 1.0},
								Left:     []float64{1.0, 0.0, 0.0},
								Up:       []float64{0.0, 1.0, 0.0},
							},
							Velocity: []float64{0.1, 0.2, 0.3},
						},
					},
				},
				{
					TeamName: "ORANGE TEAM",
					Players: []*enginev1.TeamMember{
						{
							SlotNumber:  1,
							DisplayName: "Player2",
							IsBlocking:  true,
						},
					},
				},
			},
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}

	if len(ea.Players) != 2 {
		t.Fatalf("len(Players) = %d, want 2", len(ea.Players))
	}

	p0 := ea.Players[0]
	if p0.Slot != 0 {
		t.Errorf("Player[0].Slot = %d, want 0", p0.Slot)
	}
	if p0.Ping != 30 {
		t.Errorf("Player[0].Ping = %d, want 30", p0.Ping)
	}
	// Check stunned flag (bit 0) and possession flag (bit 3).
	if p0.Flags&(1<<0) == 0 {
		t.Error("Player[0] should have stunned flag set")
	}
	if p0.Flags&(1<<3) == 0 {
		t.Error("Player[0] should have possession flag set")
	}
	if p0.Head == nil {
		t.Error("Player[0] should have head pose")
	}
	if p0.Velocity == nil {
		t.Fatal("Player[0] should have velocity")
	}
	if p0.Velocity.X != 0.1 || p0.Velocity.Y != 0.2 || p0.Velocity.Z != 0.3 {
		t.Errorf("Player[0].Velocity = (%f,%f,%f), want (0.1,0.2,0.3)",
			p0.Velocity.X, p0.Velocity.Y, p0.Velocity.Z)
	}

	// Check disc holder.
	if ea.DiscHolderSlot == nil || *ea.DiscHolderSlot != 0 {
		t.Errorf("DiscHolderSlot = %v, want 0", ea.DiscHolderSlot)
	}

	p1 := ea.Players[1]
	if p1.Slot != 1 {
		t.Errorf("Player[1].Slot = %d, want 1", p1.Slot)
	}
	// Check blocking flag (bit 2).
	if p1.Flags&(1<<2) == 0 {
		t.Error("Player[1] should have blocking flag set")
	}
}

func TestMapFrame_WithDisc(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.New(baseTime),
		Session: &enginev1.SessionResponse{
			GameStatus: "playing",
			Disc: &enginev1.Disc{
				Position:    []float64{10.0, 5.0, -3.0},
				Forward:     []float64{0.0, 0.0, 1.0},
				Left:        []float64{1.0, 0.0, 0.0},
				Up:          []float64{0.0, 1.0, 0.0},
				Velocity:    []float64{1.5, 2.5, -0.5},
				BounceCount: 3,
			},
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}

	disc := ea.GetDisc()
	if disc == nil {
		t.Fatal("expected DiscState")
	}
	if disc.BounceCount != 3 {
		t.Errorf("BounceCount = %d, want 3", disc.BounceCount)
	}
	if disc.Pose == nil {
		t.Fatal("expected disc pose")
	}
	pos := disc.Pose.GetPosition()
	if pos.X != 10.0 || pos.Y != 5.0 || pos.Z != -3.0 {
		t.Errorf("disc position = (%f,%f,%f), want (10,5,-3)",
			pos.X, pos.Y, pos.Z)
	}
	vel := disc.GetVelocity()
	if vel == nil {
		t.Fatal("expected disc velocity")
	}
	if vel.X != 1.5 || vel.Y != 2.5 || vel.Z != -0.5 {
		t.Errorf("disc velocity = (%f,%f,%f), want (1.5,2.5,-0.5)",
			vel.X, vel.Y, vel.Z)
	}
}

func TestMapFrame_WithBones(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	transforms := []float32{1.0, 2.0, 3.0, 4.0, 5.0, 6.0}
	orientations := []float32{0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8}

	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.New(baseTime),
		Session: &enginev1.SessionResponse{
			GameStatus: "playing",
		},
		PlayerBones: &enginev1.PlayerBonesResponse{
			UserBones: []*enginev1.UserBones{
				{
					PlayerIndex: 0,
					BoneT:       transforms,
					BoneO:       orientations,
				},
				{
					PlayerIndex: 1,
					BoneT:       []float32{7.0, 8.0, 9.0},
				},
			},
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}

	if len(ea.PlayerBones) != 2 {
		t.Fatalf("len(PlayerBones) = %d, want 2", len(ea.PlayerBones))
	}

	pb0 := ea.PlayerBones[0]
	if pb0.Slot != 0 {
		t.Errorf("PlayerBones[0].Slot = %d, want 0", pb0.Slot)
	}

	// Verify transforms are packed correctly.
	expectedTransformBytes := len(transforms) * 4
	if len(pb0.Transforms) != expectedTransformBytes {
		t.Fatalf("Transforms len = %d, want %d", len(pb0.Transforms), expectedTransformBytes)
	}
	// Verify first transform value round-trips.
	gotFloat := math.Float32frombits(binary.LittleEndian.Uint32(pb0.Transforms[:4]))
	if gotFloat != 1.0 {
		t.Errorf("first transform = %f, want 1.0", gotFloat)
	}

	expectedOrientBytes := len(orientations) * 4
	if len(pb0.Orientations) != expectedOrientBytes {
		t.Fatalf("Orientations len = %d, want %d", len(pb0.Orientations), expectedOrientBytes)
	}

	pb1 := ea.PlayerBones[1]
	if pb1.Slot != 1 {
		t.Errorf("PlayerBones[1].Slot = %d, want 1", pb1.Slot)
	}
	if len(pb1.Orientations) != 0 {
		t.Errorf("PlayerBones[1] should have no orientations, got %d bytes", len(pb1.Orientations))
	}
}

func TestMapFrame_WithEvents(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 5,
		Timestamp:  timestamppb.New(baseTime.Add(time.Second)),
		Session: &enginev1.SessionResponse{
			GameStatus: "playing",
		},
		Events: []*telemetryv1.LobbySessionEvent{
			{
				Event: &telemetryv1.LobbySessionEvent_RoundStarted{
					RoundStarted: &telemetryv1.RoundStarted{RoundNumber: 1},
				},
			},
			{
				Event: &telemetryv1.LobbySessionEvent_PlayerGoal{
					PlayerGoal: &telemetryv1.PlayerGoal{
						PlayerSlot: 3,
						TotalGoals: 2,
						Points:     3,
					},
				},
			},
			{
				Event: &telemetryv1.LobbySessionEvent_ScoreboardUpdated{
					ScoreboardUpdated: &telemetryv1.ScoreboardUpdated{
						BluePoints:   5,
						OrangePoints: 3,
					},
				},
			},
			{
				Event: &telemetryv1.LobbySessionEvent_PlayerStun{
					PlayerStun: &telemetryv1.PlayerStun{
						PlayerSlot: 2,
						TotalStuns: 10,
					},
				},
			},
			{
				Event: &telemetryv1.LobbySessionEvent_GenericEvent{
					GenericEvent: &telemetryv1.GenericEvent{
						EventType: "custom",
						Data:      map[string]string{"key": "val"},
						Payload:   "raw data",
					},
				},
			},
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}

	if len(ea.Events) != 5 {
		t.Fatalf("len(Events) = %d, want 5", len(ea.Events))
	}

	// Verify round started.
	rs := ea.Events[0].GetRoundStarted()
	if rs == nil {
		t.Fatal("Events[0] should be RoundStarted")
	}
	if rs.RoundNumber != 1 {
		t.Errorf("RoundNumber = %d, want 1", rs.RoundNumber)
	}

	// Verify player goal.
	pg := ea.Events[1].GetPlayerGoal()
	if pg == nil {
		t.Fatal("Events[1] should be PlayerGoal")
	}
	if pg.PlayerSlot != 3 || pg.TotalGoals != 2 || pg.Points != 3 {
		t.Errorf("PlayerGoal = (%d,%d,%d), want (3,2,3)",
			pg.PlayerSlot, pg.TotalGoals, pg.Points)
	}

	// Verify scoreboard updated.
	su := ea.Events[2].GetScoreboardUpdated()
	if su == nil {
		t.Fatal("Events[2] should be ScoreboardUpdated")
	}
	if su.BluePoints != 5 || su.OrangePoints != 3 {
		t.Errorf("Scoreboard = (%d,%d), want (5,3)", su.BluePoints, su.OrangePoints)
	}

	// Verify player stun.
	ps := ea.Events[3].GetPlayerStun()
	if ps == nil {
		t.Fatal("Events[3] should be PlayerStun")
	}
	if ps.PlayerSlot != 2 || ps.TotalStuns != 10 {
		t.Errorf("PlayerStun = (%d,%d), want (2,10)", ps.PlayerSlot, ps.TotalStuns)
	}

	// Verify generic event.
	ge := ea.Events[4].GetGenericEvent()
	if ge == nil {
		t.Fatal("Events[4] should be GenericEvent")
	}
	if ge.EventType != "custom" {
		t.Errorf("GenericEvent.EventType = %q, want %q", ge.EventType, "custom")
	}
	if ge.Data["key"] != "val" {
		t.Errorf("GenericEvent.Data[key] = %q, want %q", ge.Data["key"], "val")
	}
	if ge.Payload != "raw data" {
		t.Errorf("GenericEvent.Payload = %q, want %q", ge.Payload, "raw data")
	}
}

func TestMapFrame_NilFields(t *testing.T) {
	baseTime := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)

	// Frame with session but no disc, no teams, no bones, no events.
	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.New(baseTime),
		Session: &enginev1.SessionResponse{
			GameStatus: "pre_match",
		},
	}

	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaFrame")
	}
	if ea.Disc != nil {
		t.Error("expected nil disc")
	}
	if len(ea.Players) != 0 {
		t.Errorf("expected 0 players, got %d", len(ea.Players))
	}
	if len(ea.PlayerBones) != 0 {
		t.Errorf("expected 0 bones, got %d", len(ea.PlayerBones))
	}
	if len(ea.Events) != 0 {
		t.Errorf("expected 0 events, got %d", len(ea.Events))
	}
	if ea.VrRoot != nil {
		t.Error("expected nil VrRoot")
	}
	if ea.DiscHolderSlot != nil {
		t.Error("expected nil DiscHolderSlot")
	}
}

func TestMapFrame_EmptyEvents(t *testing.T) {
	baseTime := time.Now()
	v1f := &telemetryv1.LobbySessionStateFrame{
		FrameIndex: 0,
		Timestamp:  timestamppb.New(baseTime),
		Session:    &enginev1.SessionResponse{GameStatus: "playing"},
		Events:     []*telemetryv1.LobbySessionEvent{},
	}
	got, err := MapFrame(v1f, baseTime)
	if err != nil {
		t.Fatalf("MapFrame: %v", err)
	}
	ea := got.GetEchoArena()
	if len(ea.Events) != 0 {
		t.Errorf("expected 0 events, got %d", len(ea.Events))
	}
}

func TestMapEvent_AllTypes(t *testing.T) {
	tests := []struct {
		name  string
		v1evt *telemetryv1.LobbySessionEvent
		check func(t *testing.T, e *capturepb.EchoEvent)
	}{
		{
			name: "RoundPaused",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_RoundPaused{
					RoundPaused: &telemetryv1.RoundPaused{},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetRoundPaused() == nil {
					t.Error("expected RoundPaused")
				}
			},
		},
		{
			name: "RoundUnpaused",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_RoundUnpaused{
					RoundUnpaused: &telemetryv1.RoundUnpaused{},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetRoundUnpaused() == nil {
					t.Error("expected RoundUnpaused")
				}
			},
		},
		{
			name: "RoundEnded",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_RoundEnded{
					RoundEnded: &telemetryv1.RoundEnded{
						RoundNumber: 2,
						WinningTeam: telemetryv1.Role(capturepb.Role_ROLE_BLUE_TEAM),
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				re := e.GetRoundEnded()
				if re == nil {
					t.Fatal("expected RoundEnded")
				}
				if re.RoundNumber != 2 {
					t.Errorf("RoundNumber = %d, want 2", re.RoundNumber)
				}
				if re.WinningTeam != capturepb.Role_ROLE_BLUE_TEAM {
					t.Errorf("WinningTeam = %v, want ROLE_BLUE_TEAM", re.WinningTeam)
				}
			},
		},
		{
			name: "MatchEnded",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_MatchEnded{
					MatchEnded: &telemetryv1.MatchEnded{
						WinningTeam: telemetryv1.Role(capturepb.Role_ROLE_ORANGE_TEAM),
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				me := e.GetMatchEnded()
				if me == nil {
					t.Fatal("expected MatchEnded")
				}
				if me.WinningTeam != capturepb.Role_ROLE_ORANGE_TEAM {
					t.Errorf("WinningTeam = %v, want ROLE_ORANGE_TEAM", me.WinningTeam)
				}
			},
		},
		{
			name: "PlayerJoined",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerJoined{
					PlayerJoined: &telemetryv1.PlayerJoined{
						Player: &enginev1.TeamMember{
							SlotNumber:    5,
							AccountNumber: 999,
							DisplayName:   "NewPlayer",
						},
						Role: telemetryv1.Role(capturepb.Role_ROLE_BLUE_TEAM),
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				pj := e.GetPlayerJoined()
				if pj == nil {
					t.Fatal("expected PlayerJoined")
				}
				if pj.Slot != 5 {
					t.Errorf("Slot = %d, want 5", pj.Slot)
				}
				if pj.DisplayName != "NewPlayer" {
					t.Errorf("DisplayName = %q, want %q", pj.DisplayName, "NewPlayer")
				}
				if pj.AccountNumber != 999 {
					t.Errorf("AccountNumber = %d, want 999", pj.AccountNumber)
				}
				if pj.Role != capturepb.Role_ROLE_BLUE_TEAM {
					t.Errorf("Role = %v, want ROLE_BLUE_TEAM", pj.Role)
				}
			},
		},
		{
			name: "PlayerLeft",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerLeft{
					PlayerLeft: &telemetryv1.PlayerLeft{
						PlayerSlot:  3,
						DisplayName: "Leaver",
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				pl := e.GetPlayerLeft()
				if pl == nil {
					t.Fatal("expected PlayerLeft")
				}
				if pl.PlayerSlot != 3 {
					t.Errorf("PlayerSlot = %d, want 3", pl.PlayerSlot)
				}
			},
		},
		{
			name: "DiscPossessionChanged",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_DiscPossessionChanged{
					DiscPossessionChanged: &telemetryv1.DiscPossessionChanged{
						PlayerSlot:         2,
						PreviousPlayerSlot: 5,
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				dp := e.GetDiscPossessionChanged()
				if dp == nil {
					t.Fatal("expected DiscPossessionChanged")
				}
				if dp.GetPlayerSlot() != 2 || dp.GetPreviousPlayerSlot() != 5 {
					t.Errorf("slots = (%d,%d), want (2,5)",
						dp.GetPlayerSlot(), dp.GetPreviousPlayerSlot())
				}
			},
		},
		{
			name: "DiscThrown",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_DiscThrown{
					DiscThrown: &telemetryv1.DiscThrown{PlayerSlot: 1},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetDiscThrown() == nil {
					t.Error("expected DiscThrown")
				}
			},
		},
		{
			name: "DiscCaught",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_DiscCaught{
					DiscCaught: &telemetryv1.DiscCaught{PlayerSlot: 4},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetDiscCaught() == nil {
					t.Error("expected DiscCaught")
				}
			},
		},
		{
			name: "GoalScored",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_GoalScored{
					GoalScored: &telemetryv1.GoalScored{},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetGoalScored() == nil {
					t.Error("expected GoalScored")
				}
			},
		},
		{
			name: "PlayerSave",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerSave{
					PlayerSave: &telemetryv1.PlayerSave{PlayerSlot: 1, TotalSaves: 5},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				ps := e.GetPlayerSave()
				if ps == nil {
					t.Fatal("expected PlayerSave")
				}
				if ps.TotalSaves != 5 {
					t.Errorf("TotalSaves = %d, want 5", ps.TotalSaves)
				}
			},
		},
		{
			name: "PlayerPass",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerPass{
					PlayerPass: &telemetryv1.PlayerPass{PlayerSlot: 2, TotalPasses: 3},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				pp := e.GetPlayerPass()
				if pp == nil {
					t.Fatal("expected PlayerPass")
				}
				if pp.TotalPasses != 3 {
					t.Errorf("TotalPasses = %d, want 3", pp.TotalPasses)
				}
			},
		},
		{
			name: "PlayerSteal",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerSteal{
					PlayerSteal: &telemetryv1.PlayerSteal{
						PlayerSlot:       1,
						TotalSteals:      7,
						VictimPlayerSlot: 3,
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				ps := e.GetPlayerSteal()
				if ps == nil {
					t.Fatal("expected PlayerSteal")
				}
				if ps.GetVictimPlayerSlot() != 3 {
					t.Errorf("VictimPlayerSlot = %d, want 3", ps.GetVictimPlayerSlot())
				}
			},
		},
		{
			name: "PlayerBlock",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerBlock{
					PlayerBlock: &telemetryv1.PlayerBlock{PlayerSlot: 0, TotalBlocks: 2},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetPlayerBlock() == nil {
					t.Error("expected PlayerBlock")
				}
			},
		},
		{
			name: "PlayerInterception",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerInterception{
					PlayerInterception: &telemetryv1.PlayerInterception{PlayerSlot: 4, TotalInterceptions: 1},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetPlayerInterception() == nil {
					t.Error("expected PlayerInterception")
				}
			},
		},
		{
			name: "PlayerAssist",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerAssist{
					PlayerAssist: &telemetryv1.PlayerAssist{PlayerSlot: 2, TotalAssists: 4},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetPlayerAssist() == nil {
					t.Error("expected PlayerAssist")
				}
			},
		},
		{
			name: "PlayerShotTaken",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerShotTaken{
					PlayerShotTaken: &telemetryv1.PlayerShotTaken{PlayerSlot: 1, TotalShots: 8},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				if e.GetPlayerShotTaken() == nil {
					t.Error("expected PlayerShotTaken")
				}
			},
		},
		{
			name: "PlayerSwitchedTeam",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_PlayerSwitchedTeam{
					PlayerSwitchedTeam: &telemetryv1.PlayerSwitchedTeam{
						PlayerSlot: 3,
						NewRole:    telemetryv1.Role(capturepb.Role_ROLE_ORANGE_TEAM),
						PrevRole:   telemetryv1.Role(capturepb.Role_ROLE_BLUE_TEAM),
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				pst := e.GetPlayerSwitchedTeam()
				if pst == nil {
					t.Fatal("expected PlayerSwitchedTeam")
				}
				if pst.NewRole != capturepb.Role_ROLE_ORANGE_TEAM {
					t.Errorf("NewRole = %v, want ROLE_ORANGE_TEAM", pst.NewRole)
				}
			},
		},
		{
			name: "EmotePlayed",
			v1evt: &telemetryv1.LobbySessionEvent{
				Event: &telemetryv1.LobbySessionEvent_EmotePlayed{
					EmotePlayed: &telemetryv1.EmotePlayed{
						PlayerSlot: 1,
						Emote:      telemetryv1.EmotePlayed_EmoteType(capturepb.EmotePlayed_EMOTE_TYPE_PRIMARY),
					},
				},
			},
			check: func(t *testing.T, e *capturepb.EchoEvent) {
				ep := e.GetEmotePlayed()
				if ep == nil {
					t.Fatal("expected EmotePlayed")
				}
				if ep.Emote != capturepb.EmotePlayed_EMOTE_TYPE_PRIMARY {
					t.Errorf("Emote = %v, want EMOTE_TYPE_PRIMARY", ep.Emote)
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mapEvent(tt.v1evt, nil)
			if got == nil {
				t.Fatal("mapEvent returned nil")
			}
			tt.check(t, got)
		})
	}
}

func TestMapEvent_DiscThrownWithThrowDetails(t *testing.T) {
	v1evt := &telemetryv1.LobbySessionEvent{
		Event: &telemetryv1.LobbySessionEvent_DiscThrown{
			DiscThrown: &telemetryv1.DiscThrown{
				PlayerSlot: 2,
				ThrowDetails: &enginev1.LastThrowInfo{
					ArmSpeed:                10.5,
					TotalSpeed:              15.3,
					OffAxisSpinDeg:          2.1,
					WristThrowPenalty:       0.8,
					RotPerSec:               3.7,
					PotSpeedFromRot:         4.2,
					SpeedFromArm:            6.1,
					SpeedFromMovement:       1.9,
					SpeedFromWrist:          2.3,
					WristAlignToThrowDeg:    5.4,
					ThrowAlignToMovementDeg: 8.7,
					OffAxisPenalty:          0.3,
					ThrowMovePenalty:        0.6,
				},
			},
		},
	}

	got := mapEvent(v1evt, nil)
	if got == nil {
		t.Fatal("mapEvent returned nil")
	}

	dt := got.GetDiscThrown()
	if dt == nil {
		t.Fatal("expected DiscThrown event")
	}
	if dt.PlayerSlot != 2 {
		t.Errorf("PlayerSlot = %d, want 2", dt.PlayerSlot)
	}

	td := dt.GetThrowDetails()
	if td == nil {
		t.Fatal("expected ThrowDetails to be populated")
	}

	checks := []struct {
		name string
		got  float32
		want float32
	}{
		{"ArmSpeed", td.GetArmSpeed(), 10.5},
		{"TotalSpeed", td.GetTotalSpeed(), 15.3},
		{"OffAxisSpinDeg", td.GetOffAxisSpinDeg(), 2.1},
		{"WristThrowPenalty", td.GetWristThrowPenalty(), 0.8},
		{"RotPerSec", td.GetRotPerSec(), 3.7},
		{"PotSpeedFromRot", td.GetPotSpeedFromRot(), 4.2},
		{"SpeedFromArm", td.GetSpeedFromArm(), 6.1},
		{"SpeedFromMovement", td.GetSpeedFromMovement(), 1.9},
		{"SpeedFromWrist", td.GetSpeedFromWrist(), 2.3},
		{"WristAlignToThrowDeg", td.GetWristAlignToThrowDeg(), 5.4},
		{"ThrowAlignToMovementDeg", td.GetThrowAlignToMovementDeg(), 8.7},
		{"OffAxisPenalty", td.GetOffAxisPenalty(), 0.3},
		{"ThrowMovePenalty", td.GetThrowMovePenalty(), 0.6},
	}

	for _, c := range checks {
		if c.got != c.want {
			t.Errorf("%s = %f, want %f", c.name, c.got, c.want)
		}
	}
}

func TestMapEvent_DiscThrownWithoutThrowDetails(t *testing.T) {
	v1evt := &telemetryv1.LobbySessionEvent{
		Event: &telemetryv1.LobbySessionEvent_DiscThrown{
			DiscThrown: &telemetryv1.DiscThrown{
				PlayerSlot: 5,
			},
		},
	}

	got := mapEvent(v1evt, nil)
	if got == nil {
		t.Fatal("mapEvent returned nil")
	}

	dt := got.GetDiscThrown()
	if dt == nil {
		t.Fatal("expected DiscThrown event")
	}
	if dt.PlayerSlot != 5 {
		t.Errorf("PlayerSlot = %d, want 5", dt.PlayerSlot)
	}
	if dt.GetThrowDetails() != nil {
		t.Error("expected nil ThrowDetails when v1 has no ThrowDetails")
	}
}

func TestMapEvent_Nil(t *testing.T) {
	if got := mapEvent(nil, nil); got != nil {
		t.Errorf("mapEvent(nil) = %v, want nil", got)
	}
}

func TestMapHeaderFromSession(t *testing.T) {
	now := time.Now().UTC()
	v1h := &telemetryv1.TelemetryHeader{
		CaptureId: "cap-1",
		CreatedAt: timestamppb.New(now),
	}
	session := &enginev1.SessionResponse{
		SessionId:       "sess-xyz",
		MapName:         "mpl_arena_a",
		MatchType:       "Echo_Arena",
		ClientName:      "MyClient",
		PrivateMatch:    true,
		TournamentMatch: false,
		TotalRoundCount: 3,
	}

	got := MapHeaderFromSession(v1h, session)
	if got == nil {
		t.Fatal("MapHeaderFromSession returned nil")
	}
	ea := got.GetEchoArena()
	if ea == nil {
		t.Fatal("expected EchoArenaHeader")
	}
	if ea.SessionId != "sess-xyz" {
		t.Errorf("SessionId = %q, want %q", ea.SessionId, "sess-xyz")
	}
	// game_type is the engine's symbol, verbatim, on the container header.
	if gt := got.GetGameType(); gt != "Echo_Arena" {
		t.Errorf("GameType = %q, want %q (verbatim, not narrowed)", gt, "Echo_Arena")
	}
	// AND HERE IS A REAL LOSS, ASSERTED RATHER THAN HIDDEN. This session carries
	// match_type "Echo_Arena" AND private_match=true as SEPARATE v1 fields, and
	// they disagree: the symbol does not say private, the boolean does. v2 stores
	// only the symbol, so the boolean's fact is not recoverable — DeriveGameType
	// reads Private=false, which is what the symbol says and not what the engine
	// said.
	//
	// This is asserted as the CURRENT behaviour, not endorsed. The open question
	// is whether real engine data can ever present that disagreement; if it can,
	// single-source-of-truth on the symbol drops a bit the old two-field layout
	// kept. Raised as a Decision-line rather than resolved here, and this
	// assertion is what will go red if the answer changes the code.
	//
	// EVIDENCE SO FAR IS ONE CAPTURE, and it says the disagreement does not
	// arise: testdata/sample.echoreplay converts to a header reading
	// `game_type: Echo_Arena_Private (mode=echo_arena private=true
	// tournament=false)` — the engine spelled the privacy into the symbol, so the
	// derived axis agrees with what the removed boolean said. ONE capture is not
	// the corpus and must not be read as "this cannot happen"; it is the only
	// sample anyone has measured.
	if facts := DeriveGameType(got.GetGameType()); facts.Private {
		t.Errorf("DeriveGameType(%q).Private = true; the symbol carries no "+
			"'private' segment, so this test's premise about the loss is wrong "+
			"and the comment above needs revisiting", got.GetGameType())
	}
	if ea.TotalRoundCount != 3 {
		t.Errorf("TotalRoundCount = %d, want 3", ea.TotalRoundCount)
	}
}

func TestFloat32SliceToBytes(t *testing.T) {
	input := []float32{1.0, 2.5, -3.0}
	buf := float32SliceToBytes(input)
	if len(buf) != 12 {
		t.Fatalf("len = %d, want 12", len(buf))
	}
	for i, want := range input {
		got := math.Float32frombits(binary.LittleEndian.Uint32(buf[i*4:]))
		if got != want {
			t.Errorf("buf[%d] = %f, want %f", i, got, want)
		}
	}
}

// The converter declares the format SemVer in the header: 2.3.0, the minor that
// adds MatchSettingsChanged.
func TestMapHeader_DeclaresFormatVersion(t *testing.T) {
	got := MapHeader(&telemetryv1.TelemetryHeader{CaptureId: "x"})
	if got.GetFormatVersion() != 2 || got.GetFormatMinor() != 3 || got.GetFormatPatch() != 0 {
		t.Errorf("format = %d.%d.%d, want 2.3.0",
			got.GetFormatVersion(), got.GetFormatMinor(), got.GetFormatPatch())
	}
}

// Item 2 (F): the converter tags the capture as SPARSE-encoded.
func TestMapHeader_FrameEncodingSparse(t *testing.T) {
	got := MapHeader(&telemetryv1.TelemetryHeader{CaptureId: "x"})
	if got.GetFrameEncoding() != capturepb.FrameEncoding_FRAME_ENCODING_SPARSE {
		t.Errorf("frame_encoding = %v, want SPARSE", got.GetFrameEncoding())
	}
}

// Item 3 (F-2): RoundPaused carries the pause state, requesting team, and timer.
func TestMapEvent_RoundPausedCarriesPauseState(t *testing.T) {
	v1e := &telemetryv1.LobbySessionEvent{
		Event: &telemetryv1.LobbySessionEvent_RoundPaused{
			RoundPaused: &telemetryv1.RoundPaused{
				PauseState: &enginev1.PauseState{
					PausedState:         "paused",
					PausedRequestedTeam: "blue",
					PausedTimer:         12.5,
				},
			},
		},
	}
	rp := mapEvent(v1e, nil).GetRoundPaused()
	if rp.GetPauseState() != capturepb.PauseState_PAUSE_STATE_PAUSED {
		t.Errorf("pause_state = %v, want PAUSED", rp.GetPauseState())
	}
	if rp.GetRequestingTeam() != capturepb.Role_ROLE_BLUE_TEAM {
		t.Errorf("requesting_team = %v, want BLUE", rp.GetRequestingTeam())
	}
	if rp.GetPauseTimer() != 12.5 {
		t.Errorf("pause_timer = %v, want 12.5", rp.GetPauseTimer())
	}
}
