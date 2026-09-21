package codec

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"slices"
	"sync"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/proto"
)

const (
	// DefaultKeyframeInterval is how often keyframes are indexed in the footer.
	DefaultKeyframeInterval = 100
)

// Writer writes tape captures in the envelope stream format.
//
// Wire format (the default, and what NewWriter has always produced):
//
//	[Zstd frame]
//	  [varint len][Envelope{CaptureHeader}]   — exactly one
//	  [varint len][Envelope{Frame}]           — repeated
//	  ...
//	  [varint len][Envelope{CaptureFooter}]   — exactly one
//	[Zstd trailer]
//
// The footer records frame count, duration, byte offsets, keyframe and event
// indexes. The sketch above shows one continuous Zstd frame, which is what
// WithWholeStreamCompression still produces; in that layout byte-offset
// seeking requires decompressing from the start, so the keyframe index gives
// positions nothing can seek to.
//
// THE DEFAULT SINCE v4.1.0 IS THE PER-BLOCK LAYOUT, in which it can: the file
// is independent per-block Zstd frames plus a seek table, a keyframe offset is
// a servable byte range, and reaching frame N costs one block rather than the
// whole file. See tape_blocks.go for the layout and seekable.go for the
// index.
//
// Stream contract: exactly one WriteHeader, then any number of WriteFrame
// calls, then one Close. Any call outside this order (a frame before the
// header, a duplicate header, or any call after Close) returns ErrWriteOrder,
// and a nil or payload-less frame is rejected rather than written.
//
// The footer's keyframe and event indexes record each frame's own frame_index
// (the value serialized on the wire), so the indexes are always consistent with
// the frames regardless of whether the caller's indices are sequential.
// frame_count in the footer is the number of frames written, not the last
// index.
//
// Writer is safe for concurrent use: WriteHeader, WriteFrame, and Close are
// serialized by an internal mutex.
type Writer struct {
	mu         sync.Mutex
	file       *os.File
	encoder    *zstd.Encoder
	eventIndex map[capturepb.EventType][]uint32
	keyframes  []*capturepb.KeyframeEntry
	// state tracks the stream contract (see writerState).
	state writerState
	// bytesWritten tracks uncompressed bytes for footer_offset.
	bytesWritten     uint64
	frameCount       uint32
	lastTimestampMs  uint32
	keyframeInterval uint32
	// blocks is non-nil in the default per-block layout, in which case it owns
	// compression and encoder is nil; WithWholeStreamCompression is what leaves
	// it nil. See tape_blocks.go for the layout and the ruling behind the
	// default.
	blocks *blockWriter
}

// writerState tracks where the Writer sits in the stream contract: a new
// writer is writerStateNew until WriteHeader succeeds, then
// writerStateHeaderWritten, and finally writerStateClosed after Close.
type writerState uint8

const (
	writerStateNew writerState = iota
	writerStateHeaderWritten
	writerStateClosed
)

// NewWriter creates a writer for tape files with every default feature on:
// the per-block seekable layout at DefaultKeyframeInterval, zstd
// SpeedFastest. See WithWholeStreamCompression to opt out of the layout.
func NewWriter(filename string) (*Writer, error) {
	return NewWriterWithKeyframeInterval(filename, DefaultKeyframeInterval)
}

// NewWriterWithKeyframeInterval creates a writer with a custom keyframe
// interval. An interval of zero is clamped to DefaultKeyframeInterval: it
// reached `frameIndex % keyframeInterval` in WriteFrame and panicked with an
// integer divide by zero (GH #23). events.WithFrameBufferSize answers the same
// class of bug the same way.
func NewWriterWithKeyframeInterval(filename string, keyframeInterval uint32) (*Writer, error) {
	return NewWriterWithOptions(filename, WithKeyframeInterval(keyframeInterval))
}

// NewWriterWithOptions creates a writer configured by WriterOptions. With no
// options it is identical to NewWriter, and every layout-affecting option
// names itself as such (see tape_blocks.go). Since v4.1.0 the default output is
// the PER-BLOCK layout — every feature that can be on by default is on, and an
// argument is what turns one off.
func NewWriterWithOptions(filename string, opts ...WriterOption) (*Writer, error) {
	cfg := applyWriterOptions(opts)

	file, err := os.Create(filename) //nolint:gosec // filename is caller-provided path
	if err != nil {
		return nil, err
	}

	w := &Writer{
		file:             file,
		keyframeInterval: cfg.keyframeInterval,
		eventIndex:       make(map[capturepb.EventType][]uint32),
	}

	if cfg.perBlock {
		blocks, blockErr := newBlockWriter(file, cfg)
		if blockErr != nil {
			return nil, errors.Join(blockErr, file.Close())
		}
		w.blocks = blocks
		return w, nil
	}

	zopts := []zstd.EOption{zstd.WithEncoderLevel(cfg.level)}
	if len(cfg.dict) > 0 {
		zopts = append(zopts, zstd.WithEncoderDict(cfg.dict))
	}
	encoder, err := zstd.NewWriter(file, zopts...)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}
	w.encoder = encoder
	return w, nil
}

// WriteHeader writes the capture header as the first envelope. It is an error
// to call it twice, to call it after Close, or to call WriteFrame before it.
func (w *Writer) WriteHeader(header *capturepb.CaptureHeader) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == writerStateClosed {
		return fmt.Errorf("tape: WriteHeader: %w (writer already closed)", ErrWriteOrder)
	}
	if w.state == writerStateHeaderWritten {
		return fmt.Errorf("tape: WriteHeader: %w (header already written)", ErrWriteOrder)
	}

	env := &capturepb.Envelope{
		Message: &capturepb.Envelope_Header{Header: header},
	}
	if err := w.writeEnvelope(env); err != nil {
		return err
	}
	// Under per-block compression the header is its own block, so rewriting it
	// — the edit a metadata change or an audio strip performs — recompresses
	// one small block rather than the first interval of frames along with it.
	if w.blocks != nil {
		if err := w.blocks.flush(); err != nil {
			return fmt.Errorf("tape: WriteHeader: %w", err)
		}
	}
	w.state = writerStateHeaderWritten
	return nil
}

// WriteFrame writes a frame envelope and updates indexes. The frame must
// arrive after WriteHeader and carry a game payload. The footer's keyframe and
// event indexes record the frame's own frame_index, so they stay consistent
// with the wire even for legacy captures whose stored indices are not
// sequential.
func (w *Writer) WriteFrame(frame *capturepb.Frame) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state != writerStateHeaderWritten {
		return fmt.Errorf("tape: WriteFrame: %w (header not yet written, or writer closed)", ErrWriteOrder)
	}
	if frame == nil {
		return fmt.Errorf("tape: WriteFrame: %w", ErrNilFrame)
	}
	if frame.Payload == nil {
		return fmt.Errorf("tape: WriteFrame: %w", ErrEmptyFrame)
	}

	frameIndex := frame.GetFrameIndex()

	// Track keyframe offset before writing.
	//
	// The recorded offset means different things in the two layouts, and the
	// difference is the whole point of per-block compression. In the DEFAULT
	// per-block layout the keyframe opens a new block, so the offset is the
	// block's position in the COMPRESSED file — a servable byte range,
	// resolvable against the seek table. Under WithWholeStreamCompression it is
	// a position in the DECOMPRESSED stream, which nothing can seek to.
	if frameIndex%w.keyframeInterval == 0 {
		offset := w.bytesWritten
		if w.blocks != nil {
			if err := w.blocks.flush(); err != nil {
				return fmt.Errorf("tape: WriteFrame: %w", err)
			}
			offset = w.blocks.offset
		}
		w.keyframes = append(w.keyframes, &capturepb.KeyframeEntry{
			FrameIndex: frameIndex,
			ByteOffset: offset,
		})
	}

	// Track events from Echo Arena frames.
	if ea := frame.GetEchoArena(); ea != nil {
		for _, evt := range ea.GetEvents() {
			eventType := classifyEvent(evt)
			if eventType != capturepb.EventType_EVENT_TYPE_UNSPECIFIED {
				w.eventIndex[eventType] = append(w.eventIndex[eventType], frameIndex)
			}
		}
	}

	env := &capturepb.Envelope{
		Message: &capturepb.Envelope_Frame{Frame: frame},
	}
	if err := w.writeEnvelope(env); err != nil {
		return err
	}

	// The format stores frame_count as uint32 in the footer (and frame
	// indices in keyframe/event entries as uint32).  Over 4 billion frames
	// in one capture (~82 days at 600 Hz) cannot be represented.
	if w.frameCount == math.MaxUint32 {
		return fmt.Errorf("tape: %w", ErrFrameCountOverflow)
	}
	w.frameCount++
	w.lastTimestampMs = frame.GetTimestampOffsetMs()
	return nil
}

// Close writes the footer, closes the zstd encoder, and closes the file. It is
// an error to call it before WriteHeader or more than once.
func (w *Writer) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.state == writerStateClosed {
		return fmt.Errorf("tape: Close: %w (writer already closed)", ErrWriteOrder)
	}
	if w.state != writerStateHeaderWritten {
		return fmt.Errorf("tape: Close: %w (no header written)", ErrWriteOrder)
	}

	// Build event index entries in deterministic order.
	types := make([]capturepb.EventType, 0, len(w.eventIndex))
	for t := range w.eventIndex {
		types = append(types, t)
	}
	slices.Sort(types)

	var eventEntries []*capturepb.EventIndexEntry
	for _, eventType := range types {
		frameIndices := w.eventIndex[eventType]
		eventEntries = append(eventEntries, &capturepb.EventIndexEntry{
			EventType:    eventType,
			FrameIndices: frameIndices,
		})
	}

	footer := &capturepb.CaptureFooter{
		FrameCount:    w.frameCount,
		DurationMs:    w.lastTimestampMs,
		FooterOffset:  w.bytesWritten,
		KeyframeIndex: w.keyframes,
		EventIndex:    eventEntries,
	}

	env := &capturepb.Envelope{
		Message: &capturepb.Envelope_Footer{Footer: footer},
	}

	var firstErr error

	// Under per-block compression the last frame block is closed BEFORE the
	// footer is written, so the footer lands in a block of its own: it is
	// derivable and rewritable without disturbing a single frame byte.
	if w.blocks != nil {
		if err := w.blocks.flush(); err != nil {
			firstErr = err
		}
	}

	if err := w.writeEnvelope(env); err != nil && firstErr == nil {
		firstErr = err
	}

	if w.blocks != nil {
		// finish flushes the footer block and appends the seek table, which
		// is what makes every keyframe offset above resolvable.
		if err := w.blocks.finish(); err != nil && firstErr == nil {
			firstErr = err
		}
	} else if err := w.encoder.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	if err := w.file.Close(); err != nil && firstErr == nil {
		firstErr = err
	}

	// Mark the stream closed even when the footer write failed: the encoder and
	// file are released, so no later call may be accepted.
	w.state = writerStateClosed
	return firstErr
}

// writeEnvelope marshals and writes a length-delimited envelope.
func (w *Writer) writeEnvelope(env *capturepb.Envelope) error {
	data, err := proto.Marshal(env)
	if err != nil {
		return err
	}

	// Write varint length prefix.
	var buf [10]byte
	length := uint64(len(data))
	i := 0
	for length >= 0x80 {
		buf[i] = byte(length) | 0x80
		length >>= 7
		i++
	}
	buf[i] = byte(length)
	i++

	// bytesWritten counts UNCOMPRESSED bytes in both layouts, because that is
	// what footer_offset has always meant. Per-block compression changes where
	// blocks land in the file, not what the footer's own offset counts.
	if w.blocks != nil {
		w.blocks.write(buf[:i])
		w.blocks.write(data)
		w.bytesWritten += uint64(i) + uint64(len(data))
		return nil
	}

	n, err := w.encoder.Write(buf[:i])
	w.bytesWritten += uint64(max(n, 0)) //nolint:gosec // n is non-negative on success
	if err != nil {
		return err
	}

	n, err = w.encoder.Write(data)
	w.bytesWritten += uint64(max(n, 0)) //nolint:gosec // n is non-negative on success
	return err
}

// classifyEvent returns the EventType for an EchoEvent based on its oneof case.
func classifyEvent(evt *capturepb.EchoEvent) capturepb.EventType {
	switch evt.GetEvent().(type) {
	case *capturepb.EchoEvent_RoundStarted:
		return capturepb.EventType_EVENT_TYPE_ROUND_STARTED
	case *capturepb.EchoEvent_RoundPaused:
		return capturepb.EventType_EVENT_TYPE_ROUND_PAUSED
	case *capturepb.EchoEvent_RoundUnpaused:
		return capturepb.EventType_EVENT_TYPE_ROUND_UNPAUSED
	case *capturepb.EchoEvent_RoundEnded:
		return capturepb.EventType_EVENT_TYPE_ROUND_ENDED
	case *capturepb.EchoEvent_MatchEnded:
		return capturepb.EventType_EVENT_TYPE_MATCH_ENDED
	case *capturepb.EchoEvent_ScoreboardUpdated:
		return capturepb.EventType_EVENT_TYPE_SCOREBOARD_UPDATED
	case *capturepb.EchoEvent_MatchSettingsChanged:
		return capturepb.EventType_EVENT_TYPE_MATCH_SETTINGS_CHANGED
	case *capturepb.EchoEvent_PlayerJoined:
		return capturepb.EventType_EVENT_TYPE_PLAYER_JOINED
	case *capturepb.EchoEvent_PlayerLeft:
		return capturepb.EventType_EVENT_TYPE_PLAYER_LEFT
	case *capturepb.EchoEvent_PlayerSwitchedTeam:
		return capturepb.EventType_EVENT_TYPE_PLAYER_SWITCHED_TEAM
	case *capturepb.EchoEvent_EmotePlayed:
		return capturepb.EventType_EVENT_TYPE_EMOTE_PLAYED
	case *capturepb.EchoEvent_DiscPossessionChanged:
		return capturepb.EventType_EVENT_TYPE_DISC_POSSESSION_CHANGED
	case *capturepb.EchoEvent_DiscThrown:
		return capturepb.EventType_EVENT_TYPE_DISC_THROWN
	case *capturepb.EchoEvent_DiscCaught:
		return capturepb.EventType_EVENT_TYPE_DISC_CAUGHT
	case *capturepb.EchoEvent_GoalScored:
		return capturepb.EventType_EVENT_TYPE_GOAL_SCORED
	case *capturepb.EchoEvent_PlayerGoal:
		return capturepb.EventType_EVENT_TYPE_PLAYER_GOAL
	case *capturepb.EchoEvent_PlayerSave:
		return capturepb.EventType_EVENT_TYPE_PLAYER_SAVE
	case *capturepb.EchoEvent_PlayerStun:
		return capturepb.EventType_EVENT_TYPE_PLAYER_STUN
	case *capturepb.EchoEvent_PlayerPass:
		return capturepb.EventType_EVENT_TYPE_PLAYER_PASS
	case *capturepb.EchoEvent_PlayerSteal:
		return capturepb.EventType_EVENT_TYPE_PLAYER_STEAL
	case *capturepb.EchoEvent_PlayerBlock:
		return capturepb.EventType_EVENT_TYPE_PLAYER_BLOCK
	case *capturepb.EchoEvent_PlayerInterception:
		return capturepb.EventType_EVENT_TYPE_PLAYER_INTERCEPTION
	case *capturepb.EchoEvent_PlayerAssist:
		return capturepb.EventType_EVENT_TYPE_PLAYER_ASSIST
	case *capturepb.EchoEvent_PlayerShotTaken:
		return capturepb.EventType_EVENT_TYPE_PLAYER_SHOT_TAKEN
	case *capturepb.EchoEvent_PlayerStatsUpdated:
		return capturepb.EventType_EVENT_TYPE_PLAYER_STATS_UPDATED
	case *capturepb.EchoEvent_PlayerInfoUpdated:
		return capturepb.EventType_EVENT_TYPE_PLAYER_INFO_UPDATED
	case *capturepb.EchoEvent_LoadoutChanged:
		return capturepb.EventType_EVENT_TYPE_LOADOUT_CHANGED
	case *capturepb.EchoEvent_GrabChanged:
		return capturepb.EventType_EVENT_TYPE_GRAB_CHANGED
	case *capturepb.EchoEvent_GenericEvent:
		return capturepb.EventType_EVENT_TYPE_GENERIC
	default:
		// Unreachable for any variant the proto currently defines;
		// TestClassifyEventCoversEveryEchoEventVariant fails if a newly added
		// one lands here, because WriteFrame (tape.go:96) skips UNSPECIFIED and
		// it would silently vanish from the footer's event index.
		return capturepb.EventType_EVENT_TYPE_UNSPECIFIED
	}
}

// Reader reads tape captures from the envelope stream format.
type Reader struct {
	file    *os.File
	decoder *zstd.Decoder
	reader  io.Reader

	// limits bounds hostile-input resource use (SEC-001); see Limits.
	limits Limits
	// framesRead counts frames returned by ReadFrame, checked against
	// limits.MaxFrameCount.
	framesRead int64

	// pendingFooter holds a footer envelope encountered during ReadFrame.
	pendingFooter *capturepb.CaptureFooter

	// requireFooter makes a stream that ends with no footer an error rather
	// than a clean short read (F2; see ErrTruncatedCapture). On by default;
	// WithoutFooterRequired turns it off.
	requireFooter bool

	// tailVerified records that verifyTail already ran. The check consumes the
	// stream, so it must happen exactly once however the caller arrives at the
	// footer — ReadFrame reaching it, or ReadFooter reading it directly.
	tailVerified bool

	// skippedEnvelopes counts envelopes carrying a oneof variant this reader
	// does not know (F9). Surfaced by SkippedEnvelopes; see isUnknownEnvelope.
	skippedEnvelopes int64
}

// SkippedEnvelopes returns how many envelopes this reader skipped because they
// carried a variant it does not know (F9).
//
// IT IS NOT DIAGNOSTIC DECORATION. Skipping unknown variants is what lets a new
// envelope kind ship without breaking every deployed reader, and the price is
// that a reader can now not see part of a capture. AGENTS.md §4: "Never silently
// drop input. A skipped line, a rejected frame, an unmapped enum must be counted
// and the counter must have a consumer." This is that counter and `tapedeck
// show` is that consumer — a non-zero value means the file was written by
// something newer than this binary, which is the fact a confused operator needs
// and cannot otherwise obtain.
//
// WHAT BOUNDS IT: a skipped envelope does not increment framesRead, so
// MaxFrameCount does not bound skips. MaxDecodedBytes does — every byte arrives
// through the budget reader installed in NewReader (tape.go:512) — so a stream
// of nothing but unknown variants ends on the decoded-bytes budget rather than
// spinning.
func (r *Reader) SkippedEnvelopes() int64 { return r.skippedEnvelopes }

// isUnknownEnvelope reports whether env carries a oneof variant this reader does
// not know, as opposed to a known variant in the wrong place or an empty
// envelope. The three cases are mechanically distinct and this is the split F9
// turns on — measured 2026-09-07 against the pinned protobuf runtime:
//
//	unknown variant (field 99)  Message==nil  unknown bytes=6, round-trip identical
//	stray KNOWN header          Message!=nil  unknown bytes=0
//	empty envelope              Message==nil  unknown bytes=0
//
// So Message==nil alone is NOT the test: an empty envelope satisfies it and
// carries nothing, which is malformation rather than a variant from the future.
// Requiring the unknown fields to be non-empty is what distinguishes "this
// envelope said something I could not read" from "this envelope said nothing".
func isUnknownEnvelope(env *capturepb.Envelope) bool {
	return env.Message == nil && len(env.ProtoReflect().GetUnknown()) > 0
}

// NewReader opens a tape file for streaming reads. Reads are bounded by
// DefaultLimits (SEC-001 decompression-bomb guard); pass ReaderOptions to
// raise or disable the budgets for a legitimately giant capture.
func NewReader(filename string, opts ...ReaderOption) (*Reader, error) {
	return NewReaderWithDictionary(filename, nil, opts...)
}

// NewReaderWithDictionary opens a tape file that was written with a trained
// zstd dictionary (Writer's WithDictionary).
//
// A dictionary is a permanent obligation: without it the capture does not
// decode at all — zstd records the dictionary's id in every frame header and a
// decoder lacking it fails with ErrUnknownDictionary rather than returning
// wrong bytes. This is the read side of that obligation; a write path with no
// matching read path is the recurring defect this repo names in AGENTS.md §4.
//
// A nil dictionary is exactly NewReader, so callers that do not know whether a
// capture used one can pass what they have.
func NewReaderWithDictionary(filename string, dict []byte, opts ...ReaderOption) (*Reader, error) {
	cfg := applyReaderOptions(opts)
	limits := cfg.limits

	file, err := os.Open(filename) //nolint:gosec // filename is caller-provided path
	if err != nil {
		return nil, err
	}

	// A tape is normally a zstd frame, but an uncompressed envelope stream is
	// still a valid capture; read it rather than failing on magic mismatch.
	compressed, err := startsWith(file, zstdMagic)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}

	r := &Reader{file: file, limits: limits, requireFooter: cfg.requireFooter}
	if !compressed {
		r.reader = newBudgetReader(file, limits.MaxDecodedBytes)
		return r, nil
	}

	// SEC-001: cap the decoder's own memory (streaming window) alongside the
	// total decoded-bytes budget enforced by budgetReader.
	var zstdOpts []zstd.DOption
	if limits.MaxDecodedBytes > 0 {
		zstdOpts = append(zstdOpts, zstd.WithDecoderMaxMemory(uint64(limits.MaxDecodedBytes)))
	}
	if len(dict) > 0 {
		zstdOpts = append(zstdOpts, zstd.WithDecoderDicts(dict))
	}
	decoder, err := zstd.NewReader(file, zstdOpts...)
	if err != nil {
		return nil, errors.Join(err, file.Close())
	}

	r.decoder = decoder
	r.reader = newBudgetReader(decoder, limits.MaxDecodedBytes)
	return r, nil
}

// Limits returns the resource budgets in effect for this reader. Frame
// accumulators (e.g. conversion.OpenSession) enforce these too, so raising or
// disabling a budget on the reader flows through every consumer.
func (r *Reader) Limits() Limits { return r.limits }

// ReadHeader reads the first envelope, which must be a CaptureHeader.
//
// The header's format_version is validated against the versions this reader
// understands: 0 (pre-2.1 captures, treated as 2) and 2. Any other version
// returns ErrUnsupportedVersion.
func (r *Reader) ReadHeader() (*capturepb.CaptureHeader, error) {
	env, err := r.readEnvelope()
	if err != nil {
		return nil, err
	}

	header := env.GetHeader()
	if header == nil {
		return nil, io.ErrUnexpectedEOF
	}

	ver := header.GetFormatVersion()
	if ver != 0 && ver != 2 {
		return nil, fmt.Errorf("tape: header declares format_version=%d: %w", ver, ErrUnsupportedVersion)
	}
	return header, nil
}

// ReadFrame reads the next frame envelope. Returns io.EOF when the stream
// ends or a non-frame envelope (the footer) is encountered.
//
// Three things are checked before io.EOF is returned, and a caller that scans
// to the end gets all three for free:
//
//   - the footer must EXIST. A stream that ends with no footer is a cut file
//     and returns ErrTruncatedCapture (F2). This used to be a clean io.EOF,
//     which the per-block layout turned into silent data loss: independent
//     frames make every block boundary a clean end of stream, so a truncated
//     capture came back short and successful. WithoutFooterRequired opts out,
//     for salvage.
//   - the footer's frame_count must match the frames actually read, or
//     ErrFooterMismatch.
//   - the compressor's content checksum must verify, or ErrCorruptCapture
//     (F1), and nothing may follow the footer, or ErrTrailingData.
//
// A caller that stops early and calls ReadFooter directly is not subject to
// the frame_count check, since a partial read is not a defect.
func (r *Reader) ReadFrame() (*capturepb.Frame, error) {
	// The loop exists for F9: an unknown envelope is skipped and the next one
	// read, so a reader that meets a variant from the future keeps going rather
	// than failing the capture.
	for {
		env, err := r.readEnvelope()
		if err != nil {
			// The stream ended. Whether that is the end of the CAPTURE depends on
			// whether a footer was ever seen — and before e27a62a nothing asked
			// (F2). Independent per-block frames make every block boundary a clean
			// EOF, so a cut file returned a short capture and no error.
			if errors.Is(err, io.EOF) && r.pendingFooter == nil && r.requireFooter {
				return nil, fmt.Errorf("tape: stream ended after %d frames with no footer: %w",
					r.framesRead, ErrTruncatedCapture)
			}
			return nil, err
		}

		frame := env.GetFrame()
		if frame != nil {
			// SEC-001: bound the number of frames a capture may yield.
			r.framesRead++
			if err := r.limits.checkFrameBudget(r.framesRead); err != nil {
				return nil, fmt.Errorf("tape: %w", err)
			}
			return frame, nil
		}

		// A footer marks the clean end of frames. Validate it against what the
		// stream actually carried before reporting a clean EOF: a footer that
		// disagrees with the frames read means the capture lost data, and
		// answering io.EOF there would report success on a damaged file.
		if footer := env.GetFooter(); footer != nil {
			r.pendingFooter = footer
			if declared := int64(footer.GetFrameCount()); declared != r.framesRead {
				return nil, fmt.Errorf("tape: footer declares %d frames, stream carried %d: %w",
					declared, r.framesRead, ErrFooterMismatch)
			}
			// The footer agreeing is tape's OWN trailer checking out. The
			// compressor has a trailer too, and reaching it is what F1 never did.
			if err := r.verifyTail(); err != nil {
				return nil, err
			}
			return nil, io.EOF
		}

		// F9. A variant this reader does not know is SKIPPED AND COUNTED, which
		// is what makes every future envelope kind additive instead of breaking.
		//
		// THE SPLIT IS THE WHOLE POINT AND COLLAPSING IT WOULD SHIP A
		// REGRESSION. Only an UNKNOWN variant is skipped. A KNOWN envelope in the
		// wrong place — a second CaptureHeader mid-stream — is an ORDERING defect
		// in a file this reader fully understands, and it stays an error; that is
		// what stream_integrity_test.go asserts. "I cannot read this" and "I read
		// this and it is in the wrong place" are different facts and only the
		// first is safe to continue past.
		if isUnknownEnvelope(env) {
			r.skippedEnvelopes++
			continue
		}

		// A known variant out of order, or an envelope carrying nothing at all.
		return nil, fmt.Errorf("tape: %w", ErrUnexpectedEnvelope)
	}
}

// ReadFooter returns the capture footer. If ReadFrame already encountered it,
// the cached value is returned. Otherwise reads the next envelope from the stream.
func (r *Reader) ReadFooter() (*capturepb.CaptureFooter, error) {
	if r.pendingFooter != nil {
		return r.pendingFooter, nil
	}

	// Same loop and the same reason as ReadFrame (F9): an unknown variant sitting
	// between the last frame and the footer must not cost a caller its footer.
	for {
		env, err := r.readEnvelope()
		if err != nil {
			if errors.Is(err, io.EOF) && r.requireFooter {
				return nil, fmt.Errorf("tape: no footer envelope in the stream: %w", ErrTruncatedCapture)
			}
			return nil, err
		}

		if footer := env.GetFooter(); footer != nil {
			// A caller that reads the footer directly gets the same integrity
			// check as one that scanned to it through ReadFrame.
			if err := r.verifyTail(); err != nil {
				return nil, err
			}
			return footer, nil
		}

		if isUnknownEnvelope(env) {
			r.skippedEnvelopes++
			continue
		}

		return nil, io.ErrUnexpectedEOF
	}
}

// verifyTail consumes what remains after the footer envelope, which is what
// makes the compressor verify its own content checksum.
//
// THE BUG THIS CLOSES (F1, measured 2026-09-05; see ErrCorruptCapture). zstd
// writes a content checksum on every frame, and klauspost's streaming decoder
// validates it only when the frame's end is consumed. ReadFrame returned
// io.EOF the moment it decoded the footer ENVELOPE, which sits before the end
// of the zstd FRAME, so the last few bytes were never read and the checksum was
// never checked — for any capture, in either layout, ever. 58% of single-bit
// flips read back as clean captures with wrong telemetry.
//
// WHY DRAINING IS THE WHOLE FIX AND NOT A WORKAROUND: the checksum is already
// written, so nothing on disk changes; the decoder is already the thing that
// validates it; the only defect was that this reader stopped one read short of
// asking. It is also layout-neutral — in the per-block layout the drain walks
// off the end of the footer block and the decoder skips the seek table's
// skippable frame, so it costs one Read either way.
//
// A NON-EMPTY TAIL IS ALSO A DEFECT, and a different one: a capture ends at its
// footer, so bytes after it mean concatenation or appending
// (ErrTrailingData). The two are reported separately because they need
// different answers from whoever reads the error.
func (r *Reader) verifyTail() error {
	if r.tailVerified {
		return nil
	}
	r.tailVerified = true

	var buf [512]byte
	// A bound, not a belief: an io.Reader is permitted to return (0, nil), and
	// an unbounded loop over one would hang rather than fail. Any real stream
	// reaches EOF or an error in one or two reads here.
	for range 64 {
		n, err := r.reader.Read(buf[:])
		if n > 0 {
			return fmt.Errorf("tape: %d byte(s) after the footer: %w", n, ErrTrailingData)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			// The decoder's own verdict, wrapped so callers can match on the
			// sentinel while still seeing what the compressor said.
			return fmt.Errorf("tape: %w: %w", ErrCorruptCapture, err)
		}
	}
	return fmt.Errorf("tape: the stream after the footer never ended: %w", ErrTrailingData)
}

// Close closes the decoder and underlying file.
func (r *Reader) Close() error {
	// decoder is nil for an uncompressed capture.
	if r.decoder != nil {
		r.decoder.Close()
	}
	return r.file.Close()
}

// readEnvelope reads a single length-delimited envelope from the stream.
func (r *Reader) readEnvelope() (*capturepb.Envelope, error) {
	// Read varint length.
	var length uint64
	var shift uint
	var b [1]byte
	for {
		if _, err := r.reader.Read(b[:]); err != nil {
			return nil, err
		}
		length |= uint64(b[0]&0x7F) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
		if shift >= 64 {
			return nil, io.ErrUnexpectedEOF
		}
	}

	if length > MaxMessageSize {
		return nil, fmt.Errorf("envelope size %d exceeds maximum %d", length, MaxMessageSize)
	}

	// Read message data. SEC-002: allocation is bounded by bytes actually
	// present in the stream, not by the declared length.
	data, err := readMessageBody(r.reader, length)
	if err != nil {
		return nil, err
	}

	env := &capturepb.Envelope{}
	if err := proto.Unmarshal(data, env); err != nil {
		return nil, err
	}
	return env, nil
}
