// Hostile-input resource guards for the tape decoders.
//
// SEC-001 (decompression bomb) and SEC-002 (allocate-before-verify)
// document the attacks these guards close. The principle is audit-not-restrict:
// every limit is documented, configurable, and disableable, so a legitimate
// giant capture stays readable by explicit opt-in — but a crafted few-KB file
// can no longer force gigabytes of allocation by default.
//
// The package overview lives in doc.go; the blank line below keeps this a file
// comment rather than a second package comment.

package codec

import (
	"errors"
	"fmt"
	"io"
	"slices"

	"github.com/klauspost/compress/zstd"
)

const (
	// DefaultMaxDecodedBytes is the default budget for total decoded
	// (uncompressed) bytes per capture read (SEC-001).
	//
	// Source: measured decoded sizes on the repo samples (testdata/): the v2
	// tape decodes to ~1.8 KB/frame (sample.tape.golden: 1,862,589 bytes /
	// 1023 frames) and the v1/echoreplay lane to ~8.5 KB/frame
	// (sample.echoreplay: 8,757,915 bytes / 1023 frames). The largest audited
	// real capture is 22,727 frames (docs/format-design.md §3) ≈ 195 MB
	// decoded on the heavier lane. 8 GiB is ~40x that ceiling — no real
	// capture is refused, while a bomb decoding kilobytes into gigabytes is
	// stopped with a clear error. Raise with WithMaxDecodedBytes or disable
	// with WithoutLimits for a legitimately larger capture.
	DefaultMaxDecodedBytes int64 = 8 << 30 // 8 GiB

	// DefaultMaxFrameCount is the default budget for frames read per capture
	// (SEC-001).
	//
	// Source: the largest audited real capture is 22,727 frames
	// (docs/format-design.md §3); the fastest supported capture rate is
	// 600 Hz (pkg/processing, CLAUDE.md). 10M frames ≈ 4.6 hours of
	// continuous 600 Hz capture, ~440x the largest observed recording. Raise
	// with WithMaxFrameCount or disable with WithoutLimits.
	DefaultMaxFrameCount int64 = 10_000_000
	// maxEagerMessageAlloc bounds the buffer allocated for a length-delimited
	// message BEFORE any of its bytes have been read (SEC-002). Messages larger
	// than this are still read in full — the buffer grows in steps of this size
	// as bytes actually arrive, so allocation tracks bytes present in the
	// stream, not an attacker's declared length.
	//
	// Source: measured message sizes on the repo samples (testdata/): the v2
	// tape averages ~1.8 KB/frame decoded (sample.tape.golden: 1,862,589 bytes
	// / 1023 frames) and the v1/echoreplay lane ~8.5 KB/frame
	// (sample.echoreplay: 8,757,915 bytes decoded / 1023 frames). 1 MiB is
	// >100x the largest observed per-message size.
	maxEagerMessageAlloc = 1 << 20 // 1 MiB
)

var (
	// ErrMaxDecodedBytes is returned when a capture's decoded stream exceeds
	// the configured MaxDecodedBytes budget (SEC-001: decompression bomb).
	ErrMaxDecodedBytes = errors.New("decoded stream exceeds MaxDecodedBytes budget; raise with codec.WithMaxDecodedBytes or disable with codec.WithoutLimits")

	// ErrMaxFrameCount is returned when a capture yields more frames than the
	// configured MaxFrameCount budget (SEC-001: decompression bomb).
	ErrMaxFrameCount = errors.New("frame count exceeds MaxFrameCount budget; raise with codec.WithMaxFrameCount or disable with codec.WithoutLimits")

	// ErrUnexpectedEnvelope is returned when the frame stream contains a
	// non-frame, non-footer envelope (e.g. a stray header or an empty envelope)
	// before the footer — a malformed or truncated-and-concatenated capture.
	//
	// IT NO LONGER COVERS EVERY NON-FRAME ENVELOPE (F9). An envelope carrying a
	// oneof variant this reader does not know is SKIPPED and counted rather than
	// refused, so that adding an envelope kind does not break deployed readers;
	// see Reader.SkippedEnvelopes. This error is now specifically the other two
	// cases, and they are the ones that mean the file is wrong rather than new:
	//
	//	a KNOWN variant in the wrong place  — a second header mid-stream, i.e. an
	//	                                      ORDERING defect in a file this reader
	//	                                      fully understands
	//	an envelope carrying nothing at all — no known case set and no unknown
	//	                                      fields either; malformation, not a
	//	                                      message from the future
	ErrUnexpectedEnvelope = errors.New("unexpected non-frame envelope before footer")

	// ErrFooterMismatch is returned by ReadFrame when the capture's footer
	// disagrees with what the stream actually carried — the capture is
	// truncated, corrupt, or was concatenated from pieces.
	//
	// The tape container is Zstd, not gzip; the standard library's gzip reader
	// is cited only as the precedent for the behaviour. It validates its
	// trailer's CRC32 and ISIZE against the bytes it decompressed and reports
	// ErrChecksum from Read rather than a clean io.EOF. frame_count is this
	// format's ISIZE. A reader that ignores its own trailer reports success on
	// a file that lost data, which is the one failure this library must never
	// have.
	ErrFooterMismatch = errors.New("footer frame_count disagrees with the frames read; capture is truncated or corrupt")

	// ErrCorruptCapture is returned when the COMPRESSOR's own integrity check
	// fails — zstd writes a content checksum on every frame and this is that
	// checksum disagreeing with the bytes decoded.
	//
	// WHY IT EXISTS (F1, measured 2026-09-05). It did not, and the check it
	// reports was never reached: ReadFrame returned io.EOF the instant it saw
	// the footer envelope, while klauspost's streaming decoder verifies the
	// checksum only when the end of the zstd frame is consumed. The
	// whole-stream layout is ONE frame, so no capture ever written was
	// checksum-verified on this path. A sweep of every single-bit flip of a
	// 7,553-byte capture: 35,085 of 60,424 flips read back as a clean capture
	// with WRONG telemetry, and all 32 flips inside the checksum bytes were
	// accepted. `zstd -d` rejected the same files.
	//
	// This is ErrFooterMismatch's argument applied one layer down: tape's
	// trailer is frame_count, zstd's trailer is the checksum, and ignoring
	// either "reports success on a file that lost data, which is the one
	// failure this library must never have."
	ErrCorruptCapture = errors.New("capture failed its compressor's integrity check; the bytes on disk are damaged")

	// ErrTruncatedCapture is returned when the frame stream ends without a
	// footer. The capture is cut: a crashed writer, a full disk, a partial
	// copy, or a file still being written.
	//
	// WHY IT EXISTS (F2, measured 2026-09-05). ReadFrame's doc has always
	// promised "a truncated or concatenated capture is reported rather than
	// read as a short-but-successful one", and nothing implemented it. The
	// promise held for the whole-stream layout by accident — a cut mid-frame is
	// a zstd error at every offset — and the per-block layout removed the
	// accident, because independent frames make every block boundary a clean
	// EOF. Measured on a 35-frame capture: 4 of 697 truncation offsets returned
	// 10 frames with err=nil, and `tapedeck show` printed "frames: 10" and
	// exited 0. Data loss read as success.
	//
	// WithoutFooterRequired opts out, for salvaging a capture whose tail is
	// known to be gone.
	ErrTruncatedCapture = errors.New("capture ends with no footer; it is truncated or still being written")

	// ErrTrailingData is returned when bytes remain after the footer envelope.
	//
	// A capture ends at its footer. Anything after it means the file is two
	// captures concatenated, or a capture with something appended — and either
	// way the reader has just returned frames from a file it does not
	// understand the whole of. Reporting it is the same promise ReadFrame's doc
	// already makes about concatenation.
	ErrTrailingData = errors.New("bytes remain after the capture footer; the file is concatenated or appended to")

	// ErrWindowTooLarge is returned by BlockIndex.ReadBlock when a block's zstd
	// frame declares a back-reference window larger than this library's own
	// writer can emit (maxWriterWindow, 8 MiB).
	//
	// It is a REFUSAL BY POLICY, not a corruption report, and the distinction is
	// deliberate: such a frame is valid zstd and `zstd -d` will decode it. Nothing
	// in docs/format-design.md specifies a window for the container, so a
	// conformant third-party writer could in principle produce one. This reader
	// declines it anyway, because the alternative is letting the file choose how
	// much memory reading it costs.
	//
	// WHY IT EXISTS (the residual half of F4, measured 2026-09-07). The bound that
	// preceded it computed the decoder's memory cap as max(declared,
	// hdr.WindowSize, 1) — and hdr.WindowSize is decoded from the hostile file's
	// own frame header, so the attacker set their own ceiling. klauspost sizes the
	// history buffer from that window (zstd/framedec.go:255-266), and the cost
	// tracked the file's number linearly: crafted files of 2,719 / 7,711 / 27,679
	// / 54,303 bytes drove 39.5 / 191.6 / 788.7 / 1433.4 MiB of allocation, every
	// one of them with the caller's MaxDecodedBytes budget set to 1 MiB and
	// ignored. The frames carried no Frame_Content_Size, so the frame-header
	// cross-check above them never fired.
	ErrWindowTooLarge = errors.New("block declares a compression window larger than this format's writer produces; refusing to size a read from the file's own header")

	// ErrUnsupportedVersion is returned by ReadHeader when the capture's
	// format_version is not 2 (or 0 for pre-2.1 captures). A future reader
	// might understand additional versions, but this one does not.
	ErrUnsupportedVersion = errors.New("unsupported format_version; this reader understands version 2")

	// ErrFrameCountOverflow is returned by WriteFrame or Close when the
	// capture would exceed the format's uint32 frame count limit
	// (~4.29 billion frames, >82 days at 600 Hz).
	ErrFrameCountOverflow = errors.New("frame count would exceed uint32 max; split the capture")

	// ErrWriteOrder is returned by the Writer when calls arrive out of the
	// stream contract: a frame before the header, a duplicate header, a call
	// after Close, or Close before any header. The format is exactly one
	// header, then frames, then one footer.
	ErrWriteOrder = errors.New("writer call out of order; exactly one header, then frames, then one Close")

	// ErrNilFrame is returned by WriteFrame when passed a nil *Frame.
	ErrNilFrame = errors.New("frame is nil")

	// ErrEmptyFrame is returned by WriteFrame when the frame's payload oneof is
	// unset. Such a frame encodes no game state — it round-trips as a
	// payload-less frame every consumer reads as absent data — so it is refused
	// rather than written.
	ErrEmptyFrame = errors.New("frame has no payload; a tape frame must carry game state")
)

// Limits bounds the resources a reader will spend on a single capture, so a
// tiny hostile file cannot force unbounded memory use (SEC-001). A zero or
// negative field disables that limit — the opt-in escape hatch that keeps a
// legitimate giant capture readable.
type Limits struct {
	// MaxDecodedBytes caps total decoded (uncompressed) bytes read from the
	// capture stream. <= 0 disables the cap.
	MaxDecodedBytes int64
	// MaxFrameCount caps the number of frames read from the capture.
	// <= 0 disables the cap.
	MaxFrameCount int64
}

// DefaultLimits returns the budgets applied when a reader is constructed
// without options. See DefaultMaxDecodedBytes / DefaultMaxFrameCount for the
// sizing rationale.
func DefaultLimits() Limits {
	return Limits{
		MaxDecodedBytes: DefaultMaxDecodedBytes,
		MaxFrameCount:   DefaultMaxFrameCount,
	}
}

// readerConfig is the folded result of the options passed to a reader
// constructor. Every integrity check it can perform is ON here, and an option
// is what turns one off. A check that must be asked for by name protects only
// the callers who already know it exists.
//
// WHY IT IS A STRUCT AND NOT Limits. ReaderOption used to be func(*Limits), so
// the only thing an option could reach was a resource budget, and "is a missing
// footer an error" is not a budget. Folding it into Limits would have made
// WithoutLimits — documented as disabling "every resource limit" — silently
// re-arm or disarm an integrity check as a side effect. The config is
// unexported, which also closes the option type: a ReaderOption can now only
// come from this package, so the set of things a caller can switch off is
// enumerable here rather than open-ended.
type readerConfig struct {
	limits Limits
	// requireFooter makes a footerless stream an error rather than a clean
	// short read. See ErrTruncatedCapture.
	requireFooter bool
}

// ReaderOption adjusts a capture reader: its resource limits, or which
// integrity checks it enforces.
type ReaderOption func(*readerConfig)

// WithMaxDecodedBytes sets the total decoded-bytes budget. n <= 0 disables it.
func WithMaxDecodedBytes(n int64) ReaderOption {
	return func(c *readerConfig) { c.limits.MaxDecodedBytes = n }
}

// WithMaxFrameCount sets the frame-count budget. n <= 0 disables it.
func WithMaxFrameCount(n int64) ReaderOption {
	return func(c *readerConfig) { c.limits.MaxFrameCount = n }
}

// WithoutLimits disables every resource limit. Explicit opt-in for reading a
// trusted, legitimately giant capture.
//
// It disables RESOURCE limits only. Integrity checks are not limits and are
// unaffected: a caller reading a trusted giant capture still wants to know if
// it is damaged, and that is the case this option exists for.
func WithoutLimits() ReaderOption {
	return func(c *readerConfig) { c.limits = Limits{} }
}

// WithoutFooterRequired accepts a capture whose frame stream ends with no
// footer, returning the frames that survived instead of ErrTruncatedCapture.
//
// THE SALVAGE PATH, and the only intended use. A capture cut by a crashed
// writer or a full disk still contains every frame before the cut, and
// refusing the whole file to protect a caller from the missing tail is a
// different kind of data loss. A live tail — reading a capture while it is
// still being written — is the other case.
//
// WHAT IT COSTS: the reader can no longer distinguish "this capture ended" from
// "this capture was cut", so a caller passing it is asserting that it already
// knows. It also loses the footer's frame_count cross-check, since there is no
// footer. Do not pass it to make an error go away.
func WithoutFooterRequired() ReaderOption {
	return func(c *readerConfig) { c.requireFooter = false }
}

// applyReaderOptions folds opts over the defaults: the default budgets, and
// every integrity check on.
func applyReaderOptions(opts []ReaderOption) readerConfig {
	cfg := readerConfig{limits: DefaultLimits(), requireFooter: true}
	for _, opt := range opts {
		opt(&cfg)
	}
	return cfg
}

// checkFrameBudget returns ErrMaxFrameCount when framesRead exceeds the
// frame-count budget. framesRead is the count INCLUDING the frame just read.
func (l Limits) checkFrameBudget(framesRead int64) error {
	if l.MaxFrameCount > 0 && framesRead > l.MaxFrameCount {
		return fmt.Errorf("frame %d: %w (budget %d)", framesRead, ErrMaxFrameCount, l.MaxFrameCount)
	}
	return nil
}

// budgetReader enforces a total decoded-bytes budget over r (SEC-001). Once
// the budget is exhausted the next Read fails with ErrMaxDecodedBytes; a
// single Read may overshoot by at most one caller buffer (<= 1 MiB with
// readMessageBody), which keeps the guard O(1) per read.
type budgetReader struct {
	r        io.Reader
	limit    int64
	consumed int64
}

// newBudgetReader wraps r with a decoded-bytes budget. limit <= 0 returns r
// unwrapped (unlimited).
func newBudgetReader(r io.Reader, limit int64) io.Reader {
	if limit <= 0 {
		return r
	}
	return &budgetReader{r: r, limit: limit}
}

func (b *budgetReader) Read(p []byte) (int, error) {
	// Fail BEFORE reading once the budget is spent: returning the error with
	// n > 0 would let io.ReadFull swallow it while data keeps flowing.
	if b.consumed >= b.limit {
		return 0, fmt.Errorf("%d bytes decoded: %w (budget %d)", b.consumed, ErrMaxDecodedBytes, b.limit)
	}
	n, err := b.r.Read(p)
	b.consumed += int64(n)
	// The zstd decoder enforces its own memory bound (WithDecoderMaxMemory,
	// set from the same budget). Translate its limit errors into the
	// documented sentinel so callers have one contract regardless of which
	// layer trips first.
	if err != nil && (errors.Is(err, zstd.ErrDecoderSizeExceeded) || errors.Is(err, zstd.ErrWindowSizeExceeded)) {
		err = fmt.Errorf("zstd decoder limit (%v): %w (budget %d)", err, ErrMaxDecodedBytes, b.limit)
	}
	return n, err
}

// readMessageBody reads exactly length bytes from r.
//
// SEC-002 guard: the buffer is never eagerly sized to the declared length.
// Up to maxEagerMessageAlloc bytes are allocated up front; beyond that the
// buffer grows only as bytes are actually read, so a tiny stream declaring a
// 256 MiB message costs a bounded allocation plus a clean error — not 256 MiB.
func readMessageBody(r io.Reader, length uint64) ([]byte, error) {
	if length <= maxEagerMessageAlloc {
		data := make([]byte, length)
		if _, err := io.ReadFull(r, data); err != nil {
			return nil, err
		}
		return data, nil
	}

	data := make([]byte, 0, maxEagerMessageAlloc)
	for remaining := length; remaining > 0; {
		chunk := int(min(remaining, maxEagerMessageAlloc)) //nolint:gosec // bounded by maxEagerMessageAlloc (1 MiB), always fits int
		start := len(data)
		data = slices.Grow(data, chunk)[:start+chunk]
		if _, err := io.ReadFull(r, data[start:]); err != nil {
			// A mid-message EOF is an unexpected end of the message, even when
			// ReadFull saw zero bytes of this chunk.
			if err == io.EOF && start > 0 {
				err = io.ErrUnexpectedEOF
			}
			return nil, fmt.Errorf("message truncated after %d of %d declared bytes: %w", start, length, err)
		}
		remaining -= uint64(chunk)
	}
	return data, nil
}
