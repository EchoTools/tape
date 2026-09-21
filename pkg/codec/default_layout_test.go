package codec

import (
	"errors"
	"io"
	"testing"
)

// The default layout, and the option that opts out of it.
//
// WHY THIS FILE EXISTS. Per-block compression shipped as opt-in, which meant
// the property it was built for — a keyframe offset that is a servable byte
// range — was off for every caller that did not know to ask. A feature that is
// off unless named is a feature almost nobody gets, and the format was not yet
// released, so nothing held the old default in place. So the zero-option writer produces the per-block layout, and
// whole-stream is what now takes an argument.
//
// These tests assert the DEFAULT, from the constructor with no options at all —
// NewWriter, the one every existing call site uses. A test that passed
// WithPerBlockCompression() would prove the option works, which was never in
// doubt; it would not prove what a caller who passes nothing gets.

// writeDefaultCapture writes frames through the zero-option NewWriter — no
// options, no interval, nothing. This is deliberately NOT writeCapture, which
// goes through NewWriterWithOptions: the claim under test is about the plain
// constructor.
func writeDefaultCapture(t *testing.T, name string, frames int) string {
	t.Helper()
	path := t.TempDir() + "/" + name
	w, err := NewWriter(path)
	if err != nil {
		t.Fatalf("NewWriter: %v", err)
	}
	if err := w.WriteHeader(blockTestHeader()); err != nil {
		t.Fatalf("WriteHeader: %v", err)
	}
	for _, f := range blockTestFrames(frames) {
		if err := w.WriteFrame(f); err != nil {
			t.Fatalf("WriteFrame %d: %v", f.GetFrameIndex(), err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	return path
}

// readCaptureFrameIndexes reads every frame of a capture through the public
// sequential reader and returns their frame indexes.
func readCaptureFrameIndexes(t *testing.T, path string) []uint32 {
	t.Helper()
	r, err := NewReader(path)
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() {
		if closeErr := r.Close(); closeErr != nil {
			t.Errorf("Close: %v", closeErr)
		}
	}()
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var out []uint32
	for {
		f, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			return out
		}
		if err != nil {
			t.Fatalf("ReadFrame after %d frames: %v", len(out), err)
		}
		out = append(out, f.GetFrameIndex())
	}
}

// MUTATION WITNESS, recorded because a review could not find the red and the
// red is the only thing that makes this test worth having. Setting
// applyWriterOptions' perBlock back to false — exactly the whole-stream default —
// produces:
//
//	=== RUN   TestZeroOptionWriterIsPerBlockByDefault
//	    default_layout_test.go:97: the DEFAULT capture has no seek table (tape:
//	    no zstd seek table at end of file); NewWriter with no options must
//	    produce the per-block layout
//	--- FAIL: TestZeroOptionWriterIsPerBlockByDefault (0.00s)
//
// That is what a future change silently reverting the default would look like,
// and it is the regression this repository has the most history with.

// TestZeroOptionWriterIsPerBlockByDefault is that policy as code: a capture
// written with NO options is a per-block capture.
//
// The two facts that together prove a capture is genuinely blocked rather than
// merely readable are the pair property 4 of compat_baseline_test.go uses: it
// HAS a seek table (OpenBlockIndex succeeds), and it has MORE THAN ONE block.
// Either alone can be satisfied by a whole-stream file with something
// appended; both together cannot.
func TestZeroOptionWriterIsPerBlockByDefault(t *testing.T) {
	// 400 frames at DefaultKeyframeInterval (100) is 4 keyframe blocks, plus
	// the header block and the footer block: 6. The count is derived from the
	// interval rather than read off a run, so a layout change moves it.
	const frames = 400
	wantBlocks := frames/int(DefaultKeyframeInterval) + 2

	path := writeDefaultCapture(t, "zero-option.tape", frames)

	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("the DEFAULT capture has no seek table (%v); NewWriter with no "+
			"options must produce the per-block layout", err)
	}
	if index.Blocks() <= 1 {
		t.Fatalf("the DEFAULT capture has %d block(s); it is not actually blocked",
			index.Blocks())
	}
	if index.Blocks() != wantBlocks {
		t.Errorf("the DEFAULT capture has %d blocks, want %d (header + %d keyframe "+
			"blocks at interval %d + footer)",
			index.Blocks(), wantBlocks, frames/int(DefaultKeyframeInterval), DefaultKeyframeInterval)
	}
	if n := countZstdFrames(t, path); n <= 1 {
		t.Errorf("the DEFAULT capture is %d zstd frame(s), want more than 1", n)
	}

	// And it must still read back as itself through the sequential reader —
	// changing the default is worthless if it changes what comes out.
	if got := readCaptureFrameIndexes(t, path); len(got) != frames {
		t.Fatalf("default capture read back %d frames, want %d", len(got), frames)
	}
}

// TestWholeStreamCompressionIsTheOptOut is the other half of the ruling: the
// old layout is still reachable, and reaching it now takes an argument.
func TestWholeStreamCompressionIsTheOptOut(t *testing.T) {
	frames := blockTestFrames(400)
	path := writeCapture(t, "opt-out.tape", frames, WithWholeStreamCompression())

	if _, err := OpenBlockIndex(path); !errors.Is(err, ErrNoSeekTable) {
		t.Errorf("WithWholeStreamCompression: OpenBlockIndex returned %v, want ErrNoSeekTable", err)
	}
	if n := countZstdFrames(t, path); n != 1 {
		t.Errorf("WithWholeStreamCompression produced %d zstd frames, want exactly 1", n)
	}
	if got := readCaptureFrameIndexes(t, path); len(got) != len(frames) {
		t.Fatalf("opt-out capture read back %d frames, want %d", len(got), len(frames))
	}

	// Last option wins, both directions, so a caller composing option slices
	// gets what they wrote rather than what the order happened to be.
	back := writeCapture(t, "opt-out-then-in.tape", frames,
		WithWholeStreamCompression(), WithPerBlockCompression())
	if _, err := OpenBlockIndex(back); err != nil {
		t.Errorf("WithPerBlockCompression after WithWholeStreamCompression: %v, want a seek table", err)
	}
}
