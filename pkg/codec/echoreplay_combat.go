package codec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

// Echo Combat /session shape.
//
// The engine writes the payload state nested under "payload" on payload maps
// (Fission, Gauss), with the keys checkpoint, defenders, distance, multiplier
// and speed. The v2 schema holds the same values as flat SessionResponse fields
// (payload_checkpoint and so on), so on combat frames the writer nests them and
// the reader flattens them back. Combat frames also carry an "events" array,
// which is empty until the tape carries EchoCombatFrame.events.
//
// Only combat frames are reshaped. Arena frames pass through unchanged, so the
// arena output stays byte-identical.

// combatPayloadFlat maps each nested payload key to its flat SessionResponse
// (protojson) name.
var combatPayloadFlat = map[string]string{
	"checkpoint": "payload_checkpoint",
	"defenders":  "payload_defenders",
	"distance":   "payload_distance",
	"multiplier": "payload_multiplier",
	"speed":      "payload_speed",
}

// isCombatMatchType reports whether a SessionResponse match_type names an Echo
// Combat match. The engine writes "Echo_Combat"; the arena writer writes
// "echo_arena".
func isCombatMatchType(matchType string) bool {
	return strings.HasPrefix(strings.ToLower(matchType), "echo_combat")
}

// shapeCombatSession rewrites one protojson session object for a combat frame:
// the flat payload_* fields move under "payload", and "events" is set to an
// empty array. The input is the protojson output before the byte fixers run.
// Values are kept as the raw JSON tokens protojson wrote, so numbers and
// strings are not re-encoded.
func shapeCombatSession(in []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(in, &obj); err != nil {
		return nil, fmt.Errorf("codec.shapeCombatSession: decode session: %w", err)
	}
	nested := make(map[string]json.RawMessage, len(combatPayloadFlat))
	for key, flat := range combatPayloadFlat {
		v, ok := obj[flat]
		if !ok {
			continue
		}
		nested[key] = v
		delete(obj, flat)
	}
	if len(nested) > 0 {
		payload, err := marshalNoEscape(nested)
		if err != nil {
			return nil, fmt.Errorf("codec.shapeCombatSession: encode payload: %w", err)
		}
		obj["payload"] = payload
	}
	obj["events"] = json.RawMessage("[]")
	out, err := marshalNoEscape(obj)
	if err != nil {
		return nil, fmt.Errorf("codec.shapeCombatSession: encode session: %w", err)
	}
	return out, nil
}

// unnestCombatPayload is the inverse of shapeCombatSession: a nested "payload"
// object is flattened back into the payload_* fields that SessionResponse
// unmarshals. Input without a top-level "payload" key, which is every arena
// frame, is returned unchanged.
func unnestCombatPayload(in []byte) ([]byte, error) {
	if !bytes.Contains(in, []byte(`"payload"`)) {
		return in, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(in, &obj); err != nil {
		return nil, fmt.Errorf("codec.unnestCombatPayload: decode session: %w", err)
	}
	raw, ok := obj["payload"]
	if !ok {
		return in, nil
	}
	var nested map[string]json.RawMessage
	if err := json.Unmarshal(raw, &nested); err != nil {
		return nil, fmt.Errorf("codec.unnestCombatPayload: payload is not an object: %w", err)
	}
	for key, flat := range combatPayloadFlat {
		if v, ok := nested[key]; ok {
			obj[flat] = v
		}
	}
	delete(obj, "payload")
	out, err := marshalNoEscape(obj)
	if err != nil {
		return nil, fmt.Errorf("codec.unnestCombatPayload: encode session: %w", err)
	}
	return out, nil
}

// marshalNoEscape encodes v as compact JSON without HTML escaping, so that
// characters such as & and < in strings keep their bytes.
func marshalNoEscape(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}
