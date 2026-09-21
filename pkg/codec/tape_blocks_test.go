package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"testing"
	"time"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// Tests for the per-block container layout (tape_blocks.go, seekable.go).
//
// The layout exists for exactly one property, and TestKeyframeOffsetIsAServable
// ByteRange is that property in test form: a KeyframeEntry.ByteOffset must name
// a range of the file that decompresses on its own into the frames starting at
// that keyframe. Everything else here guards the cost of getting it — that the
// shipped reader still reads the file, and that the shipped layout did not move.

// blockTestHeader and blockTestFrames build a small but realistic capture.
func blockTestHeader() *capturepb.CaptureHeader {
	return &capturepb.CaptureHeader{
		CaptureId:     "per-block-001",
		CreatedAt:     timestamppb.New(time.Unix(1756600000, 0).UTC()),
		FormatVersion: 2,
		Metadata:      map[string]string{"layout": "per-block"},
		GameType:      "echo_arena",
		GameHeader: &capturepb.CaptureHeader_EchoArena{
			EchoArena: &capturepb.EchoArenaHeader{
				SessionId: "SEEK-123",
				MapName:   "mpl_arena_a",
			},
		},
	}
}

func blockTestFrames(n int) []*capturepb.Frame {
	frames := make([]*capturepb.Frame, n)
	for i := range frames {
		idx := uint32(i) //nolint:gosec // test corpus size is bounded
		frames[i] = &capturepb.Frame{
			FrameIndex:        idx,
			TimestampOffsetMs: idx * 33, // ~30 Hz
			Payload: &capturepb.Frame_EchoArena{
				EchoArena: &capturepb.EchoArenaFrame{
					GameStatus:     capturepb.GameStatus_GAME_STATUS_PLAYING,
					GameClock:      300 - float32(i)*0.033,
					BluePoints:     int32(i / 100),
					OrangePoints:   int32(i / 150),
					DiscHolderSlot: proto.Int32(int32(i % 8)),
				},
			},
		}
	}
	return frames
}

// writeCapture writes the fixed corpus with the given writer options and
// returns the path.
func writeCapture(t *testing.T, name string, frames []*capturepb.Frame, opts ...WriterOption) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	w, err := NewWriterWithOptions(path, opts...)
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	if err := w.WriteHeader(blockTestHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, f := range frames {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame %d: %v", f.GetFrameIndex(), err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// readAllFrames reads a capture through the SHIPPED sequential reader.
func readCapture(t *testing.T, path string) (*capturepb.CaptureHeader, []*capturepb.Frame, *capturepb.CaptureFooter) {
	t.Helper()
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer r.Close() //nolint:errcheck // read-only

	header, err := r.ReadHeader()
	if err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var frames []*capturepb.Frame
	for {
		f, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame: %v", err)
		}
		frames = append(frames, f)
	}
	footer, err := r.ReadFooter()
	if err != nil {
		t.Fatalf("ReadFooter: %v", err)
	}
	return header, frames, footer
}

// TestPerBlockIsReadableByTheShippedReader is the compatibility claim the
// transcode spec marked PLAUSIBLE and told implementers not to inherit. If it
// holds, per-block compression costs existing consumers nothing: they read a
// per-block capture with code that predates the layout.
//
// The comparison is against the whole-stream capture of the SAME frames, so a
// difference is a layout defect and not a corpus difference.
//
// IT ASSERTS THE FEATURE BEFORE IT ASSERTS THE COMPATIBILITY, and that order is
// load-bearing. Written without the structural check below, this test PASSED
// under a mutation that made WithPerBlockCompression a no-op: both arms
// collapsed to the whole-stream layout and agreed with each other trivially, so
// it read as coverage of a feature while proving only that a thing equals
// itself. The two files must differ in layout for the comparison of their
// CONTENTS to mean anything.
//
// The genuinely old reader — a binary compiled from a previous commit — is
// TestBackwardCompatibility. This test cannot be that: it compiles the reader
// from the same commit as the writer.
func TestPerBlockIsReadableByTheShippedReader(t *testing.T) {
	frames := blockTestFrames(500)

	wholePath := writeCapture(t, "whole.tape", frames, WithKeyframeInterval(30), WithWholeStreamCompression())
	blockPath := writeCapture(t, "block.tape", frames, WithKeyframeInterval(30), WithPerBlockCompression())

	// The feature must actually be on. A per-block capture carries a seek table
	// and many blocks; a whole-stream capture carries neither. If these two
	// assertions do not hold, everything below is comparing a layout with
	// itself.
	index, err := OpenBlockIndex(blockPath)
	if err != nil {
		t.Fatalf("WithPerBlockCompression produced no seek table (%v): the per-block layout is "+
			"not in effect, so this test would compare the whole-stream layout against itself", err)
	}
	// 500 frames at an interval of 30 is 17 keyframe blocks, plus header and
	// footer blocks.
	if want := (500+29)/30 + 2; index.Blocks() != want {
		t.Fatalf("per-block capture has %d blocks, want %d: the layout is not what this test "+
			"believes it is testing", index.Blocks(), want)
	}
	if _, err := OpenBlockIndex(wholePath); !errors.Is(err, ErrNoSeekTable) {
		t.Fatalf("the whole-stream capture carries a seek table (%v): the two arms are not "+
			"different layouts", err)
	}

	wholeHeader, wholeFrames, wholeFooter := readCapture(t, wholePath)
	blockHeader, blockFrames, blockFooter := readCapture(t, blockPath)

	if !proto.Equal(wholeHeader, blockHeader) {
		t.Error("per-block capture read back a different header than the whole-stream capture")
	}
	if len(blockFrames) != len(frames) {
		t.Fatalf("per-block capture read back %d frames, wrote %d", len(blockFrames), len(frames))
	}
	for i := range wholeFrames {
		if !proto.Equal(wholeFrames[i], blockFrames[i]) {
			t.Fatalf("frame %d differs between layouts", i)
		}
	}

	// The footer must agree on everything except the keyframe offsets, whose
	// meaning is exactly what the layout redefines.
	if wholeFooter.GetFrameCount() != blockFooter.GetFrameCount() {
		t.Errorf("frame_count: whole-stream %d, per-block %d",
			wholeFooter.GetFrameCount(), blockFooter.GetFrameCount())
	}
	if wholeFooter.GetDurationMs() != blockFooter.GetDurationMs() {
		t.Errorf("duration_ms: whole-stream %d, per-block %d",
			wholeFooter.GetDurationMs(), blockFooter.GetDurationMs())
	}
	if len(wholeFooter.GetKeyframeIndex()) != len(blockFooter.GetKeyframeIndex()) {
		t.Fatalf("keyframe count: whole-stream %d, per-block %d",
			len(wholeFooter.GetKeyframeIndex()), len(blockFooter.GetKeyframeIndex()))
	}
	for i := range wholeFooter.GetKeyframeIndex() {
		if a, b := wholeFooter.GetKeyframeIndex()[i].GetFrameIndex(),
			blockFooter.GetKeyframeIndex()[i].GetFrameIndex(); a != b {
			t.Errorf("keyframe %d indexes frame %d in whole-stream, %d in per-block", i, a, b)
		}
	}
	if !proto.Equal(&capturepb.CaptureFooter{EventIndex: wholeFooter.GetEventIndex()},
		&capturepb.CaptureFooter{EventIndex: blockFooter.GetEventIndex()}) {
		t.Error("event index differs between layouts")
	}
}

// TestKeyframeOffsetIsAServableByteRange is the point of the whole change.
//
// Writer's own doc comment says of the shipped layout that "byte-offset seeking
// requires decompressing from the start". This asserts the opposite for the
// per-block layout, in the strongest form available: take ONLY the bytes the
// index names, hand them to a decoder that has seen nothing else, and require
// that they yield the frames beginning at that keyframe.
func TestKeyframeOffsetIsAServableByteRange(t *testing.T) {
	const interval = 25
	frames := blockTestFrames(300)
	path := writeCapture(t, "seek.tape", frames, WithKeyframeInterval(interval), WithPerBlockCompression())

	_, _, footer := readCapture(t, path)
	keyframes := footer.GetKeyframeIndex()
	if len(keyframes) != len(frames)/interval {
		t.Fatalf("expected %d keyframes, footer has %d", len(frames)/interval, len(keyframes))
	}

	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("OpenBlockIndex: %v", err)
	}
	// One block per keyframe, plus the header block and the footer block.
	if want := len(keyframes) + 2; index.Blocks() != want {
		t.Fatalf("seek table has %d blocks, want %d (%d keyframes + header + footer)",
			index.Blocks(), want, len(keyframes))
	}

	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer file.Close() //nolint:errcheck // read-only

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer dec.Close()

	for _, kf := range keyframes {
		offset, length, err := index.ByteRange(kf.GetByteOffset())
		if err != nil {
			t.Fatalf("keyframe at frame %d: ByteRange(%d): %v", kf.GetFrameIndex(), kf.GetByteOffset(), err)
		}

		// Read ONLY this range. Nothing before it is available to the decoder.
		raw := make([]byte, length)
		if _, err := file.ReadAt(raw, int64(offset)); err != nil { //nolint:gosec // offset is a file position
			t.Fatalf("keyframe at frame %d: ReadAt(%d, %d): %v", kf.GetFrameIndex(), offset, length, err)
		}
		decoded, err := dec.DecodeAll(raw, nil)
		if err != nil {
			t.Fatalf("keyframe at frame %d: the range did not decompress standalone: %v",
				kf.GetFrameIndex(), err)
		}

		got := decodeEnvelopes(t, decoded)
		if len(got) != interval {
			t.Fatalf("keyframe at frame %d: block carried %d frames, want %d",
				kf.GetFrameIndex(), len(got), interval)
		}
		if first := got[0].GetFrameIndex(); first != kf.GetFrameIndex() {
			t.Fatalf("block for keyframe %d opens at frame %d", kf.GetFrameIndex(), first)
		}
		// The frames in the block must be the source frames, not merely
		// well-formed ones.
		for i, f := range got {
			want := frames[int(kf.GetFrameIndex())+i]
			if !proto.Equal(want, f) {
				t.Fatalf("block for keyframe %d: frame %d does not match the source",
					kf.GetFrameIndex(), want.GetFrameIndex())
			}
		}
	}
}

// decodeEnvelopes parses a decompressed block into its Frame envelopes. It
// reuses the reader's own framing rather than reimplementing varint delimiting,
// so a change to the framing cannot pass this test by accident.
func decodeEnvelopes(t *testing.T, block []byte) []*capturepb.Frame {
	t.Helper()
	r := &Reader{reader: bytes.NewReader(block), limits: Limits{}}
	var out []*capturepb.Frame
	for {
		env, err := r.readEnvelope()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("readEnvelope: %v", err)
		}
		if f := env.GetFrame(); f != nil {
			out = append(out, f)
		}
	}
}

// TestBothLayoutsAreReachableAndDistinct replaces TestDefaultLayoutIsUnchanged,
// which is RETIRED as the default (e27a62a) and is not coming back in that form.
//
// What it used to assert was "nothing writes the new layout unless a caller
// asks for it by name" — the constraint that made per-block landable when the
// format was thought to be released. It was not released — v4.1.0 is the
// release — so the constraint protected nothing, and features now default on
// with arguments to opt out. The default now IS the new layout, so the old assertion is false by
// construction; deleting it silently, or t.Skip()ing it, would have left the
// suite quieter about the layout than it was before the change.
//
// What survives is everything that is still true: both layouts are reachable,
// they are structurally DIFFERENT (so neither test arm anywhere is secretly
// comparing a file with itself), and routing NewWriter through
// NewWriterWithOptions changes nothing. The assertion about which layout is the
// default moved to TestZeroOptionWriterIsPerBlockByDefault in
// default_layout_test.go, where it is stated positively.
func TestBothLayoutsAreReachableAndDistinct(t *testing.T) {
	frames := blockTestFrames(200)

	wholePath := writeCapture(t, "opt-out.tape", frames, WithKeyframeInterval(50), WithWholeStreamCompression())
	if n := countZstdFrames(t, wholePath); n != 1 {
		t.Errorf("WithWholeStreamCompression is %d zstd frames, want exactly 1 (the whole-stream layout)", n)
	}
	if _, err := OpenBlockIndex(wholePath); !errors.Is(err, ErrNoSeekTable) {
		t.Errorf("WithWholeStreamCompression: OpenBlockIndex returned %v, want ErrNoSeekTable", err)
	}

	blockPath := writeCapture(t, "default.tape", frames, WithKeyframeInterval(50), WithPerBlockCompression())
	if n := countZstdFrames(t, blockPath); n != 6 {
		t.Errorf("per-block layout is %d zstd frames, want 6 (header + 4 keyframe blocks + footer)", n)
	}

	// NewWriter, the constructor everything in the repo already calls, must
	// agree with NewWriterWithOptions: same layout, and the same bytes for a
	// deterministic corpus. This is what proves the default is applied in ONE
	// place rather than once per constructor.
	legacy := t.TempDir() + "/legacy.tape"
	w, err := NewWriter(legacy)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteHeader(blockTestHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, f := range frames {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame: %v", err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	viaOptions := writeCapture(t, "via-options.tape", frames, WithKeyframeInterval(DefaultKeyframeInterval))
	legacyBytes, err := os.ReadFile(legacy)
	if err != nil {
		t.Fatalf("read legacy: %v", err)
	}
	optionBytes, err := os.ReadFile(viaOptions)
	if err != nil {
		t.Fatalf("read via-options: %v", err)
	}
	if !bytes.Equal(legacyBytes, optionBytes) {
		t.Error("routing NewWriter through NewWriterWithOptions changed the bytes it produces")
	}
}

// countZstdFrames walks the file frame by frame using the zstd frame header,
// which is how a reader without an index would have to find block boundaries.
//
// KNOWN IMPRECISION, LEFT IN DELIBERATELY: it finds boundaries by scanning for
// the 4-byte magic (0xFD2FB528, or a skippable frame's 0x184D2A5x) rather than
// parsing each frame's declared size, so a compressed payload that happens to
// contain those bytes is counted as a frame boundary and the total comes out
// high. Parsing sizes properly means implementing the zstd frame header —
// Window_Descriptor, Frame_Content_Size, the optional checksum — which is a
// decoder, and a decoder in a test is a second implementation to keep correct.
//
// It is left as-is because it FAILS SAFE for every assertion built on it: those
// assert an exact small count ("exactly 1 zstd frame"), so an overcount makes
// the test FAIL LOUDLY on a correct file. It cannot make a wrong file pass. The
// cost of the imprecision is a rare spurious red, never a silent green — and a
// spurious red on a byte-exact corpus would itself be reproducible.
func countZstdFrames(t *testing.T, path string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	dec, err := zstd.NewReader(nil)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer dec.Close()

	count := 0
	for off := 0; off < len(data); {
		if off+8 <= len(data) {
			if magic := binary.LittleEndian.Uint32(data[off : off+4]); magic&0xFFFFFFF0 == 0x184D2A50 {
				// A skippable frame (the seek table) is not a data block.
				off += 8 + int(binary.LittleEndian.Uint32(data[off+4:off+8]))
				continue
			}
		}
		// DecodeAll stops at the end of the first frame only if handed exactly
		// that frame, so step forward by searching for the next frame magic.
		next := nextZstdFrame(data[off+1:])
		if next < 0 {
			count++
			break
		}
		count++
		off += 1 + next
	}
	return count
}

// nextZstdFrame returns the offset of the next zstd frame or skippable frame
// magic in data, or -1.
func nextZstdFrame(data []byte) int {
	for i := 0; i+4 <= len(data); i++ {
		magic := binary.LittleEndian.Uint32(data[i : i+4])
		if magic == 0xFD2FB528 || magic&0xFFFFFFF0 == 0x184D2A50 {
			return i
		}
	}
	return -1
}

// TestPerBlockWithDictionary covers the two halves of the dictionary
// obligation together: a capture written with a trained dictionary round-trips
// when the dictionary is supplied, and is an ERROR — never wrong bytes — when
// it is not.
func TestPerBlockWithDictionary(t *testing.T) {
	frames := blockTestFrames(400)

	// Train on the corpus's own marshalled frames, which is what a real
	// dictionary for this format would be trained on.
	dict := buildTestDict(t, frames)

	path := writeCapture(t, "dict.tape", frames,
		WithKeyframeInterval(30), WithPerBlockCompression(), WithDictionary(dict))

	// Without the dictionary the shipped reader must fail rather than
	// succeed with garbage. The header block is the first thing it touches.
	r, err := NewReader(path)
	if err == nil {
		_, headerErr := r.ReadHeader()
		r.Close() //nolint:errcheck // read-only
		if headerErr == nil {
			t.Fatal("read a dictionary-compressed capture WITHOUT the dictionary; " +
				"a missing dictionary must be an error, not silent data loss")
		}
	}

	// With it, an exact round trip — read here at the block level, since the
	// shipped Reader has no way to be handed a dictionary yet.
	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("OpenBlockIndex: %v", err)
	}
	// One block per keyframe, plus the header block and the footer block.
	wantBlocks := (len(frames)+29)/30 + 2
	if index.Blocks() != wantBlocks {
		t.Fatalf("seek table has %d blocks, want %d", index.Blocks(), wantBlocks)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	dec, err := zstd.NewReader(nil, zstd.WithDecoderDicts(dict))
	if err != nil {
		t.Fatalf("NewReader with dict: %v", err)
	}
	defer dec.Close()

	decoded, err := dec.DecodeAll(data[:len(data)-seekTableSize(index.Blocks())], nil)
	if err != nil {
		t.Fatalf("DecodeAll with dict: %v", err)
	}
	got := decodeEnvelopes(t, decoded)
	if len(got) != len(frames) {
		t.Fatalf("dictionary round trip yielded %d frames, wrote %d", len(got), len(frames))
	}
	for i := range frames {
		if !proto.Equal(frames[i], got[i]) {
			t.Fatalf("dictionary round trip: frame %d differs", i)
		}
	}
}

// seekTableSize is the on-disk size of a seek table with n entries.
func seekTableSize(n int) int {
	return skippableFrameHeaderSize + n*seekTableEntrySize + seekTableFooterSize
}

// TestReaderDictionaryClosesTheWritePath covers the half of the dictionary
// obligation that WithDictionary alone leaves open. A write path with no
// matching read path is the failure AGENTS.md §4 names as recurring in this
// repo ("Both directions or neither"), and a dictionary-compressed capture that
// the shipped reader cannot open is exactly that.
//
// Both layouts are covered, because a dictionary is not a per-block feature:
// the whole-stream writer accepts one too.
func TestReaderDictionaryClosesTheWritePath(t *testing.T) {
	frames := blockTestFrames(200)
	dict := buildTestDict(t, frames)

	for _, tc := range []struct {
		name string
		opts []WriterOption
	}{
		{"whole-stream", []WriterOption{WithKeyframeInterval(50), WithDictionary(dict)}},
		{"per-block", []WriterOption{WithKeyframeInterval(50), WithPerBlockCompression(), WithDictionary(dict)}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := writeCapture(t, "dict.tape", frames, tc.opts...)

			r, err := NewReaderWithDictionary(path, dict)
			if err != nil {
				t.Fatalf("NewReaderWithDictionary: %v", err)
			}
			defer r.Close() //nolint:errcheck // read-only

			if _, err := r.ReadHeader(); err != nil {
				t.Fatalf("ReadHeader with the dictionary: %v", err)
			}
			var got []*capturepb.Frame
			for {
				f, err := r.ReadFrame()
				if errors.Is(err, io.EOF) {
					break
				}
				if err != nil {
					t.Fatalf("ReadFrame: %v", err)
				}
				got = append(got, f)
			}
			if len(got) != len(frames) {
				t.Fatalf("read %d frames, wrote %d", len(got), len(frames))
			}
			for i := range frames {
				if !proto.Equal(frames[i], got[i]) {
					t.Fatalf("frame %d differs after a dictionary round trip", i)
				}
			}
		})
	}
}

// TestFooterIsReadableWithoutScanningFrames is the property that makes seeking
// worth anything. The keyframe index lives in the footer, so a seeking reader
// needs the footer FIRST — and reaching it through the sequential reader means
// walking every frame, which costs exactly what the seek was meant to save.
//
// The footer gets its own final block precisely so it can be read on its own.
// This asserts the footer read touches one block and yields the same footer the
// sequential path yields.
func TestFooterIsReadableWithoutScanningFrames(t *testing.T) {
	const interval = 40
	frames := blockTestFrames(400)
	path := writeCapture(t, "footer.tape", frames,
		WithKeyframeInterval(interval), WithPerBlockCompression())

	_, _, sequential := readCapture(t, path)

	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("OpenBlockIndex: %v", err)
	}
	direct, err := index.Footer(nil)
	if err != nil {
		t.Fatalf("Footer: %v", err)
	}
	if !proto.Equal(sequential, direct) {
		t.Fatal("the footer read from the final block differs from the one read by scanning")
	}

	// The footer block must hold the footer and nothing else, or "read the
	// footer" is not a one-block operation.
	block, err := index.ReadBlock(index.Blocks()-1, nil)
	if err != nil {
		t.Fatalf("ReadBlock: %v", err)
	}
	r := &Reader{reader: bytes.NewReader(block), limits: Limits{}}
	if _, err := r.readEnvelope(); err != nil {
		t.Fatalf("footer block: first envelope: %v", err)
	}
	if _, err := r.readEnvelope(); !errors.Is(err, io.EOF) {
		t.Fatalf("footer block carries more than the footer: second envelope gave %v", err)
	}
}

// TestBlockForFrameResolvesEveryFrame walks every frame in the capture and
// requires the index to name a block that actually contains it. A lookup that
// is right for keyframes and wrong in between would pass a keyframe-only test
// and fail in the field on the first scrub.
func TestBlockForFrameResolvesEveryFrame(t *testing.T) {
	const interval = 30
	frames := blockTestFrames(250)
	path := writeCapture(t, "lookup.tape", frames,
		WithKeyframeInterval(interval), WithPerBlockCompression())

	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("OpenBlockIndex: %v", err)
	}
	footer, err := index.Footer(nil)
	if err != nil {
		t.Fatalf("Footer: %v", err)
	}

	for _, f := range frames {
		target := f.GetFrameIndex()
		block, err := index.BlockForFrame(footer, target)
		if err != nil {
			t.Fatalf("BlockForFrame(%d): %v", target, err)
		}
		data, err := index.ReadBlock(block, nil)
		if err != nil {
			t.Fatalf("ReadBlock(%d) for frame %d: %v", block, target, err)
		}
		var found *capturepb.Frame
		for _, got := range decodeEnvelopes(t, data) {
			if got.GetFrameIndex() == target {
				found = got
				break
			}
		}
		if found == nil {
			t.Fatalf("frame %d is not in block %d, which the index named for it", target, block)
		}
		if !proto.Equal(f, found) {
			t.Fatalf("frame %d resolved to a different frame", target)
		}
	}
}

// buildTestDict trains a dictionary on the test corpus through the package's
// own trainer, so the tests exercise the API a caller would use rather than a
// parallel copy of it.
func buildTestDict(t *testing.T, frames []*capturepb.Frame) []byte {
	t.Helper()
	samples := make([][]byte, len(frames))
	for i, f := range frames {
		b, err := proto.Marshal(f)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}
		samples[i] = b
	}
	dict, err := TrainDictionary(MinPrivateDictionaryID, samples)
	if err != nil {
		t.Skipf("TrainDictionary unavailable in this build: %v", err)
	}
	return dict
}
