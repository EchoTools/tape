package fidelity

import (
	"strings"
	"testing"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
)

func sessionPlan(t *testing.T) *Schema {
	t.Helper()
	s, err := New(&enginev1.SessionResponse{}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func scan(t *testing.T, plan *Schema, payload string) (map[string]int, error) {
	t.Helper()
	counts := map[string]int{}
	var sc scanner
	sc.reset([]byte(payload))
	return counts, sc.scanObject(plan.root, counts)
}

// TestScannerFindsUnknownKeysAtEveryLevel pins the nesting requirement: a
// top-level-only check would be worthless, because the levels that carry the
// per-team and per-player data are exactly the ones worth not losing.
func TestScannerFindsUnknownKeysAtEveryLevel(t *testing.T) {
	plan := sessionPlan(t)
	payload := `{
	  "sessionid": "s",
	  "future_top": 1,
	  "disc": {"position": [1,2,3], "future_disc": {"a": [1, {"b": 2}]}},
	  "teams": [
	    {"team": "BLUE", "future_team": "x",
	     "players": [{"name": "p", "future_player": [1,2]}, {"name": "q"}]},
	    {"team": "ORANGE", "players": []}
	  ]
	}`
	counts, err := scan(t, plan, payload)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	want := map[string]int{
		"SessionResponse.future_top":                      1,
		"SessionResponse.disc.future_disc":                1,
		"SessionResponse.teams[].future_team":             1,
		"SessionResponse.teams[].players[].future_player": 1,
	}
	for p, n := range want {
		if counts[p] != n {
			t.Errorf("%s: got %d, want %d (all findings: %v)", p, counts[p], n, counts)
		}
	}
	if len(counts) != len(want) {
		t.Errorf("unexpected extra findings: %v", counts)
	}
}

// TestScannerAcceptsBothNameForms: protojson accepts a field's proto name and
// its json name, so the guard must too, or it invents unknown keys.
func TestScannerAcceptsBothNameForms(t *testing.T) {
	plan := sessionPlan(t)
	// "sessionid" is the json name; "session_id" the proto name. Likewise
	// "userid"/"account_number" and "possession"/"has_possession" on a member.
	counts, err := scan(t, plan, `{"sessionid":"a","session_id":"a",
		"teams":[{"players":[{"userid":1,"account_number":1,"possession":true,"has_possession":true}]}]}`)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(counts) != 0 {
		t.Fatalf("known fields reported as unknown: %v", counts)
	}
}

func TestScannerRejectsMalformedJSON(t *testing.T) {
	plan := sessionPlan(t)
	for _, tc := range []struct{ name, payload string }{
		{"truncated_object", `{"sessionid": "a"`},
		{"truncated_string", `{"sessionid": "a}`},
		{"missing_colon", `{"sessionid" "a"}`},
		{"unbalanced_array", `{"possession": [1,2}`},
		{"not_an_object", `[1,2,3]`},
		{"empty", ``},
		{"garbage", `{"sessionid": @}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := scan(t, plan, tc.payload); err == nil {
				t.Fatalf("scanner accepted malformed payload %q", tc.payload)
			}
		})
	}
}

func TestScannerHandlesEscapesAndNesting(t *testing.T) {
	plan := sessionPlan(t)
	counts, err := scan(t, plan, `{"map_name":"a\"b}{[,\\","future_key":{"nested":"}"},`+
		`"teams":[{"team":"x\ty"}]}`)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if counts["SessionResponse.future_key"] != 1 {
		t.Fatalf("escaped key not decoded to future_key: %v", counts)
	}
	if len(counts) != 1 {
		t.Fatalf("string contents leaked into key findings: %v", counts)
	}
}

func TestScannerScansBonesPayload(t *testing.T) {
	plan, err := New(&enginev1.PlayerBonesResponse{}, Options{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	counts := map[string]int{}
	var sc scanner
	sc.reset([]byte(`{"user_bones":[{"playerid":1,"bone_t":[1,2],"future_bone":7}],"future_bones":1}`))
	if err := sc.scanObject(plan.root, counts); err != nil {
		t.Fatalf("scan: %v", err)
	}
	for _, want := range []string{"PlayerBonesResponse.user_bones[].future_bone", "PlayerBonesResponse.future_bones"} {
		if counts[want] != 1 {
			t.Errorf("%s not reported: %v", want, counts)
		}
	}
}

// TestScannerAllocatesNothingPerFrame is the performance property that lets the
// guard be exhaustive instead of sampled. An earlier map[string]any-based scan
// of a 102,892-frame recording was killed after ~20 minutes at 13 GB RSS; that
// cost was materialization, and this test is what keeps it from coming back.
func TestScannerAllocatesNothingPerFrame(t *testing.T) {
	plan := sessionPlan(t)
	payload := []byte(`{"sessionid":"s","game_clock":123.5,"disc":{"position":[1.5,2.5,3.5],` +
		`"velocity":[0,0,0],"bounce_count":2},"teams":[{"team":"BLUE","players":[` +
		`{"name":"p","playerid":1,"stunned":false,"lhand":{"pos":[1,2,3]}}]}]}`)
	counts := map[string]int{}
	var sc scanner
	got := testing.AllocsPerRun(200, func() {
		sc.reset(payload)
		if err := sc.scanObject(plan.root, counts); err != nil {
			t.Fatal(err)
		}
	})
	if got > 0 {
		t.Errorf("scanning one frame allocated %v object(s); the exhaustive scan of a 100k-frame "+
			"recording depends on this being zero", got)
	}
	if len(counts) != 0 {
		t.Fatalf("clean payload produced findings: %v", counts)
	}
}

func TestTabField(t *testing.T) {
	line := []byte("2025-08-02T00:53:15.123\t{\"a\":1}\t {\"b\":2}")
	if got := string(tabField(line, 0)); got != "2025-08-02T00:53:15.123" {
		t.Errorf("part 0 = %q", got)
	}
	if got := string(tabField(line, 1)); got != `{"a":1}` {
		t.Errorf("part 1 = %q", got)
	}
	if got := string(tabField(line, 2)); got != `{"b":2}` {
		t.Errorf("part 2 = %q (leading space must be trimmed, as the codec does)", got)
	}
	if got := tabField(line, 3); got != nil {
		t.Errorf("missing part = %q, want nil", got)
	}
}

// TestKeyScanSummaryStatesExhaustiveness pins both halves of the receipt's
// honesty: a scan that RAN states its coverage, and a scan that did not run
// never borrows the word "exhaustive" to describe itself.
func TestKeyScanSummaryStatesExhaustiveness(t *testing.T) {
	ran := KeyScanResult{FramesTotal: 10, FramesScanned: 10, ZipMembers: 1, ExpectedFields: 3, Ran: true}
	if !strings.Contains(ran.Summary(), "10/10") || !strings.Contains(ran.Summary(), "exhaustive") {
		t.Errorf("summary does not state coverage: %q", ran.Summary())
	}
	var never KeyScanResult
	if strings.Contains(never.Summary(), "exhaustive") {
		t.Errorf("a scan that never ran describes itself as exhaustive: %q", never.Summary())
	}
}

// TestScannerSeparatesZeroValuedUnknownKeys pins what an unknown key costs. A
// key the proto has no field for is discarded at read time, but if its value
// is null, false, 0, "" or an empty composite the discard loses nothing: proto3
// cannot tell those from absent anyway. Real recordings carry exactly that —
// older recorders write the unused spelling of a field as null next to the
// populated one (head.pos: null beside head.position) — and failing them would
// block archiving files that lost nothing. Any non-zero value is still loss.
func TestScannerSeparatesZeroValuedUnknownKeys(t *testing.T) {
	plan := sessionPlan(t)
	payload := `{"z_null": null, "z_false": false, "z_zero": 0, "z_negzero": -0.0e5,
	  "z_str": "", "z_arr": [ ], "z_obj": { },
	  "nz_true": true, "nz_num": 0.5, "nz_str": "x", "nz_arr": [0], "nz_obj": {"a": null}}`
	var sc scanner
	sc.nonzero = map[string]int{}
	counts := map[string]int{}
	sc.reset([]byte(payload))
	if err := sc.scanObject(plan.root, counts); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if len(counts) != 12 {
		t.Fatalf("every unknown key must be counted, zero or not: got %d (%v)", len(counts), counts)
	}
	for p := range counts {
		name := p[strings.LastIndex(p, ".")+1:]
		wantNonZero := 0
		if strings.HasPrefix(name, "nz_") {
			wantNonZero = 1
		}
		if got := sc.nonzero[p]; got != wantNonZero {
			t.Errorf("%s: nonzero=%d, want %d", p, got, wantNonZero)
		}
	}
}

// TestZeroValuedUnknownKeysDoNotFailTheVerdict pins the other half: the key is
// still reported, but only a non-zero occurrence is a reason to keep the
// original.
func TestZeroValuedUnknownKeysDoNotFailTheVerdict(t *testing.T) {
	base := func(f KeyFinding) *Verdict {
		v := &Verdict{KeyScan: KeyScanResult{Ran: true, ZipMembers: 1, Findings: []KeyFinding{f}}}
		v.Classify(nil)
		v.MarkComplete()
		return v
	}
	if r := base(KeyFinding{Path: "SessionResponse.contested", Count: 232}).FailureReasons(); len(r) != 0 {
		t.Errorf("a key that was always zero-valued failed the verdict: %v", r)
	}
	r := base(KeyFinding{Path: "SessionResponse.contested", Count: 232, NonZero: 3}).FailureReasons()
	if len(r) != 1 || !strings.Contains(r[0], "SessionResponse.contested") {
		t.Errorf("a key carrying non-zero values must fail and name itself, got %v", r)
	}
}
