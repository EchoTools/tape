package conversion

import "testing"

// Every symbol we know of, from every source we have:
//
//	[A] Andrew, verbatim: "in teh actual game egnien tehre is echo_arena,
//	    echo_arena_private, echo_combat, echo_combat_private, etc. as gametype"
//	    and separately combat_tournament / arena_tournament.
//	[T] the Title_Case v1 echoreplay spellings the shipped matchTypeMap held
//	    before it was removed (mapping.go:40-47 @ cba7273).
//	[C] nevr-common proto/api/v1/http_v1.proto:98-101 @ 4e367f3 -- api/v1 `mode`,
//	    "e.g. \"echo_arena_private\"" (lowercase).
//	[F] echo_arena_frenzy -- announced, never observed. The whole point.
var gameTypeCases = []struct {
	src        string
	symbol     string
	mode       string
	private    bool
	tournament bool
	known      bool
}{
	// --- lowercase engine / api-v1 spellings -------------------------------
	{"A/C", "echo_arena", "echo_arena", false, false, true},
	{"A/C", "echo_arena_private", "echo_arena", true, false, true},
	{"A", "echo_combat", "echo_combat", false, false, true},
	{"A", "echo_combat_private", "echo_combat", true, false, true},
	{"A", "echo_arena_tournament", "echo_arena", false, true, true},
	{"A", "echo_combat_tournament", "echo_combat", false, true, true},
	{"A", "arena_tournament", "arena", false, true, true},
	{"A", "combat_tournament", "combat", false, true, true},
	{"T", "echo_arena_ffa", "echo_arena_ffa", false, false, true},
	{"T", "social_2.0", "social_2.0", false, false, true},
	{"T", "social_2.0_private", "social_2.0", true, false, true},
	{"T", "echo_pass", "echo_pass", false, false, true},

	// --- Title_Case v1 echoreplay spellings --------------------------------
	{"T", "Echo_Arena", "echo_arena", false, false, true},
	{"T", "Echo_Arena_Private", "echo_arena", true, false, true},
	{"T", "Echo_Arena_Tournament", "echo_arena", false, true, true},
	{"T", "Echo_Combat", "echo_combat", false, false, true},
	{"T", "Social_2.0", "social_2.0", false, false, true},
	{"T", "Social_2.0_Private", "social_2.0", true, false, true},
	{"T", "Echo_Arena_FFA", "echo_arena_ffa", false, false, true},

	// --- announced but never observed --------------------------------------
	{"F", "echo_arena_frenzy", "echo_arena_frenzy", false, false, true},

	// --- THE PROPERTY THE OLD ENUM COULD NOT HAVE. A mode nobody has ever seen
	//     still yields correct axes. Under MatchType these were
	//     MATCH_TYPE_UNSPECIFIED and every fact was destroyed.
	{"none", "echo_arena_frenzy_private", "echo_arena_frenzy", true, false, true},
	{"none", "echo_dodgeball", "echo_dodgeball", false, false, false},
	{"none", "echo_dodgeball_private", "echo_dodgeball", true, false, false},
	{"none", "echo_dodgeball_tournament", "echo_dodgeball", false, true, false},

	// --- empty asserts nothing ----------------------------------------------
	{"none", "", "", false, false, false},
}

func TestDeriveGameTypeEverySymbolWeKnowOf(t *testing.T) {
	for _, c := range gameTypeCases {
		got := DeriveGameType(c.symbol)
		if got.Symbol != c.symbol {
			t.Errorf("[%s] %q: Symbol = %q, want the input verbatim", c.src, c.symbol, got.Symbol)
		}
		if got.Mode != c.mode {
			t.Errorf("[%s] %q: Mode = %q, want %q", c.src, c.symbol, got.Mode, c.mode)
		}
		if got.Private != c.private {
			t.Errorf("[%s] %q: Private = %v, want %v", c.src, c.symbol, got.Private, c.private)
		}
		if got.Tournament != c.tournament {
			t.Errorf("[%s] %q: Tournament = %v, want %v", c.src, c.symbol, got.Tournament, c.tournament)
		}
		if got.KnownMode != c.known {
			t.Errorf("[%s] %q: KnownMode = %v, want %v", c.src, c.symbol, got.KnownMode, c.known)
		}
	}
	t.Logf("%d symbols derived", len(gameTypeCases))
}

// TestGameTypeCaseFoldSeam asserts a BOUNDARY, not a box: the two spellings that
// arrive from two different producers must AGREE. A Title_Case symbol deriving
// different facts from its lowercase twin loses nothing loudly — it just answers
// wrong.
func TestGameTypeCaseFoldSeam(t *testing.T) {
	pairs := [][2]string{
		{"echo_arena", "Echo_Arena"},
		{"echo_arena_private", "Echo_Arena_Private"},
		{"echo_arena_tournament", "Echo_Arena_Tournament"},
		{"echo_combat", "Echo_Combat"},
		{"social_2.0", "Social_2.0"},
		{"social_2.0_private", "Social_2.0_Private"},
		{"echo_arena_ffa", "Echo_Arena_FFA"},
		{"echo_arena_private", "ECHO_ARENA_PRIVATE"},
	}
	for _, p := range pairs {
		a, b := DeriveGameType(p[0]), DeriveGameType(p[1])
		if a.Mode != b.Mode || a.Private != b.Private || a.Tournament != b.Tournament || a.KnownMode != b.KnownMode {
			t.Errorf("spellings disagree: %q -> %+v  vs  %q -> %+v", p[0], a, p[1], b)
		}
		// ...and the verbatim symbol must NOT be folded, because that is what
		// gets written back to the wire.
		if a.Symbol == b.Symbol {
			t.Errorf("Symbol was folded: both %q and %q returned %q", p[0], p[1], a.Symbol)
		}
	}
}

// TestGameTypeRecoversWhatTheEnumLost is the direction check, and it names the
// two failures separately because they are different.
//
// Measured on the shipped converter before removal (2026-09-07):
//
//	"Echo_Arena_Private"  -> MATCH_TYPE_PRIVATE -> "Echo_Arena_Private"
//	                         round-tripped INTACT; what was lost was
//	                         expressiveness for a v2 consumer, which could not
//	                         tell arena from combat behind MATCH_TYPE_PRIVATE
//	"Echo_Combat_Private" -> MATCH_TYPE_UNSPECIFIED -> ""
//	                         no map entry at all; the value was destroyed
func TestGameTypeRecoversWhatTheEnumLost(t *testing.T) {
	// The v2-expressiveness failure: both were PRIVATE, and the mode is what
	// told them apart. Derivation keeps it.
	arena := DeriveGameType("Echo_Arena_Private")
	combat := DeriveGameType("Echo_Combat_Private")
	if !arena.Private || !combat.Private {
		t.Fatalf("both are private matches: arena=%+v combat=%+v", arena, combat)
	}
	if arena.Mode == combat.Mode {
		t.Errorf("arena and combat derive the same mode %q — this is exactly what "+
			"MATCH_TYPE_PRIVATE could not distinguish", arena.Mode)
	}

	// The outright-destruction failure: Echo_Combat_Private had no entry, so it
	// reversed to "". Here it keeps its symbol and both axes.
	if combat.Symbol != "Echo_Combat_Private" || combat.Mode != "echo_combat" {
		t.Errorf("Echo_Combat_Private: the old table round-tripped this to \"\"; got %+v", combat)
	}
	t.Logf("arena=%+v", arena)
	t.Logf("combat=%+v", combat)
}
