package codec

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"google.golang.org/protobuf/proto"
)

// Corruption, detected rather than served.
//
// WHY THIS FILE EXISTS. A bug hunt on 2026-09-05 (F1,
// spritz/findings/2026-09-05-tape-bughunt-fable.md) measured the shipped reader
// against every single-bit flip of a 7,553-byte capture:
//
//	error            25098
//	clean-identical    241
//	SILENT-WRONG     35085   <- clean read, wrong frame content, no error
//	flips inside the 4 checksum bytes: 32, of which reported an error: 0
//
// 58% of flips read back as a clean capture with wrong telemetry. zstd writes a
// content checksum on every frame and the reader never reached it: ReadFrame
// returns io.EOF the instant it sees the footer envelope, and klauspost's
// streaming decoder verifies the checksum only when the frame's end is
// consumed. The whole-stream layout is ONE frame, so the check was never
// reached for any capture ever written.
//
// limits.go already says what that means, about tape's own trailer, in words
// that apply exactly: "A reader that ignores its own trailer reports success on
// a file that lost data, which is the one failure this library must never
// have." The trailer being ignored was zstd's.
//
// WHY A SWEEP AND NOT ONE FIXTURE. A single committed corrupt file proves one
// byte position and silently stops covering the rest the moment the corpus,
// the compressor or the layout moves. The property is universal — NO single-bit
// flip anywhere may produce a clean read of wrong content — so the test is
// universal over the file, and it re-derives its own corpus every run. It also
// cannot pass vacuously: it asserts a floor on how many flips it actually
// exercised and how many the reader rejected, so a corpus that stopped being
// corruptible fails here instead of going quiet.

// THE CORPUS THESE NUMBERS COME FROM, stated because the findings doc reports
// 35,085 of 60,424 for the same bug and two different numbers for one defect
// invite someone later to pick one. The bug hunt swept every single bit of a
// 7,553-byte capture of 60 float-heavy frames (8 players x Pose). This sweep
// uses corruptionCorpus below — 60 frames of blockTestFrames at keyframe
// interval 10, which is 836 bytes whole-stream and 1,216 per-block — and flips
// three bits per byte rather than all eight. Different corpus, same defect,
// and the rate differs because the payloads differ.

// corruptionCorpus builds a float-heavy capture and returns its path and the
// frames that went into it. Floats are the point: at SpeedFastest most of a
// real telemetry frame is raw literals, and a flipped literal byte is a
// different valid float inside a valid protobuf — which is why the silent-wrong
// rate is 58% and not 5%.
func corruptionCorpus(t *testing.T, name string, opts ...WriterOption) (string, []*capturepb.Frame) {
	t.Helper()
	frames := blockTestFrames(60)
	path := filepath.Join(t.TempDir(), name)
	w, err := NewWriterWithOptions(path, append([]WriterOption{WithKeyframeInterval(10)}, opts...)...)
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
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
	return path, frames
}

// readWholeCapture reads a capture the way a CAREFUL consumer does — header,
// frames to io.EOF, footer, Close — and returns the frames together with the
// first error any of those steps produced. A nil error is the reader saying
// "this file is intact", which is the claim under test.
func readWholeCapture(t *testing.T, path string) ([]*capturepb.Frame, error) {
	t.Helper()
	r, err := NewReader(path)
	if err != nil {
		return nil, err
	}
	var out []*capturepb.Frame
	readErr := func() error {
		if _, err := r.ReadHeader(); err != nil {
			return err
		}
		for {
			f, err := r.ReadFrame()
			if errors.Is(err, io.EOF) {
				break
			}
			if err != nil {
				return err
			}
			out = append(out, f)
		}
		if _, err := r.ReadFooter(); err != nil {
			return err
		}
		return nil
	}()
	if closeErr := r.Close(); readErr == nil {
		readErr = closeErr
	}
	return out, readErr
}

// readFramesOnly reads a capture the way the consumers in this repo ACTUALLY
// do: header, then ReadFrame until io.EOF, and no ReadFooter at all.
// conversion.OpenSession (session.go:88-98), NewSessionReconstructor
// (reconstruct.go:170-180) and tapedeck stats/verify/trim/diff/replay are all
// this shape.
//
// IT EXISTS BECAUSE THE FIRST VERSION OF THIS SWEEP USED readWholeCapture AND
// COULD NOT FAIL. Calling ReadFooter caught every truncation by itself, so the
// sweep reported green with F2 unfixed — verified by mutation: defaulting
// requireFooter to false left it passing. The defect was never on the path
// that calls ReadFooter; it is on the path that does not, which is every
// consumer. A test modelling a more careful caller than the real one measures
// a program nobody runs.
func readFramesOnly(t *testing.T, path string) ([]*capturepb.Frame, error) {
	t.Helper()
	r, err := NewReader(path)
	if err != nil {
		return nil, err
	}
	var out []*capturepb.Frame
	readErr := func() error {
		if _, err := r.ReadHeader(); err != nil {
			return err
		}
		for {
			f, err := r.ReadFrame()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			out = append(out, f)
		}
	}()
	if closeErr := r.Close(); readErr == nil {
		readErr = closeErr
	}
	return out, readErr
}

// TestNoBitFlipIsSilentlyAccepted is F1 in assertion form: over every
// single-bit flip of a real capture, the reader must never return a clean read
// of content that is not what was written.
func TestNoBitFlipIsSilentlyAccepted(t *testing.T) {
	for _, layout := range []struct {
		name string
		opts []WriterOption
	}{
		{"default-perblock", nil},
		{"whole-stream", []WriterOption{WithWholeStreamCompression()}},
	} {
		t.Run(layout.name, func(t *testing.T) {
			path, truth := corruptionCorpus(t, "corpus.tape", layout.opts...)
			clean, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read corpus: %v", err)
			}
			// The corpus must read cleanly BEFORE anything is flipped.
			// Otherwise every "rejected" below is the reader rejecting a file
			// that was never valid, and the sweep proves nothing.
			if got, err := readWholeCapture(t, path); err != nil {
				t.Fatalf("the unmodified corpus does not read cleanly: %v", err)
			} else if len(got) != len(truth) {
				t.Fatalf("the unmodified corpus read back %d frames, wrote %d", len(got), len(truth))
			}

			dir := t.TempDir()
			target := filepath.Join(dir, "flipped.tape")

			var rejected, silentWrong, cleanIdentical, exercised int
			var firstSilent string
			for off := range clean {
				for _, bit := range []uint{0, 3, 7} {
					damaged := bytes.Clone(clean)
					damaged[off] ^= 1 << bit
					if err := os.WriteFile(target, damaged, 0o600); err != nil {
						t.Fatalf("write flipped: %v", err)
					}
					exercised++

					got, err := readFramesOnly(t, target)
					if err != nil {
						rejected++
						continue
					}
					// A clean read. Every frame must then be exactly what was
					// written; anything else is silent corruption.
					identical := len(got) == len(truth)
					if identical {
						for i := range got {
							if !proto.Equal(truth[i], got[i]) {
								identical = false
								break
							}
						}
					}
					if identical {
						cleanIdentical++
						continue
					}
					silentWrong++
					if firstSilent == "" {
						firstSilent = describeSilentRead(truth, got, off, bit)
					}
				}
			}

			if silentWrong > 0 {
				t.Errorf("SILENT CORRUPTION: %d of %d single-bit flips read back as a clean "+
					"capture with content that is not what was written (F1). The reader must "+
					"verify zstd's content checksum.\n  first: %s",
					silentWrong, exercised, firstSilent)
			}

			// Guard the guard. A sweep that exercised nothing, or that rejected
			// nothing, would report zero silent reads and look identical to a
			// pass.
			if exercised < 3*len(clean) {
				t.Fatalf("the sweep exercised %d flips over a %d-byte file; it did not run",
					exercised, len(clean))
			}
			if rejected == 0 {
				t.Fatalf("the reader rejected NONE of %d flips; it is not reading the damaged "+
					"file at all", exercised)
			}
			t.Logf("%d flips over %d bytes: %d rejected, %d clean-and-identical, %d SILENT-WRONG",
				exercised, len(clean), rejected, cleanIdentical, silentWrong)
		})
	}
}

// TestCorruptedContentChecksumIsRejected pins the sharpest case in the F1
// measurement: all 32 flips inside the 4 checksum bytes were accepted. Those
// leave the payload intact, so a content comparison cannot catch them — the
// only thing that can is actually verifying the checksum, which makes this the
// one assertion that fails for exactly one reason.
func TestCorruptedContentChecksumIsRejected(t *testing.T) {
	path, _ := corruptionCorpus(t, "corpus.tape", WithWholeStreamCompression())
	clean, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	if _, err := readWholeCapture(t, path); err != nil {
		t.Fatalf("the unmodified corpus does not read cleanly: %v", err)
	}

	// In the whole-stream layout the file is exactly one zstd frame, so the
	// last four bytes are that frame's content checksum (klauspost writes one
	// by default). Corrupting only those changes no payload byte at all.
	damaged := bytes.Clone(clean)
	for i := len(damaged) - 4; i < len(damaged); i++ {
		damaged[i] ^= 0xFF
	}
	target := filepath.Join(t.TempDir(), "bad-checksum.tape")
	if err := os.WriteFile(target, damaged, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if _, err := readWholeCapture(t, target); err == nil {
		t.Error("a capture whose zstd content checksum was overwritten read back with NO " +
			"error. The checksum is written on every frame and was never verified (F1): the " +
			"reader stops at the footer envelope and never consumes the end of the zstd frame.")
	}
}

// describeSilentRead names the first frame that came back wrong, so a failure
// says what was served rather than only that something was.
func describeSilentRead(truth, got []*capturepb.Frame, off int, bit uint) string {
	if len(got) != len(truth) {
		return "byte " + itoa(off) + " bit " + itoa(int(bit)) + ": read " +
			itoa(len(got)) + " frames, wrote " + itoa(len(truth))
	}
	for i := range got {
		if !proto.Equal(truth[i], got[i]) {
			return "byte " + itoa(off) + " bit " + itoa(int(bit)) + ": frame " + itoa(i) +
				"\n    wrote " + truth[i].String() + "\n    read  " + got[i].String()
		}
	}
	return "byte " + itoa(off) + " bit " + itoa(int(bit)) + ": (no differing frame found)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}

// TestTruncationIsNeverACleanShortRead is F2: a capture cut anywhere must not
// read back as a shorter, successful capture.
//
// WHY IT IS NEW WITH THE PER-BLOCK LAYOUT. ReadFrame's own contract says "a
// truncated or concatenated capture is reported rather than read as a
// short-but-successful one. Callers that scan to the end therefore get the
// integrity check for free." That sentence was TRUE for whole-stream by
// accident: a cut mid-frame is a zstd error at every offset, so the promise
// held without anything implementing it. Independent per-block frames make
// every block boundary a clean EOF, so the same cut lands as success. Measured
// on a 35-frame capture: 4 of 697 truncation offsets returned 10 frames and
// err=nil, and `tapedeck show` printed "frames: 10" and exited 0.
//
// The sweep is over EVERY offset because the failure was at 4 of them — a
// spot-check at a plausible boundary is exactly the test that would have
// missed it.
func TestTruncationIsNeverACleanShortRead(t *testing.T) {
	for _, layout := range []struct {
		name string
		opts []WriterOption
	}{
		{"default-perblock", nil},
		{"whole-stream", []WriterOption{WithWholeStreamCompression()}},
	} {
		t.Run(layout.name, func(t *testing.T) {
			path, truth := corruptionCorpus(t, "corpus.tape", layout.opts...)
			clean, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read corpus: %v", err)
			}
			if _, err := readFramesOnly(t, path); err != nil {
				t.Fatalf("the unmodified corpus does not read cleanly: %v", err)
			}

			target := filepath.Join(t.TempDir(), "cut.tape")
			var rejected, silentShort, cleanAndComplete int
			var offsets []int
			// THE PROPERTY, stated precisely, because a looser version of it
			// failed this test on a case that is not data loss. A truncation
			// must either be REPORTED, or return the capture COMPLETE. What it
			// must never do is return a SHORT capture with no error — that is
			// F2, and it is the case where a consumer acts on partial data
			// believing it is whole.
			//
			// The complete-and-clean outcome is real and is not a defect: the
			// last bytes of a per-block capture are the seek table, and cutting
			// only those loses the INDEX, not a frame. The index's own reader
			// reports that (see the OpenBlockIndex assertion below); the
			// sequential reader has nothing to report, because nothing it
			// returns is wrong.
			//
			// Every cut from 1 byte to one byte short of the whole file. A cut
			// at len(clean) is the intact file and is not a truncation.
			for cut := 1; cut < len(clean); cut++ {
				if err := os.WriteFile(target, clean[:cut], 0o600); err != nil {
					t.Fatalf("write truncated: %v", err)
				}
				got, err := readFramesOnly(t, target)
				if err != nil {
					rejected++
					continue
				}
				if len(got) == len(truth) {
					// Every frame back, byte for byte, from a file missing only
					// its trailing index. Verify that rather than assume it.
					for i := range got {
						if !proto.Equal(truth[i], got[i]) {
							t.Fatalf("cut at %d read back cleanly but frame %d differs from "+
								"what was written; that is silent corruption, not a lost index",
								cut, i)
						}
					}
					cleanAndComplete++
					continue
				}
				silentShort++
				offsets = append(offsets, cut)
			}

			if silentShort > 0 {
				show := offsets
				if len(show) > 12 {
					show = show[:12]
				}
				t.Errorf("SILENT DATA LOSS: %d of %d truncation offsets read back as a CLEAN "+
					"capture (F2). A cut file must be reported, not served short.\n  offsets: %v%s",
					silentShort, len(clean)-1, show,
					map[bool]string{true: " (truncated list)", false: ""}[len(offsets) > 12])
			}
			if rejected == 0 {
				t.Fatalf("the reader rejected NONE of %d truncations; it is not reading the "+
					"cut file at all", len(clean)-1)
			}

			// The other half of the property, and the reason the
			// complete-and-clean outcome above is licensed: a cut that costs
			// the capture its seek table must be visible to the reader that
			// USES the seek table. Silence there would mean a servable capture
			// stopped being servable with nothing said.
			if len(layout.opts) == 0 {
				noIndex := filepath.Join(t.TempDir(), "no-index.tape")
				if err := os.WriteFile(noIndex, clean[:len(clean)-1], 0o600); err != nil {
					t.Fatalf("write: %v", err)
				}
				if _, err := OpenBlockIndex(noIndex); err == nil {
					t.Error("a per-block capture with its seek table cut short opened a block " +
						"index with no error; the index reader cannot see its own damage")
				} else {
					t.Logf("seek table cut short: OpenBlockIndex reports %v", err)
				}
			}

			t.Logf("%d truncation offsets: %d rejected, %d clean-and-COMPLETE, %d silent SHORT reads",
				len(clean)-1, rejected, cleanAndComplete, silentShort)
		})
	}
}

// TestWithoutFooterRequiredIsTheSalvagePath is the opt-out half of F2's fix,
// and it exists for a real need rather than symmetry: an archive cut by a
// crashed writer or a full disk still contains every frame before the cut, and
// refusing the whole file to protect a caller from the missing tail is a
// different kind of data loss.
//
// The DEFAULT is that a footerless capture is an error (that is F2). This is
// the argument that opts out: every check is on by default and an argument is
// what turns one off. It must return the frames that survived AND it
// must not resurrect the silent case — a caller who passes it knows the tail is
// gone, which is exactly the knowledge the default cannot assume.
func TestWithoutFooterRequiredIsTheSalvagePath(t *testing.T) {
	path, truth := corruptionCorpus(t, "corpus.tape")
	clean, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}

	// Cut at a block boundary so the surviving prefix is intact: the seek
	// table names where the blocks end, so this is a cut the format itself
	// can describe rather than one chosen by eye.
	index, err := OpenBlockIndex(path)
	if err != nil {
		t.Fatalf("OpenBlockIndex: %v", err)
	}
	if index.Blocks() < 4 {
		t.Fatalf("corpus has %d blocks; need several to cut between", index.Blocks())
	}
	offset, _, err := index.BlockRange(index.Blocks() - 2)
	if err != nil {
		t.Fatalf("BlockRange: %v", err)
	}
	target := filepath.Join(t.TempDir(), "cut.tape")
	if err := os.WriteFile(target, clean[:offset], 0o600); err != nil {
		t.Fatalf("write truncated: %v", err)
	}

	// Default: refused.
	if _, err := readWholeCapture(t, target); !errors.Is(err, ErrTruncatedCapture) {
		t.Errorf("a footerless capture read back with err=%v; want ErrTruncatedCapture", err)
	}

	// Opt-out: the surviving frames, and no footer.
	r, err := NewReader(target, WithoutFooterRequired())
	if err != nil {
		t.Fatalf("NewReader: %v", err)
	}
	defer func() {
		if err := r.Close(); err != nil {
			t.Errorf("Close: %v", err)
		}
	}()
	if _, err := r.ReadHeader(); err != nil {
		t.Fatalf("ReadHeader: %v", err)
	}
	var salvaged []*capturepb.Frame
	for {
		f, err := r.ReadFrame()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("ReadFrame with WithoutFooterRequired after %d frames: %v", len(salvaged), err)
		}
		salvaged = append(salvaged, f)
	}
	if len(salvaged) == 0 {
		t.Fatal("WithoutFooterRequired salvaged no frames at all; the option is not a salvage path")
	}
	if len(salvaged) >= len(truth) {
		t.Fatalf("salvaged %d frames from a capture cut before its end, which wrote %d; the "+
			"file was not actually truncated", len(salvaged), len(truth))
	}
	// What survived must be exactly what was written. A salvage path that
	// returns approximate frames is worse than one that refuses.
	for i := range salvaged {
		if !proto.Equal(truth[i], salvaged[i]) {
			t.Fatalf("salvaged frame %d differs from what was written", i)
		}
	}
	t.Logf("cut at block boundary %d of %d bytes: salvaged %d of %d frames",
		offset, len(clean), len(salvaged), len(truth))
}
