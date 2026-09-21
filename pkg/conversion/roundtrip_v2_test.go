package conversion_test

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/echotools/tape/v4/pkg/conversion"
)

// TestRoundTripBAC is the acceptance gate on the committed sample: the same
// per-file verification `tapedeck verify` runs — every frame, every field of
// every message, compared by the descriptor-driven differ in pkg/fidelity —
// must pass with only the documented holes in conversion.KnownUnpreserved.
//
// It replaced a hand-written field-by-field comparison that had to be extended
// by hand and had missed fields; the differ walks the schema, so a field added
// to the proto is compared without anyone editing this file.
func TestRoundTripBAC(t *testing.T) {
	verifyRoundTrip(t, filepath.Join("..", "..", "testdata", "sample.echoreplay"))
}

// TestRoundTripBACAudit runs the same verification on an external recording
// when TAPE_AUDIT_FILE is set. `tapedeck verify <file>` is the same check as a
// command.
func TestRoundTripBACAudit(t *testing.T) {
	src := os.Getenv("TAPE_AUDIT_FILE")
	if src == "" {
		t.Skip("set TAPE_AUDIT_FILE=/path/to.echoreplay to run the audit round-trip")
	}
	verifyRoundTrip(t, src)
}

func verifyRoundTrip(t *testing.T, src string) {
	t.Helper()
	if _, err := os.Stat(src); err != nil {
		t.Skipf("no recording: %v", err)
	}
	v, err := conversion.VerifyEchoReplayRoundTrip(src, conversion.VerifyOptions{Allowlist: conversion.KnownUnpreserved})
	if err != nil {
		t.Fatalf("verify %s: %v", src, err)
	}
	t.Logf("%s", v)
	if !v.Pass() {
		t.Fatalf("%s did not round-trip: %v", src, v.FailureReasons())
	}
}
