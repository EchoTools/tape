// Package fidelity compares two protobuf messages field by field, driven
// entirely by their descriptors.
//
// WHY IT EXISTS: fidelity checks in this repository used to be hand-written
// enumerations living inside _test.go files. Both properties were defects.
//
//   - A hand list rots. Independent verification found 6 of 36 SessionResponse
//     fields with no comparison at all, PauseState compared on 1 of its 5
//     fields, Team.has_possession never compared, and last_throw / last_score
//     probed only nil-vs-non-nil — so a reconstruction with WRONG VALUES in
//     their 20 sub-fields passed silently. Invisible drift is the whole bug.
//   - A check that only exists in the test suite is deletable, skippable, and
//     can be walked past with a strict=false flag. All three happened here.
//     This package is ordinary library code so the conversion path can call it
//     and refuse to proceed: load-bearing, not decorative.
//
// The comparison plan is derived from the descriptors at build time, so a field
// added to the schema tomorrow is compared tomorrow, without anyone editing
// this file. Coverage is asserted, not assumed: every node in the plan records
// whether the walk actually reached it (see Comparator.Unreached), and
// fidelity_coverage_test.go proves against a synthetically fully-populated
// message that the walk reaches — and compares — 100% of the schema.
//
// What the engine does NOT decide: which differences are acceptable. That is
// the caller's Allowlist, and it is the only escape hatch.
package fidelity

import (
	"bytes"
	"fmt"
	"math"
	"sort"
	"strings"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// DiffKind classifies what about a field differs. The kind is part of a Diff's
// reported Path (as a "#presence" / "#count" suffix) so that every distinct
// failure mode of a field is separately visible and separately allowlistable.
type DiffKind uint8

const (
	// DiffValue: the field is present on both sides with different contents.
	DiffValue DiffKind = iota
	// DiffPresence: a message field is set on one side and not the other.
	DiffPresence
	// DiffCount: a repeated or map field has a different number of entries.
	DiffCount

	numDiffKinds = 3
)

// Suffix is what a diff of this kind appends to the schema field path.
func (k DiffKind) Suffix() string {
	switch k {
	case DiffPresence:
		return "#presence"
	case DiffCount:
		return "#count"
	default:
		return ""
	}
}

// Diff is one field path that did not survive, aggregated over every message
// pair the Comparator was given.
type Diff struct {
	// Path is the schema field path plus the DiffKind suffix, e.g.
	// "SessionResponse.teams[].players[].stats.stuns" or
	// "SessionResponse.teams[]#count". It is a SCHEMA path: repeated fields
	// appear once, as "[]", with the concrete instance named in Example.
	Path  string
	Field string // Path without the kind suffix.
	// Example is the first observed occurrence, naming the concrete instance
	// and both values, e.g.
	// "frame 42 teams[key=[8]]/players[key=8]: orig=3 recon=0".
	Example string
	// Reason is the allowlist entry that excuses this path, if any.
	Reason string
	// Count is the number of comparisons in which this path differed.
	Count int
	Kind  DiffKind
	// Allowed is set by Verdict/Allowlist classification.
	Allowed bool
}

func (d Diff) String() string {
	return fmt.Sprintf("%s x%d  e.g. %s", d.Path, d.Count, d.Example)
}

// KeyFunc derives a stable identity for one element of a repeated message
// field, so elements can be paired across the two sides by identity instead of
// by array index.
//
// This is the engine's ONLY hand-supplied configuration, and it deliberately
// controls ALIGNMENT, never which fields are compared. It exists because a
// reconstruction may legitimately reorder or omit elements (echo's
// reconstruction drops empty teams, so team index 1 is not the same team on
// both sides); pairing by index would then report every field of every element
// as different and drown the real signal.
type KeyFunc func(protoreflect.Message) string

// Options configures a Schema.
type Options struct {
	// ListKeys pairs elements of a repeated message field by identity. The map
	// key is "<message full name>.<field name>", e.g.
	// "engine.v1.SessionResponse.teams". A repeated field with no entry here is
	// paired by index.
	ListKeys map[string]KeyFunc

	// Tolerance returns the absolute tolerance for a float leaf; 0 (the zero
	// value, and the behaviour when Tolerance is nil) means exact equality.
	// Called once per field at Schema build time, never per comparison.
	Tolerance func(fd protoreflect.FieldDescriptor, path string) float64

	// Equal returns a custom equality for a scalar leaf, or nil to keep the
	// default (exact, or Tolerance for floats). It is for a leaf whose value is
	// DERIVED from others and whose derivation has a measured, bounded error;
	// it must accept that bound and nothing wider. Called once per field at
	// Schema build time, never per comparison.
	Equal func(fd protoreflect.FieldDescriptor, path string) func(a, b protoreflect.Value) bool
}

// node is one field in the precomputed comparison plan. Everything expensive —
// the descriptor lookup, the path string, the tolerance decision, the pairing
// function — is resolved once at Schema build time, so the per-comparison walk
// allocates nothing for metadata regardless of how many million times it runs.
type node struct {
	// byName resolves a JSON object key to the child node it belongs to, in
	// both the proto-name and json-name forms protojson accepts. It is what
	// lets the key-set scanner (keyscan.go) walk raw bytes against this same
	// plan with precomputed paths and no per-key allocation.
	byName   map[string]*node
	key      KeyFunc
	eq       func(a, b protoreflect.Value) bool
	fd       protoreflect.FieldDescriptor // nil only for the schema root
	path     string
	children []*node // non-nil for message-valued fields
	tol      float64
	idx      int
	// wellKnown marks google.protobuf.* messages, whose JSON shape is not
	// driven by field descriptors (Struct/Value/Any carry arbitrary keys).
	wellKnown bool
}

// Schema is a precomputed, descriptor-derived comparison plan for one root
// message type. It is immutable and safe to share; per-run state lives in
// Comparator.
type Schema struct {
	desc  protoreflect.MessageDescriptor
	name  string
	root  *node
	nodes []*node // index == node.idx; nodes[0] is the root
}

// New builds a comparison plan for m's type.
//
// It returns an error on a recursive message type rather than silently
// truncating the walk: a truncated plan is exactly the invisible hole this
// package exists to remove. (No engine.v1 or telemetry.v1 message is recursive
// today; this is the guard for the day one becomes so.)
func New(m proto.Message, opts Options) (*Schema, error) {
	md := m.ProtoReflect().Descriptor()
	s := &Schema{desc: md, name: string(md.Name())}
	s.root = &node{idx: 0, path: s.name}
	s.nodes = []*node{s.root}
	kids, err := s.build(md, s.name, opts, []protoreflect.FullName{md.FullName()})
	if err != nil {
		return nil, err
	}
	s.root.children = kids
	s.root.byName = nameIndex(kids)
	s.root.wellKnown = isWellKnown(md)
	return s, nil
}

// nameIndex maps every JSON-visible name of these fields to its node.
func nameIndex(kids []*node) map[string]*node {
	m := make(map[string]*node, len(kids)*2)
	for _, k := range kids {
		m[string(k.fd.Name())] = k
		m[k.fd.JSONName()] = k
	}
	return m
}

func isWellKnown(md protoreflect.MessageDescriptor) bool {
	return strings.HasPrefix(string(md.FullName()), "google.protobuf.")
}

func (s *Schema) build(md protoreflect.MessageDescriptor, path string, opts Options, stack []protoreflect.FullName) ([]*node, error) {
	fields := md.Fields()
	out := make([]*node, 0, fields.Len())
	for i := 0; i < fields.Len(); i++ {
		fd := fields.Get(i)
		suffix := ""
		switch {
		case fd.IsMap():
			suffix = "{}"
		case fd.IsList():
			suffix = "[]"
		}
		p := path + "." + string(fd.Name()) + suffix

		n := &node{idx: len(s.nodes), fd: fd, path: p}
		s.nodes = append(s.nodes, n)
		if opts.Tolerance != nil {
			n.tol = opts.Tolerance(fd, p)
		}
		if opts.Equal != nil {
			n.eq = opts.Equal(fd, p)
		}
		if opts.ListKeys != nil {
			n.key = opts.ListKeys[fmt.Sprintf("%s.%s", md.FullName(), fd.Name())]
		}

		sub := subMessage(fd)
		if sub != nil {
			for _, seen := range stack {
				if seen == sub.FullName() {
					return nil, fmt.Errorf("fidelity: recursive message %s at %s: "+
						"the comparison plan cannot be finite, and a truncated plan is an invisible hole", sub.FullName(), p)
				}
			}
			kids, err := s.build(sub, p, opts, append(stack, sub.FullName()))
			if err != nil {
				return nil, err
			}
			n.children = kids
			n.byName = nameIndex(kids)
			n.wellKnown = isWellKnown(sub)
		}
		out = append(out, n)
	}
	return out, nil
}

// subMessage returns the message descriptor a field's values carry, or nil for
// scalar fields. For maps it is the map VALUE's type: map keys are scalars.
func subMessage(fd protoreflect.FieldDescriptor) protoreflect.MessageDescriptor {
	if fd.IsMap() {
		if v := fd.MapValue(); v.Kind() == protoreflect.MessageKind || v.Kind() == protoreflect.GroupKind {
			return v.Message()
		}
		return nil
	}
	if fd.Kind() == protoreflect.MessageKind || fd.Kind() == protoreflect.GroupKind {
		return fd.Message()
	}
	return nil
}

// Name is the root message's short name, and the first segment of every path.
func (s *Schema) Name() string { return s.name }

// Paths lists every field path in the schema, in declaration order, depth
// first. This is the coverage denominator: it comes from the descriptors, so it
// grows the moment the proto does.
func (s *Schema) Paths() []string {
	out := make([]string, 0, len(s.nodes)-1)
	for _, n := range s.nodes[1:] {
		out = append(out, n.path)
	}
	return out
}

// NewComparator starts a fresh accumulation over this schema.
func (s *Schema) NewComparator() *Comparator {
	return &Comparator{
		s:        s,
		counts:   make([]int, len(s.nodes)*numDiffKinds),
		examples: make([]string, len(s.nodes)*numDiffKinds),
		reached:  make([]bool, len(s.nodes)),
	}
}

// Comparator accumulates field-path differences across many message pairs.
//
// PERFORMANCE: this is the per-frame hot path. Diff bookkeeping is flat-slice
// indexed by the precomputed node index — no map lookups, no path string
// building, and no descriptor metadata allocated per comparison. Example
// strings are formatted at most once per (path, kind). The only per-comparison
// allocation is the pairing key for identity-paired repeated fields (a few
// short strings per frame), which is the price of comparing the right elements
// to each other.
//
// A Comparator is not safe for concurrent use.
type Comparator struct {
	s        *Schema
	counts   []int
	examples []string
	reached  []bool

	// Scratch, reused across comparisons. The pairing scratch is per NESTING
	// DEPTH, not shared: teams are paired by key while each team's players are
	// paired by key inside that same loop, so one shared map let the inner
	// pairing clobber the outer one and every team after the first compared
	// against a zero message. Caught by TestTeamPairingSurvivesReordering.
	crumbs    []string
	indexes   []map[string]int
	takens    [][]bool
	pairDepth int
	compared  int
}

// Compared is the number of message pairs handed to Compare.
func (c *Comparator) Compared() int { return c.compared }

// Compare walks one pair. label identifies this pair in examples (e.g.
// "frame 42"). Either side may be nil or a typed-nil message.
func (c *Comparator) Compare(a, b proto.Message, label string) {
	c.compared++
	ra, rb := reflectOf(a), reflectOf(b)
	pa, pb := ra != nil && ra.IsValid(), rb != nil && rb.IsValid()
	if !pa && !pb {
		return
	}
	if pa != pb {
		if c.hit(c.s.root, DiffPresence) {
			c.setExample(c.s.root, DiffPresence, fmt.Sprintf("%s: orig present=%v recon present=%v", label, pa, pb))
		}
	}
	if !pa {
		ra = rb.Type().Zero()
	}
	if !pb {
		rb = ra.Type().Zero()
	}
	c.crumbs = c.crumbs[:0]
	c.walk(c.s.root.children, ra, rb, label)
}

func reflectOf(m proto.Message) protoreflect.Message {
	if m == nil {
		return nil
	}
	return m.ProtoReflect()
}

func (c *Comparator) walk(nodes []*node, a, b protoreflect.Message, label string) {
	for _, n := range nodes {
		c.reached[n.idx] = true
		fd := n.fd
		switch {
		case fd.IsMap():
			c.compareMap(n, a.Get(fd).Map(), b.Get(fd).Map(), label)
		case fd.IsList():
			c.compareList(n, a.Get(fd).List(), b.Get(fd).List(), label)
		case n.children != nil:
			ha, hb := a.Has(fd), b.Has(fd)
			if ha != hb {
				if c.hit(n, DiffPresence) {
					c.setExample(n, DiffPresence, fmt.Sprintf("%s %s: orig set=%v recon set=%v", label, c.where(), ha, hb))
				}
			}
			if ha || hb {
				c.walk(n.children, a.Get(fd).Message(), b.Get(fd).Message(), label)
			}
		default:
			c.compareScalar(n, a.Get(fd), b.Get(fd), "", label)
		}
	}
}

func (c *Comparator) compareList(n *node, la, lb protoreflect.List, label string) {
	if la.Len() != lb.Len() {
		if c.hit(n, DiffCount) {
			c.setExample(n, DiffCount, fmt.Sprintf("%s %s: orig len=%d recon len=%d", label, c.where(), la.Len(), lb.Len()))
		}
	}
	if n.children == nil {
		min := la.Len()
		if lb.Len() < min {
			min = lb.Len()
		}
		for i := 0; i < min; i++ {
			c.compareScalar(n, la.Get(i), lb.Get(i), fmt.Sprintf("[%d]", i), label)
		}
		return
	}
	if n.key == nil {
		c.pairByIndex(n, la, lb, label)
		return
	}
	c.pairByKey(n, la, lb, label)
}

// pairByIndex is the default for repeated message fields: element i on the left
// is element i on the right. Surplus elements on either side are compared
// against a zero message, so their fields are reported as differing rather than
// quietly skipped.
func (c *Comparator) pairByIndex(n *node, la, lb protoreflect.List, label string) {
	max := la.Len()
	if lb.Len() > max {
		max = lb.Len()
	}
	for i := 0; i < max; i++ {
		var ea, eb protoreflect.Message
		switch {
		case i < la.Len():
			ea = la.Get(i).Message()
		default:
			ea = zeroLike(lb.Get(i).Message())
		}
		switch {
		case i < lb.Len():
			eb = lb.Get(i).Message()
		default:
			eb = zeroLike(la.Get(i).Message())
		}
		c.crumbs = append(c.crumbs, fmt.Sprintf("%s[%d]", leaf(n.path), i))
		c.walk(n.children, ea, eb, label)
		c.crumbs = c.crumbs[:len(c.crumbs)-1]
	}
}

// pairByKey pairs elements by KeyFunc identity. Elements present on one side
// only are compared against a zero message — the truth is that every one of
// their fields is absent from the other side, and reporting that is the point.
// Duplicate keys pair first-with-first; the remaining duplicates fall out as
// unmatched, and the length difference is already reported as #count.
func (c *Comparator) pairByKey(n *node, la, lb protoreflect.List, label string) {
	idx, taken := c.pairScratch(lb.Len())
	c.pairDepth++
	defer func() { c.pairDepth-- }()
	for i := 0; i < lb.Len(); i++ {
		k := n.key(lb.Get(i).Message())
		if _, dup := idx[k]; dup {
			continue
		}
		idx[k] = i
	}
	for i := 0; i < la.Len(); i++ {
		ea := la.Get(i).Message()
		k := n.key(ea)
		var eb protoreflect.Message
		if j, ok := idx[k]; ok && !taken[j] {
			taken[j] = true
			eb = lb.Get(j).Message()
		} else {
			eb = zeroLike(ea)
		}
		c.crumbs = append(c.crumbs, fmt.Sprintf("%s[key=%s]", leaf(n.path), k))
		c.walk(n.children, ea, eb, label)
		c.crumbs = c.crumbs[:len(c.crumbs)-1]
	}
	for j := 0; j < lb.Len(); j++ {
		if taken[j] {
			continue
		}
		eb := lb.Get(j).Message()
		c.crumbs = append(c.crumbs, fmt.Sprintf("%s[key=%s,recon-only]", leaf(n.path), n.key(eb)))
		c.walk(n.children, zeroLike(eb), eb, label)
		c.crumbs = c.crumbs[:len(c.crumbs)-1]
	}
}

// pairScratch hands out the pairing map and match flags for the CURRENT
// nesting depth, so a nested pairing cannot clobber the one it is nested in.
func (c *Comparator) pairScratch(n int) (map[string]int, []bool) {
	for len(c.indexes) <= c.pairDepth {
		c.indexes = append(c.indexes, map[string]int{})
		c.takens = append(c.takens, nil)
	}
	idx := c.indexes[c.pairDepth]
	clear(idx)
	if cap(c.takens[c.pairDepth]) < n {
		c.takens[c.pairDepth] = make([]bool, n)
	}
	taken := c.takens[c.pairDepth][:n]
	for i := range taken {
		taken[i] = false
	}
	return idx, taken
}

func (c *Comparator) compareMap(n *node, ma, mb protoreflect.Map, label string) {
	if ma.Len() != mb.Len() {
		if c.hit(n, DiffCount) {
			c.setExample(n, DiffCount, fmt.Sprintf("%s %s: orig len=%d recon len=%d", label, c.where(), ma.Len(), mb.Len()))
		}
	}
	// Iterate deterministically so the reported example does not depend on map
	// ordering.
	keys := make([]protoreflect.MapKey, 0, ma.Len())
	ma.Range(func(k protoreflect.MapKey, _ protoreflect.Value) bool {
		keys = append(keys, k)
		return true
	})
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		va, vb := ma.Get(k), mb.Get(k)
		if n.children == nil {
			if !mb.Has(k) {
				if c.hit(n, DiffPresence) {
					c.setExample(n, DiffPresence, fmt.Sprintf("%s %s: key %s missing in recon", label, c.where(), k.String()))
				}
				continue
			}
			c.compareScalar(n, va, vb, "{"+k.String()+"}", label)
			continue
		}
		eb := vb.Message()
		if !mb.Has(k) {
			eb = zeroLike(va.Message())
		}
		c.crumbs = append(c.crumbs, fmt.Sprintf("%s{%s}", leaf(n.path), k.String()))
		c.walk(n.children, va.Message(), eb, label)
		c.crumbs = c.crumbs[:len(c.crumbs)-1]
	}
	mb.Range(func(k protoreflect.MapKey, v protoreflect.Value) bool {
		if ma.Has(k) {
			return true
		}
		if n.children == nil {
			if c.hit(n, DiffPresence) {
				c.setExample(n, DiffPresence, fmt.Sprintf("%s %s: key %s only in recon", label, c.where(), k.String()))
			}
			return true
		}
		c.crumbs = append(c.crumbs, fmt.Sprintf("%s{%s,recon-only}", leaf(n.path), k.String()))
		c.walk(n.children, zeroLike(v.Message()), v.Message(), label)
		c.crumbs = c.crumbs[:len(c.crumbs)-1]
		return true
	})
}

func (c *Comparator) compareScalar(n *node, a, b protoreflect.Value, at string, label string) {
	if n.eq != nil {
		if n.eq(a, b) {
			return
		}
	} else if scalarEqual(n.fd, a, b, n.tol) {
		return
	}
	if c.hit(n, DiffValue) {
		c.setExample(n, DiffValue, fmt.Sprintf("%s %s%s: orig=%v recon=%v", label, c.where(), at, a.Interface(), b.Interface()))
	}
}

// scalarEqual compares one leaf value. tol > 0 admits a bounded absolute
// difference for float leaves and applies to nothing else: a tolerance on an
// integer, string, bool or enum would be a licence to be wrong.
func scalarEqual(fd protoreflect.FieldDescriptor, a, b protoreflect.Value, tol float64) bool {
	kind := fd.Kind()
	if fd.IsMap() {
		kind = fd.MapValue().Kind()
	}
	switch kind {
	case protoreflect.DoubleKind, protoreflect.FloatKind:
		x, y := a.Float(), b.Float()
		if math.IsNaN(x) && math.IsNaN(y) {
			return true
		}
		if tol > 0 {
			return math.Abs(x-y) <= tol
		}
		return x == y
	case protoreflect.BytesKind:
		return bytes.Equal(a.Bytes(), b.Bytes())
	default:
		return a.Interface() == b.Interface()
	}
}

// zeroLike returns a read-only empty message of m's type, used as the
// counterpart for an element that exists on one side only.
func zeroLike(m protoreflect.Message) protoreflect.Message { return m.Type().Zero() }

func leaf(path string) string {
	if i := strings.LastIndexByte(path, '.'); i >= 0 {
		return path[i+1:]
	}
	return path
}

func (c *Comparator) where() string {
	if len(c.crumbs) == 0 {
		return ""
	}
	return strings.Join(c.crumbs, "/")
}

// hit records one occurrence and reports whether an example is still wanted, so
// callers format example strings at most once per (path, kind) instead of once
// per frame.
func (c *Comparator) hit(n *node, k DiffKind) bool {
	i := n.idx*numDiffKinds + int(k)
	c.counts[i]++
	return c.examples[i] == ""
}

func (c *Comparator) setExample(n *node, k DiffKind, s string) {
	c.examples[n.idx*numDiffKinds+int(k)] = s
}

// Diffs returns every field path that differed, in schema declaration order.
func (c *Comparator) Diffs() []Diff {
	var out []Diff
	for _, n := range c.s.nodes {
		for k := range DiffKind(numDiffKinds) {
			i := n.idx*numDiffKinds + int(k)
			if c.counts[i] == 0 {
				continue
			}
			out = append(out, Diff{
				Path:    n.path + k.Suffix(),
				Field:   n.path,
				Kind:    k,
				Count:   c.counts[i],
				Example: c.examples[i],
			})
		}
	}
	return out
}

// Unreached lists schema paths the walk never visited. A path goes unreached
// when every message on the way to it was absent from BOTH sides, so nothing
// about it was ever tested. For a per-file verification that is a real hole:
// the file was not fully checked, and "we compared everything" would be a lie.
func (c *Comparator) Unreached() []string {
	var out []string
	for _, n := range c.s.nodes[1:] {
		if !c.reached[n.idx] {
			out = append(out, n.path)
		}
	}
	return out
}

// Reached is the number of schema paths the walk visited.
func (c *Comparator) Reached() int {
	n := 0
	for _, v := range c.reached[1:] {
		if v {
			n++
		}
	}
	return n
}

// SchemaSize is the number of schema paths in the plan.
func (c *Comparator) SchemaSize() int { return len(c.s.nodes) - 1 }

// RootName is the compared message type's short name.
func (c *Comparator) RootName() string { return c.s.name }
