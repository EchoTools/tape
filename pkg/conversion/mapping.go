package conversion

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"slices"
	"time"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	spatialv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/spatial/v1"
	telemetryv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"

	"github.com/echotools/tape/v4/pkg/events"
)

// ErrTimestampOffsetOverflow is returned by MapFrame and FrameMapper.MapFrame
// when a frame's timestamp is more than math.MaxUint32 milliseconds after
// baseTime. TimestampOffsetMs is a uint32 delta from created_at, so an offset
// past ~49.7 days cannot be represented; the frame is not mapped rather than
// silently wrapping to an unrelated offset (release-audit finding R5).
var ErrTimestampOffsetOverflow = errors.New("timestamp offset exceeds uint32 max (~49.7 days from created_at); cannot represent in TimestampOffsetMs")

// gameStatusMap maps v1 string game_status to v2 GameStatus enum.
var gameStatusMap = map[string]capturepb.GameStatus{
	"pre_match":         capturepb.GameStatus_GAME_STATUS_PRE_MATCH,
	"round_start":       capturepb.GameStatus_GAME_STATUS_ROUND_START,
	"playing":           capturepb.GameStatus_GAME_STATUS_PLAYING,
	"score":             capturepb.GameStatus_GAME_STATUS_SCORE,
	"round_over":        capturepb.GameStatus_GAME_STATUS_ROUND_OVER,
	"post_match":        capturepb.GameStatus_GAME_STATUS_POST_MATCH,
	"pre_sudden_death":  capturepb.GameStatus_GAME_STATUS_PRE_SUDDEN_DEATH,
	"sudden_death":      capturepb.GameStatus_GAME_STATUS_SUDDEN_DEATH,
	"post_sudden_death": capturepb.GameStatus_GAME_STATUS_POST_SUDDEN_DEATH,
}

// matchTypeMap is GONE, and this note is here so the next reader does not
// reinvent it. It mapped seven v1 match_type strings onto a MatchType enum;
// CaptureHeader.game_type now carries the engine's symbol verbatim and
// DeriveGameType reads the axes back out of it (gametype.go).
//
// The table was lossy in two different ways and only one of them was obvious.
// A symbol it HELD round-tripped intact, because matchTypeReverse was its
// literal inverse — what was lost there was expressiveness for a v2 consumer,
// which could not tell arena from combat behind MATCH_TYPE_PRIVATE. A symbol it
// did NOT hold lost its value outright: measured 2026-09-07,
// "Echo_Combat_Private" forward-mapped to MATCH_TYPE_UNSPECIFIED and reversed to
// "". Storing the symbol cannot do either.

// pauseStateMap maps v1 PauseState.paused_state strings to v2 PauseState enum.
var pauseStateMap = map[string]capturepb.PauseState{
	"":       capturepb.PauseState_PAUSE_STATE_NOT_PAUSED,
	"none":   capturepb.PauseState_PAUSE_STATE_NONE,
	"paused": capturepb.PauseState_PAUSE_STATE_PAUSED,
	// 2.1.0 added PAUSE_STATE_NONE and PAUSE_STATE_PAUSED_REQUESTED, so the
	// former narrowing of "none" onto NOT_PAUSED and "paused_requested" onto
	// PAUSED is gone; both now round-trip exactly.
	"unpaused":         capturepb.PauseState_PAUSE_STATE_NOT_PAUSED,
	"paused_requested": capturepb.PauseState_PAUSE_STATE_PAUSED_REQUESTED,
	"unpausing":        capturepb.PauseState_PAUSE_STATE_UNPAUSING,
	"autopause_replay": capturepb.PauseState_PAUSE_STATE_AUTOPAUSE_REPLAY,
}

// teamStringToRole maps v1 team name strings to v2 Role enum.
var teamStringToRoleMap = map[string]capturepb.Role{
	"blue":   capturepb.Role_ROLE_BLUE_TEAM,
	"orange": capturepb.Role_ROLE_ORANGE_TEAM,
}

func teamStringToRole(team string) capturepb.Role {
	if r, ok := teamStringToRoleMap[team]; ok {
		return r
	}
	return capturepb.Role_ROLE_UNSPECIFIED
}

// goalTypeStringMap covers the engine's complete goal-type table, which sits
// contiguously in echovr.exe at 0x1416e22b0-0x1416e2360. An unmapped value
// becomes GOAL_TYPE_UNSPECIFIED and is lost, so this must stay exhaustive —
// TestGoalTypeMapCoversEveryEngineValue fails if the engine gains one.
var goalTypeStringMap = map[string]capturepb.GoalType{
	"[NO GOAL]":        capturepb.GoalType_GOAL_TYPE_NO_GOAL,
	"SLAM DUNK":        capturepb.GoalType_GOAL_TYPE_SLAM_DUNK,
	"INSIDE SHOT":      capturepb.GoalType_GOAL_TYPE_INSIDE_SHOT,
	"LONG SHOT":        capturepb.GoalType_GOAL_TYPE_LONG_SHOT,
	"BOUNCE SHOT":      capturepb.GoalType_GOAL_TYPE_BOUNCE_SHOT,
	"LONG BOUNCE SHOT": capturepb.GoalType_GOAL_TYPE_LONG_BOUNCE_SHOT,
	"HEADBUTT":         capturepb.GoalType_GOAL_TYPE_HEADBUTT,
	"LONG HEADBUTT":    capturepb.GoalType_GOAL_TYPE_LONG_HEADBUTT,
	"BUMPER SHOT":      capturepb.GoalType_GOAL_TYPE_BUMPER_SHOT,
	"LONG BUMPER SHOT": capturepb.GoalType_GOAL_TYPE_LONG_BUMPER_SHOT,
	"SELF GOAL":        capturepb.GoalType_GOAL_TYPE_SELF_GOAL,
}

// goalTypeReverse renders a GoalType back to the engine's exact spelling.
var goalTypeReverse = buildReverse(goalTypeStringMap, capturepb.GoalType_GOAL_TYPE_UNSPECIFIED, "")

// teamRoleReverse renders a Role back to the lowercase team string the engine
// uses in last_score.team. Distinct from roleToTeamString, which spells the
// absent case "none" for the pause sub-fields.
var teamRoleReverse = buildReverse(teamStringToRoleMap, capturepb.Role_ROLE_UNSPECIFIED, "")

// goalTypeStringToEnum maps a v1 goal_type string to the v2 GoalType enum,
// discarding the record of an unrecognised value. Conversion paths use
// (*vocabulary).goalType instead so the loss is counted.
func goalTypeStringToEnum(goalType string) capturepb.GoalType {
	return (*vocabulary)(nil).goalType(goalType)
}

// MapHeader converts a v1 TelemetryHeader to a v2 CaptureHeader, discarding
// the record of any unrecognised engine string. Conversion uses mapHeader so
// the loss reaches ConvertResult.UnmappedValues.
func MapHeader(v1 *telemetryv1.TelemetryHeader) *capturepb.CaptureHeader {
	return mapHeader(v1, nil)
}

func mapHeader(v1 *telemetryv1.TelemetryHeader, vocab *vocabulary) *capturepb.CaptureHeader {
	if v1 == nil {
		return nil
	}

	header := &capturepb.CaptureHeader{
		CaptureId:     v1.GetCaptureId(),
		CreatedAt:     v1.GetCreatedAt(),
		FormatVersion: 2,
		// 2.2.0. THE FIRST MINOR THAT REMOVES FIELDS, which the schema's own
		// compatibility sentence does not permit — `capture.proto` promises that a
		// reader understanding MAJOR=2 reads any 2.x file and that unknown minors
		// add only OPTIONAL fields. This batch removed EchoArenaHeader.match_type,
		// .private_match and .tournament_match plus eight MatchType values, so that
		// promise is already false and is false whichever number is chosen here.
		// It was legitimate only because the format was unreleased: v4.1.0 is the
		// release, so there was no shipped 2.x reader to break. nevr-proto is
		// amending the sentence; this is the writer's half.
		//
		// WHY NOT LEAVE IT AT 1. 2.1.0 has been EMITTED — the rig captures on disk
		// carry format_minor: 1 and one was read on 2026-09-07. Unreleased is not
		// the same as nonexistent, and reusing the number would leave two
		// byte-incompatible schemas both stamped 2.1.0 with nothing able to tell
		// them apart. Detectable beats tidy.
		//
		// WHY NOT MAJOR 3. gh #46 is an open architecture decision titled "v2 as
		// archival bridge, v3 as engine-native", so 3 already has a planned
		// meaning; spending it here would pre-empt a decision nobody has made.
		//
		// THE COST OF THIS BUMP IS ZERO AND THAT IS THE ARGUMENT, not a caveat.
		// Nothing in this repository READS format_minor to change behaviour —
		// `grep -rn "GetFormatMinor" pkg cmd` finds one assertion in a test and
		// nothing else; the version gate is on format_version (MAJOR) at
		// tape.go:426-429. So the field's only function is telling artifacts apart
		// after the fact, and a marker left unchanged across a byte-incompatible
		// schema change is a marker doing nothing. The counter-argument — "a
		// version nobody consumed is noise" — assumes a cost this field does not
		// have.
		FormatMinor:   2,
		FormatPatch:   0,
		FrameEncoding: capturepb.FrameEncoding_FRAME_ENCODING_SPARSE,
		Metadata:      v1.GetMetadata(),
	}

	// If metadata contains session_id, populate EchoArenaHeader.
	if meta := v1.GetMetadata(); meta != nil {
		if sessionID, ok := meta["session_id"]; ok {
			eaHeader := &capturepb.EchoArenaHeader{
				SessionId: sessionID,
			}
			if mapName, ok := meta["map_name"]; ok {
				eaHeader.MapName = mapName
			}
			if matchType, ok := meta["match_type"]; ok {
				// Verbatim onto the header, not narrowed into an enum. The
				// engine's spelling is the value; DeriveGameType reads the axes
				// back out of it when a consumer wants them.
				header.GameType = matchType
			}
			if clientName, ok := meta["client_name"]; ok {
				eaHeader.ClientName = clientName
			}
			header.GameHeader = &capturepb.CaptureHeader_EchoArena{
				EchoArena: eaHeader,
			}
		}
	}

	return header
}

// MapHeaderFromSession creates a CaptureHeader from a v1 TelemetryHeader
// enriched with data from the first SessionResponse frame, discarding the
// record of any unrecognised engine string. Conversion uses
// mapHeaderFromSession so the loss reaches ConvertResult.UnmappedValues.
func MapHeaderFromSession(v1hdr *telemetryv1.TelemetryHeader, session *enginev1.SessionResponse) *capturepb.CaptureHeader {
	return mapHeaderFromSession(v1hdr, session, nil)
}

func mapHeaderFromSession(v1hdr *telemetryv1.TelemetryHeader, session *enginev1.SessionResponse, vocab *vocabulary) *capturepb.CaptureHeader {
	header := mapHeader(v1hdr, vocab)
	if header == nil {
		return nil
	}

	if session == nil {
		return header
	}

	// Build or enrich EchoArenaHeader from session data.
	eaHeader := header.GetEchoArena()
	if eaHeader == nil {
		eaHeader = &capturepb.EchoArenaHeader{}
		header.GameHeader = &capturepb.CaptureHeader_EchoArena{
			EchoArena: eaHeader,
		}
	}

	if eaHeader.SessionId == "" {
		eaHeader.SessionId = session.GetSessionId()
	}
	if eaHeader.MapName == "" {
		eaHeader.MapName = session.GetMapName()
	}
	if header.GetGameType() == "" {
		header.GameType = session.GetMatchType()
	}
	if eaHeader.ClientName == "" {
		eaHeader.ClientName = session.GetClientName()
	}
	// private_match and tournament_match are NOT stored. They were never
	// MatchType's to lose — the engine sends them as its own booleans and the old
	// code copied them straight through, independent of the enum — but they are
	// redundant with the symbol now that it is kept verbatim, and two fields that
	// can disagree with a third are a bug waiting to be written. DeriveGameType
	// answers both from game_type; see reconstructSession.
	eaHeader.TotalRoundCount = session.GetTotalRoundCount()
	if eaHeader.SessionIp == "" {
		eaHeader.SessionIp = session.GetSessionIp()
	}

	// Session constants measured invariant across every frame of 15 sampled
	// captures, so they are written once rather than per frame.
	if eaHeader.RulesChangedBy == "" {
		eaHeader.RulesChangedBy = session.GetRulesChangedBy()
	}
	if eaHeader.RulesChangedAt == 0 {
		eaHeader.RulesChangedAt = session.GetRulesChangedAt()
	}

	// Team display names, indexed by team. Not derivable from Role: private and
	// tournament matches use custom names. Constant within a capture (measured
	// 25/25), so they ride the header.
	if names := buildTeamNames(session.GetTeams()); len(names) > 0 {
		eaHeader.TeamNames = names
	}

	if roster := buildInitialRoster(session.GetTeams()); len(roster) > 0 {
		eaHeader.InitialRoster = roster
	}

	return header
}

// buildTeamNames collects team display names in team-array order, returning nil
// when every name is empty so an all-blank capture costs nothing on the wire.
func buildTeamNames(teams []*enginev1.Team) []string {
	names := make([]string, 0, len(teams))
	any := false
	for _, team := range teams {
		if team.GetTeamName() != "" {
			any = true
		}
		names = append(names, team.GetTeamName())
	}
	if !any {
		return nil
	}
	return names
}

// buildInitialRoster builds the v2 initial roster from the session's teams.
// Team order is preserved (team 0 = blue, team 1 = orange); any other team
// index, or a jersey number of -1, marks a spectator.
func buildInitialRoster(teams []*enginev1.Team) []*capturepb.PlayerInfo {
	var roster []*capturepb.PlayerInfo
	for teamIdx, team := range teams {
		for _, m := range team.GetPlayers() {
			roster = append(roster, &capturepb.PlayerInfo{
				Slot:          m.GetSlotNumber(),
				AccountNumber: m.GetAccountNumber(),
				DisplayName:   m.GetDisplayName(),
				Role:          rosterRole(teamIdx, m.GetJerseyNumber()),
				JerseyNumber:  m.GetJerseyNumber(),
				Level:         m.GetLevel(),
			})
		}
	}
	return roster
}

// rosterRole maps a team index and jersey number to a v2 Role using the shared
// events.ClassifyRole source of truth, so the InitialRoster role can never
// diverge from the PlayerJoined role resolved in pkg/events. A jersey number of
// -1, or any team index other than blue (0) or orange (1), marks a spectator.
func rosterRole(teamIdx int, jersey int32) capturepb.Role {
	switch events.ClassifyRole(teamIdx, jersey) {
	case events.RoleClassBlue:
		return capturepb.Role_ROLE_BLUE_TEAM
	case events.RoleClassOrange:
		return capturepb.Role_ROLE_ORANGE_TEAM
	default:
		return capturepb.Role_ROLE_SPECTATOR
	}
}

// FrameMapper holds state that persists across frames during conversion.
type FrameMapper struct {
	BaseTime    time.Time
	RoundNumber int32
	// Per-slot previous loadout/grab/stats/roster-info, for emitting change events.
	prevLoadout map[int32]loadoutState
	prevGrab    map[int32]grabState
	prevStats   map[int32]statCounters
	prevInfo    map[int32]rosterInfo
	// vocab accumulates engine strings no conversion table recognised
	// (VOCAB-001). Nil is valid and records nothing.
	vocab *vocabulary
}

// Unmapped returns the engine strings this mapper could not translate, ordered
// by field then value. Non-empty means the tape is not a complete record of its
// source: an unrecognised value converts to *_UNSPECIFIED and renders back as
// the empty string, so the loss is otherwise silent (VOCAB-001).
func (m *FrameMapper) Unmapped() []UnmappedValue {
	return m.vocab.sorted()
}

// rosterInfo holds the roster attributes that are session-constant by intent but
// are reported as 0 by the engine for the first frames after a join.
type rosterInfo struct{ jersey, level int32 }

type loadoutState struct{ weapon, ordnance, tacMod string }
type grabState struct{ left, right string }

// statCounters holds the eleven integer counters of the engine's per-player
// stats block. possession_time is excluded: it ticks continuously and rides
// PlayerState per-frame instead, so including it here would turn a rare event
// into a per-frame one.
type statCounters struct {
	points, saves, goals, stuns, passes, catches int32
	steals, blocks, interceptions, assists       int32
	shotsTaken                                   int32
}

func countersFrom(s *enginev1.PlayerStats) statCounters {
	return statCounters{
		points: s.GetPoints(), saves: s.GetSaves(), goals: s.GetGoals(),
		stuns: s.GetStuns(), passes: s.GetPasses(), catches: s.GetCatches(),
		steals: s.GetSteals(), blocks: s.GetBlocks(),
		interceptions: s.GetInterceptions(), assists: s.GetAssists(),
		shotsTaken: s.GetShotsTaken(),
	}
}

// Reset clears the mapper's accumulated state for a new conversion session.
func (m *FrameMapper) Reset() {
	m.RoundNumber = 0
	m.prevLoadout = nil
	m.prevGrab = nil
	m.prevStats = nil
	m.prevInfo = nil
	m.vocab = nil
}

// MapFrame converts a v1 LobbySessionStateFrame to a v2 Frame using the
// mapper's accumulated state. It updates RoundNumber when a RoundStarted
// event is present on the frame. It returns an error wrapping
// ErrTimestampOffsetOverflow when the frame's timestamp offset from BaseTime
// exceeds the v2 TimestampOffsetMs uint32 range, so the conversion pipeline can
// abort instead of writing a tape with silently wrapped offsets (R5).
func (m *FrameMapper) MapFrame(v1f *telemetryv1.LobbySessionStateFrame) (*capturepb.Frame, error) {
	// Scan events for RoundStarted to update round number.
	for _, evt := range v1f.GetEvents() {
		if rs, ok := evt.GetEvent().(*telemetryv1.LobbySessionEvent_RoundStarted); ok {
			m.RoundNumber = rs.RoundStarted.GetRoundNumber()
		}
	}

	frame, err := mapFrame(v1f, m.BaseTime, m.RoundNumber, m.vocab)
	if err != nil {
		return nil, fmt.Errorf("FrameMapper.MapFrame: %w", err)
	}
	m.appendLoadoutGrabEvents(v1f, frame)
	return frame, nil
}

// appendLoadoutGrabEvents detects per-player loadout (weapon/ordnance/tac-mod)
// and grab (left/right holding) changes since the previous frame and appends
// LoadoutChanged/GrabChanged events to the frame. Each slot is seeded on first
// sighting (so frame 0 carries the initial loadout/grab). Events are emitted in
// ascending slot order for deterministic output. Reconstruct the per-frame
// value by replaying these via the Session layer.
func (m *FrameMapper) appendLoadoutGrabEvents(v1f *telemetryv1.LobbySessionStateFrame, frame *capturepb.Frame) {
	ea := frame.GetEchoArena()
	if ea == nil {
		return
	}
	if m.prevLoadout == nil {
		m.prevLoadout = make(map[int32]loadoutState)
		m.prevGrab = make(map[int32]grabState)
		m.prevStats = make(map[int32]statCounters)
		m.prevInfo = make(map[int32]rosterInfo)
	}

	type entry struct {
		tm   *enginev1.TeamMember
		slot int32
	}
	var entries []entry
	for _, team := range v1f.GetSession().GetTeams() {
		for _, tm := range team.GetPlayers() {
			entries = append(entries, entry{tm: tm, slot: tm.GetSlotNumber()})
		}
	}
	slices.SortFunc(entries, func(a, b entry) int {
		switch {
		case a.slot < b.slot:
			return -1
		case a.slot > b.slot:
			return 1
		default:
			return 0
		}
	})

	for _, e := range entries {
		ld := loadoutState{e.tm.GetWeapon(), e.tm.GetOrdnance(), e.tm.GetTacMod()}
		if prev, ok := m.prevLoadout[e.slot]; !ok || prev != ld {
			ea.Events = append(ea.Events, &capturepb.EchoEvent{
				Event: &capturepb.EchoEvent_LoadoutChanged{
					LoadoutChanged: &capturepb.LoadoutChanged{
						PlayerSlot: e.slot,
						Weapon:     ld.weapon,
						Ordnance:   ld.ordnance,
						TacMod:     ld.tacMod,
					},
				},
			})
			m.prevLoadout[e.slot] = ld
		}

		// Roster attributes, corrected after the join snapshot. The engine
		// reports jersey/level as 0 for the first frames after a join, so the
		// value PlayerJoined captured is not necessarily final. Seeded on first
		// sighting (the join value is already on PlayerJoined, so the seed
		// emits nothing); afterwards any change becomes a delta. See ROSTER-001.
		ri := rosterInfo{jersey: e.tm.GetJerseyNumber(), level: e.tm.GetLevel()}
		if prev, ok := m.prevInfo[e.slot]; !ok {
			m.prevInfo[e.slot] = ri
		} else if prev != ri {
			ea.Events = append(ea.Events, &capturepb.EchoEvent{
				Event: &capturepb.EchoEvent_PlayerInfoUpdated{
					PlayerInfoUpdated: &capturepb.PlayerInfoUpdated{
						PlayerSlot:   e.slot,
						JerseyNumber: ri.jersey,
						Level:        ri.level,
					},
				},
			})
			m.prevInfo[e.slot] = ri
		}

		// The engine's own stat counters, seeded on first sighting so a capture
		// starting mid-match records its opening totals (they carry a
		// pre-capture baseline and cannot be rebuilt by counting events).
		if st := e.tm.GetStats(); st != nil {
			sc := countersFrom(st)
			if prev, ok := m.prevStats[e.slot]; !ok || prev != sc {
				ea.Events = append(ea.Events, &capturepb.EchoEvent{
					Event: &capturepb.EchoEvent_PlayerStatsUpdated{
						PlayerStatsUpdated: &capturepb.PlayerStatsUpdated{
							PlayerSlot:    e.slot,
							Points:        sc.points,
							Saves:         sc.saves,
							Goals:         sc.goals,
							Stuns:         sc.stuns,
							Passes:        sc.passes,
							Catches:       sc.catches,
							Steals:        sc.steals,
							Blocks:        sc.blocks,
							Interceptions: sc.interceptions,
							Assists:       sc.assists,
							ShotsTaken:    sc.shotsTaken,
						},
					},
				})
				m.prevStats[e.slot] = sc
			}
		}

		gr := grabState{e.tm.GetLeftHoldingOnto(), e.tm.GetRightHoldingOnto()}
		if prev, ok := m.prevGrab[e.slot]; !ok || prev != gr {
			ea.Events = append(ea.Events, &capturepb.EchoEvent{
				Event: &capturepb.EchoEvent_GrabChanged{
					GrabChanged: &capturepb.GrabChanged{
						PlayerSlot:   e.slot,
						LeftHolding:  gr.left,
						RightHolding: gr.right,
					},
				},
			})
			m.prevGrab[e.slot] = gr
		}
	}
}

// MapFrame converts a v1 LobbySessionStateFrame to a v2 Frame, discarding the
// record of any unrecognised engine string. baseTime is the capture creation
// time, used to compute timestamp_offset_ms. Conversion goes through
// FrameMapper.MapFrame so the loss reaches ConvertResult.UnmappedValues. It
// returns an error wrapping ErrTimestampOffsetOverflow when the frame's
// timestamp offset from baseTime exceeds the v2 TimestampOffsetMs uint32 range
// (R5).
func MapFrame(v1f *telemetryv1.LobbySessionStateFrame, baseTime time.Time) (*capturepb.Frame, error) {
	frame, err := mapFrame(v1f, baseTime, 0, nil)
	if err != nil {
		return nil, fmt.Errorf("MapFrame: %w", err)
	}
	return frame, nil
}

func mapFrame(v1f *telemetryv1.LobbySessionStateFrame, baseTime time.Time, roundNumber int32, vocab *vocabulary) (*capturepb.Frame, error) {
	if v1f == nil {
		return nil, nil
	}

	frameTime := v1f.GetTimestamp().AsTime()
	// Clamp to zero if frame predates baseTime — prevents uint32 wrap from
	// a negative Sub() result.
	diffMs := max(frameTime.Sub(baseTime).Milliseconds(), 0)
	// A positive offset past the uint32 field's range cannot be represented.
	// Erroring rather than wrapping makes the loss loud (R5); the frame is not
	// mapped because any offset we wrote would be silently wrong.
	if diffMs > math.MaxUint32 {
		return nil, fmt.Errorf("mapFrame: frame %d timestamp offset %d ms exceeds uint32 max (~49.7 days): %w",
			v1f.GetFrameIndex(), diffMs, ErrTimestampOffsetOverflow)
	}
	offsetMs := uint32(diffMs)

	frame := &capturepb.Frame{
		FrameIndex:        v1f.GetFrameIndex(),
		TimestampOffsetMs: offsetMs,
	}

	session := v1f.GetSession()
	if session == nil {
		return frame, nil
	}

	ea := &capturepb.EchoArenaFrame{
		GameStatus:   vocab.gameStatus(session.GetGameStatus()),
		GameClock:    float32(session.GetGameClock()),
		BluePoints:   session.GetBluePoints(),
		OrangePoints: session.GetOrangePoints(),
	}

	// Map pause state. The sub-fields ride a per-frame message that stays nil
	// unless a pause is actually in progress.
	if pause := session.GetPause(); pause != nil {
		ea.PauseState = vocab.pauseState(pause.GetPausedState())
		if detail := mapPauseDetail(pause); detail != nil {
			ea.PauseDetail = detail
		}
	}

	// Transient request flags. Measured 0 on every sampled frame; proto3 writes
	// nothing for a zero value, so carrying them per-frame is free and stays
	// correct if they ever toggle.
	ea.ErrCode = session.GetErrCode()
	ea.BlueTeamRestartRequest = session.GetBlueTeamRestartRequest()
	ea.OrangeTeamRestartRequest = session.GetOrangeTeamRestartRequest()

	// Map disc.
	if disc := session.GetDisc(); disc != nil {
		ea.Disc = mapDisc(disc)
	}

	// Map players from teams.
	ea.Players = mapPlayers(session.GetTeams())

	// Find disc holder. First possessor wins, matching findPossessorSlot
	// (pkg/events/sensor_disc.go) so the frame's disc_holder_slot never
	// disagrees with the DiscPossessionChanged events for the same frame.
possessor:
	for _, team := range session.GetTeams() {
		for _, p := range team.GetPlayers() {
			if p.GetHasPossession() {
				slot := p.GetSlotNumber()
				ea.DiscHolderSlot = &slot
				break possessor
			}
		}
	}

	// Map VR root.
	if playerRoot := session.GetPlayer(); playerRoot != nil {
		ea.VrRoot = mapPlayerRoot(playerRoot)
	}

	// Map player bones.
	if bones := v1f.GetPlayerBones(); bones != nil {
		ea.PlayerBones = mapPlayerBones(bones)
	}

	// Map events from v1.
	ea.Events = mapEvents(v1f.GetEvents(), vocab)

	// Set round number from conversion state.
	ea.RoundNumber = roundNumber

	// Capture-client analog shoulder input (session-level, like vr_root; 0 when
	// not pressed, so free on the wire).
	ea.LeftShoulderPressed = float32(session.GetLeftShoulderPressed())
	ea.RightShoulderPressed = float32(session.GetRightShoulderPressed())
	ea.LeftShoulderPressed_2 = float32(session.GetLeftShoulderPressed2())
	ea.RightShoulderPressed_2 = float32(session.GetRightShoulderPressed2())

	// Echo Combat payload state — present only in payload matches; nil in arena.
	if session.GetPayloadMultiplier() != 0 || session.GetPayloadCheckpoint() != 0 ||
		session.GetPayloadDistance() != 0 || session.GetPayloadDefenders() != 0 ||
		session.GetPayloadSpeed() != 0 {
		ea.Payload = &capturepb.PayloadState{
			Multiplier: float32(session.GetPayloadMultiplier()),
			Checkpoint: session.GetPayloadCheckpoint(),
			Distance:   float32(session.GetPayloadDistance()),
			Defenders:  session.GetPayloadDefenders(),
			Speed:      float32(session.GetPayloadSpeed()),
		}
	}

	frame.Payload = &capturepb.Frame_EchoArena{EchoArena: ea}
	return frame, nil
}

// mapPauseDetail carries the PauseState sub-fields the per-frame enum cannot
// express. Returns nil when all four are at their zero value, which is every
// frame outside an active pause.
func mapPauseDetail(pause *enginev1.PauseState) *capturepb.PauseDetail {
	unpausedTeam := teamStringToRole(pause.GetUnpausedTeam())
	requestedTeam := teamStringToRole(pause.GetPausedRequestedTeam())
	unpausedTimer := float32(pause.GetUnpausedTimer())
	pausedTimer := float32(pause.GetPausedTimer())

	if unpausedTeam == capturepb.Role_ROLE_UNSPECIFIED &&
		requestedTeam == capturepb.Role_ROLE_UNSPECIFIED &&
		unpausedTimer == 0 && pausedTimer == 0 {
		return nil
	}

	return &capturepb.PauseDetail{
		UnpausedTeam:        unpausedTeam,
		PausedRequestedTeam: requestedTeam,
		UnpausedTimer:       unpausedTimer,
		PausedTimer:         pausedTimer,
	}
}

func mapDisc(disc *enginev1.Disc) *capturepb.DiscState {
	ds := &capturepb.DiscState{
		BounceCount: uint32(disc.GetBounceCount()), //nolint:gosec // bounce count is non-negative
	}

	pos := disc.GetPosition()
	fwd := disc.GetForward()
	left := disc.GetLeft()
	up := disc.GetUp()
	if len(pos) >= 3 {
		ds.Pose = poseFromVectors(pos, fwd, left, up)
	}

	vel := disc.GetVelocity()
	if len(vel) >= 3 {
		ds.Velocity = &spatialv1.Vec3{
			X: float32(vel[0]),
			Y: float32(vel[1]),
			Z: float32(vel[2]),
		}
	}

	return ds
}

// poseFromVectors builds a Pose from position and orientation vectors.
// Orientation is converted from forward/left/up to quaternion.
func poseFromVectors(pos, fwd, left, up []float64) *spatialv1.Pose {
	pose := &spatialv1.Pose{
		Position: &spatialv1.Vec3{
			X: float32(pos[0]),
			Y: float32(pos[1]),
			Z: float32(pos[2]),
		},
	}

	if len(fwd) >= 3 && len(left) >= 3 && len(up) >= 3 {
		pose.Orientation = quaternionFromAxes(fwd, left, up)
	}

	return pose
}

// quaternionFromAxes converts forward/left/up basis vectors to a quaternion.
// Uses the standard rotation matrix to quaternion conversion.
func quaternionFromAxes(fwd, left, up []float64) *spatialv1.Quat {
	// The rotation matrix columns are: left, up, forward (right-handed).
	// m00=left[0]  m01=up[0]  m02=fwd[0]
	// m10=left[1]  m11=up[1]  m12=fwd[1]
	// m20=left[2]  m21=up[2]  m22=fwd[2]
	m00, m01, m02 := left[0], up[0], fwd[0]
	m10, m11, m12 := left[1], up[1], fwd[1]
	m20, m21, m22 := left[2], up[2], fwd[2]

	trace := m00 + m11 + m22
	var qw, qx, qy, qz float64

	if trace > 0 {
		s := math.Sqrt(trace+1.0) * 2 // s=4*qw
		qw = 0.25 * s
		qx = (m21 - m12) / s
		qy = (m02 - m20) / s
		qz = (m10 - m01) / s
	} else if m00 > m11 && m00 > m22 {
		s := math.Sqrt(1.0+m00-m11-m22) * 2 // s=4*qx
		qw = (m21 - m12) / s
		qx = 0.25 * s
		qy = (m01 + m10) / s
		qz = (m02 + m20) / s
	} else if m11 > m22 {
		s := math.Sqrt(1.0+m11-m00-m22) * 2 // s=4*qy
		qw = (m02 - m20) / s
		qx = (m01 + m10) / s
		qy = 0.25 * s
		qz = (m12 + m21) / s
	} else {
		s := math.Sqrt(1.0+m22-m00-m11) * 2 // s=4*qz
		qw = (m10 - m01) / s
		qx = (m02 + m20) / s
		qy = (m12 + m21) / s
		qz = 0.25 * s
	}

	return &spatialv1.Quat{
		X: float32(qx),
		Y: float32(qy),
		Z: float32(qz),
		W: float32(qw),
	}
}

func mapPlayers(teams []*enginev1.Team) []*capturepb.PlayerState {
	var players []*capturepb.PlayerState
	for teamIdx, team := range teams {
		for _, member := range team.GetPlayers() {
			ps := &capturepb.PlayerState{
				Slot: member.GetSlotNumber(),
				Ping: uint32(member.GetPing()), //nolint:gosec // ping is non-negative
			}
			ps.PacketLossRatio = float32(member.GetPacketLossRatio())
			// Ticks continuously while a player holds the disc (29.4 changes
			// per 100 frames measured), so it is per-frame rather than an
			// event. The other eleven stats fields ride PlayerStatsUpdated.
			ps.PossessionTime = float32(member.GetStats().GetPossessionTime())

			// Map head pose.
			if head := member.GetHead(); head != nil {
				ps.Head = bodyPartToPose(head)
			}

			// Map body pose.
			if body := member.GetBody(); body != nil {
				ps.Body = bodyPartToPose(body)
			}

			// Map hand poses.
			if lh := member.GetLeftHand(); lh != nil {
				ps.LeftHand = handPartToPose(lh)
			}
			if rh := member.GetRightHand(); rh != nil {
				ps.RightHand = handPartToPose(rh)
			}

			// Map velocity.
			vel := member.GetVelocity()
			if len(vel) >= 3 {
				ps.Velocity = &spatialv1.Vec3{
					X: float32(vel[0]),
					Y: float32(vel[1]),
					Z: float32(vel[2]),
				}
			}

			// Pack boolean flags.
			var flags uint32
			if member.GetIsStunned() {
				flags |= 1 << 0
			}
			if member.GetIsInvulnerable() {
				flags |= 1 << 1
			}
			if member.GetIsBlocking() {
				flags |= 1 << 2
			}
			if member.GetHasPossession() {
				flags |= 1 << 3
			}
			if member.GetIsEmotePlaying() {
				flags |= 1 << 4
			}
			// Encode team index in bits 5-6 (0=blue, 1=orange, 2=spectator).
			flags |= uint32(teamIdx&0x3) << 5
			ps.Flags = flags

			players = append(players, ps)
		}
	}
	return players
}

func bodyPartToPose(bp *enginev1.BodyPart) *spatialv1.Pose {
	pos := bp.GetPosition()
	fwd := bp.GetForward()
	left := bp.GetLeft()
	up := bp.GetUp()

	if len(pos) < 3 {
		return nil
	}

	return poseFromVectors(pos, fwd, left, up)
}

func handPartToPose(hp *enginev1.HandPart) *spatialv1.Pose {
	pos := hp.GetPos()
	fwd := hp.GetForward()
	left := hp.GetLeft()
	up := hp.GetUp()

	if len(pos) < 3 {
		return nil
	}

	return poseFromVectors(pos, fwd, left, up)
}

func mapPlayerRoot(pr *enginev1.PlayerRoot) *spatialv1.Pose {
	pos := pr.GetVrPosition()
	fwd := pr.GetVrForward()
	left := pr.GetVrLeft()
	up := pr.GetVrUp()

	if len(pos) < 3 {
		return nil
	}

	return poseFromVectors(pos, fwd, left, up)
}

func mapPlayerBones(bones *enginev1.PlayerBonesResponse) []*capturepb.PlayerBones {
	userBones := bones.GetUserBones()
	if len(userBones) == 0 {
		return nil
	}

	result := make([]*capturepb.PlayerBones, 0, len(userBones))
	for _, ub := range userBones {
		pb := &capturepb.PlayerBones{
			Slot: ub.GetPlayerIndex(),
		}

		// Convert float32 slices to packed little-endian bytes.
		if transforms := ub.GetBoneT(); len(transforms) > 0 {
			pb.Transforms = float32SliceToBytes(transforms)
		}
		if orientations := ub.GetBoneO(); len(orientations) > 0 {
			pb.Orientations = float32SliceToBytes(orientations)
		}

		result = append(result, pb)
	}
	return result
}

// float32SliceToBytes converts a []float32 to packed little-endian bytes.
func float32SliceToBytes(fs []float32) []byte {
	buf := make([]byte, len(fs)*4)
	for i, f := range fs {
		binary.LittleEndian.PutUint32(buf[i*4:], math.Float32bits(f))
	}
	return buf
}

// mapEvents converts v1 LobbySessionEvent list to v2 EchoEvent list.
func mapEvents(v1Events []*telemetryv1.LobbySessionEvent, vocab *vocabulary) []*capturepb.EchoEvent {
	if len(v1Events) == 0 {
		return nil
	}

	result := make([]*capturepb.EchoEvent, 0, len(v1Events))
	for _, v1e := range v1Events {
		if v2e := mapEvent(v1e, vocab); v2e != nil {
			result = append(result, v2e)
		}
	}
	return result
}

// mapEvent converts a single v1 LobbySessionEvent to a v2 EchoEvent.
// Both packages define the same event sub-message types with the same fields,
// so we can copy field-by-field.
func mapEvent(v1e *telemetryv1.LobbySessionEvent, vocab *vocabulary) *capturepb.EchoEvent {
	if v1e == nil {
		return nil
	}

	evt := &capturepb.EchoEvent{}

	switch e := v1e.GetEvent().(type) {
	case *telemetryv1.LobbySessionEvent_RoundStarted:
		evt.Event = &capturepb.EchoEvent_RoundStarted{
			RoundStarted: &capturepb.RoundStarted{
				RoundNumber: e.RoundStarted.GetRoundNumber(),
			},
		}
	case *telemetryv1.LobbySessionEvent_RoundPaused:
		rp := &capturepb.RoundPaused{}
		if ps := e.RoundPaused.GetPauseState(); ps != nil {
			rp.PauseState = vocab.pauseState(ps.GetPausedState())
			rp.RequestingTeam = teamStringToRole(ps.GetPausedRequestedTeam())
			rp.PauseTimer = float32(ps.GetPausedTimer())
		}
		evt.Event = &capturepb.EchoEvent_RoundPaused{RoundPaused: rp}
	case *telemetryv1.LobbySessionEvent_RoundUnpaused:
		ru := &capturepb.RoundUnpaused{}
		if ps := e.RoundUnpaused.GetPauseState(); ps != nil {
			ru.PauseState = vocab.pauseState(ps.GetPausedState())
		}
		evt.Event = &capturepb.EchoEvent_RoundUnpaused{RoundUnpaused: ru}
	case *telemetryv1.LobbySessionEvent_RoundEnded:
		evt.Event = &capturepb.EchoEvent_RoundEnded{
			RoundEnded: &capturepb.RoundEnded{
				RoundNumber: e.RoundEnded.GetRoundNumber(),
				WinningTeam: capturepb.Role(e.RoundEnded.GetWinningTeam()),
			},
		}
	case *telemetryv1.LobbySessionEvent_MatchEnded:
		evt.Event = &capturepb.EchoEvent_MatchEnded{
			MatchEnded: &capturepb.MatchEnded{
				WinningTeam: capturepb.Role(e.MatchEnded.GetWinningTeam()),
			},
		}
	case *telemetryv1.LobbySessionEvent_ScoreboardUpdated:
		evt.Event = &capturepb.EchoEvent_ScoreboardUpdated{
			ScoreboardUpdated: &capturepb.ScoreboardUpdated{
				BluePoints:       e.ScoreboardUpdated.GetBluePoints(),
				OrangePoints:     e.ScoreboardUpdated.GetOrangePoints(),
				BlueRoundScore:   e.ScoreboardUpdated.GetBlueRoundScore(),
				OrangeRoundScore: e.ScoreboardUpdated.GetOrangeRoundScore(),
				GameClockDisplay: e.ScoreboardUpdated.GetGameClockDisplay(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerJoined:
		pj := &capturepb.PlayerJoined{
			Role: capturepb.Role(e.PlayerJoined.GetRole()),
		}
		if tm := e.PlayerJoined.GetPlayer(); tm != nil {
			pj.Slot = tm.GetSlotNumber()
			pj.AccountNumber = uint64(tm.GetAccountNumber())
			pj.DisplayName = tm.GetDisplayName()
			pj.JerseyNumber = tm.GetJerseyNumber()
			pj.Level = tm.GetLevel()
		}
		evt.Event = &capturepb.EchoEvent_PlayerJoined{
			PlayerJoined: pj,
		}
	case *telemetryv1.LobbySessionEvent_PlayerLeft:
		evt.Event = &capturepb.EchoEvent_PlayerLeft{
			PlayerLeft: &capturepb.PlayerLeft{
				PlayerSlot:  e.PlayerLeft.GetPlayerSlot(),
				DisplayName: e.PlayerLeft.GetDisplayName(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerSwitchedTeam:
		evt.Event = &capturepb.EchoEvent_PlayerSwitchedTeam{
			PlayerSwitchedTeam: &capturepb.PlayerSwitchedTeam{
				PlayerSlot: e.PlayerSwitchedTeam.GetPlayerSlot(),
				NewRole:    capturepb.Role(e.PlayerSwitchedTeam.GetNewRole()),
				PrevRole:   capturepb.Role(e.PlayerSwitchedTeam.GetPrevRole()),
			},
		}
	case *telemetryv1.LobbySessionEvent_EmotePlayed:
		evt.Event = &capturepb.EchoEvent_EmotePlayed{
			EmotePlayed: &capturepb.EmotePlayed{
				PlayerSlot: e.EmotePlayed.GetPlayerSlot(),
				Emote:      capturepb.EmotePlayed_EmoteType(e.EmotePlayed.GetEmote()),
			},
		}
	case *telemetryv1.LobbySessionEvent_DiscPossessionChanged:
		playerSlot := e.DiscPossessionChanged.GetPlayerSlot()
		prevSlot := e.DiscPossessionChanged.GetPreviousPlayerSlot()
		dc := &capturepb.DiscPossessionChanged{}
		if playerSlot != -1 {
			dc.PlayerSlot = &playerSlot
		}
		if prevSlot != -1 {
			dc.PreviousPlayerSlot = &prevSlot
		}
		evt.Event = &capturepb.EchoEvent_DiscPossessionChanged{
			DiscPossessionChanged: dc,
		}
	case *telemetryv1.LobbySessionEvent_DiscThrown:
		dt := &capturepb.DiscThrown{
			PlayerSlot: e.DiscThrown.GetPlayerSlot(),
		}
		if td := e.DiscThrown.GetThrowDetails(); td != nil {
			dt.ThrowDetails = &capturepb.ThrowDetails{
				ArmSpeed:                float32(td.GetArmSpeed()),
				TotalSpeed:              float32(td.GetTotalSpeed()),
				OffAxisSpinDeg:          float32(td.GetOffAxisSpinDeg()),
				WristThrowPenalty:       float32(td.GetWristThrowPenalty()),
				RotPerSec:               float32(td.GetRotPerSec()),
				PotSpeedFromRot:         float32(td.GetPotSpeedFromRot()),
				SpeedFromArm:            float32(td.GetSpeedFromArm()),
				SpeedFromMovement:       float32(td.GetSpeedFromMovement()),
				SpeedFromWrist:          float32(td.GetSpeedFromWrist()),
				WristAlignToThrowDeg:    float32(td.GetWristAlignToThrowDeg()),
				ThrowAlignToMovementDeg: float32(td.GetThrowAlignToMovementDeg()),
				OffAxisPenalty:          float32(td.GetOffAxisPenalty()),
				ThrowMovePenalty:        float32(td.GetThrowMovePenalty()),
			}
		}
		evt.Event = &capturepb.EchoEvent_DiscThrown{
			DiscThrown: dt,
		}
	case *telemetryv1.LobbySessionEvent_DiscCaught:
		evt.Event = &capturepb.EchoEvent_DiscCaught{
			DiscCaught: &capturepb.DiscCaught{
				PlayerSlot: e.DiscCaught.GetPlayerSlot(),
			},
		}
	case *telemetryv1.LobbySessionEvent_GoalScored:
		gs := &capturepb.GoalScored{}
		if sd := e.GoalScored.GetScoreDetails(); sd != nil {
			gs.DiscSpeed = float32(sd.GetDiscSpeed())
			gs.Team = teamStringToRole(sd.GetTeam())
			gs.GoalType = vocab.goalType(sd.GetGoalType())
			gs.PointAmount = sd.GetPointAmount()
			gs.DistanceThrown = float32(sd.GetDistanceThrown())
			gs.PersonScored = sd.GetPersonScored()
			gs.AssistScored = sd.GetAssistScored()
			// ScorerSlot / AssistSlot are slot indices in v2 and cannot be
			// resolved from v1 display names alone, so they remain zero; the
			// real scorer slot rides the parallel PlayerGoal event, and the
			// display names now survive on PersonScored / AssistScored.
		}
		evt.Event = &capturepb.EchoEvent_GoalScored{
			GoalScored: gs,
		}
	case *telemetryv1.LobbySessionEvent_PlayerGoal:
		evt.Event = &capturepb.EchoEvent_PlayerGoal{
			PlayerGoal: &capturepb.PlayerGoal{
				PlayerSlot: e.PlayerGoal.GetPlayerSlot(),
				TotalGoals: e.PlayerGoal.GetTotalGoals(),
				Points:     e.PlayerGoal.GetPoints(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerSave:
		evt.Event = &capturepb.EchoEvent_PlayerSave{
			PlayerSave: &capturepb.PlayerSave{
				PlayerSlot: e.PlayerSave.GetPlayerSlot(),
				TotalSaves: e.PlayerSave.GetTotalSaves(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerStun:
		evt.Event = &capturepb.EchoEvent_PlayerStun{
			PlayerStun: &capturepb.PlayerStun{
				PlayerSlot: e.PlayerStun.GetPlayerSlot(),
				TotalStuns: e.PlayerStun.GetTotalStuns(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerPass:
		evt.Event = &capturepb.EchoEvent_PlayerPass{
			PlayerPass: &capturepb.PlayerPass{
				PlayerSlot:  e.PlayerPass.GetPlayerSlot(),
				TotalPasses: e.PlayerPass.GetTotalPasses(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerSteal:
		victimSlot := e.PlayerSteal.GetVictimPlayerSlot()
		ps := &capturepb.PlayerSteal{
			PlayerSlot:  e.PlayerSteal.GetPlayerSlot(),
			TotalSteals: e.PlayerSteal.GetTotalSteals(),
		}
		if victimSlot != -1 {
			ps.VictimPlayerSlot = &victimSlot
		}
		evt.Event = &capturepb.EchoEvent_PlayerSteal{
			PlayerSteal: ps,
		}
	case *telemetryv1.LobbySessionEvent_PlayerBlock:
		evt.Event = &capturepb.EchoEvent_PlayerBlock{
			PlayerBlock: &capturepb.PlayerBlock{
				PlayerSlot:  e.PlayerBlock.GetPlayerSlot(),
				TotalBlocks: e.PlayerBlock.GetTotalBlocks(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerInterception:
		evt.Event = &capturepb.EchoEvent_PlayerInterception{
			PlayerInterception: &capturepb.PlayerInterception{
				PlayerSlot:         e.PlayerInterception.GetPlayerSlot(),
				TotalInterceptions: e.PlayerInterception.GetTotalInterceptions(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerAssist:
		evt.Event = &capturepb.EchoEvent_PlayerAssist{
			PlayerAssist: &capturepb.PlayerAssist{
				PlayerSlot:   e.PlayerAssist.GetPlayerSlot(),
				TotalAssists: e.PlayerAssist.GetTotalAssists(),
			},
		}
	case *telemetryv1.LobbySessionEvent_PlayerShotTaken:
		evt.Event = &capturepb.EchoEvent_PlayerShotTaken{
			PlayerShotTaken: &capturepb.PlayerShotTaken{
				PlayerSlot: e.PlayerShotTaken.GetPlayerSlot(),
				TotalShots: e.PlayerShotTaken.GetTotalShots(),
			},
		}
	case *telemetryv1.LobbySessionEvent_GenericEvent:
		evt.Event = &capturepb.EchoEvent_GenericEvent{
			GenericEvent: &capturepb.GenericEvent{
				EventType: e.GenericEvent.GetEventType(),
				Data:      e.GenericEvent.GetData(),
				Payload:   e.GenericEvent.GetPayload(),
			},
		}
	default:
		return nil
	}

	return evt
}
