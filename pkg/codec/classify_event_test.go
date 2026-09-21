package codec

import (
	"testing"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
)

// eventTypeGap lists EchoEvent oneof variants that intentionally classify to
// EVENT_TYPE_UNSPECIFIED because telemetry.v2's EventType enum has no value for
// them. Writer.WriteFrame skips UNSPECIFIED, so anything named here never
// reaches the footer's event index and cannot be found by an index scan.
//
// Empty as of 2.1.0: EventType gained EVENT_TYPE_LOADOUT_CHANGED,
// EVENT_TYPE_GRAB_CHANGED and EVENT_TYPE_PLAYER_STATS_UPDATED, closing
// INDEX-001. Every variant is now mappable — keep it that way.
var eventTypeGap = map[string]bool{}

// TestClassifyEventCoversEveryEchoEventVariant walks the EchoEvent oneof from
// the descriptor so a variant added to the proto without a classifyEvent case
// fails here instead of silently vanishing from the event index.
func TestClassifyEventCoversEveryEchoEventVariant(t *testing.T) {
	t.Parallel()

	oneof := (&capturepb.EchoEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	if oneof == nil {
		t.Fatal("EchoEvent has no 'event' oneof")
	}

	fields := oneof.Fields()
	for i := range fields.Len() {
		field := fields.Get(i)
		name := string(field.Name())

		t.Run(name, func(t *testing.T) {
			t.Parallel()

			evt := &capturepb.EchoEvent{}
			// Mutable sets the oneof case with a zero message of the correct
			// concrete type.
			evt.ProtoReflect().Mutable(field)

			got := classifyEvent(evt)
			unspecified := got == capturepb.EventType_EVENT_TYPE_UNSPECIFIED

			switch {
			case eventTypeGap[name] && !unspecified:
				t.Errorf("%s now classifies as %v — EventType gained a value for it; "+
					"remove it from eventTypeGap and drop the INDEX-001 entry", name, got)
			case !eventTypeGap[name] && unspecified:
				t.Errorf("%s classifies as EVENT_TYPE_UNSPECIFIED, so Writer.WriteFrame "+
					"(tape.go:96) drops it from the footer event index; add a case to classifyEvent", name)
			}
		})
	}
}

// TestClassifyEventGapIsStillReal pins the count so the gap list cannot drift
// out of sync with the proto without a test failure.
func TestClassifyEventGapIsStillReal(t *testing.T) {
	t.Parallel()

	oneof := (&capturepb.EchoEvent{}).ProtoReflect().Descriptor().Oneofs().ByName("event")
	if oneof == nil {
		t.Fatal("EchoEvent has no 'event' oneof")
	}

	variants := oneof.Fields().Len()
	mappable := variants - len(eventTypeGap)

	const wantVariants, wantMappable = 29, 29
	if variants != wantVariants || mappable != wantMappable {
		t.Errorf("EchoEvent has %d variants (%d mappable); expected %d and %d. "+
			"The proto changed — reconcile classifyEvent and eventTypeGap",
			variants, mappable, wantVariants, wantMappable)
	}
}
