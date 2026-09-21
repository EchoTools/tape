package fidelity_test

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"testing"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	"github.com/echotools/tape/v4/pkg/fidelity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/structpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// --- coverage: the self-proving half ------------------------------------------
//
// A rule this package implements is "anything not reached is an error".
// Proving that on a recording only proves it for that recording's data.
// The test below proves it for the SCHEMA: a synthetically FULLY populated
// message — built from the descriptors, so it grows the moment the proto does —
// is compared against an empty one, and every single schema path must both be
// reached and report a difference.
//
// That is what makes the check self-proving. Add a field to SessionResponse
// tomorrow: fillMessage populates it (it walks descriptors, not a list), and if
// the differ cannot reach it this test fails on the new path by name.

func TestComparatorReachesAndComparesEverySchemaField(t *testing.T) {
	for _, root := range []proto.Message{
		&enginev1.SessionResponse{},
		&enginev1.PlayerBonesResponse{},
	} {
		name := string(root.ProtoReflect().Descriptor().Name())
		t.Run(name, func(t *testing.T) {
			schema, err := fidelity.New(root, fidelity.Options{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			full := proto.Clone(root)
			fillMessage(t, full.ProtoReflect(), 0)
			empty := proto.Clone(root)

			c := schema.NewComparator()
			c.Compare(full, empty, "synthetic")

			if un := c.Unreached(); len(un) != 0 {
				t.Errorf("%d/%d schema field(s) were NEVER REACHED by the comparison:\n  %s",
					len(un), c.SchemaSize(), strings.Join(un, "\n  "))
			}

			diffed := map[string]bool{}
			for _, d := range c.Diffs() {
				diffed[d.Field] = true
			}
			var uncompared []string
			for _, p := range schema.Paths() {
				if !diffed[p] {
					uncompared = append(uncompared, p)
				}
			}
			if len(uncompared) != 0 {
				t.Errorf("%d schema field(s) were reached but produced NO DIFF against an empty message, "+
					"so they are not actually compared:\n  %s", len(uncompared), strings.Join(uncompared, "\n  "))
			}
			t.Logf("%s: %d schema fields, %d reached, %d reported different against an empty message",
				name, c.SchemaSize(), c.Reached(), len(diffed))
		})
	}
}

// TestUnreachedDetectsAbsentSubtree is the other half of the coverage proof:
// Unreached must actually go non-empty when a subtree is never walked. Without
// this, "0 unreached" could just mean the counter never fires.
func TestUnreachedDetectsAbsentSubtree(t *testing.T) {
	schema, err := fidelity.New(&enginev1.SessionResponse{}, fidelity.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// last_throw absent on BOTH sides: nothing under it can have been compared.
	a := &enginev1.SessionResponse{SessionId: "s"}
	b := &enginev1.SessionResponse{SessionId: "s"}

	c := schema.NewComparator()
	c.Compare(a, b, "frame 0")

	un := c.Unreached()
	want := "SessionResponse.last_throw.arm_speed"
	if !contains(un, want) {
		t.Fatalf("Unreached() does not report %s; got %d entries: %v", want, len(un), un)
	}
	if contains(un, "SessionResponse.session_id") {
		t.Errorf("Unreached() reports a field that WAS compared: SessionResponse.session_id")
	}
	t.Logf("absent-subtree run: %d/%d schema fields unreached, including %s",
		len(un), c.SchemaSize(), want)
}

// --- value comparison: the fields the hand-written enumeration missed ---------

// TestCatchesValueDifferencesTheHandListMissed pins the specific holes that
// motivated this work. Every case below is a field whose VALUE the previous
// hand-written comparison never looked at: client_name and the payload_* fields
// were not compared at all; PauseState was compared on paused_state only;
// Team.has_possession was never compared; last_throw / last_score were probed
// nil-vs-non-nil only, so a reconstruction carrying wrong numbers in their 20
// sub-fields passed silently.
func TestCatchesValueDifferencesTheHandListMissed(t *testing.T) {
	schema, err := fidelity.New(&enginev1.SessionResponse{}, fidelity.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	cases := []struct {
		name     string
		mutate   func(*enginev1.SessionResponse)
		wantPath string
	}{
		{"client_name", func(s *enginev1.SessionResponse) { s.SetClientName("other") }, "SessionResponse.client_name"},
		{"payload_distance", func(s *enginev1.SessionResponse) { s.SetPayloadDistance(9.5) }, "SessionResponse.payload_distance"},
		{"pause.unpaused_team", func(s *enginev1.SessionResponse) { s.GetPause().SetUnpausedTeam("orange") }, "SessionResponse.pause.unpaused_team"},
		{"pause.paused_timer", func(s *enginev1.SessionResponse) { s.GetPause().SetPausedTimer(3) }, "SessionResponse.pause.paused_timer"},
		{"team.has_possession", func(s *enginev1.SessionResponse) { s.GetTeams()[0].SetHasPossession(false) }, "SessionResponse.teams[].has_possession"},
		{"last_throw.arm_speed", func(s *enginev1.SessionResponse) { s.GetLastThrow().SetArmSpeed(99) }, "SessionResponse.last_throw.arm_speed"},
		{"last_score.person_scored", func(s *enginev1.SessionResponse) { s.GetLastScore().SetPersonScored("nobody") }, "SessionResponse.last_score.person_scored"},
		{"player.stats.stuns", func(s *enginev1.SessionResponse) { s.GetTeams()[0].GetPlayers()[0].GetStats().SetStuns(0) }, "SessionResponse.teams[].players[].stats.stuns"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := sampleSession()
			recon := sampleSession()
			tc.mutate(recon)

			c := schema.NewComparator()
			c.Compare(orig, recon, "frame 7")

			diffs := c.Diffs()
			var found *fidelity.Diff
			for i := range diffs {
				if diffs[i].Path == tc.wantPath {
					found = &diffs[i]
				}
			}
			if found == nil {
				t.Fatalf("differ did not report %s; reported: %v", tc.wantPath, diffs)
			}
			if len(diffs) != 1 {
				t.Errorf("expected exactly one differing path, got %d: %v", len(diffs), diffs)
			}
			t.Logf("caught %s", found)
		})
	}
}

// TestIdenticalMessagesProduceNoDiff is the green: the differ must be silent on
// a faithful reconstruction, or every failure it reports is noise.
func TestIdenticalMessagesProduceNoDiff(t *testing.T) {
	schema, err := fidelity.New(&enginev1.SessionResponse{}, fidelity.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := schema.NewComparator()
	c.Compare(sampleSession(), sampleSession(), "frame 0")
	if d := c.Diffs(); len(d) != 0 {
		t.Fatalf("differ reported %d diff(s) on identical messages: %v", len(d), d)
	}
}

// --- structural cases ---------------------------------------------------------

func TestListPairingByKeyIgnoresOrderAndReportsUnmatched(t *testing.T) {
	slotKey := func(m protoreflect.Message) string {
		fd := m.Descriptor().Fields().ByName("slot_number")
		return fmt.Sprint(m.Get(fd).Int())
	}
	schema, err := fidelity.New(&enginev1.Team{}, fidelity.Options{
		ListKeys: map[string]fidelity.KeyFunc{"engine.v1.Team.players": slotKey},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	member := func(slot int32, name string) *enginev1.TeamMember {
		m := &enginev1.TeamMember{}
		m.SetSlotNumber(slot)
		m.SetDisplayName(name)
		return m
	}
	a := &enginev1.Team{}
	a.SetPlayers([]*enginev1.TeamMember{member(1, "one"), member(2, "two")})
	b := &enginev1.Team{}
	b.SetPlayers([]*enginev1.TeamMember{member(2, "two"), member(1, "one")})

	c := schema.NewComparator()
	c.Compare(a, b, "frame 0")
	if d := c.Diffs(); len(d) != 0 {
		t.Fatalf("keyed pairing reported %d diff(s) on a reordered but identical roster: %v", len(d), d)
	}

	// Now drop slot 2 from the reconstruction: the count differs and every
	// field of the missing member must be reported, not skipped.
	b2 := &enginev1.Team{}
	b2.SetPlayers([]*enginev1.TeamMember{member(1, "one")})
	c2 := schema.NewComparator()
	c2.Compare(a, b2, "frame 0")
	paths := diffPaths(c2.Diffs())
	for _, want := range []string{"Team.players[]#count", "Team.players[].display_name"} {
		if !contains(paths, want) {
			t.Errorf("dropped roster member: %s not reported; got %v", want, paths)
		}
	}
	t.Logf("dropped member reported as: %v", paths)
}

func TestNilSideIsReportedNotSkipped(t *testing.T) {
	schema, err := fidelity.New(&enginev1.PlayerBonesResponse{}, fidelity.Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	present := &enginev1.PlayerBonesResponse{}
	ub := &enginev1.UserBones{}
	ub.SetPlayerIndex(3)
	ub.SetBoneT([]float32{1, 2, 3})
	present.SetUserBones([]*enginev1.UserBones{ub})

	c := schema.NewComparator()
	c.Compare(present, nil, "frame 0")
	paths := diffPaths(c.Diffs())
	if !contains(paths, "PlayerBonesResponse#presence") {
		t.Errorf("a nil reconstruction side was not reported as a presence difference; got %v", paths)
	}
	if !contains(paths, "PlayerBonesResponse.user_bones[]#count") {
		t.Errorf("a nil reconstruction side did not report the lost bones; got %v", paths)
	}
	t.Logf("nil side reported as: %v", paths)
}

// TestFloatToleranceAppliesOnlyToFloats guards the tolerance mechanism against
// becoming a licence to be wrong on anything that is not a narrowed float.
func TestFloatToleranceAppliesOnlyToFloats(t *testing.T) {
	schema, err := fidelity.New(&enginev1.SessionResponse{}, fidelity.Options{
		Tolerance: func(fd protoreflect.FieldDescriptor, _ string) float64 {
			if fd.Kind() == protoreflect.DoubleKind {
				return 1e-3
			}
			return 0
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	orig := &enginev1.SessionResponse{}
	orig.SetGameClock(100)
	orig.SetBluePoints(3)
	recon := &enginev1.SessionResponse{}
	recon.SetGameClock(100.0000005)
	recon.SetBluePoints(4)

	c := schema.NewComparator()
	c.Compare(orig, recon, "frame 0")
	paths := diffPaths(c.Diffs())
	if contains(paths, "SessionResponse.game_clock") {
		t.Errorf("a float within tolerance was reported: %v", paths)
	}
	if !contains(paths, "SessionResponse.blue_points") {
		t.Errorf("an integer difference was NOT reported: %v", paths)
	}
}

// TestEqualOverridesOnlyItsOwnField pins the per-field equality hook: it
// decides equality for the one leaf it is attached to, and every other field
// keeps exact comparison.
func TestEqualOverridesOnlyItsOwnField(t *testing.T) {
	sameLength := func(a, b protoreflect.Value) bool { return len(a.String()) == len(b.String()) }
	schema, err := fidelity.New(&enginev1.SessionResponse{}, fidelity.Options{
		Equal: func(fd protoreflect.FieldDescriptor, _ string) func(a, b protoreflect.Value) bool {
			if fd.Name() == "game_clock_display" {
				return sameLength
			}
			return nil
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	orig := &enginev1.SessionResponse{}
	orig.SetGameClockDisplay("01:45.59")
	orig.SetMapName("mpl_arena_a")
	recon := &enginev1.SessionResponse{}
	recon.SetGameClockDisplay("01:45.58")
	recon.SetMapName("mpl_arena_b")

	c := schema.NewComparator()
	c.Compare(orig, recon, "frame 0")
	paths := diffPaths(c.Diffs())
	if contains(paths, "SessionResponse.game_clock_display") {
		t.Errorf("the field's own Equal said equal, but a diff was reported: %v", paths)
	}
	if !contains(paths, "SessionResponse.map_name") {
		t.Errorf("a field with no Equal lost exact comparison: %v", paths)
	}
}

// TestRecursiveMessageIsRejected pins that the plan builder refuses to produce
// a silently truncated walk. google.protobuf.Value nests through Struct back
// into Value.
func TestRecursiveMessageIsRejected(t *testing.T) {
	_, err := fidelity.New(&structpb.Value{}, fidelity.Options{})
	if err == nil {
		t.Fatal("New accepted a recursive message type; the resulting plan cannot cover the schema")
	}
	t.Logf("rejected as expected: %v", err)
}

// TestSchemaPathsMatchDescriptors checks the plan against an independent
// descriptor walk, so a bug in the plan builder cannot make coverage look
// complete by shrinking the denominator.
//
// It runs over EVERY root a verification compares, not just the biggest one.
// Guarding SessionResponse alone left the other two denominators unguarded:
// dropping PlayerBonesResponse.user_bones[].bone_o[] — the recorded skeleton
// ORIENTATION — from the plan reported "reached 4/4 schema fields" and kept the
// whole package green. A denominator that shrinks with the numerator measures
// nothing.
func TestSchemaPathsMatchDescriptors(t *testing.T) {
	for _, root := range comparedRoots() {
		name := string(root.ProtoReflect().Descriptor().Name())
		t.Run(name, func(t *testing.T) {
			schema, err := fidelity.New(root, fidelity.Options{})
			if err != nil {
				t.Fatalf("New: %v", err)
			}
			want := enumerate(root.ProtoReflect().Descriptor(), name)
			got := append([]string(nil), schema.Paths()...)
			sort.Strings(want)
			sort.Strings(got)
			if len(want) != len(got) {
				t.Fatalf("schema has %d paths, an independent descriptor walk finds %d:\n  plan: %s\n  walk: %s",
					len(got), len(want), strings.Join(got, "\n        "), strings.Join(want, "\n        "))
			}
			for i := range want {
				if want[i] != got[i] {
					t.Fatalf("path %d: schema %q, descriptor walk %q", i, got[i], want[i])
				}
			}
			t.Logf("plan matches descriptors: %d field paths", len(got))
		})
	}
}

// comparedRoots is every message type a .echoreplay round-trip verification
// compares — the three a frame line carries. Any guard over "the schema" has to
// be a guard over all of them, or the unguarded ones are free to shrink.
func comparedRoots() []proto.Message {
	return []proto.Message{
		&timestamppb.Timestamp{},
		&enginev1.SessionResponse{},
		&enginev1.PlayerBonesResponse{},
	}
}

// --- helpers -----------------------------------------------------------------

func enumerate(md protoreflect.MessageDescriptor, path string) []string {
	var out []string
	fs := md.Fields()
	for i := 0; i < fs.Len(); i++ {
		fd := fs.Get(i)
		suffix := ""
		switch {
		case fd.IsMap():
			suffix = "{}"
		case fd.IsList():
			suffix = "[]"
		}
		p := path + "." + string(fd.Name()) + suffix
		out = append(out, p)
		if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
			if fd.IsMap() {
				if fd.MapValue().Kind() == protoreflect.MessageKind {
					out = append(out, enumerate(fd.MapValue().Message(), p)...)
				}
				continue
			}
			out = append(out, enumerate(fd.Message(), p)...)
		}
	}
	return out
}

// fillMessage populates every field of m from its descriptors, so the coverage
// proof exercises the whole schema without anyone listing it.
func fillMessage(t *testing.T, m protoreflect.Message, depth int) {
	t.Helper()
	if depth > 12 {
		t.Fatalf("fillMessage recursion limit at %s", m.Descriptor().FullName())
	}
	fields := m.Descriptor().Fields()
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		switch {
		case fd.IsMap():
			mp := m.Mutable(fd).Map()
			key := nonZero(t, fd.MapKey()).MapKey()
			if fd.MapValue().Kind() == protoreflect.MessageKind {
				v := mp.NewValue()
				fillMessage(t, v.Message(), depth+1)
				mp.Set(key, v)
				continue
			}
			mp.Set(key, nonZero(t, fd.MapValue()))
		case fd.IsList():
			l := m.Mutable(fd).List()
			for range 2 {
				if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
					v := l.NewElement()
					fillMessage(t, v.Message(), depth+1)
					l.Append(v)
					continue
				}
				l.Append(nonZero(t, fd))
			}
		case fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind:
			fillMessage(t, m.Mutable(fd).Message(), depth+1)
		default:
			m.Set(fd, nonZero(t, fd))
		}
	}
}

// nonZero returns a value that differs from the field's zero, so that comparing
// a filled message against an empty one must report every path.
func nonZero(t *testing.T, fd protoreflect.FieldDescriptor) protoreflect.Value {
	t.Helper()
	switch fd.Kind() {
	case protoreflect.BoolKind:
		return protoreflect.ValueOfBool(true)
	case protoreflect.StringKind:
		return protoreflect.ValueOfString("x")
	case protoreflect.BytesKind:
		return protoreflect.ValueOfBytes([]byte{1})
	case protoreflect.FloatKind:
		return protoreflect.ValueOfFloat32(1.5)
	case protoreflect.DoubleKind:
		return protoreflect.ValueOfFloat64(1.5)
	case protoreflect.Int32Kind, protoreflect.Sint32Kind, protoreflect.Sfixed32Kind:
		return protoreflect.ValueOfInt32(7)
	case protoreflect.Int64Kind, protoreflect.Sint64Kind, protoreflect.Sfixed64Kind:
		return protoreflect.ValueOfInt64(7)
	case protoreflect.Uint32Kind, protoreflect.Fixed32Kind:
		return protoreflect.ValueOfUint32(7)
	case protoreflect.Uint64Kind, protoreflect.Fixed64Kind:
		return protoreflect.ValueOfUint64(7)
	case protoreflect.EnumKind:
		vals := fd.Enum().Values()
		for i := 0; i < vals.Len(); i++ {
			if vals.Get(i).Number() != 0 {
				return protoreflect.ValueOfEnum(vals.Get(i).Number())
			}
		}
		t.Fatalf("enum %s has no non-zero value, so a filled/empty comparison cannot cover it", fd.FullName())
	}
	t.Fatalf("no non-zero value for kind %s (%s)", fd.Kind(), fd.FullName())
	return protoreflect.Value{}
}

func sampleSession() *enginev1.SessionResponse {
	stats := &enginev1.PlayerStats{}
	stats.SetStuns(3)
	stats.SetPoints(5)
	member := &enginev1.TeamMember{}
	member.SetSlotNumber(1)
	member.SetDisplayName("player")
	member.SetStats(stats)
	team := &enginev1.Team{}
	team.SetTeamName("BLUE TEAM")
	team.SetHasPossession(true)
	team.SetPlayers([]*enginev1.TeamMember{member})

	pause := &enginev1.PauseState{}
	pause.SetPausedState("unpaused")
	pause.SetUnpausedTeam("blue")
	pause.SetPausedTimer(0)

	lastThrow := &enginev1.LastThrowInfo{}
	lastThrow.SetArmSpeed(12.5)
	lastThrow.SetTotalSpeed(20)

	lastScore := &enginev1.LastScore{}
	lastScore.SetPersonScored("scorer")
	lastScore.SetPointAmount(2)

	s := &enginev1.SessionResponse{}
	s.SetSessionId("session")
	s.SetClientName("client")
	s.SetPayloadDistance(1.25)
	s.SetPause(pause)
	s.SetLastThrow(lastThrow)
	s.SetLastScore(lastScore)
	s.SetTeams([]*enginev1.Team{team})
	return s
}

func diffPaths(ds []fidelity.Diff) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, d.Path)
	}
	return out
}

func contains(hay []string, needle string) bool {
	return slices.Contains(hay, needle)
}
