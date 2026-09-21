package main

import (
	"archive/zip"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// verify_test.go — `tapedeck verify` is the production caller of the round-trip
// verification.
//
// Before this, VerifyEchoReplayRoundTrip was called from _test.go files only:
// the check was callable, not called. `tapedeck verify` ran a hand-written list
// of event-shape checks instead, which say nothing about whether the recording
// survived the conversion field for field — the question someone asks before
// deleting the original.

// TestVerifyCommandRunsTheRoundTripVerification pins that the CLI's verdict IS
// the fidelity verdict: it quotes the receipt (identity, frame counts, key
// scan, per-root coverage) and passes the committed sample, whose only
// differences are the documented last_throw hole.
func TestVerifyCommandRunsTheRoundTripVerification(t *testing.T) {
	src := filepath.Join("..", "..", "testdata", "sample.echoreplay")

	var out bytes.Buffer
	cmd := newVerifyCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{src})
	err := cmd.Execute()
	got := out.String()

	if err != nil {
		t.Fatalf("verify failed on the committed sample: %v\n%s", err, got)
	}
	for _, want := range []string{
		"FIDELITY PASS",
		"sha256=",
		"keyscan=",
		"SessionResponse: compared 1023 pair(s), reached 138/138",
		"PlayerBonesResponse:",
		"allowed    SessionResponse.last_throw",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("verify output does not quote %q:\n%s", want, got)
		}
	}
}

// withUnknownKey copies src with a JSON key the proto cannot represent injected
// into the first frame line. The reader discards such a key, so only the raw
// key scan can see it: a verify that passes this file would authorize deleting
// data the tape never held.
func withUnknownKey(t *testing.T, src string) string {
	t.Helper()
	return editRecording(t, src, func(i int, line []byte) []byte {
		if i != 0 {
			return line
		}
		return bytes.Replace(line, []byte("\t{"), []byte("\t{\"unknown_future_field\":1,"), 1)
	})
}

// editRecording copies a zipped .echoreplay with edit applied to each frame
// line (i is the line ordinal) and returns the copy's path.
func editRecording(t *testing.T, src string, edit func(i int, line []byte) []byte) string {
	t.Helper()
	zr, err := zip.OpenReader(src)
	if err != nil {
		t.Fatalf("open %s: %v", src, err)
	}
	defer zr.Close() //nolint:errcheck // read-only
	member, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open member: %v", err)
	}
	body, err := io.ReadAll(member)
	_ = member.Close()
	if err != nil {
		t.Fatalf("read member: %v", err)
	}
	lines := bytes.Split(bytes.TrimRight(body, "\r\n"), []byte("\n"))
	for i, l := range lines {
		lines[i] = edit(i, bytes.TrimRight(l, "\r"))
	}

	dst := filepath.Join(t.TempDir(), "edited.echoreplay")
	f, err := os.Create(dst)
	if err != nil {
		t.Fatalf("create %s: %v", dst, err)
	}
	zw := zip.NewWriter(f)
	w, err := zw.Create(zr.File[0].Name)
	if err != nil {
		t.Fatalf("zip create: %v", err)
	}
	if _, err := w.Write(append(bytes.Join(lines, []byte("\n")), '\n')); err != nil {
		t.Fatalf("zip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("zip close: %v", err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", dst, err)
	}
	return dst
}

// TestVerifyCommandRejectsNonEchoReplay keeps the existing input contract.
func TestVerifyCommandRejectsNonEchoReplay(t *testing.T) {
	var out bytes.Buffer
	cmd := newVerifyCommand()
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{filepath.Join("..", "..", "testdata", "sample.tape.golden")})
	if err := cmd.Execute(); err == nil {
		t.Errorf("verify accepted a non-.echoreplay input:\n%s", out.String())
	}
}

// TestVerifyCommandExitsNonZero proves the process — not just the RunE return —
// fails on a file that lost data, because that exit code is what a deletion
// script reads.
func TestVerifyCommandExitsNonZero(t *testing.T) {
	if testing.Short() {
		t.Skip("builds the CLI")
	}
	src := withUnknownKey(t, filepath.Join("..", "..", "testdata", "sample.echoreplay"))
	out, err := runTapedeck(t, "verify", src)
	if err == nil {
		t.Errorf("tapedeck verify exited 0 on a file carrying a key the tape cannot hold:\n%s", out)
	}
	for _, want := range []string{"FIDELITY FAIL", "SessionResponse.unknown_future_field"} {
		if !strings.Contains(out, want) {
			t.Errorf("tapedeck verify output does not quote %q:\n%s", want, out)
		}
	}
}
