package fidelity_test

import (
	"testing"

	"github.com/echotools/tape/v4/pkg/fidelity"
)

// TestAllowlistMatchesExactPathsOnly pins the one thing the allowlist must get
// right, because any unexcused difference between source and reconstruction is
// an error: an entry excuses THE PATH IT NAMES and nothing beneath it.
//
// It previously absorbed whole subtrees, so "SessionResponse.last_throw has no
// v2 home" also excused a reconstruction that returned arm_speed=999 for a
// recorded 12.5. A wrong value is not the documented hole; it is a different
// defect wearing the hole's excuse. 52 of 138 SessionResponse paths were
// unfailable that way.
//
// The kind suffixes (#presence, #count) are the SAME path in a different
// failure mode, not a child of it, so an entry covers them.
func TestAllowlistMatchesExactPathsOnly(t *testing.T) {
	a := fidelity.Allowlist{
		"SessionResponse.last_throw":    "documented",
		"SessionResponse.teams[]#count": "documented",
	}
	covered := []string{
		"SessionResponse.last_throw",
		"SessionResponse.last_throw#presence",
		"SessionResponse.last_throw#count",
		"SessionResponse.teams[]#count",
	}
	for _, p := range covered {
		if _, ok := a.Reason(p); !ok {
			t.Errorf("%s should be covered by the allowlist", p)
		}
	}
	notCovered := []string{
		// The defect: sub-fields of an allowlisted message. A value present on
		// both sides and DIFFERENT is a loss nobody ruled on.
		"SessionResponse.last_throw.arm_speed",
		"SessionResponse.last_throw.total_speed#presence",
		"SessionResponse.last_throw.player_name",
		// Never bleed onto a name that merely starts the same way.
		"SessionResponse.last_throw_extra",
		"SessionResponse.last_score",
		"SessionResponse.teams[]",
		"SessionResponse.teams[].team_name",
		"SessionResponse.teams[]#presence",
	}
	for _, p := range notCovered {
		if r, ok := a.Reason(p); ok {
			t.Errorf("%s must NOT be excused by the allowlist (matched %q)", p, r)
		}
	}
}

func TestNilAllowlistExcusesNothing(t *testing.T) {
	var a fidelity.Allowlist
	if _, ok := a.Reason("SessionResponse.anything"); ok {
		t.Fatal("a nil allowlist excused a difference")
	}
}
