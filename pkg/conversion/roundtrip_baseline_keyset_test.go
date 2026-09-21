package conversion

import (
	"archive/zip"
	"bufio"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echotools/tape/v4/pkg/fidelity"
)

// --- the key-set completeness guard, exercised on real recordings -------------
//
// The guard itself lives in pkg/fidelity (keyscan.go) because it is part of the
// per-file verification a caller runs before trusting a converted archive, not
// a test helper. These tests are its red and green on real bytes.
//
// It exists because the round-trip audit is blind by construction: it compares
// frames parsed from the ORIGINAL against frames parsed from the REWRITE, and
// the echoreplay reader is built with
// protojson.UnmarshalOptions{DiscardUnknown: true} (pkg/codec/echoreplay.go).
// A JSON key the proto has no field for is dropped AT READ TIME, so it is
// absent from BOTH sides of the comparison and the audit calls the file
// LOSSLESS. Reproduced: a real recording rebuilt with "unknown_future_field"
// injected into all 197 session objects audited LOSSLESS with 0 mismatches.

const testInjectedKey = `"unknown_future_field":12345,`

// TestSessionKeySetGuard_FlagsInjectedUnknownKey is the guard's red: a
// recording that carries a key the proto has no field for must be reported,
// with the key's name and occurrence count.
func TestSessionKeySetGuard_FlagsInjectedUnknownKey(t *testing.T) {
	src := "../../testdata/sample.echoreplay"
	dst := filepath.Join(t.TempDir(), "sample_unknownkey.echoreplay")
	injected := injectPayloadKey(t, src, dst, 1, testInjectedKey)
	if injected == 0 {
		t.Fatalf("fixture built with 0 injections")
	}

	res, err := fidelity.ScanEchoReplayKeys(dst)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if len(res.Findings) == 0 {
		t.Fatalf("guard reported no findings on a file carrying %d copies of %s; "+
			"the audit cannot see content the codec discards at read time",
			injected, testInjectedKey)
	}

	var total int
	var named bool
	for _, f := range res.Findings {
		total += f.Count
		if strings.HasSuffix(f.Path, ".unknown_future_field") {
			named = true
		}
	}
	if !named {
		t.Errorf("findings do not name unknown_future_field: %v", res.Findings)
	}
	if total != injected {
		t.Errorf("occurrence count = %d, want %d (one per injected session object)", total, injected)
	}
	t.Logf("RED: %d frames scanned, findings: %s", res.FramesScanned, res.Summary())
}

// TestBonesKeySetGuard_FlagsInjectedUnknownKey covers parts[2]. The bones
// payload is unmarshaled with the SAME DiscardUnknown unmarshaler as the
// session (pkg/codec/echoreplay.go parseFrameLine), so an unknown key there is
// exactly as invisible — and until this test it was not checked at all.
func TestBonesKeySetGuard_FlagsInjectedUnknownKey(t *testing.T) {
	src := "../../testdata/sample.echoreplay"
	dst := filepath.Join(t.TempDir(), "sample_bones_unknownkey.echoreplay")
	injected := injectPayloadKey(t, src, dst, 2, testInjectedKey)
	if injected == 0 {
		t.Fatalf("fixture built with 0 bones injections — does the sample carry a bones payload?")
	}

	res, err := fidelity.ScanEchoReplayKeys(dst)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	want := "PlayerBonesResponse.unknown_future_field"
	var got int
	for _, f := range res.Findings {
		if f.Path == want {
			got = f.Count
		}
	}
	if got != injected {
		t.Fatalf("bones guard reported %s x%d, want x%d; findings: %v", want, got, injected, res.Findings)
	}
	t.Logf("RED (bones): %s", res.Summary())
}

// TestBonesKeySetGuard_FlagsNestedUnknownKey pins the nested level of the bones
// payload: user_bones[] is where the per-player data lives.
func TestBonesKeySetGuard_FlagsNestedUnknownKey(t *testing.T) {
	src := "../../testdata/sample.echoreplay"
	dst := filepath.Join(t.TempDir(), "sample_bones_nested.echoreplay")
	injected := injectAfterArrayOpen(t, src, dst, 2, `"user_bones":[`, testInjectedKey)
	if injected == 0 {
		t.Fatalf("fixture built with 0 nested bones injections")
	}
	res, err := fidelity.ScanEchoReplayKeys(dst)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	want := "PlayerBonesResponse.user_bones[].unknown_future_field"
	var named bool
	for _, f := range res.Findings {
		if f.Path == want {
			named = true
		}
	}
	if !named {
		t.Fatalf("nested bones key not reported as %s; findings: %v", want, res.Findings)
	}
	t.Logf("RED (nested bones): %s", res.Summary())
}

// TestSessionKeySetGuard_FlagsNestedUnknownKey pins the nesting requirement: a
// top-level-only check would be worthless, because the levels that carry the
// per-team and per-player data are exactly the ones worth not losing.
func TestSessionKeySetGuard_FlagsNestedUnknownKey(t *testing.T) {
	for _, tc := range []struct {
		name     string
		marker   string
		wantPath string
	}{
		{"team", `"teams":[`, "SessionResponse.teams[].unknown_future_field"},
		{"team_member", `"players":[`, "SessionResponse.teams[].players[].unknown_future_field"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src := "../../testdata/sample.echoreplay"
			dst := filepath.Join(t.TempDir(), "sample_nestedkey.echoreplay")
			injected := injectAfterArrayOpen(t, src, dst, 1, tc.marker, testInjectedKey)
			if injected == 0 {
				t.Fatalf("fixture built with 0 nested injections at %s", tc.marker)
			}

			res, err := fidelity.ScanEchoReplayKeys(dst)
			if err != nil {
				t.Fatalf("guard: %v", err)
			}
			var named bool
			for _, f := range res.Findings {
				if f.Path == tc.wantPath {
					named = true
				}
			}
			if !named {
				t.Fatalf("nested key at %s not reported as %s; findings: %v", tc.marker, tc.wantPath, res.Findings)
			}
			t.Logf("RED (nested %s): %s", tc.name, res.Summary())
		})
	}
}

// TestKeySetGuard_UnparseableFrameIsCounted is the red for the second lane. A
// frame whose session JSON does not parse is dropped by the codec
// (pkg/codec/echoreplay.go increments skippedFrames and continues), so it is
// absent from BOTH sides of the comparison — exactly like an unknown key. The
// guard has to see it, and the audit has to fail on it.
func TestKeySetGuard_UnparseableFrameIsCounted(t *testing.T) {
	src := "../../testdata/sample.echoreplay"
	dst := filepath.Join(t.TempDir(), "sample_broken.echoreplay")
	broken := 0
	rebuildEchoReplay(t, src, dst, 1, func(session []byte) ([]byte, int) {
		if broken > 0 {
			return session, 0
		}
		broken++
		return []byte(`{"sessionid": "unterminated`), 1
	})

	res, err := fidelity.ScanEchoReplayKeys(dst)
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if res.ParseErrors != 1 {
		t.Fatalf("guard reported %d unparseable payload(s), want 1: %s", res.ParseErrors, res.Summary())
	}
	if !strings.Contains(res.Summary(), "json-unparseable=1") {
		t.Errorf("summary does not surface the unparseable frame: %q", res.Summary())
	}
	t.Logf("RED (unparseable): %s", res.Summary())
}

// TestSessionKeySetGuard_CleanOnRealRecording is the guard's green: the
// committed real recording must report zero findings, so the guard fails only
// on files that actually carry unrepresentable content.
func TestSessionKeySetGuard_CleanOnRealRecording(t *testing.T) {
	res, err := fidelity.ScanEchoReplayKeys("../../testdata/sample.echoreplay")
	if err != nil {
		t.Fatalf("guard: %v", err)
	}
	if len(res.Findings) != 0 {
		t.Fatalf("guard flagged an unmodified real recording: %s", res.Summary())
	}
	if res.ParseErrors != 0 {
		t.Fatalf("guard reported unparseable payloads in an unmodified real recording: %s", res.Summary())
	}
	if res.FramesScanned != res.FramesTotal || res.FramesTotal == 0 {
		t.Fatalf("scan was not exhaustive: %s", res.Summary())
	}
	t.Logf("GREEN: %s", res.Summary())
}

// --- fixture builders --------------------------------------------------------

// injectPayloadKey rebuilds src at dst with raw inserted directly after the
// opening brace of every object in TSV field `part`, and returns the number of
// insertions.
func injectPayloadKey(t *testing.T, src, dst string, part int, raw string) int {
	t.Helper()
	return rebuildEchoReplay(t, src, dst, part, func(payload []byte) ([]byte, int) {
		p := bytes.TrimLeft(payload, " ")
		if len(p) == 0 || p[0] != '{' {
			return payload, 0
		}
		out := make([]byte, 0, len(p)+len(raw))
		out = append(out, '{')
		out = append(out, raw...)
		out = append(out, p[1:]...)
		return out, 1
	})
}

// injectAfterArrayOpen rebuilds src at dst with raw inserted into the first
// object of the array named by marker (e.g. `"teams":[`), exercising the
// nested levels of the guard.
func injectAfterArrayOpen(t *testing.T, src, dst string, part int, markerStr, raw string) int {
	t.Helper()
	marker := []byte(markerStr)
	return rebuildEchoReplay(t, src, dst, part, func(payload []byte) ([]byte, int) {
		i := bytes.Index(payload, marker)
		if i < 0 {
			return payload, 0
		}
		j := i + len(marker)
		if j >= len(payload) || payload[j] != '{' {
			return payload, 0
		}
		out := make([]byte, 0, len(payload)+len(raw))
		out = append(out, payload[:j+1]...)
		out = append(out, raw...)
		out = append(out, payload[j+1:]...)
		return out, 1
	})
}

// rebuildEchoReplay copies src to dst line for line, applying edit to TSV field
// `part` of each line. The zip member keeps its original name so the codec's
// member-selection logic resolves it identically.
func rebuildEchoReplay(t *testing.T, src, dst string, part int, edit func([]byte) ([]byte, int)) int {
	t.Helper()

	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer func() { _ = zr.Close() }()

	member := pickReplayMember(t, zr, src)
	rc, err := member.Open()
	if err != nil {
		t.Fatalf("open member: %v", err)
	}
	defer func() { _ = rc.Close() }()

	out, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	defer func() { _ = out.Close() }()

	zw := zip.NewWriter(out)
	w, err := zw.Create(member.Name)
	if err != nil {
		t.Fatalf("create member: %v", err)
	}

	sc := bufio.NewScanner(rc)
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	edits := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		parts := bytes.Split(line, []byte("\t"))
		if len(parts) > part {
			edited, n := edit(parts[part])
			parts[part] = edited
			edits += n
		}
		if _, err := w.Write(bytes.Join(parts, []byte("\t"))); err != nil {
			t.Fatalf("write line: %v", err)
		}
		if _, err := io.WriteString(w, "\n"); err != nil {
			t.Fatalf("write newline: %v", err)
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("close zip: %v", err)
	}
	return edits
}

func pickReplayMember(t *testing.T, zr *zip.ReadCloser, name string) *zip.File {
	t.Helper()
	f, err := fidelity.ReplayMember(&zr.Reader, name)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	return f
}
