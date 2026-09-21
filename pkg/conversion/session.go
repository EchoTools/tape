package conversion

import (
	"fmt"
	"io"
	"maps"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
)

// Loadout is a player's combat loadout (weapon/ordnance/tactical-mod) at a
// frame, reconstructed by replaying LoadoutChanged events.
type Loadout struct {
	Weapon   string
	Ordnance string
	TacMod   string
}

// Grab is what a player is holding in each hand at a frame, reconstructed by
// replaying GrabChanged events.
type Grab struct {
	Left  string
	Right string
}

// Score is the scoreboard snapshot at a frame, reconstructed by replaying
// ScoreboardUpdated events. Points are also carried per-frame on EchoArenaFrame;
// round scores and the display clock survive only as event samples (the score
// sensor seeds its baseline at frame 0 without emitting, so any value present
// before the first change is not represented here).
type Score struct {
	GameClockDisplay string
	BluePoints       int32
	OrangePoints     int32
	BlueRoundScore   int32
	OrangeRoundScore int32
}

// Session is a replayed view over a v2 Echo Arena capture. It replays the
// header roster and every frame's events once at construction, then answers
// per-frame derived state — roster, loadout, grab, score — in O(1) by frame
// ordinal. It is the reader-side counterpart of the event-emitting conversion
// path (GH #34): per-frame PlayerState is anonymized (slot only), so identity
// and other rarely-changing state are recovered here from events + the header.
type Session struct {
	header  *capturepb.EchoArenaHeader
	frames  []*capturepb.Frame
	roster  []map[int32]*capturepb.PlayerInfo
	loadout []map[int32]Loadout
	grab    []map[int32]Grab
	score   []Score
	stats   []map[int32]*capturepb.PlayerStatsUpdated
	// lastGoal is the most recent GoalScored as of each frame. The engine's
	// last_score field is a carried-forward snapshot, so replaying the event
	// forward reproduces it on every frame.
	lastGoal []*capturepb.GoalScored
	// settings is the latest MatchSettingsChanged as of each frame, nil before
	// the first (the header's capture-start values apply).
	settings []*capturepb.MatchSettingsChanged
}

// MatchSettings are the session settings the header records at capture start
// and MatchSettingsChanged updates.
type MatchSettings struct {
	RulesChangedBy  string
	RulesChangedAt  uint64
	TotalRoundCount int32
}

// NewSession builds a Session from a v2 Echo Arena header and its frames, in
// order. The header may be nil (an empty initial roster is assumed). Frames are
// indexed by ordinal position; the per-frame state reflects all events up to and
// including that frame.
func NewSession(header *capturepb.EchoArenaHeader, frames []*capturepb.Frame) *Session {
	s := &Session{
		header:   header,
		frames:   frames,
		roster:   make([]map[int32]*capturepb.PlayerInfo, len(frames)),
		loadout:  make([]map[int32]Loadout, len(frames)),
		grab:     make([]map[int32]Grab, len(frames)),
		score:    make([]Score, len(frames)),
		stats:    make([]map[int32]*capturepb.PlayerStatsUpdated, len(frames)),
		lastGoal: make([]*capturepb.GoalScored, len(frames)),
		settings: make([]*capturepb.MatchSettingsChanged, len(frames)),
	}
	s.replay()
	return s
}

// OpenSession reads a full v2 capture from r and builds a Session. The reader is
// consumed (header, then every frame until io.EOF); callers retain ownership of
// closing it.
func OpenSession(r *codec.Reader) (*Session, error) {
	hdr, err := r.ReadHeader()
	if err != nil {
		return nil, fmt.Errorf("conversion.OpenSession: read header: %w", err)
	}

	var frames []*capturepb.Frame
	for {
		frame, err := r.ReadFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("conversion.OpenSession: read frame %d: %w", len(frames), err)
		}
		frames = append(frames, frame)

		// SEC-001 accumulation guard (belt to the reader's braces): never
		// grow the slice past the reader's frame budget. Checked after the
		// append with > rather than >=, matching Limits.checkFrameBudget: a
		// capture holding exactly MaxFrameCount frames is AT budget, not over
		// it, and must load. Guarding before the read rejected it, because the
		// iteration that would have returned EOF tripped the guard first.
		// Raising/disabling the budget on the reader (codec.WithMaxFrameCount,
		// codec.WithoutLimits) flows through here.
		if max := r.Limits().MaxFrameCount; max > 0 && int64(len(frames)) > max {
			return nil, fmt.Errorf("conversion.OpenSession: %d frames accumulated: %w (budget %d)", len(frames), codec.ErrMaxFrameCount, max)
		}
	}

	return NewSession(hdr.GetEchoArena(), frames), nil
}

// FrameCount returns the number of frames in the session.
func (s *Session) FrameCount() int { return len(s.frames) }

// RosterAt returns slot->identity for the given frame ordinal, or nil when the
// ordinal is out of range. The returned map is shared across frames and must be
// treated as read-only.
func (s *Session) RosterAt(frame int) map[int32]*capturepb.PlayerInfo {
	if frame < 0 || frame >= len(s.roster) {
		return nil
	}
	return s.roster[frame]
}

// LoadoutAt returns slot->Loadout for the given frame ordinal, or nil when out
// of range. The returned map is shared and must be treated as read-only.
func (s *Session) LoadoutAt(frame int) map[int32]Loadout {
	if frame < 0 || frame >= len(s.loadout) {
		return nil
	}
	return s.loadout[frame]
}

// GrabAt returns slot->Grab for the given frame ordinal, or nil when out of
// range. The returned map is shared and must be treated as read-only.
func (s *Session) GrabAt(frame int) map[int32]Grab {
	if frame < 0 || frame >= len(s.grab) {
		return nil
	}
	return s.grab[frame]
}

// StatsAt returns slot -> the engine's per-player stat counters as of frame,
// replayed from PlayerStatsUpdated events. The returned map must not be
// mutated; it is shared across frames where nothing changed.
//
// These are the engine's own counters, which carry a pre-capture baseline and
// are not the same quantity as tape's PlayerGoal/PlayerSave/... sensor events —
// see STATS-001. possession_time is not here: it is per-frame on
// PlayerState.
func (s *Session) StatsAt(frame int) map[int32]*capturepb.PlayerStatsUpdated {
	if frame < 0 || frame >= len(s.stats) {
		return nil
	}
	return s.stats[frame]
}

// LastGoalAt returns the most recent GoalScored as of frame, or nil when no
// goal has been recorded yet. The engine carries last_score forward on every
// frame, so replaying the event reproduces it.
func (s *Session) LastGoalAt(frame int) *capturepb.GoalScored {
	if frame < 0 || frame >= len(s.lastGoal) {
		return nil
	}
	return s.lastGoal[frame]
}

// ScoreAt returns the scoreboard snapshot for the given frame ordinal, or the
// zero Score when out of range.
func (s *Session) ScoreAt(frame int) Score {
	if frame < 0 || frame >= len(s.score) {
		return Score{}
	}
	return s.score[frame]
}

// MatchSettingsAt returns the match settings in effect at the given frame
// ordinal: the latest MatchSettingsChanged at or before it, else the header's
// capture-start values. An out-of-range ordinal (e.g. -1) returns the header's.
func (s *Session) MatchSettingsAt(frame int) MatchSettings {
	if frame >= 0 && frame < len(s.settings) {
		if e := s.settings[frame]; e != nil {
			return MatchSettings{e.GetRulesChangedBy(), e.GetRulesChangedAt(), e.GetTotalRoundCount()}
		}
	}
	return MatchSettings{s.header.GetRulesChangedBy(), s.header.GetRulesChangedAt(), s.header.GetTotalRoundCount()}
}

// replay walks the frames once, applying each frame's events on top of the
// running state and recording the post-event state per frame. State maps are
// shared between frames and cloned only when a frame actually mutates them
// (clone-on-write), so memory is O(frames) pointers + O(changes) maps.
func (s *Session) replay() {
	roster := map[int32]*capturepb.PlayerInfo{}
	for _, pi := range s.header.GetInitialRoster() {
		roster[pi.GetSlot()] = pi
	}
	loadout := map[int32]Loadout{}
	grab := map[int32]Grab{}
	stats := map[int32]*capturepb.PlayerStatsUpdated{}
	var score Score
	var lastGoal *capturepb.GoalScored
	var settings *capturepb.MatchSettingsChanged

	for i, f := range s.frames {
		rosterDirty, loadoutDirty, grabDirty, statsDirty := false, false, false, false

		for _, ev := range f.GetEchoArena().GetEvents() {
			switch e := ev.GetEvent().(type) {
			case *capturepb.EchoEvent_PlayerJoined:
				if !rosterDirty {
					roster = maps.Clone(roster)
					rosterDirty = true
				}
				pj := e.PlayerJoined
				roster[pj.GetSlot()] = &capturepb.PlayerInfo{
					Slot:          pj.GetSlot(),
					AccountNumber: pj.GetAccountNumber(),
					DisplayName:   pj.GetDisplayName(),
					Role:          pj.GetRole(),
					JerseyNumber:  pj.GetJerseyNumber(),
					Level:         pj.GetLevel(),
				}
			case *capturepb.EchoEvent_PlayerLeft:
				// Only identity is cleared on leave, mirroring the join/leave
				// sensors. Loadout/grab baselines are intentionally NOT cleared:
				// the forward mapper (mapping.go appendLoadoutGrabEvents) keeps
				// prevLoadout/prevGrab across leaves, so a reused slot whose value
				// equals the departed player's emits no change event. Clearing
				// here would drop that carried-forward value (e.g. grab "none").
				if !rosterDirty {
					roster = maps.Clone(roster)
					rosterDirty = true
				}
				delete(roster, e.PlayerLeft.GetPlayerSlot())
			case *capturepb.EchoEvent_PlayerSwitchedTeam:
				st := e.PlayerSwitchedTeam
				if cur, ok := roster[st.GetPlayerSlot()]; ok && cur.GetRole() != st.GetNewRole() {
					if !rosterDirty {
						roster = maps.Clone(roster)
						rosterDirty = true
					}
					updated := &capturepb.PlayerInfo{
						Slot:          cur.GetSlot(),
						AccountNumber: cur.GetAccountNumber(),
						DisplayName:   cur.GetDisplayName(),
						Role:          st.GetNewRole(),
						JerseyNumber:  cur.GetJerseyNumber(),
						Level:         cur.GetLevel(),
					}
					roster[st.GetPlayerSlot()] = updated
				}
			case *capturepb.EchoEvent_LoadoutChanged:
				lc := e.LoadoutChanged
				if !loadoutDirty {
					loadout = maps.Clone(loadout)
					loadoutDirty = true
				}
				loadout[lc.GetPlayerSlot()] = Loadout{
					Weapon:   lc.GetWeapon(),
					Ordnance: lc.GetOrdnance(),
					TacMod:   lc.GetTacMod(),
				}
			case *capturepb.EchoEvent_GrabChanged:
				gc := e.GrabChanged
				if !grabDirty {
					grab = maps.Clone(grab)
					grabDirty = true
				}
				grab[gc.GetPlayerSlot()] = Grab{
					Left:  gc.GetLeftHolding(),
					Right: gc.GetRightHolding(),
				}
			case *capturepb.EchoEvent_GoalScored:
				lastGoal = e.GoalScored
			case *capturepb.EchoEvent_PlayerInfoUpdated:
				// Corrects the join snapshot: the engine reports jersey/level
				// as 0 for the first frames after a join.
				iu := e.PlayerInfoUpdated
				if cur, ok := roster[iu.GetPlayerSlot()]; ok {
					if !rosterDirty {
						roster = maps.Clone(roster)
						rosterDirty = true
					}
					roster[iu.GetPlayerSlot()] = &capturepb.PlayerInfo{
						Slot:          cur.GetSlot(),
						AccountNumber: cur.GetAccountNumber(),
						DisplayName:   cur.GetDisplayName(),
						Role:          cur.GetRole(),
						JerseyNumber:  iu.GetJerseyNumber(),
						Level:         iu.GetLevel(),
					}
				}
			case *capturepb.EchoEvent_PlayerStatsUpdated:
				su := e.PlayerStatsUpdated
				if !statsDirty {
					stats = maps.Clone(stats)
					statsDirty = true
				}
				stats[su.GetPlayerSlot()] = su
			case *capturepb.EchoEvent_MatchSettingsChanged:
				settings = e.MatchSettingsChanged
			case *capturepb.EchoEvent_ScoreboardUpdated:
				sb := e.ScoreboardUpdated
				score = Score{
					GameClockDisplay: sb.GetGameClockDisplay(),
					BluePoints:       sb.GetBluePoints(),
					OrangePoints:     sb.GetOrangePoints(),
					BlueRoundScore:   sb.GetBlueRoundScore(),
					OrangeRoundScore: sb.GetOrangeRoundScore(),
				}
			}
		}

		s.roster[i] = roster
		s.loadout[i] = loadout
		s.grab[i] = grab
		s.score[i] = score
		s.stats[i] = stats
		s.lastGoal[i] = lastGoal
		s.settings[i] = settings
	}
}
