package conversion

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	telemetryv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/echotools/tape/v4/pkg/fidelity"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// fidelity.go — per-FILE round-trip verification: did THIS recording survive
// echoreplay -> v2 -> echoreplay, completely?
//
// This is ordinary library code, not a test helper, on purpose. The question it
// answers is whether an irreplaceable recording can be deleted in favour of its
// converted form, so the check has to be callable at conversion time and its
// verdict has to be a structured receipt. A check that only runs inside `go
// test` is deletable, skippable, and can be walked past with a strict=false
// flag — all three of which already happened to the fidelity checks in this
// package.
//
// The comparison itself is pkg/fidelity: a descriptor-driven differ that walks
// every field of every message, so nothing here enumerates fields by hand.
// What this file owns is POLICY:
//
//   - which messages an .echoreplay frame line actually carries (VerifiedFrameFields),
//   - how to pair repeated elements across a reconstruction that reorders and
//     omits them (echoListKeys),
//   - which float loss mechanisms are physical rather than a defect (echoTolerance),
//   - and KnownUnpreserved: the single allowlist of accepted loss.

// Two float tolerances, by error mechanism:
//
//   - magTol bounds positions, velocities, the display clock, shoulder input,
//     packet loss and every other float64 leaf. These are a plain
//     float64->float32 narrowing. At the ±40 coordinate range a float32 ULP is
//     ~4 µm, so observed error is ~1e-5; the bound is generous.
//   - orientTol bounds forward/left/up basis vectors. v2 stores orientation as a
//     quaternion (a proper rotation). The source basis is written to ~3 decimals
//     (e.g. -0.461), so it is slightly non-orthonormal; the quaternion captures
//     the nearest true rotation and the round-trip returns the *orthonormalized*
//     basis. That residual — not float32 storage — sets the bound. This loss is
//     introduced by the v2 forward mapping (mapping.go), not by reconstruction.
//
// Neither tolerance applies to a `float` (32-bit) leaf: bone_t/bone_o are
// already float32 in the source, nothing narrows them, and they round-trip
// exactly. Giving them a tolerance would be a licence to be wrong.
const (
	magTol    = 1e-3
	orientTol = 5e-3
)

// KnownUnpreserved is the explicit allowlist of field paths that are KNOWN not
// to survive echoreplay -> v2 -> echoreplay, each with the reason the loss is
// accepted. docs/format-design.md §4 is the record of those decisions.
//
// A difference on a path in this map is reported but does not fail. A
// difference on ANY other path FAILS: a gate that cannot fail on the loss it
// detects is not a gate.
//
// Keys are field PATHS in the differ's own path language, and a key covers
// EXACTLY the path it names — plus that path's #presence / #count forms, which
// are the same field failing in a different way. It does NOT cover sub-fields,
// so each sub-field needs its own entry: an entry for a missing message must not
// also excuse a reconstruction that returns a WRONG VALUE for one of its fields.
//
// When a field gains a v2 home and round-trips, DELETE its entry — the gate then
// holds that field forever after. A new entry is a new documented hole in the
// archive format and needs its reason recorded in format-design §4 first.
var KnownUnpreserved = lastThrowAllowlist()

// lastThrowReason is why last_throw is accepted loss. It is also the reason
// every one of its sub-fields is listed: the only correct reconstruction of
// last_throw is none at all.
const lastThrowReason = "last_throw is the recording client's own last throw; server-recorded " +
	"captures never carry it, so it is out of scope and must never be reconstructed from " +
	"DiscThrown (format-design §4, gh #35)"

// lastThrowAllowlist lists SessionResponse.last_throw and each of its fields by
// name, derived from the schema so a field added to LastThrowInfo is covered by
// the same decision rather than failing every file.
func lastThrowAllowlist() fidelity.Allowlist {
	allow := fidelity.Allowlist{"SessionResponse.last_throw": lastThrowReason}
	fields := (&enginev1.LastThrowInfo{}).ProtoReflect().Descriptor().Fields()
	for i := range fields.Len() {
		allow["SessionResponse.last_throw."+string(fields.Get(i).Name())] = lastThrowReason
	}
	return allow
}

// VerifiedFrameFields classifies EVERY field of telemetry.v1
// LobbySessionStateFrame, which is what the echoreplay reader produces from one
// frame line. A field is either verified by the round-trip comparison or
// explicitly recorded as not being file content — there is no third category,
// and TestFrameFieldsAreClassified fails the day a field appears that is in
// neither map. That is how "everything is compared" stays true after someone
// edits the proto.
var VerifiedFrameFields = map[string]string{
	"timestamp":    "frame line field 0 — compared as its own schema root",
	"session":      "frame line field 1 — compared as the SessionResponse root",
	"player_bones": "frame line field 2 — compared as the PlayerBonesResponse root",
}

// NotFileContent are LobbySessionStateFrame fields that no .echoreplay line
// carries: they are assigned or derived by the reader/converter, so comparing
// them would measure this repository's own enrichment, not the recording.
var NotFileContent = map[string]string{
	"frame_index": "assigned by the reader from line order; not present in the file",
	"events":      "produced by pkg/events detection during conversion; not present in the file",
}

// echoTolerance picks a comparison tolerance from the loss MECHANISM, by field
// kind and name, not from a hand-listed set of paths.
func echoTolerance(fd protoreflect.FieldDescriptor, _ string) float64 {
	if fd.Kind() != protoreflect.DoubleKind {
		return 0 // exact: float32 leaves do not narrow, and nothing else may drift
	}
	switch fd.Name() {
	case "forward", "left", "up", "vr_forward", "vr_left", "vr_up":
		return orientTol
	}
	return magTol
}

// echoEqual attaches custom equality to derived leaves with a measured, bounded
// reconstruction error. Everything else keeps exact or tolerance comparison.
func echoEqual(fd protoreflect.FieldDescriptor, _ string) func(a, b protoreflect.Value) bool {
	if fd.FullName() == "engine.v1.SessionResponse.game_clock_display" {
		return clockDisplayEqual
	}
	return nil
}

// clockDisplayEqual accepts two "MM:SS.CC" clocks at most one centisecond
// apart. game_clock_display is rebuilt from the stored float32 game_clock
// (gameClockDisplay, reconstruct.go), but the engine formats it from internal
// clock bits the printed game_clock does not carry — "227.64" displayed as
// "03:47.63" — so no formula over the stored value is exact. Measured over
// 197,125 real frames, the current formula is off by one centisecond on 2 and
// exact on the rest; every other candidate did worse. Text that is not a clock
// must match exactly.
func clockDisplayEqual(a, b protoreflect.Value) bool {
	x, y := a.String(), b.String()
	if x == y {
		return true
	}
	cx, okx := parseClockDisplay(x)
	cy, oky := parseClockDisplay(y)
	if !okx || !oky {
		return false
	}
	d := cx - cy
	return d >= -1 && d <= 1
}

// parseClockDisplay reads "MM:SS.CC" as centiseconds.
func parseClockDisplay(s string) (int, bool) {
	if len(s) != 8 || s[2] != ':' || s[5] != '.' {
		return 0, false
	}
	mm, err1 := strconv.Atoi(s[0:2])
	ss, err2 := strconv.Atoi(s[3:5])
	cc, err3 := strconv.Atoi(s[6:8])
	if err1 != nil || err2 != nil || err3 != nil {
		return 0, false
	}
	return (mm*60+ss)*100 + cc, true
}

// echoListKeys pairs repeated message elements by identity instead of index.
//
// This is alignment, never field selection. Reconstruction omits empty teams
// and may order teams differently, so teams[1] is not the same team on both
// sides; comparing by index would report every field of every player as
// different and bury the real loss.
var echoListKeys = map[string]fidelity.KeyFunc{
	"engine.v1.SessionResponse.teams":          teamSlotSetKey,
	"engine.v1.Team.players":                   slotNumberKey,
	"engine.v1.PlayerBonesResponse.user_bones": playerIndexKey,
}

var (
	descOnce      sync.Once
	fdTeamPlayers protoreflect.FieldDescriptor
	fdTeamName    protoreflect.FieldDescriptor
	fdSlotNumber  protoreflect.FieldDescriptor
	fdPlayerIndex protoreflect.FieldDescriptor
)

func initDescriptors() {
	descOnce.Do(func() {
		team := (&enginev1.Team{}).ProtoReflect().Descriptor()
		fdTeamPlayers = team.Fields().ByName("players")
		fdTeamName = team.Fields().ByName("team_name")
		member := (&enginev1.TeamMember{}).ProtoReflect().Descriptor()
		fdSlotNumber = member.Fields().ByName("slot_number")
		bones := (&enginev1.UserBones{}).ProtoReflect().Descriptor()
		fdPlayerIndex = bones.Fields().ByName("player_index")
	})
}

// teamSlotSetKey identifies a team by the sorted slot numbers of its players.
// An empty team has no slots, and several can be empty at once (an unoccupied
// orange team beside the spectators), so an empty team is identified by its
// name instead. If a reconstruction lost that name the teams fail to pair,
// which is reported rather than hidden.
func teamSlotSetKey(m protoreflect.Message) string {
	initDescriptors()
	players := m.Get(fdTeamPlayers).List()
	if players.Len() == 0 {
		return "empty:" + m.Get(fdTeamName).String()
	}
	slots := make([]int, 0, players.Len())
	for i := 0; i < players.Len(); i++ {
		slots = append(slots, int(players.Get(i).Message().Get(fdSlotNumber).Int()))
	}
	sort.Ints(slots)
	var b strings.Builder
	for i, s := range slots {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(strconv.Itoa(s))
	}
	return b.String()
}

func slotNumberKey(m protoreflect.Message) string {
	initDescriptors()
	return strconv.FormatInt(m.Get(fdSlotNumber).Int(), 10)
}

func playerIndexKey(m protoreflect.Message) string {
	initDescriptors()
	return strconv.FormatInt(m.Get(fdPlayerIndex).Int(), 10)
}

// --- schemas -----------------------------------------------------------------

var (
	schemaOnce                                  sync.Once
	sessionSchema, bonesSchema, timestampSchema *fidelity.Schema
	schemaErr                                   error
)

func echoSchemas() (session, bones, ts *fidelity.Schema, err error) {
	schemaOnce.Do(func() {
		opts := fidelity.Options{ListKeys: echoListKeys, Tolerance: echoTolerance, Equal: echoEqual}
		sessionSchema, schemaErr = fidelity.New(&enginev1.SessionResponse{}, opts)
		if schemaErr != nil {
			return
		}
		bonesSchema, schemaErr = fidelity.New(&enginev1.PlayerBonesResponse{}, opts)
		if schemaErr != nil {
			return
		}
		timestampSchema, schemaErr = fidelity.New(&timestamppb.Timestamp{}, opts)
	})
	return sessionSchema, bonesSchema, timestampSchema, schemaErr
}

// EchoReplaySchemas returns the comparison plans used to verify an .echoreplay
// round trip: the three messages a frame line carries.
func EchoReplaySchemas() ([]*fidelity.Schema, error) {
	s, b, ts, err := echoSchemas()
	if err != nil {
		return nil, err
	}
	return []*fidelity.Schema{ts, s, b}, nil
}

// VerifyOptions configures VerifyEchoReplayRoundTrip.
type VerifyOptions struct {
	// Allowlist overrides KnownUnpreserved. A nil Allowlist means "no loss is
	// acceptable" — which is what a caller deciding whether to delete an
	// original should be asking.
	Allowlist fidelity.Allowlist
	// WorkDir holds the intermediate .tape and reconstructed .echoreplay. If
	// empty a temporary directory is created and removed.
	WorkDir string
}

// There is deliberately NO option to skip a lane.
//
// There was one — SkipKeyScan, "so a caller that has already scanned the file
// does not pay twice". With it set, a file carrying 1023 discarded JSON keys
// returned FIDELITY PASS and the receipt still printed "keyscan=0/0 frames
// (exhaustive) unknown-keys=0": a claim of exhaustive coverage over a scan that
// never ran. The saving was one pass over the bytes; the cost was a verdict
// that authorizes deleting an irreplaceable recording on the strength of work
// nobody did. The knob is gone, the scan is unconditional, and
// fidelity.KeyScanResult.Ran makes a skipped scan visible and fatal if some
// future caller ever assembles a Verdict by hand.

// The intermediate artifacts a verification produces inside its WorkDir. They
// are named so a caller that supplies a WorkDir can inspect or reuse them —
// notably `tapedeck verify`, which runs its event checks against the tape the
// verification already wrote instead of converting the recording a second time.
const (
	VerifyTapeName           = "verify.tape"
	VerifyReconstructionName = "verify-recon.echoreplay"
)

// VerifyEchoReplayRoundTrip converts one .echoreplay to v2, reconstructs it
// back, and compares the reconstruction against the original EXHAUSTIVELY:
// every frame, every field of every message, no sampling.
//
// It streams both files frame by frame and holds one frame per side at a time,
// so a 100k-frame recording costs no more memory than a 100-frame one. (The
// conversion and reconstruction steps themselves still materialize, which is a
// property of ConvertFile/ReconstructFile, not of this comparison.)
//
// The returned Verdict is the structured result: source identity, coverage,
// every differing field path with counts and an example, and a single
// pass/fail. An error is returned only when the verification could not be
// performed at all; a file that fails to round-trip is a Verdict with
// Pass()==false, not an error.
func VerifyEchoReplayRoundTrip(src string, opts VerifyOptions) (verdict *fidelity.Verdict, err error) {
	sessionS, bonesS, tsS, err := echoSchemas()
	if err != nil {
		return nil, err
	}

	v := &fidelity.Verdict{}
	if idErr := v.Identify(src); idErr != nil {
		return nil, fmt.Errorf("identify %s: %w", src, idErr)
	}

	work := opts.WorkDir
	if work == "" {
		work, err = os.MkdirTemp("", "tape-fidelity-")
		if err != nil {
			return nil, err
		}
		defer func() { err = errors.Join(err, os.RemoveAll(work)) }()
	}
	tapePath := filepath.Join(work, VerifyTapeName)
	reconPath := filepath.Join(work, VerifyReconstructionName)

	if _, convErr := ConvertFile(src, tapePath); convErr != nil {
		return nil, fmt.Errorf("convert %s: %w", src, convErr)
	}
	if _, recErr := ReconstructFile(tapePath, reconPath); recErr != nil {
		return nil, fmt.Errorf("reconstruct %s: %w", src, recErr)
	}

	ks, scanErr := fidelity.ScanEchoReplayKeys(src)
	if scanErr != nil {
		return nil, fmt.Errorf("key-set guard on %s: %w", src, scanErr)
	}
	v.KeyScan = ks

	tsC := tsS.NewComparator()
	sessionC := sessionS.NewComparator()
	bonesC := bonesS.NewComparator()

	var guard lastThrowGuard
	origR, err := codec.NewEchoReplayReader(src)
	if err != nil {
		return nil, fmt.Errorf("open original: %w", err)
	}
	defer func() { err = errors.Join(err, origR.Close()) }()
	reconR, err := codec.NewEchoReplayReader(reconPath)
	if err != nil {
		return nil, fmt.Errorf("open reconstruction: %w", err)
	}
	defer func() { err = errors.Join(err, reconR.Close()) }()

	for {
		a, aerr := readNextFrame(origR)
		b, berr := readNextFrame(reconR)
		if aerr != nil {
			v.Errors = append(v.Errors, fmt.Sprintf("read original frame %d: %v", v.FramesOriginal, aerr))
		}
		if berr != nil {
			v.Errors = append(v.Errors, fmt.Sprintf("read reconstruction frame %d: %v", v.FramesRecon, berr))
		}
		if a == nil && b == nil {
			break
		}
		if a != nil {
			v.FramesOriginal++
		}
		if b != nil {
			v.FramesRecon++
		}
		if a == nil || b == nil {
			// One side ran out. The frame counts already record it; there is
			// nothing to compare it against.
			continue
		}
		guard.observe(b.GetSession())
		label := "frame " + strconv.Itoa(v.FramesOriginal-1)
		tsC.Compare(a.GetTimestamp(), b.GetTimestamp(), label)
		sessionC.Compare(a.GetSession(), b.GetSession(), label)
		bonesC.Compare(contentOrNil(a.GetPlayerBones()), contentOrNil(b.GetPlayerBones()), label)
	}
	v.SkippedFrames = int(origR.SkippedFrames()) + int(reconR.SkippedFrames())
	guard.report(v)

	for _, c := range []*fidelity.Comparator{tsC, sessionC, bonesC} {
		v.Roots = append(v.Roots, fidelity.RootReport{
			Root:        c.RootName(),
			Compared:    c.Compared(),
			SchemaPaths: c.SchemaSize(),
			Reached:     c.Reached(),
			Unreached:   c.Unreached(),
			Diffs:       c.Diffs(),
		})
	}

	allow := opts.Allowlist
	v.Classify(allow)
	// Last, and only here: every lane has now run. Before this call the verdict
	// fails closed, so an abort on any path above can never be mistaken for a
	// clean result.
	v.MarkComplete()
	return v, nil
}

// contentOrNil returns m, or nil when m has no content at all. A bones payload
// of {"user_bones":[],"err_code":0} parses to a present but empty message; it
// carries no data, the tape stores none, and comparing it as present would
// fail a file that lost nothing.
func contentOrNil[M proto.Message](m M) M {
	var zero M
	if proto.Size(m) == 0 {
		return zero
	}
	return m
}

// lastThrowGuard counts reconstructed frames that carry last_throw. Every
// last_throw path is allowlisted because the correct reconstruction is none at
// all; this guard is what stops those entries from excusing a fabricated one.
type lastThrowGuard struct{ fabricated int }

func (g *lastThrowGuard) observe(recon *enginev1.SessionResponse) {
	if recon.HasLastThrow() {
		g.fabricated++
	}
}

func (g *lastThrowGuard) report(v *fidelity.Verdict) {
	if g.fabricated > 0 {
		v.Errors = append(v.Errors, fmt.Sprintf("the reconstruction sets last_throw on %d frame(s); "+
			"it must never be reconstructed (format-design §4)", g.fabricated))
	}
}

// readNextFrame returns the next frame, or nil at end of file.
func readNextFrame(r *codec.EchoReplay) (*telemetryv1.LobbySessionStateFrame, error) {
	f, err := r.ReadFrame()
	if err != nil {
		if errors.Is(err, io.EOF) {
			return nil, nil
		}
		return nil, err
	}
	return f, nil
}
