package events

import telemetry "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"

// Sensor inspects frames and emits events.
//
// AddFrame is also called with nil to drain: after it returns a non-nil event,
// the detector calls it again with nil until it returns nil (detectEvents in
// events.go). Implementations return queued events first and treat a nil frame
// as "no new frame". Nothing filters nil frames passed to ProcessFrame either,
// so a nil can reach a sensor from the caller too.
type Sensor interface {
	AddFrame(*telemetry.LobbySessionStateFrame) *telemetry.LobbySessionEvent
}

// Resettable is an optional interface that sensors can implement to clear
// their state when the detector is reset between sessions.
type Resettable interface {
	Reset()
}
