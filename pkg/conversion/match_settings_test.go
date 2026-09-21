package conversion

import (
	"fmt"
	"path/filepath"
	"regexp"
	"testing"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
)

var (
	reRulesBy    = regexp.MustCompile(`"rules_changed_by":"[^"]*"`)
	reRulesAt    = regexp.MustCompile(`"rules_changed_at":[0-9]+`)
	reRoundCount = regexp.MustCompile(`"total_round_count":[0-9]+`)
)

// TestMatchSettingsChangesRoundTrip pins that rules_changed_by,
// rules_changed_at and total_round_count survive a change mid-capture. They
// were header-only, so every frame after a change came back with the
// capture-start value — measured on 3 of 14 real recordings (gh #59). The
// edited sample reproduces all three shapes seen in the wild: the rules editor
// changes (by), rules_changed_at ticks every frame while an edit is open, and
// the round count changes later.
func TestMatchSettingsChangesRoundTrip(t *testing.T) {
	t.Parallel()
	src := filepath.Join("..", "..", "testdata", "sample.echoreplay")
	dst := filepath.Join(t.TempDir(), "settings.echoreplay")

	frame := -1
	rebuildEchoReplay(t, src, dst, 1, func(p []byte) ([]byte, int) {
		frame++
		switch {
		case frame >= 300 && frame < 310: // an open rules edit: ticks every frame
			p = reRulesBy.ReplaceAll(p, []byte(`"rules_changed_by":"Editor"`))
			p = reRulesAt.ReplaceAll(p, []byte(fmt.Sprintf(`"rules_changed_at":%d`, 600000000+frame)))
		case frame >= 310:
			p = reRulesBy.ReplaceAll(p, []byte(`"rules_changed_by":"Editor"`))
			p = reRulesAt.ReplaceAll(p, []byte(`"rules_changed_at":600000309`))
		}
		if frame >= 700 {
			p = reRoundCount.ReplaceAll(p, []byte(`"total_round_count":5`))
		}
		return p, 1
	})

	v, err := VerifyEchoReplayRoundTrip(dst, VerifyOptions{Allowlist: KnownUnpreserved})
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !v.Pass() {
		t.Fatalf("settings that change mid-capture did not round-trip: %v", v.FailureReasons())
	}

	// One event per frame on which any setting changed: frames 300..309 (the
	// first differs from the header's value, then it ticks), 310 does not
	// change (same values as 309), and 700.
	tape := filepath.Join(t.TempDir(), "settings.tape")
	if _, err := ConvertFile(dst, tape); err != nil {
		t.Fatalf("convert: %v", err)
	}
	r, err := codec.NewReader(tape)
	if err != nil {
		t.Fatalf("open tape: %v", err)
	}
	defer r.Close() //nolint:errcheck // read-only
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("read header: %v", err)
	}
	var at []uint32
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break
		}
		for _, e := range f.GetEchoArena().GetEvents() {
			if _, ok := e.GetEvent().(*capturepb.EchoEvent_MatchSettingsChanged); ok {
				at = append(at, f.GetFrameIndex())
			}
		}
	}
	want := []uint32{300, 301, 302, 303, 304, 305, 306, 307, 308, 309, 700}
	if fmt.Sprint(at) != fmt.Sprint(want) {
		t.Errorf("MatchSettingsChanged on frames %v, want %v", at, want)
	}
}
