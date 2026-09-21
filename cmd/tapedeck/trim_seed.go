package main

import (
	"slices"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/conversion"
	"google.golang.org/protobuf/proto"
)

// seedEventsForCut builds the events a trimmed capture needs on its first
// frame so that replaying it from the cut reproduces the state the source had
// there (GH #44).
//
// v2 is a delta format: identity, loadout, grab, stats, the scoreboard and the
// last goal are carried by events and materialized by replaying them from the
// beginning. Dropping the frames before --start therefore discards the frame-0
// seeding and every change that happened before the cut, and the trimmed file
// reconstructs into state that is not missing but *fabricated as absent* —
// players with no stats block, empty hands, no last_score, zero round scores.
// A round-trip of the trimmed file does not notice, because both sides agree.
//
// The state comes from conversion.Session, the same replay the reconstructor
// uses, rather than from a second hand-written accumulator. The recurring
// defect in this repo is a complete write path against a partial read path, and
// a second seeding implementation would be exactly that.
//
// prev is the ordinal of the frame BEFORE the first written frame, so its state
// excludes the cut frame's own events; those still ride the cut frame and apply
// on top. Returns nil when there is nothing to seed.
func seedEventsForCut(sess *conversion.Session, prev int) []*capturepb.EchoEvent {
	if sess == nil || prev < 0 {
		return nil
	}

	var events []*capturepb.EchoEvent

	// Loadout, per slot, in slot order so identical input yields identical
	// output (map iteration is randomized). A zero-valued entry is still
	// restated: a slot's presence in the map is itself state, and dropping it
	// would leave the trimmed capture reporting fewer known slots than the
	// source had.
	for _, slot := range sortedSlots(sess.LoadoutAt(prev)) {
		lo := sess.LoadoutAt(prev)[slot]
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_LoadoutChanged{
				LoadoutChanged: &capturepb.LoadoutChanged{
					PlayerSlot: slot,
					Weapon:     lo.Weapon,
					Ordnance:   lo.Ordnance,
					TacMod:     lo.TacMod,
				},
			},
		})
	}

	// Grab, per slot.
	for _, slot := range sortedSlots(sess.GrabAt(prev)) {
		g := sess.GrabAt(prev)[slot]
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_GrabChanged{
				GrabChanged: &capturepb.GrabChanged{
					PlayerSlot:   slot,
					LeftHolding:  g.Left,
					RightHolding: g.Right,
				},
			},
		})
	}

	// Engine stat counters, per slot. These carry a pre-capture baseline and
	// cannot be rebuilt by accumulating detected events, so they are restated
	// verbatim (STATS-001).
	for _, slot := range sortedSlots(sess.StatsAt(prev)) {
		st := sess.StatsAt(prev)[slot]
		if st == nil {
			continue
		}
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_PlayerStatsUpdated{
				PlayerStatsUpdated: proto.Clone(st).(*capturepb.PlayerStatsUpdated),
			},
		})
	}

	// Scoreboard.
	if sc := sess.ScoreAt(prev); sc != (conversion.Score{}) {
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_ScoreboardUpdated{
				ScoreboardUpdated: &capturepb.ScoreboardUpdated{
					BluePoints:       sc.BluePoints,
					OrangePoints:     sc.OrangePoints,
					BlueRoundScore:   sc.BlueRoundScore,
					OrangeRoundScore: sc.OrangeRoundScore,
					GameClockDisplay: sc.GameClockDisplay,
				},
			},
		})
	}

	// Match settings — the trimmed capture keeps the source header, whose values
	// are the capture-start ones; restate the settings in effect at the cut when
	// a change happened before it (gh #59). MatchSettingsAt(-1) is the header.
	if ms := sess.MatchSettingsAt(prev); ms != sess.MatchSettingsAt(-1) {
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_MatchSettingsChanged{
				MatchSettingsChanged: &capturepb.MatchSettingsChanged{
					RulesChangedBy:  ms.RulesChangedBy,
					RulesChangedAt:  ms.RulesChangedAt,
					TotalRoundCount: ms.TotalRoundCount,
				},
			},
		})
	}

	// Last goal — the engine carries last_score forward on every frame, so the
	// most recent GoalScored has to survive the cut.
	if g := sess.LastGoalAt(prev); g != nil {
		events = append(events, &capturepb.EchoEvent{
			Event: &capturepb.EchoEvent_GoalScored{
				GoalScored: proto.Clone(g).(*capturepb.GoalScored),
			},
		})
	}

	return events
}

// rosterAtCut returns the roster as of ordinal prev, for restating the header's
// initial_roster on a trimmed capture.
//
// Rewriting the roster is preferred over synthesizing PlayerJoined/PlayerLeft
// events: the header field is authoritative for capture-start identity, so
// setting it states the truth once instead of encoding a delta against a roster
// that no longer applies. A player who left before the cut would otherwise stay
// in initial_roster forever, because the PlayerLeft that removed them was
// discarded along with its frame.
func rosterAtCut(sess *conversion.Session, prev int) []*capturepb.PlayerInfo {
	if sess == nil || prev < 0 {
		return nil
	}
	roster := sess.RosterAt(prev)
	if len(roster) == 0 {
		return nil
	}
	out := make([]*capturepb.PlayerInfo, 0, len(roster))
	for _, slot := range sortedSlots(roster) {
		out = append(out, proto.Clone(roster[slot]).(*capturepb.PlayerInfo))
	}
	return out
}

// sortedSlots returns m's keys in ascending order. Go randomizes map iteration,
// and the footer's indexes plus any byte-level comparison require that the same
// input produce the same output.
func sortedSlots[V any](m map[int32]V) []int32 {
	slots := make([]int32, 0, len(m))
	for slot := range m {
		slots = append(slots, slot)
	}
	slices.Sort(slots)
	return slots
}
