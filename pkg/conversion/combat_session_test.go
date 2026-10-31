package conversion

import (
	"archive/zip"
	"bufio"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echotools/tape/v4/pkg/codec"
)

// combatKeyGaps are real combat paths the tape cannot produce yet, because the
// v2 schema has no field for them. They are pinned absent: when one gains a
// home, this test fails and the list is updated with it.
var combatKeyGaps = map[string]bool{
	"contested":               true,
	"timers":                  true,
	"timers.game_timer":       true,
	"timers.overtime_timer":   true,
	"payload.attackers":       true,
	"payload.pos":             true,
	"payload.pos.[]":          true,
	"teams.[].players.[].Arm": true,
}

// combatKeyTypeGaps are real paths the output carries with a different type.
// last_throw is out of scope (gh #35): the output has it as null.
var combatKeyTypeGaps = map[string]string{"last_throw": "null"}

// combatExtraPaths are paths the output carries that the real combat files do
// not. The reader ignores unknown keys, and the tape holds the data for these,
// so they are kept. "events" is the one the tape cannot yet fill.
var combatExtraPaths = map[string]bool{
	"events":                            true,
	"blue_team_restart_request":         true,
	"orange_team_restart_request":       true,
	"disc":                              true,
	"game_clock":                        true,
	"game_clock_display":                true,
	"game_status":                       true,
	"last_score":                        true,
	"possession":                        true,
	"possession.[]":                     true,
	"rules_changed_at":                  true,
	"rules_changed_by":                  true,
	"teams.[].possession":               true,
	"teams.[].stats":                    true,
	"teams.[].players.[].stats":         true,
	"teams.[].players.[].blocking":      true,
	"teams.[].players.[].holding_left":  true,
	"teams.[].players.[].holding_right": true,
	"teams.[].players.[].invulnerable":  true,
	"teams.[].players.[].possession":    true,
	"teams.[].players.[].stunned":       true,
}

// readSessionLines returns each line's session as a decoded JSON object and
// fails on any line that is not a timestamp, a tab, and one JSON value.
func readSessionLines(t *testing.T, path string) []map[string]any {
	t.Helper()
	zr, err := zip.OpenReader(path)
	if err != nil {
		t.Fatalf("open echoreplay zip: %v", err)
	}
	defer zr.Close() //nolint:errcheck // read-only test handle
	if len(zr.File) != 1 {
		t.Fatalf("echoreplay zip has %d entries, want 1", len(zr.File))
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open zip entry: %v", err)
	}
	defer rc.Close() //nolint:errcheck // read-only test handle

	var sessions []map[string]any
	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	for n := 1; scanner.Scan(); n++ {
		fields := strings.Split(scanner.Text(), "\t")
		if len(fields) < 2 {
			t.Fatalf("line %d has no tab separator", n)
		}
		var obj map[string]any
		if err := json.Unmarshal([]byte(fields[1]), &obj); err != nil {
			t.Fatalf("line %d: session is not one JSON object: %v", n, err)
		}
		// Optional bones column: also JSON.
		for _, extra := range fields[2:] {
			if !json.Valid([]byte(extra)) {
				t.Fatalf("line %d: trailing column is not JSON", n)
			}
		}
		sessions = append(sessions, obj)
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan echoreplay entry: %v", err)
	}
	return sessions
}

// keyTreeOf walks decoded JSON and returns path -> set of value types, using
// the same path form as testdata/combat/keytree.tsv (array elements are ".[]").
func keyTreeOf(sessions []map[string]any) map[string]map[string]bool {
	tree := map[string]map[string]bool{}
	var walk func(v any, path string)
	walk = func(v any, path string) {
		switch x := v.(type) {
		case map[string]any:
			for k, child := range x {
				p := k
				if path != "" {
					p = path + "." + k
				}
				addType(tree, p, child)
				walk(child, p)
			}
		case []any:
			p := "[]"
			if path != "" {
				p = path + ".[]"
			}
			for _, child := range x {
				addType(tree, p, child)
				walk(child, p)
			}
		}
	}
	for _, s := range sessions {
		walk(s, "")
	}
	return tree
}

func addType(tree map[string]map[string]bool, path string, v any) {
	if tree[path] == nil {
		tree[path] = map[string]bool{}
	}
	tree[path][jsonType(v)] = true
}

func jsonType(v any) string {
	switch v.(type) {
	case map[string]any:
		return "object"
	case []any:
		return "array"
	case string:
		return "string"
	case float64:
		return "number"
	case bool:
		return "boolean"
	case nil:
		return "null"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// loadCombatKeyTree reads testdata/combat/keytree.tsv: path, TAB, type.
func loadCombatKeyTree(t *testing.T) map[string]string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", "combat", "keytree.tsv"))
	if err != nil {
		t.Fatalf("read key tree: %v", err)
	}
	want := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		path, typ, ok := strings.Cut(line, "\t")
		if !ok {
			t.Fatalf("key tree line has no tab: %q", line)
		}
		want[path] = typ
	}
	if len(want) == 0 {
		t.Fatal("key tree is empty")
	}
	return want
}

// reconstructCombatFixture writes the combat fixture and reconstructs it,
// returning the echoreplay path.
func reconstructCombatFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	tape := writeCombatFixture(t, dir)
	recon := filepath.Join(dir, "combat.echoreplay")
	written, err := ReconstructFile(tape, recon)
	if err != nil {
		t.Fatalf("ReconstructFile: %v", err)
	}
	if written != 3 {
		t.Fatalf("ReconstructFile wrote %d frames, fixture has 3", written)
	}
	return recon
}

// TestCombatSessionKeyTree holds the combat /session output to the real key
// tree: every real path the tape can produce is present with the real type;
// every real path it cannot produce is a pinned gap; and every extra path is a
// pinned extra.
func TestCombatSessionKeyTree(t *testing.T) {
	t.Parallel()
	recon := reconstructCombatFixture(t)
	got := keyTreeOf(readSessionLines(t, recon))
	want := loadCombatKeyTree(t)

	for path, typ := range want {
		if combatKeyGaps[path] || strings.HasPrefix(path, "last_throw.") {
			if _, ok := got[path]; ok {
				t.Errorf("%s is now emitted: move it out of combatKeyGaps", path)
			}
			continue
		}
		if wantOut, ok := combatKeyTypeGaps[path]; ok {
			if !got[path][wantOut] {
				t.Errorf("%s: pinned type %s not emitted (got %v)", path, wantOut, got[path])
			}
			continue
		}
		types, ok := got[path]
		if !ok {
			t.Errorf("missing real path %s (%s)", path, typ)
			continue
		}
		if !types[typ] {
			t.Errorf("%s: type %s, want %s", path, keysOf(types), typ)
		}
	}
	for path := range got {
		if _, ok := want[path]; ok {
			continue
		}
		if !combatExtraPaths[path] {
			t.Errorf("unexpected extra path %s", path)
		}
	}
	for path := range combatExtraPaths {
		if _, ok := got[path]; !ok {
			t.Errorf("pinned extra %s is no longer emitted: update combatExtraPaths", path)
		}
	}
}

func keysOf(m map[string]bool) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TestCombatSessionPayloadNested pins the payload shape: one nested object with
// the fixture's values, and no flat payload_* keys.
func TestCombatSessionPayloadNested(t *testing.T) {
	t.Parallel()
	sessions := readSessionLines(t, reconstructCombatFixture(t))
	for i, s := range sessions {
		for k := range s {
			if strings.HasPrefix(k, "payload_") {
				t.Errorf("frame %d: flat key %s", i, k)
			}
		}
		payload, ok := s["payload"].(map[string]any)
		if !ok {
			t.Fatalf("frame %d: payload is %T, want object", i, s["payload"])
		}
		wantDistance := 12.5 + float64(i)
		checks := map[string]float64{"checkpoint": 2, "defenders": 1, "multiplier": 1, "speed": 3, "distance": wantDistance}
		for k, want := range checks {
			if got, _ := payload[k].(float64); math.Abs(got-want) > 1e-6 {
				t.Errorf("frame %d: payload.%s = %v, want %v", i, k, payload[k], want)
			}
		}
		if ev, ok := s["events"].([]any); !ok || len(ev) != 0 {
			t.Errorf("frame %d: events = %v, want empty array", i, s["events"])
		}
	}
}

// TestCombatEchoreplayReadsPayload reads the reconstruction back through the
// echoreplay reader. The payload must come back on the SessionResponse, not be
// dropped as an unknown key.
func TestCombatEchoreplayReadsPayload(t *testing.T) {
	t.Parallel()
	r, err := codec.NewEchoReplayReader(reconstructCombatFixture(t))
	if err != nil {
		t.Fatalf("open reconstruction: %v", err)
	}
	frames, err := r.ReadFrames()
	_ = r.Close()
	if err != nil {
		t.Fatalf("read reconstruction: %v", err)
	}
	if len(frames) != 3 {
		t.Fatalf("read %d frames, want 3", len(frames))
	}
	for i, f := range frames {
		s := f.GetSession()
		if s.GetPayloadCheckpoint() != 2 || s.GetPayloadDefenders() != 1 || s.GetPayloadSpeed() != 3 {
			t.Errorf("frame %d: payload lost on read: checkpoint=%d defenders=%d speed=%v",
				i, s.GetPayloadCheckpoint(), s.GetPayloadDefenders(), s.GetPayloadSpeed())
		}
		if want := 12.5 + float64(i); math.Abs(s.GetPayloadDistance()-want) > 1e-6 {
			t.Errorf("frame %d: payload distance = %v, want %v", i, s.GetPayloadDistance(), want)
		}
	}
}

// TestArenaSessionShapeUnchanged pins that arena frames keep the flat payload_*
// fields and carry no events array, so the arena output is unchanged.
func TestArenaSessionShapeUnchanged(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tape := filepath.Join(dir, "arena.tape")
	w, err := codec.NewWriterWithOptions(tape)
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	if err := w.WriteHeader(shippedHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	frames := loadCorpusProtos(t)
	if len(frames) == 0 {
		t.Fatal("corpus sample missing or empty: testdata/sample.echoreplay")
	}
	for _, f := range frames[:10] {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recon := filepath.Join(dir, "arena.echoreplay")
	if _, err := ReconstructFile(tape, recon); err != nil {
		t.Fatalf("ReconstructFile: %v", err)
	}
	for i, s := range readSessionLines(t, recon) {
		if _, ok := s["events"]; ok {
			t.Errorf("frame %d: arena session has an events key", i)
		}
		if _, ok := s["payload_checkpoint"]; !ok {
			t.Errorf("frame %d: arena session lost the flat payload_checkpoint key", i)
		}
		if _, ok := s["payload"]; ok {
			t.Errorf("frame %d: arena session has a nested payload key", i)
		}
	}
}
