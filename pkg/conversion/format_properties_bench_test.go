package conversion

// Format-property benchmarks for the .tape container.
//
// These benchmarks exist to decide FORMAT questions while the format is still
// unreleased and breaking changes are free. They are deliberately written
// against format PROPERTIES -- bytes per frame at interval X, decode cost to
// reach frame N, whether byte-range access is possible at all -- and NOT
// against the current writer API. A benchmark coupled to
// NewWriterWithKeyframeInterval would obstruct the very change it exists to
// measure, and would then be deleted rather than fixed.
//
// Every benchmark below opens with the QUESTION it answers, WHY IT MATTERS,
// and what would ANSWER it. A benchmark whose question is settled says so and
// stays as a regression guard; it is not silently re-run forever as if open.
//
// FIXED CORPUS: testdata/sample.echoreplay, checked into this repo. Numbers are
// only comparable across commits if the input does not drift, so the corpus is
// pinned rather than taken from a scratch directory. Frames are the real
// marshalled Envelope payloads produced by the real conversion pipeline.
//
// -----------------------------------------------------------------------------
// ESTABLISHED BEFORE WRITING THESE -- read this before interpreting any number.
// Sources are named because the scope of each claim is exactly the scope of its
// citation; a broader sentence than its evidence supports is a defect.
//
// v2 IS delta-encoded, but in TIERS. docs/format-design.md §2 states the
// design: "v2 is not 'every field, every frame'. It is ... organized by how
// often data changes."
//
//   - Session-constant -> the header, written once (§2).
//   - Discrete changes -> EVENTS, and this tier IS delta. pkg/conversion/
//     mapping.go:399-431 holds prevLoadout / prevInfo across frames and emits an
//     event only on change: ":417  afterwards any change becomes a delta. See
//     ROSTER-001." Reconstruction replays events from the beginning (§5).
//   - Per-frame-varying -> per-frame fields, and this tier is NOT delta. §2:
//     "Frames are self-contained for random access (scores/round repeat every
//     frame, ~6-9 bytes)."
//
// So "is a frame delta-encoded" has no single answer: the kinematic tier is
// self-contained, the event tier is not.
//
// What the CONTAINER does, separately: Writer.WriteFrame (pkg/codec/tape.go:
// 154-160) appends a KeyframeEntry{FrameIndex, ByteOffset} every interval and
// computes no delta of its own. That is an index entry; the frame at a boundary
// is an ordinary frame with no materialized state written into it.
//
// The consequence, and it is the point of this file: a boundary frame is
// startable for its kinematic tier and NOT startable for its event tier, so a
// reader must still replay events from frame 0 -- which is exactly why
// cmd/tapedeck/trim_seed.go must rebuild seed events when it cuts a capture.
// And because the whole container is one zstd stream (tape.go:97), the recorded
// byte offset cannot be seeked to either.
//
// Therefore the keyframe interval is measured here TWICE, and the two answers
// are different:
//   - BenchmarkKeyframeIntervalSizeCost -- what the interval costs TODAY (footer
//     index density only, because nothing hydrates).
//   - BenchmarkHydratedKeyframeCost -- what a REAL keyframe would cost, by
//     pricing the materialized event-tier state a reader would need. This is the
//     number that answers "is one per second reasonable".
// -----------------------------------------------------------------------------

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"testing"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"
)

// convertCorpus turns the checked-in .echoreplay into a real .tape using the
// real pipeline. Going through ConvertFile rather than hand-building frames is
// deliberate: the corpus must be what the product actually produces.
func convertCorpus(src, dst string) error {
	_, err := ConvertFile(src, dst)
	return err
}

// readFramePayloads reads a .tape through the public reader and returns each
// frame's marshalled bytes. It deliberately does not reimplement framing, so it
// keeps working if the container framing changes.
func readFramePayloads(tapePath string) ([][]byte, []*capturepb.Frame, error) {
	r, err := codec.NewReader(tapePath)
	if err != nil {
		return nil, nil, err
	}
	defer r.Close() //nolint:errcheck // read-only

	if _, err := r.ReadHeader(); err != nil {
		return nil, nil, err
	}
	var out [][]byte
	var protos []*capturepb.Frame
	for {
		f, err := r.ReadFrame()
		if err != nil {
			break // io.EOF at the footer terminates the stream
		}
		b, err := proto.Marshal(f)
		if err != nil {
			return nil, nil, err
		}
		out = append(out, b)
		protos = append(protos, f)
	}
	return out, protos, nil
}

// loadCorpusProtos returns the same corpus as decoded frames.
func loadCorpusProtos(tb testing.TB) []*capturepb.Frame {
	tb.Helper()
	loadCorpusFrames(tb)
	return corpusProtos
}

// stateKey identifies one slot of reconstructable state. Events with a
// player_slot field are per-slot state; the rest are singletons (scoreboard,
// last goal). Keeping only the LAST event per key is exactly what materializing
// the delta state means -- the same thing cmd/tapedeck/trim_seed.go builds when
// it seeds a cut.
func stateKey(evt *capturepb.EchoEvent) string {
	inner := evt.ProtoReflect().WhichOneof(evt.ProtoReflect().Descriptor().Oneofs().ByName("event"))
	if inner == nil {
		return "?"
	}
	key := string(inner.Name())
	msg := evt.ProtoReflect().Get(inner).Message()
	if fd := msg.Descriptor().Fields().ByName("player_slot"); fd != nil {
		key += "|" + strconv.FormatInt(msg.Get(fd).Int(), 10)
	}
	return key
}

// Frame rate the intervals below are expressed against. The corpus is replayed
// at engine rate; 30fps is the production producer rate measured 2026-08-31.
const benchFPS = 30

// keyframeIntervalCases is the interval axis, in FRAMES, named in seconds so a
// reader can tell what question is being asked. 0 means "none".
var keyframeIntervalCases = []struct {
	name   string
	frames int
}{
	{"1s", 1 * benchFPS},
	{"2s", 2 * benchFPS},
	{"5s", 5 * benchFPS},
	{"none", 0},
}

var (
	corpusOnce   sync.Once
	corpusFrames [][]byte           // marshalled Envelope payloads, in order
	corpusProtos []*capturepb.Frame // the same frames, decoded
	corpusErr    error
)

// loadCorpusFrames returns the fixed corpus as raw per-frame payloads. It reads
// the container through the public reader rather than reimplementing framing,
// so the corpus stays correct if the framing changes.
func loadCorpusFrames(tb testing.TB) [][]byte {
	tb.Helper()
	corpusOnce.Do(func() {
		const samplePath = "../../testdata/sample.echoreplay"
		if _, err := os.Stat(samplePath); os.IsNotExist(err) {
			return // corpusFrames stays nil; callers skip
		}
		dir, err := os.MkdirTemp("", "tapebench")
		if err != nil {
			corpusErr = err
			return
		}
		defer os.RemoveAll(dir) //nolint:errcheck // best-effort cleanup

		tapePath := filepath.Join(dir, "corpus.tape")
		if corpusErr = convertCorpus(samplePath, tapePath); corpusErr != nil {
			return
		}
		corpusFrames, corpusProtos, corpusErr = readFramePayloads(tapePath)
	})
	if corpusErr != nil {
		tb.Fatalf("corpus: %v", corpusErr)
	}
	if len(corpusFrames) == 0 {
		tb.Skip("testdata/sample.echoreplay not available; corpus benchmarks skipped")
	}
	return corpusFrames
}

// --- layouts -----------------------------------------------------------------
//
// Each layout answers the same two questions in different ways: how many bytes,
// and can a reader jump to frame N without decompressing everything before it.

type layoutResult struct {
	bytes         int
	byteRangeable bool // BINARY, and the column that matters most
}

// layoutWholeStream is today's container: one continuous zstd stream over every
// frame. Byte offsets recorded in the footer are positions in the DECOMPRESSED
// stream, so nothing can seek to them.
func layoutWholeStream(frames [][]byte, level zstd.EncoderLevel) layoutResult {
	var buf bytes.Buffer
	enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderLevel(level))
	for _, f := range frames {
		writeDelimited(enc, f)
	}
	enc.Close() //nolint:errcheck // buffer writer
	return layoutResult{bytes: buf.Len(), byteRangeable: false}
}

// layoutPerBlock emits one INDEPENDENT zstd frame per block of n frames. Each
// block can be decompressed without the ones before it, so byte-range access
// becomes possible. The cost is that zstd's match window resets every block.
func layoutPerBlock(frames [][]byte, n int, level zstd.EncoderLevel, dict []byte) layoutResult {
	if n <= 0 {
		n = len(frames)
	}
	total := 0
	for start := 0; start < len(frames); start += n {
		end := min(start+n, len(frames))
		var raw bytes.Buffer
		for _, f := range frames[start:end] {
			writeDelimited(&raw, f)
		}
		opts := []zstd.EOption{zstd.WithEncoderLevel(level)}
		if dict != nil {
			opts = append(opts, zstd.WithEncoderDict(dict))
		}
		var out bytes.Buffer
		enc, _ := zstd.NewWriter(&out, opts...)
		enc.Write(raw.Bytes()) //nolint:errcheck // buffer writer
		enc.Close()            //nolint:errcheck // buffer writer
		total += out.Len()
	}
	// A per-block layout needs a seek table to be useful. The zstd seekable
	// format puts one in a SKIPPABLE frame (magic 0x184D2A5E) with a 9-byte
	// footer, so decoders that do not know about it skip the index instead of
	// choking. Entries are 8 bytes (compressed size, decompressed size), or 12
	// with the optional checksum. That overhead is charged here rather than
	// quietly omitted, so the size comparison is honest.
	blocks := (len(frames) + n - 1) / n
	total += blocks*8 + 9 + 8 // entries + footer + skippable frame header
	return layoutResult{bytes: total, byteRangeable: true}
}

func writeDelimited(w interface{ Write([]byte) (int, error) }, b []byte) {
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	w.Write(hdr[:n]) //nolint:errcheck // buffer/encoder writer
	w.Write(b)       //nolint:errcheck // buffer/encoder writer
}

// buildCorpusDict trains a zstd dictionary on the corpus frames.
func buildCorpusDict(tb testing.TB, frames [][]byte) []byte {
	tb.Helper()
	sample := frames
	if len(sample) > 2000 {
		sample = sample[:2000]
	}
	// History is the dictionary CONTENT (the window a block may match
	// against); Contents are the samples the entropy tables are trained on.
	// Both are required -- BuildDict rejects an empty History.
	var hist []byte
	const histCap = 112 << 10 // conventional zstd dictionary size
	for _, f := range sample {
		if len(hist)+len(f) > histCap {
			break
		}
		hist = append(hist, f...)
	}
	d, err := zstd.BuildDict(zstd.BuildDictOptions{
		ID:       1,
		Contents: sample,
		History:  hist,
		Level:    zstd.SpeedDefault,
	})
	if err != nil {
		tb.Skipf("BuildDict: %v", err)
	}
	return d
}

// =============================================================================
// QUESTION: what does one frame actually cost on disk, after compression, in a
// real recording?
//
// WHY IT MATTERS: every other number in this file is a ratio against this one,
// and every capacity estimate for nevr-stream (disk retention, tape sizes,
// memory for a live ring) is derived from it. A guessed frame size silently
// propagates into all of them.
//
// ANSWERED WHEN: this is a SETTLED question. It is kept as a regression guard:
// if bytes/frame moves materially without an intentional format change, the
// format changed under us.
// =============================================================================
func BenchmarkFrameBytesOnDisk(b *testing.B) {
	frames := loadCorpusFrames(b)
	var raw int
	for _, f := range frames {
		raw += len(f)
	}
	res := layoutWholeStream(frames, zstd.SpeedFastest)

	b.ReportMetric(float64(len(frames)), "frames")
	b.ReportMetric(float64(raw)/float64(len(frames)), "rawB/frame")
	b.ReportMetric(float64(res.bytes)/float64(len(frames)), "zstdB/frame")
	b.ReportMetric(float64(raw)/float64(res.bytes), "ratio")
	b.ReportMetric(float64(len(frames))/benchFPS, "seconds@30fps")
}

// =============================================================================
// QUESTION: at 30fps, how much tape SIZE does a 1-second keyframe interval cost
// versus 2s, 5s, or none at all?
//
// WHY IT MATTERS: "one per second" is a round number, not a measured
// one. If the cost is 3% the interval should be dense; if it is 30% it is a
// real trade that has to be argued.
//
// ANSWERED WHEN: the size curve across 1s/2s/5s/none is flat or it is not.
//
// STATUS -- READ THIS, AND DO NOT READ THE FLAT CURVE AS "THE INTERVAL IS FREE".
// This benchmark measures only what the interval costs in the CONTAINER AS IT
// STANDS: footer index density, because WriteFrame hydrates nothing
// (pkg/codec/tape.go:154-160). The frame stream is byte-identical at every
// interval, so of course the curve is flat -- that is a fact about the current
// writer, NOT about whether one keyframe per second is a good idea.
//
// The interval question is a live one and it is answered by
// BenchmarkHydratedKeyframeCost below, which prices the state a keyframe would
// have to carry to actually be a keyframe. Read the two together or neither.
// This one stays as the regression guard: if the writer ever starts hydrating,
// this curve stops being flat by itself.
// =============================================================================
func BenchmarkKeyframeIntervalSizeCost(b *testing.B) {
	frames := loadCorpusFrames(b)
	base := layoutWholeStream(frames, zstd.SpeedFastest).bytes

	for _, kc := range keyframeIntervalCases {
		b.Run(kc.name, func(b *testing.B) {
			entries := 0
			if kc.frames > 0 {
				entries = (len(frames) + kc.frames - 1) / kc.frames
			}
			// A KeyframeEntry is two varint fields (frame index, byte offset);
			// 8 bytes is a generous upper bound at these magnitudes.
			footer := entries * 8
			b.ReportMetric(float64(entries), "keyframes")
			b.ReportMetric(float64(base+footer), "totalB")
			b.ReportMetric(100*float64(footer)/float64(base), "%%overFrames")
		})
	}
}

// =============================================================================
// QUESTION: can a reader fetch frame N without decompressing everything before
// it -- and what does making that possible cost in bytes?
//
// WHY IT MATTERS: this is the question that decides whether seek, trim, range
// download and partial replay are cheap or impossible. It is BINARY before it
// is numeric: today the answer is NO for the shipped layout, because
// zstd.NewWriter wraps the entire container in one continuous stream
// (tape.go:97), which makes the footer's byte offsets unusable for seeking.
//
// Per-block layouts make it YES. The zstd seekable format is the published
// solution to exactly this shape -- independent frames plus a seek table in a
// SKIPPABLE frame (magic 0x184D2A5E, 9-byte footer, magic 0x8F92EAB1) that
// older decoders ignore rather than choke on. The seek-table overhead is
// charged in the numbers below.
//
// ANSWERED WHEN: the byte cost of per-block independence is known at each block
// size, and we can say whether a trained dictionary recovers the ratio lost to
// resetting the match window every block.
// =============================================================================
func BenchmarkByteRangeAccessCost(b *testing.B) {
	frames := loadCorpusFrames(b)
	whole := layoutWholeStream(frames, zstd.SpeedFastest)
	dict := buildCorpusDict(b, frames)

	report := func(b *testing.B, r layoutResult) {
		b.ReportMetric(float64(r.bytes), "totalB")
		b.ReportMetric(float64(r.bytes)/float64(len(frames)), "B/frame")
		b.ReportMetric(100*float64(r.bytes-whole.bytes)/float64(whole.bytes), "%%vsWhole")
		v := 0.0
		if r.byteRangeable {
			v = 1
		}
		b.ReportMetric(v, "byteRangeable")
	}

	b.Run("whole-stream", func(b *testing.B) { report(b, whole) })

	for _, kc := range keyframeIntervalCases {
		if kc.frames == 0 {
			continue // "none" == whole-stream, already reported
		}
		b.Run("per-block/"+kc.name, func(b *testing.B) {
			report(b, layoutPerBlock(frames, kc.frames, zstd.SpeedFastest, nil))
		})
		b.Run("per-block+dict/"+kc.name, func(b *testing.B) {
			report(b, layoutPerBlock(frames, kc.frames, zstd.SpeedFastest, dict))
		})
	}
	b.ReportMetric(float64(len(dict)), "dictB")
}

// =============================================================================
// QUESTION: how expensive is it to reach frame N, and does a per-block layout
// actually make it cheaper in practice rather than just in principle?
//
// WHY IT MATTERS: "seek to frame N" is the operation trim, replay and the live
// stream's seek control all rest on. If reaching a frame late in a capture
// costs a full decompress regardless of layout, then byte-range access buys
// nothing and the format question is settled the other way.
//
// ANSWERED WHEN: decode-to-frame-N is measured for both layouts at an N deep
// enough to matter (90% through the capture). Note this measures CONTAINER
// decode only -- it does NOT include replaying events to hydrate state, which
// is a separate and unavoidable cost under a semantic delta format.
// =============================================================================
func BenchmarkDecodeToFrameN(b *testing.B) {
	frames := loadCorpusFrames(b)
	target := len(frames) * 9 / 10
	dict := buildCorpusDict(b, frames)

	b.Run("whole-stream", func(b *testing.B) {
		var buf bytes.Buffer
		enc, _ := zstd.NewWriter(&buf, zstd.WithEncoderLevel(zstd.SpeedFastest))
		for _, f := range frames {
			writeDelimited(enc, f)
		}
		enc.Close() //nolint:errcheck // buffer writer
		blob := buf.Bytes()
		b.SetBytes(int64(len(blob)))
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			dec, _ := zstd.NewReader(bytes.NewReader(blob))
			out, _ := dec.DecodeAll(blob, nil) //nolint:errcheck // benchmark
			_ = out
			dec.Close()
		}
		b.StopTimer()
		b.ReportMetric(float64(target), "targetFrame")
		b.ReportMetric(1, "blocksDecompressed")
	})

	for _, kc := range keyframeIntervalCases {
		if kc.frames == 0 {
			continue
		}
		b.Run("per-block/"+kc.name, func(b *testing.B) {
			var blocks [][]byte
			for start := 0; start < len(frames); start += kc.frames {
				end := min(start+kc.frames, len(frames))
				var raw bytes.Buffer
				for _, f := range frames[start:end] {
					writeDelimited(&raw, f)
				}
				var out bytes.Buffer
				enc, _ := zstd.NewWriter(&out, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderDict(dict))
				enc.Write(raw.Bytes()) //nolint:errcheck // buffer writer
				enc.Close()            //nolint:errcheck // buffer writer
				blocks = append(blocks, out.Bytes())
			}
			// Byte-range access: only the block containing the target is read.
			blk := blocks[target/kc.frames]
			b.SetBytes(int64(len(blk)))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				dec, _ := zstd.NewReader(nil, zstd.WithDecoderDicts(dict))
				out, _ := dec.DecodeAll(blk, nil) //nolint:errcheck // benchmark
				_ = out
				dec.Close()
			}
			b.StopTimer()
			b.ReportMetric(float64(target), "targetFrame")
			b.ReportMetric(1, "blocksDecompressed")
			b.ReportMetric(float64(len(blocks)), "totalBlocks")
		})
	}
}

// =============================================================================
// QUESTION: does encoder layout change how much memory a live capture churns
// through while it is being written?
//
// WHY IT MATTERS: nevr-stream holds an open writer per live match on a box with
// mem_limit 512m, so writer-side memory behaviour is a production capacity
// question, not a tidiness one.
//
// ANSWERED WHEN: NOT YET, and read this before quoting the numbers. This
// measures ALLOCATION CHURN (TotalAlloc delta), which is a proxy and not the
// same thing as peak resident memory -- and because the churn accumulates
// across b.N iterations it is comparable BETWEEN layouts in the same run and
// meaningless as an absolute. Per-block encoders allocate more total because
// each block constructs a fresh encoder; that says nothing about the high-water
// mark, which is what the 512m limit actually constrains. A real answer needs
// peak RSS of a single encode under a memory profiler, or the running service
// measured directly. Left in place because the relative churn signal is still
// worth tracking across format changes; do not cite it as peak memory.
// =============================================================================
func BenchmarkEncodePeakMemory(b *testing.B) {
	frames := loadCorpusFrames(b)

	measure := func(b *testing.B, fn func()) {
		runtime.GC()
		var before, after runtime.MemStats
		runtime.ReadMemStats(&before)
		fn()
		runtime.ReadMemStats(&after)
		b.ReportMetric(float64(after.TotalAlloc-before.TotalAlloc)/(1<<20), "churnMB")
		b.ReportMetric(float64(after.HeapSys)/(1<<20), "heapSysMB")
	}

	b.Run("whole-stream", func(b *testing.B) {
		b.ResetTimer()
		measure(b, func() { layoutWholeStream(frames, zstd.SpeedFastest) })
	})
	for _, kc := range keyframeIntervalCases {
		if kc.frames == 0 {
			continue
		}
		b.Run("per-block/"+kc.name, func(b *testing.B) {
			b.ResetTimer()
			measure(b, func() { layoutPerBlock(frames, kc.frames, zstd.SpeedFastest, nil) })
		})
	}
}

// =============================================================================
// QUESTION: what would a REAL keyframe cost — one that carries enough
// materialized state that a reader can start there without replaying from
// frame 0 — at a 1s, 2s or 5s interval?
//
// WHY IT MATTERS: this is the question BenchmarkKeyframeIntervalSizeCost cannot
// answer, and the reason that one comes out flat. v2 is tiered
// (docs/format-design.md §2): per-frame kinematic fields are "self-contained
// for random access", but identity, loadout, roster attributes, stats and the
// scoreboard are DELTA — carried by events and materialized by replaying them
// from the start (§5, and pkg/conversion/mapping.go:399-431 keeps prevLoadout /
// prevInfo so "afterwards any change becomes a delta. See ROSTER-001").
//
// So a frame at a keyframe boundary is startable for its kinematic tier and NOT
// startable for its event tier. A keyframe that carries no materialized state
// is not a keyframe — which is precisely why cmd/tapedeck/trim_seed.go has to
// rebuild seed events when it cuts a capture. THAT payload is what a keyframe
// would have to carry, and this benchmark prices it.
//
// Method: at each candidate boundary, materialize the event-tier state the way
// a seed does — keep the LAST event per (event type, player slot) seen so far —
// and measure its serialized size. This is a lower bound on a hydrated
// keyframe: real state is at most the distinct per-slot last-values.
//
// ANSWERED WHEN: the cost of hydrating at 1s/2s/5s is known as a percentage of
// the frame bytes it sits among, so "one keyframe per second" can be argued
// from a number instead of a round figure.
// =============================================================================
func BenchmarkHydratedKeyframeCost(b *testing.B) {
	frames := loadCorpusProtos(b)
	payloads := loadCorpusFrames(b)
	if len(frames) == 0 {
		b.Skip("corpus unavailable")
	}
	var frameBytes int
	for _, p := range payloads {
		frameBytes += len(p)
	}

	for _, kc := range keyframeIntervalCases {
		if kc.frames == 0 {
			continue // "none" means no keyframes to hydrate
		}
		b.Run(kc.name, func(b *testing.B) {
			state := map[string]*capturepb.EchoEvent{}
			totalSeed, seeds, maxSeed := 0, 0, 0
			for i, f := range frames {
				if i > 0 && i%kc.frames == 0 {
					// Materialize: the seed is the current last-value set.
					sz := 0
					for _, e := range state {
						sz += proto.Size(e)
					}
					totalSeed += sz
					seeds++
					if sz > maxSeed {
						maxSeed = sz
					}
				}
				if ea := f.GetEchoArena(); ea != nil {
					for _, e := range ea.GetEvents() {
						state[stateKey(e)] = e
					}
				}
			}
			if seeds == 0 {
				b.Skip("corpus shorter than one interval")
			}
			b.ReportMetric(float64(seeds), "keyframes")
			b.ReportMetric(float64(totalSeed)/float64(seeds), "seedB/keyframe")
			b.ReportMetric(float64(maxSeed), "maxSeedB")
			b.ReportMetric(float64(totalSeed), "totalSeedB")
			b.ReportMetric(100*float64(totalSeed)/float64(frameBytes), "%%overFrames")
		})
	}
}
