package fidelity_test

import (
	"strings"
	"testing"

	"github.com/echotools/tape/v4/pkg/fidelity"
)

// verdict_test.go — the receipt's own honesty.
//
// A Verdict authorizes deleting an irreplaceable recording. Two properties are
// therefore not negotiable, and both are tested here rather than assumed:
//
//   - it never describes work it did not do, and
//   - it never passes by default. Passing is something a verification has to
//     EARN by running to the end; the zero value of a struct is not a proof.

// TestZeroVerdictFailsClosed pins that the zero value of an exported type whose
// meaning is "this file is safe to delete" does not mean that. A Verdict is a
// receipt for work performed; an unpopulated one is a receipt for nothing.
func TestZeroVerdictFailsClosed(t *testing.T) {
	var v fidelity.Verdict
	if v.Pass() {
		t.Errorf("the zero Verdict passes: an unpopulated struct certifies a file as losslessly converted")
	}
	if n := len(v.FailureReasons()); n == 0 {
		t.Errorf("the zero Verdict gives no reason to distrust it")
	}
	got := v.String()
	if !strings.Contains(got, "FIDELITY FAIL") {
		t.Errorf("the zero Verdict renders as:\n%s\nwant FIDELITY FAIL", got)
	}
	if !strings.Contains(got, "INCOMPLETE") {
		t.Errorf("the zero Verdict does not say it is incomplete:\n%s", got)
	}
	t.Logf("zero verdict: pass=%v reasons=%v", v.Pass(), v.FailureReasons())
}

// TestUnrunKeyScanIsFatal pins the lane D1 exists for: a verdict whose key scan
// did not run cannot pass, whatever else it found. The comparison lanes are
// blind to anything the reader discarded, so without the scan the verdict has
// no basis at all for "nothing was lost".
func TestUnrunKeyScanIsFatal(t *testing.T) {
	var v fidelity.Verdict
	reasons := strings.Join(v.FailureReasons(), "\n")
	if !strings.Contains(reasons, "key-set scan did not run") {
		t.Errorf("a verdict with no key scan does not say so:\n%s", reasons)
	}
}

// TestKeyScanSummaryNeverClaimsWorkItDidNotDo is the D1 receipt lie, directly:
// the summary printed "keyscan=0/0 frames (exhaustive) unknown-keys=0" over a
// scan that never ran. "exhaustive" is a claim about work performed.
func TestKeyScanSummaryNeverClaimsWorkItDidNotDo(t *testing.T) {
	var r fidelity.KeyScanResult
	got := r.Summary()
	if strings.Contains(got, "exhaustive") {
		t.Errorf("a scan that never ran renders as %q: the receipt describes work it did not do", got)
	}
	if !strings.Contains(got, "NOT RUN") {
		t.Errorf("a scan that never ran renders as %q, want it to say so unmistakably", got)
	}
}

// TestAbsentOnBothSidesIsReportedNotFailed pins what an unreached field means
// for ONE file. The walk visits every child of any message present on either
// side, so a field goes unreached only when its parent is absent from both the
// source and the reconstruction on every frame — a recording without bones, a
// server capture without last_throw. There is nothing there to lose: a subtree
// the source has and the reconstruction drops is a presence difference, which
// fails on its own. Differ coverage is proven against the schema by
// TestComparatorReachesAndComparesEverySchemaField, not per file.
func TestAbsentOnBothSidesIsReportedNotFailed(t *testing.T) {
	v := &fidelity.Verdict{
		KeyScan: fidelity.KeyScanResult{Ran: true, ZipMembers: 1},
		Roots: []fidelity.RootReport{{
			Root:        "PlayerBonesResponse",
			Compared:    232,
			SchemaPaths: 5,
			Unreached:   []string{"PlayerBonesResponse.user_bones[]", "PlayerBonesResponse.err_code"},
		}},
	}
	v.Classify(nil)
	v.MarkComplete()
	if r := v.FailureReasons(); len(r) != 0 {
		t.Errorf("fields absent on both sides failed the verdict: %v", r)
	}
	if got := v.String(); !strings.Contains(got, "absent     PlayerBonesResponse.user_bones[]") {
		t.Errorf("the receipt does not list the absent fields:\n%s", got)
	}
}
