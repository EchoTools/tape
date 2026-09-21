package fidelity

import (
	"archive/zip"
	"bufio"
	"bytes"
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
)

// keyscan.go — the key-set completeness guard.
//
// WHY IT EXISTS: a round-trip comparison reads a recording, writes it back,
// reads it again, and compares the two parsed frame sets. That comparison
// cannot see anything the READER dropped, because a dropped key is missing from
// both sides. The echoreplay reader is constructed with
// protojson.UnmarshalOptions{DiscardUnknown: true} (pkg/codec/echoreplay.go),
// so every JSON key the proto has no field for is discarded at read time and
// the comparison reports LOSSLESS. Verified: a real recording rebuilt with
// "unknown_future_field" injected into all 197 session objects audited
// LOSSLESS, frames=197, session-mismatch=0, whole-frame-mismatch=0.
//
// So the guard reads the ORIGINAL FILE'S BYTES and checks every JSON key in
// every payload against the generated proto descriptors. The accepted-name set
// is the same descriptor-derived comparison plan the differ walks, in both the
// `name=` and `json=` forms protojson accepts — never a hand-maintained list,
// because a hand list rots and invisible drift is the exact bug this guards.
//
// BOTH payloads are scanned. parts[1] (the session) and parts[2] (the player
// bones) are read with the same DiscardUnknown unmarshaler, so an unknown key
// under bones is exactly as invisible as one under the session.
//
// EXHAUSTIVE, NOT SAMPLED: every frame of every file, no stride. An earlier
// version sampled because scanning a 100k-frame recording by decoding each
// payload into map[string]any took ~20 minutes at 13 GB RSS. That was a
// materialization cost, not an inherent one: this scanner walks the raw bytes
// with a keys-only tokenizer that allocates nothing per frame, so exhaustive is
// affordable and sampling — which can only ever say "we did not look" — is
// gone.
//
// SCOPE: audit-side detection only. It deliberately does not change the codec.
// Making the reader strict (erroring on unknown keys) is a product decision
// with blast radius across every reader of the format.

// KeyFinding is one JSON key present in a recording that the proto cannot
// represent, with the number of objects it appeared in.
type KeyFinding struct {
	// Path is the descriptor path to the offending key, e.g.
	// "SessionResponse.teams[].players[].unknown_future_field".
	Path  string
	Count int
	// NonZero is how many of those occurrences carried a value other than
	// null, false, 0, "", [] or {}. Only these lose data: proto3 cannot tell a
	// zero value from an absent one, so discarding a zero-valued key is lossless.
	NonZero int
}

// KeyScanResult is the guard's verdict for one recording.
type KeyScanResult struct {
	ParseExample string
	// ExtraFieldExample names the first line carrying surplus tab-separated
	// fields, and quotes what was on it.
	ExtraFieldExample string
	Findings          []KeyFinding
	FramesTotal       int // frame lines in the file
	// FramesScanned is frame lines inspected — always all of them.
	FramesScanned int
	// ParseErrors counts payloads that are not parseable JSON at all. Such a
	// payload is dropped by the codec (echoreplay.go increments skippedFrames)
	// and is therefore absent from BOTH sides of any comparison — the same
	// blindness as an unknown key, so it is counted here and it is fatal.
	ParseErrors int
	// ZipMembers is how many members the archive holds. The codec's reader and
	// this scanner both bind exactly ONE (see ReplayMember); every other member
	// is content that is read by nobody and reconstructed by nothing, so more
	// than one is fatal. Measured: every .echoreplay in the chi1 corpus holds
	// exactly one.
	ZipMembers int
	// ExpectedFields is how many tab-separated fields of a frame line the codec
	// actually parses, derived from the payload targets rather than assumed.
	ExpectedFields int
	// ExtraFields counts frame lines carrying MORE than ExpectedFields.
	// pkg/codec's parseFrameLine reads parts[0..2] and silently drops the rest
	// of the line, so a 4th payload is in the file, in no comparison, and in no
	// reconstruction. Measured: every line of every chi1 recording has exactly
	// three.
	ExtraFields int
	// Ran reports whether the scan actually executed. It exists because a
	// receipt must never describe work it did not do: the summary used to print
	// "keyscan=0/0 frames (exhaustive) unknown-keys=0" for a scan that never
	// happened, which reads as proof of absence and is the opposite.
	Ran bool
}

// Summary renders the verdict in the audit's log style. It always states the
// coverage, because a guard that quietly inspects a fraction of a file and
// prints OK is the same blindness the guard exists to remove — and it states
// FIRST whether it ran at all, because "0/0 frames (exhaustive)" over a scan
// that never happened is a lie in the shape of a pass.
func (r KeyScanResult) Summary() string {
	if !r.Ran {
		return "keyscan=NOT RUN — this verdict does not certify key completeness"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "keyscan=%d/%d frames (exhaustive) zip-members=%d fields<=%d surplus-fields=%d",
		r.FramesScanned, r.FramesTotal, r.ZipMembers, r.ExpectedFields, r.ExtraFields)
	if r.ExtraFields > 0 {
		fmt.Fprintf(&b, " (e.g. %s)", r.ExtraFieldExample)
	}
	if r.ParseErrors > 0 {
		fmt.Fprintf(&b, " json-unparseable=%d (e.g. %s)", r.ParseErrors, r.ParseExample)
	}
	if len(r.Findings) == 0 {
		b.WriteString(" unknown-keys=0")
		return b.String()
	}
	fmt.Fprintf(&b, " unknown-keys=%d [", len(r.Findings))
	for i, f := range r.Findings {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s x%d", f.Path, f.Count)
		if f.NonZero == 0 {
			b.WriteString(" (always zero-valued)")
		} else {
			fmt.Fprintf(&b, " (%d non-zero)", f.NonZero)
		}
	}
	b.WriteString("]")
	return b.String()
}

// PayloadTarget binds one tab-separated field of an .echoreplay frame line to
// the comparison plan of the message the codec unmarshals it into.
type PayloadTarget struct {
	Schema *Schema
	// Part is the index of the tab-separated field on the line.
	Part int
}

var (
	keyscanOnce    sync.Once
	keyscanTargets []PayloadTarget
	keyscanErr     error
)

// EchoReplayPayloads is the full set of JSON payloads an .echoreplay frame line
// carries, in the order the codec parses them (pkg/codec/echoreplay.go
// parseFrameLine): parts[0] is the timestamp, parts[1] the session, parts[2]
// the player bones.
func EchoReplayPayloads() ([]PayloadTarget, error) {
	keyscanOnce.Do(func() {
		session, err := New(&enginev1.SessionResponse{}, Options{})
		if err != nil {
			keyscanErr = err
			return
		}
		bones, err := New(&enginev1.PlayerBonesResponse{}, Options{})
		if err != nil {
			keyscanErr = err
			return
		}
		keyscanTargets = []PayloadTarget{
			{Part: 1, Schema: session},
			{Part: 2, Schema: bones},
		}
	})
	return keyscanTargets, keyscanErr
}

// ScanEchoReplayKeys scans every frame of a .echoreplay file and reports every
// JSON key, at every nesting level of every payload, that the corresponding
// proto has no field for.
func ScanEchoReplayKeys(path string) (KeyScanResult, error) {
	targets, err := EchoReplayPayloads()
	if err != nil {
		return KeyScanResult{}, err
	}
	return ScanEchoReplayKeysWith(path, targets)
}

// ScanEchoReplayKeysWith is ScanEchoReplayKeys against caller-supplied payload
// targets.
func ScanEchoReplayKeysWith(path string, targets []PayloadTarget) (KeyScanResult, error) {
	var res KeyScanResult
	counts := map[string]int{}
	sc := scanner{nonzero: map[string]int{}}

	// The expected field count comes from the targets — the same list that says
	// which payloads the codec parses — so it cannot drift from what is read.
	for _, t := range targets {
		if t.Part+1 > res.ExpectedFields {
			res.ExpectedFields = t.Part + 1
		}
	}

	members, err := eachFrameLine(path, func(i int, line []byte) error {
		res.FramesTotal++
		res.FramesScanned++
		if n := bytes.Count(line, []byte("\t")) + 1; n > res.ExpectedFields {
			res.ExtraFields++
			if res.ExtraFieldExample == "" {
				res.ExtraFieldExample = fmt.Sprintf("frame %d has %d fields, surplus starts %q",
					i, n, clip(tabField(line, res.ExpectedFields), 64))
			}
		}
		for _, t := range targets {
			payload := tabField(line, t.Part)
			if len(payload) == 0 {
				continue
			}
			sc.reset(payload)
			if err := sc.scanObject(t.Schema.root, counts); err != nil {
				res.ParseErrors++
				if res.ParseExample == "" {
					res.ParseExample = fmt.Sprintf("frame %d %s: %v", i, t.Schema.Name(), err)
				}
			}
		}
		return nil
	})
	res.ZipMembers = members
	if err != nil {
		return res, err
	}
	res.Ran = true

	paths := make([]string, 0, len(counts))
	for p := range counts {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	for _, p := range paths {
		res.Findings = append(res.Findings, KeyFinding{Path: p, Count: counts[p], NonZero: sc.nonzero[p]})
	}
	return res, nil
}

// clip bounds a quoted example so one pathological line cannot dominate a
// receipt.
func clip(b []byte, n int) string {
	if len(b) <= n {
		return string(b)
	}
	return string(b[:n]) + "…"
}

// tabField returns field n of a tab-separated line, without allocating.
func tabField(line []byte, n int) []byte {
	for range n {
		j := bytes.IndexByte(line, '\t')
		if j < 0 {
			return nil
		}
		line = line[j+1:]
	}
	if j := bytes.IndexByte(line, '\t'); j >= 0 {
		line = line[:j]
	}
	return bytes.TrimSpace(line)
}

// --- keys-only JSON scanner ---------------------------------------------------
//
// The scanner walks the payload's raw bytes and never materializes values: it
// needs object KEYS and structure, nothing else. Values are skipped in place,
// and every path it reports was precomputed when the Schema was built, so a
// frame costs no allocations at all.

type scanner struct {
	// nonzero counts, per unknown-key path, the occurrences whose value is not
	// a proto3 zero value. Nil disables the tally.
	nonzero map[string]int
	b       []byte
	i       int
}

func (s *scanner) reset(b []byte) { s.b, s.i = b, 0 }

func (s *scanner) ws() {
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

func (s *scanner) errAt(what string) error {
	return fmt.Errorf("%s at byte %d", what, s.i)
}

// scanObject expects a JSON object and walks its keys against n's children.
func (s *scanner) scanObject(n *node, counts map[string]int) error {
	s.ws()
	if s.i >= len(s.b) {
		return s.errAt("unexpected end of input, want object")
	}
	if s.b[s.i] == 'n' { // JSON null in a message position
		return s.skipValue()
	}
	if s.b[s.i] != '{' {
		return s.errAt(fmt.Sprintf("want '{' for %s, got %q", n.path, s.b[s.i]))
	}
	s.i++
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == '}' {
		s.i++
		return nil
	}
	for {
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != '"' {
			return s.errAt("want object key")
		}
		key, err := s.stringToken()
		if err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != ':' {
			return s.errAt("want ':'")
		}
		s.i++

		switch child, known := n.byName[string(key)]; {
		case n.wellKnown:
			// Struct/Value/Any legitimately carry arbitrary keys.
			err = s.skipValue()
		case known:
			err = s.scanField(child, counts)
		default:
			path := n.path + "." + string(key)
			counts[path]++
			s.ws()
			start := s.i
			if err = s.skipValue(); err == nil && !isZeroJSON(s.b[start:s.i]) && s.nonzero != nil {
				s.nonzero[path]++
			}
		}
		if err != nil {
			return err
		}

		s.ws()
		if s.i >= len(s.b) {
			return s.errAt("unterminated object")
		}
		switch s.b[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return nil
		default:
			return s.errAt("want ',' or '}'")
		}
	}
}

// scanField descends into a value whose node is known.
func (s *scanner) scanField(n *node, counts map[string]int) error {
	fd := n.fd
	switch {
	case fd.IsMap():
		// A proto map is a JSON object keyed by the map key; its VALUES carry
		// the descriptor, and the keys are data, not field names.
		s.ws()
		if s.i < len(s.b) && s.b[s.i] == '{' && n.children != nil {
			return s.scanMapObject(n, counts)
		}
		return s.skipValue()
	case fd.IsList():
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != '[' {
			return s.skipValue()
		}
		s.i++
		s.ws()
		if s.i < len(s.b) && s.b[s.i] == ']' {
			s.i++
			return nil
		}
		for {
			var err error
			if n.children != nil {
				err = s.scanObject(n, counts)
			} else {
				err = s.skipValue()
			}
			if err != nil {
				return err
			}
			s.ws()
			if s.i >= len(s.b) {
				return s.errAt("unterminated array")
			}
			switch s.b[s.i] {
			case ',':
				s.i++
			case ']':
				s.i++
				return nil
			default:
				return s.errAt("want ',' or ']'")
			}
		}
	case n.children != nil:
		return s.scanObject(n, counts)
	default:
		return s.skipValue()
	}
}

// scanMapObject walks {mapKey: value} where the values carry n's child plan.
func (s *scanner) scanMapObject(n *node, counts map[string]int) error {
	s.i++ // '{'
	s.ws()
	if s.i < len(s.b) && s.b[s.i] == '}' {
		s.i++
		return nil
	}
	for {
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != '"' {
			return s.errAt("want map key")
		}
		if _, err := s.stringToken(); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) || s.b[s.i] != ':' {
			return s.errAt("want ':'")
		}
		s.i++
		if err := s.scanObject(n, counts); err != nil {
			return err
		}
		s.ws()
		if s.i >= len(s.b) {
			return s.errAt("unterminated map object")
		}
		switch s.b[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return nil
		default:
			return s.errAt("want ',' or '}'")
		}
	}
}

// stringToken consumes a JSON string and returns its decoded bytes. The fast
// path — no escape sequences, which is every key a recorder actually emits —
// returns a sub-slice of the input and allocates nothing.
func (s *scanner) stringToken() ([]byte, error) {
	start := s.i
	s.i++ // opening quote
	escaped := false
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case '\\':
			escaped = true
			s.i += 2
			continue
		case '"':
			raw := s.b[start : s.i+1]
			s.i++
			if !escaped {
				return raw[1 : len(raw)-1], nil
			}
			unq, err := strconv.Unquote(string(raw))
			if err != nil {
				return nil, s.errAt("bad string escape")
			}
			return []byte(unq), nil
		default:
			s.i++
		}
	}
	return nil, s.errAt("unterminated string")
}

// skipValue consumes exactly one JSON value.
func (s *scanner) skipValue() error {
	s.ws()
	if s.i >= len(s.b) {
		return s.errAt("unexpected end of input, want value")
	}
	switch c := s.b[s.i]; c {
	case '{', '[':
		return s.skipComposite()
	case '"':
		_, err := s.stringToken()
		return err
	default:
		// A literal: number, true, false or null. Anything else is not JSON,
		// and accepting it would let a corrupt payload pass as parseable.
		if c != '-' && c != '+' && (c < '0' || c > '9') && c != 't' && c != 'f' && c != 'n' {
			return s.errAt(fmt.Sprintf("unexpected %q at start of value", c))
		}
		start := s.i
	literal:
		for s.i < len(s.b) {
			switch s.b[s.i] {
			case ',', '}', ']', ' ', '\t', '\n', '\r':
				break literal
			default:
				s.i++
			}
		}
		tok := s.b[start:s.i]
		if len(tok) == 0 {
			return s.errAt("empty value")
		}
		switch tok[0] {
		case 't':
			if string(tok) != "true" {
				return s.errAt("bad literal " + string(tok))
			}
		case 'f':
			if string(tok) != "false" {
				return s.errAt("bad literal " + string(tok))
			}
		case 'n':
			if string(tok) != "null" {
				return s.errAt("bad literal " + string(tok))
			}
		default:
			if !validNumber(tok) {
				return s.errAt("bad number " + string(tok))
			}
		}
		return nil
	}
}

// isZeroJSON reports whether a complete JSON value is one proto3 would read
// as its zero value: null, false, a number equal to zero, "", or an empty
// array or object. It does not allocate.
func isZeroJSON(v []byte) bool {
	v = bytes.TrimSpace(v)
	if len(v) == 0 {
		return false
	}
	switch v[0] {
	case 'n', 'f':
		return string(v) == "null" || string(v) == "false"
	case '"':
		return string(v) == `""`
	case '[', '{':
		return len(bytes.TrimSpace(v[1:len(v)-1])) == 0
	case 't':
		return false
	}
	// A number is zero when every digit of its mantissa is 0; the sign and
	// exponent do not change that.
	for _, c := range v {
		switch {
		case c == 'e' || c == 'E':
			return true
		case c >= '1' && c <= '9':
			return false
		}
	}
	return true
}

// validNumber checks JSON number grammar without allocating, so an exhaustive
// scan stays allocation-free per frame.
func validNumber(t []byte) bool {
	i := 0
	if i < len(t) && (t[i] == '-' || t[i] == '+') {
		i++
	}
	digits := 0
	for i < len(t) && t[i] >= '0' && t[i] <= '9' {
		i++
		digits++
	}
	if digits == 0 {
		return false
	}
	if i < len(t) && t[i] == '.' {
		i++
		frac := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
			frac++
		}
		if frac == 0 {
			return false
		}
	}
	if i < len(t) && (t[i] == 'e' || t[i] == 'E') {
		i++
		if i < len(t) && (t[i] == '-' || t[i] == '+') {
			i++
		}
		exp := 0
		for i < len(t) && t[i] >= '0' && t[i] <= '9' {
			i++
			exp++
		}
		if exp == 0 {
			return false
		}
	}
	return i == len(t)
}

// skipComposite consumes a balanced object or array, respecting strings.
func (s *scanner) skipComposite() error {
	depth := 0
	for s.i < len(s.b) {
		switch s.b[s.i] {
		case '{', '[':
			depth++
			s.i++
		case '}', ']':
			depth--
			s.i++
			if depth == 0 {
				return nil
			}
			if depth < 0 {
				return s.errAt("unbalanced brackets")
			}
		case '"':
			if _, err := s.stringToken(); err != nil {
				return err
			}
		default:
			s.i++
		}
	}
	return s.errAt("unterminated object or array")
}

// --- raw file access ---------------------------------------------------------
//
// The guard reads the file's own bytes on purpose: routing through the codec
// would reintroduce the blindness it is checking for.

// ReplayMember resolves the frame-bearing zip member using the same preference
// order as the codec's reader (pkg/codec/echoreplay.go initScanner): the member
// named like the archive without its extension, else the first .echoreplay
// member.
func ReplayMember(zr *zip.Reader, name string) (*zip.File, error) {
	base := filepath.Base(name)
	if ext := filepath.Ext(base); ext != "" {
		base = base[:len(base)-len(ext)]
	}
	for _, f := range zr.File {
		if f.Name == base {
			return f, nil
		}
	}
	for _, f := range zr.File {
		if filepath.Ext(f.Name) == ".echoreplay" {
			return f, nil
		}
	}
	return nil, fmt.Errorf("no `.echoreplay` file found in zip %s", name)
}

// EachFrameLine calls fn for every non-empty line of the recording, with the
// line's frame index. The line slice is only valid until fn returns.
func EachFrameLine(path string, fn func(i int, line []byte) error) error {
	_, err := eachFrameLine(path, fn)
	return err
}

// eachFrameLine is EachFrameLine plus the archive's MEMBER COUNT, which the
// scan needs: exactly one member is ever read, so the count is the only place
// a second one becomes visible.
func eachFrameLine(path string, fn func(i int, line []byte) error) (members int, err error) {
	// Some recorders wrote the frame lines with no zip container, and the codec
	// reads those (pkg/codec/echoreplay.go). Scan every input it accepts: a
	// plain file is one member.
	f, err := os.Open(path) //nolint:gosec // caller-provided recording path
	if err != nil {
		return 0, fmt.Errorf("fidelity.eachFrameLine: %w", err)
	}
	defer func() { err = errors.Join(err, f.Close()) }()
	magic := make([]byte, 2)
	if _, rerr := io.ReadFull(f, magic); rerr != nil && !errors.Is(rerr, io.ErrUnexpectedEOF) && !errors.Is(rerr, io.EOF) {
		return 0, fmt.Errorf("fidelity.eachFrameLine: %w", rerr)
	}
	if !bytes.Equal(magic, []byte("PK")) {
		if _, serr := f.Seek(0, io.SeekStart); serr != nil {
			return 0, fmt.Errorf("fidelity.eachFrameLine: %w", serr)
		}
		return 1, scanFrameLines(f, fn)
	}

	zr, err := zip.OpenReader(path)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, zr.Close()) }()
	members = len(zr.File)

	member, err := ReplayMember(&zr.Reader, path)
	if err != nil {
		return members, err
	}
	rc, err := member.Open()
	if err != nil {
		return members, err
	}
	defer func() { err = errors.Join(err, rc.Close()) }()
	return members, scanFrameLines(rc, fn)
}

// scanFrameLines calls fn for every non-empty line of r.
func scanFrameLines(r io.Reader, fn func(i int, line []byte) error) error {
	sc := bufio.NewScanner(r)
	// Match the codec's 10 MB line budget; frames can be very large.
	sc.Buffer(make([]byte, 64*1024), 10*1024*1024)
	i := 0
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		if err := fn(i, line); err != nil {
			return err
		}
		i++
	}
	return sc.Err()
}
