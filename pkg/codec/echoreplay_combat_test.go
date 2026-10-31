package codec

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"
)

func TestIsCombatMatchType(t *testing.T) {
	t.Parallel()
	tests := []struct {
		matchType string
		want      bool
	}{
		{"Echo_Combat", true},
		{"echo_combat", true},
		{"Echo_Combat_Private", true},
		{"echo_arena", false},
		{"", false},
		{"social_2.0", false},
	}
	for _, tt := range tests {
		if got := isCombatMatchType(tt.matchType); got != tt.want {
			t.Errorf("isCombatMatchType(%q) = %v, want %v", tt.matchType, got, tt.want)
		}
	}
}

// combatSessionInput is protojson-shaped: flat payload_* fields, as the
// SessionResponse marshal writes them.
const combatSessionInput = `{"sessionid":"S","match_type":"Echo_Combat","payload_multiplier":1,` +
	`"payload_checkpoint":2,"payload_distance":12.5,"payload_defenders":1,"payload_speed":3,` +
	`"client_name":"a&b<c>","err_code":0}`

func TestShapeCombatSessionNestsPayload(t *testing.T) {
	t.Parallel()
	out, err := shapeCombatSession([]byte(combatSessionInput))
	if err != nil {
		t.Fatalf("shapeCombatSession: %v", err)
	}
	var obj map[string]any
	if err := json.Unmarshal(out, &obj); err != nil {
		t.Fatalf("shaped session is not JSON: %v", err)
	}
	for k := range obj {
		if len(k) > len("payload_") && k[:len("payload_")] == "payload_" {
			t.Errorf("flat key %q survived the shape", k)
		}
	}
	payload, ok := obj["payload"].(map[string]any)
	if !ok {
		t.Fatalf("payload is %T, want object", obj["payload"])
	}
	want := map[string]any{"multiplier": 1.0, "checkpoint": 2.0, "distance": 12.5, "defenders": 1.0, "speed": 3.0}
	if !reflect.DeepEqual(payload, want) {
		t.Errorf("payload = %v, want %v", payload, want)
	}
	if ev, ok := obj["events"].([]any); !ok || len(ev) != 0 {
		t.Errorf("events = %v, want empty array", obj["events"])
	}
	if !bytes.Contains(out, []byte(`"client_name":"a&b<c>"`)) {
		t.Errorf("string bytes were re-escaped: %s", out)
	}
}

func TestUnnestCombatPayloadInvertsShape(t *testing.T) {
	t.Parallel()
	shaped, err := shapeCombatSession([]byte(combatSessionInput))
	if err != nil {
		t.Fatalf("shapeCombatSession: %v", err)
	}
	flat, err := unnestCombatPayload(shaped)
	if err != nil {
		t.Fatalf("unnestCombatPayload: %v", err)
	}
	var got, want map[string]any
	if err := json.Unmarshal(flat, &got); err != nil {
		t.Fatalf("unnested session is not JSON: %v", err)
	}
	if err := json.Unmarshal([]byte(combatSessionInput), &want); err != nil {
		t.Fatalf("input is not JSON: %v", err)
	}
	// The inverse restores the payload fields and leaves "events" in place; the
	// reader drops unknown keys later.
	want["events"] = []any{}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("unnest(shape(x)) != x\n got: %v\nwant: %v", got, want)
	}
}

func TestUnnestCombatPayloadPassesArenaThrough(t *testing.T) {
	t.Parallel()
	arena := []byte(`{"sessionid":"S","match_type":"echo_arena","payload_checkpoint":0}`)
	out, err := unnestCombatPayload(arena)
	if err != nil {
		t.Fatalf("unnestCombatPayload: %v", err)
	}
	if !bytes.Equal(out, arena) {
		t.Errorf("arena session changed: %s", out)
	}
}

func TestUnnestCombatPayloadRejectsNonObjectPayload(t *testing.T) {
	t.Parallel()
	if _, err := unnestCombatPayload([]byte(`{"payload":3}`)); err == nil {
		t.Fatal("unnestCombatPayload accepted a non-object payload; want an error")
	}
}
