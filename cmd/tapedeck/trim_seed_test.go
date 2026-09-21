package main

import (
	"bytes"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/echotools/tape/v4/pkg/conversion"
	"github.com/spf13/cobra"
	"google.golang.org/protobuf/proto"
)

const goldenTape = "../../testdata/sample.tape.golden"

func openSessionAt(t *testing.T, path string) *conversion.Session {
	t.Helper()
	r, err := codec.NewReader(path)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	sess, err := conversion.OpenSession(r)
	if err != nil {
		t.Fatalf("session %s: %v", path, err)
	}
	return sess
}

// frameTimestampAt returns the timestamp_offset_ms of the given frame ordinal.
func frameTimestampAt(t *testing.T, path string, ordinal int) uint32 {
	t.Helper()
	r, err := codec.NewReader(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("header: %v", err)
	}
	for i := 0; ; i++ {
		f, err := r.ReadFrame()
		if err != nil {
			t.Fatalf("ran out of frames before ordinal %d: %v", ordinal, err)
		}
		if i == ordinal {
			return f.GetTimestampOffsetMs()
		}
	}
}

func runTrimCmd(t *testing.T, in, out string, startMs uint32) {
	t.Helper()
	cmd := newTrimCommand()
	cmd.SetOut(&bytes.Buffer{})
	cmd.SetErr(&bytes.Buffer{})
	cmd.SetArgs([]string{in, "-o", out, "--start", uint32String(startMs)})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("trim: %v", err)
	}
}

func uint32String(v uint32) string {
	if v == 0 {
		return "0"
	}
	var b []byte
	for v > 0 {
		b = append([]byte{byte('0' + v%10)}, b...)
		v /= 10
	}
	return string(b)
}

// GH #44: a trimmed capture must reconstruct into the same state the source had
// at the cut. Before the fix, trim dropped the leading frames with a bare
// continue, discarding the events that materialize identity, loadout, grab,
// stats, the scoreboard and the last goal — so the output reconstructed into
// state that was not missing but fabricated as absent, which a round-trip of
// the trimmed file cannot detect.
func TestTrim_PreservesStateAcrossTheCut(t *testing.T) {
	const cutOrdinal = 400

	orig := openSessionAt(t, goldenTape)
	if orig.FrameCount() <= cutOrdinal {
		t.Skipf("golden has %d frames, need > %d", orig.FrameCount(), cutOrdinal)
	}

	startMs := frameTimestampAt(t, goldenTape, cutOrdinal)
	out := filepath.Join(t.TempDir(), "trimmed.tape")
	runTrimCmd(t, goldenTape, out, startMs)

	got := openSessionAt(t, out)
	if got.FrameCount() == 0 {
		t.Fatal("trimmed capture has no frames")
	}

	// State at ordinal 0 of the trimmed capture must equal state at the cut
	// in the source.
	wantRoster, gotRoster := orig.RosterAt(cutOrdinal), got.RosterAt(0)
	if len(gotRoster) != len(wantRoster) {
		t.Errorf("roster size = %d, want %d", len(gotRoster), len(wantRoster))
	}
	for slot, want := range wantRoster {
		g, ok := gotRoster[slot]
		if !ok {
			t.Errorf("slot %d missing from trimmed roster", slot)
			continue
		}
		if !proto.Equal(g, want) {
			t.Errorf("slot %d identity differs:\n got %v\nwant %v", slot, g, want)
		}
	}

	if want, g := orig.LoadoutAt(cutOrdinal), got.LoadoutAt(0); len(g) != len(want) {
		t.Errorf("loadout slots = %d, want %d", len(g), len(want))
	} else {
		for slot, w := range want {
			if g[slot] != w {
				t.Errorf("slot %d loadout = %+v, want %+v", slot, g[slot], w)
			}
		}
	}

	if want, g := orig.GrabAt(cutOrdinal), got.GrabAt(0); len(g) != len(want) {
		t.Errorf("grab slots = %d, want %d", len(g), len(want))
	} else {
		for slot, w := range want {
			if g[slot] != w {
				t.Errorf("slot %d grab = %+v, want %+v", slot, g[slot], w)
			}
		}
	}

	if want, g := orig.StatsAt(cutOrdinal), got.StatsAt(0); len(g) != len(want) {
		t.Errorf("stats slots = %d, want %d", len(g), len(want))
	} else {
		for slot, w := range want {
			if !proto.Equal(g[slot], w) {
				t.Errorf("slot %d stats differ:\n got %v\nwant %v", slot, g[slot], w)
			}
		}
	}

	if want, g := orig.ScoreAt(cutOrdinal), got.ScoreAt(0); g != want {
		t.Errorf("score = %+v, want %+v", g, want)
	}

	if want, g := orig.LastGoalAt(cutOrdinal), got.LastGoalAt(0); !proto.Equal(g, want) {
		t.Errorf("last goal differs:\n got %v\nwant %v", g, want)
	}
}

// Match settings change mid-capture and ride MatchSettingsChanged events
// (gh #59), so a cut after a change must restate them: without a seed the
// trimmed capture falls back to the header's capture-start values.
func TestTrim_PreservesMatchSettingsAcrossTheCut(t *testing.T) {
	const cutOrdinal = 800
	reRounds := regexp.MustCompile(`"total_round_count":[0-9]+`)
	reBy := regexp.MustCompile(`"rules_changed_by":"[^"]*"`)
	src := editRecording(t, filepath.Join("..", "..", "testdata", "sample.echoreplay"), func(i int, line []byte) []byte {
		if i >= 300 {
			line = reBy.ReplaceAll(line, []byte(`"rules_changed_by":"Editor"`))
		}
		if i >= 700 {
			line = reRounds.ReplaceAll(line, []byte(`"total_round_count":5`))
		}
		return line
	})
	tape := filepath.Join(t.TempDir(), "settings.tape")
	if _, err := conversion.ConvertFile(src, tape); err != nil {
		t.Fatalf("convert: %v", err)
	}

	orig := openSessionAt(t, tape)
	want := orig.MatchSettingsAt(cutOrdinal)
	if want.TotalRoundCount != 5 || want.RulesChangedBy != "Editor" {
		t.Fatalf("premise: source settings at the cut are %+v, want the edited values", want)
	}
	out := filepath.Join(t.TempDir(), "trimmed.tape")
	runTrimCmd(t, tape, out, frameTimestampAt(t, tape, cutOrdinal))

	if got := openSessionAt(t, out).MatchSettingsAt(0); got != want {
		t.Errorf("match settings after the cut = %+v, want %+v", got, want)
	}
}

// Trimming from 0 drops nothing, so it must not inject seed events — the output
// should carry the same event count as the source.
func TestTrim_FromZeroInjectsNoSeed(t *testing.T) {
	out := filepath.Join(t.TempDir(), "whole.tape")
	runTrimCmd(t, goldenTape, out, 0)

	orig, got := openSessionAt(t, goldenTape), openSessionAt(t, out)
	if got.FrameCount() != orig.FrameCount() {
		t.Fatalf("frame count = %d, want %d", got.FrameCount(), orig.FrameCount())
	}
	if w, g := orig.ScoreAt(0), got.ScoreAt(0); g != w {
		t.Errorf("frame-0 score = %+v, want %+v", g, w)
	}
	if w, g := len(orig.RosterAt(0)), len(got.RosterAt(0)); g != w {
		t.Errorf("frame-0 roster = %d, want %d", g, w)
	}
}

// Trimming is deterministic: the seed events are built from maps, and identical
// input must produce identical bytes.
func TestTrim_IsDeterministic(t *testing.T) {
	startMs := frameTimestampAt(t, goldenTape, 400)
	dir := t.TempDir()
	a := filepath.Join(dir, "a.tape")
	b := filepath.Join(dir, "b.tape")
	runTrimCmd(t, goldenTape, a, startMs)
	runTrimCmd(t, goldenTape, b, startMs)

	da, db := mustRead(t, a), mustRead(t, b)
	if !bytes.Equal(da, db) {
		t.Fatalf("two trims of the same input differ: %d vs %d bytes", len(da), len(db))
	}
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	r, err := codec.NewReader(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer r.Close() //nolint:errcheck // test cleanup
	var buf bytes.Buffer
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("header: %v", err)
	}
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break
		}
		b, mErr := proto.Marshal(f)
		if mErr != nil {
			t.Fatalf("marshal: %v", mErr)
		}
		buf.Write(b)
	}
	return buf.Bytes()
}

var _ = cobra.Command{}
