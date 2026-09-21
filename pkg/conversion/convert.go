package conversion

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"time"

	telemetryv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	"github.com/echotools/tape/v4/pkg/codec"
	"github.com/echotools/tape/v4/pkg/events"
	"github.com/echotools/tape/v4/pkg/processing"
)

const (
	extEchoReplay = ".echoreplay"
	extNevrcap    = ".nevrcap"
	extTape       = ".tape"
)

// ConvertResult holds statistics from a single file conversion.
type ConvertResult struct {
	InputPath  string
	OutputPath string
	FrameCount uint32
	EventCount uint32
	// SkippedLines counts input lines the reader could not parse and dropped.
	// Non-zero means the tape is not a complete record of its source: some
	// recorders emitted formats this reader does not understand (GH #31), and
	// the loss is otherwise silent.
	SkippedLines uint32
	// DroppedBones counts bones payloads the reader found on a line but could
	// not parse. The frame still converts, with its session intact and its
	// bones absent, so this loss appears in neither FrameCount nor
	// SkippedLines (CANONICAL-001 §3).
	DroppedBones uint32
	// UnmappedValues lists engine strings no conversion table recognised, with
	// occurrence counts, ordered by field then value. Non-empty is the same
	// class of loss as SkippedLines: the value became *_UNSPECIFIED and renders
	// back as the empty string, so it is gone without a trace (VOCAB-001).
	// Conversion deliberately does not fail on these — one unknown string must
	// not abort a corpus-wide pass.
	UnmappedValues []UnmappedValue
	// DroppedFrames counts frames the event detector dropped because its input
	// channel was full. Under the conversion pipeline's drain-per-frame pattern
	// this is always zero; non-zero means a regression.
	DroppedFrames uint64
	// DroppedEvents counts event batches the detector dropped because its
	// events channel was full. Under the conversion pipeline's drain-per-frame
	// pattern this is always zero; non-zero means a regression.
	DroppedEvents uint64
}

// ConvertFile converts a legacy capture file (.echoreplay or .nevrcap) to
// the v2 tape format. The input format is detected by file extension.
func ConvertFile(inputPath, outputPath string) (*ConvertResult, error) {
	ext := filepath.Ext(inputPath)
	switch ext {
	case extEchoReplay:
		return convertEchoReplay(inputPath, outputPath)
	case extNevrcap, extTape:
		return convertLegacy(inputPath, outputPath)
	default:
		return nil, fmt.Errorf("unsupported input format: %s", ext)
	}
}

func convertEchoReplay(inputPath, outputPath string) (*ConvertResult, error) {
	reader, err := codec.NewEchoReplayReader(inputPath)
	if err != nil {
		return nil, fmt.Errorf("open echoreplay: %w", err)
	}
	defer reader.Close() //nolint:errcheck // best-effort cleanup; primary errors returned from pipeline

	return convertFromV1Reader(&echoReplayAdapter{reader: reader}, inputPath, outputPath)
}

func convertLegacy(inputPath, outputPath string) (*ConvertResult, error) {
	reader, err := codec.NewLegacyReader(inputPath)
	if err != nil {
		return nil, fmt.Errorf("open nevrcap: %w", err)
	}
	defer reader.Close() //nolint:errcheck // best-effort cleanup; primary errors returned from pipeline

	return convertFromV1Reader(&legacyAdapter{reader: reader}, inputPath, outputPath)
}

// v1Reader abstracts over EchoReplay and LegacyReader for the conversion pipeline.
type v1Reader interface {
	ReadHeader() (*telemetryv1.TelemetryHeader, error)
	ReadFrame() (*telemetryv1.LobbySessionStateFrame, error)
	Close() error
}

// lineSkipper is implemented by readers that count input records they could not
// parse. Declared at the consumer because only the line-oriented echoreplay
// reader has the notion; the length-delimited legacy reader errors out instead.
type lineSkipper interface {
	SkippedFrames() uint32
	// DroppedBones counts bones payloads present but unparseable. Distinct
	// from SkippedFrames: the frame survives with its session intact, so the
	// loss shows up in neither the frame count nor the skip count.
	DroppedBones() uint32
}

// echoReplayAdapter wraps EchoReplay to implement v1Reader.
// EchoReplay doesn't have ReadHeader, so we synthesize one from the first frame.
type echoReplayAdapter struct {
	reader     *codec.EchoReplay
	firstFrame *telemetryv1.LobbySessionStateFrame
}

// SkippedFrames satisfies lineSkipper, exposing the underlying reader's count of
// unparseable lines to the conversion pipeline.
func (a *echoReplayAdapter) SkippedFrames() uint32 { return a.reader.SkippedFrames() }

// DroppedBones satisfies lineSkipper, exposing the underlying reader's count of
// bones payloads it could not parse.
func (a *echoReplayAdapter) DroppedBones() uint32 { return a.reader.DroppedBones() }

func (a *echoReplayAdapter) ReadHeader() (*telemetryv1.TelemetryHeader, error) {
	// EchoReplay files don't have a separate header. Read the first frame
	// to extract session metadata, then return a synthetic header.
	frame, err := a.reader.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("read first frame for header: %w", err)
	}
	a.firstFrame = frame

	header := &telemetryv1.TelemetryHeader{
		CaptureId: "echoreplay-import",
		CreatedAt: frame.GetTimestamp(),
	}
	return header, nil
}

func (a *echoReplayAdapter) ReadFrame() (*telemetryv1.LobbySessionStateFrame, error) {
	if a.firstFrame != nil {
		f := a.firstFrame
		a.firstFrame = nil
		return f, nil
	}
	return a.reader.ReadFrame()
}

func (a *echoReplayAdapter) Close() error {
	return a.reader.Close()
}

// legacyAdapter wraps LegacyReader to implement v1Reader.
type legacyAdapter struct {
	reader *codec.LegacyReader
}

func (a *legacyAdapter) ReadHeader() (*telemetryv1.TelemetryHeader, error) {
	return a.reader.ReadHeader()
}

func (a *legacyAdapter) ReadFrame() (*telemetryv1.LobbySessionStateFrame, error) {
	return a.reader.ReadFrame()
}

func (a *legacyAdapter) Close() error {
	return a.reader.Close()
}

// convertFromV1Reader is the core pipeline: read v1 frames, detect events,
// map to v2, and write tape output.
func convertFromV1Reader(reader v1Reader, inputPath, outputPath string) (*ConvertResult, error) {
	v1Header, err := reader.ReadHeader()
	if err != nil {
		return nil, fmt.Errorf("read header: %w", err)
	}

	// Read the first frame to enrich the header with session data.
	firstFrame, err := reader.ReadFrame()
	if err != nil {
		return nil, fmt.Errorf("read first frame: %w", err)
	}

	// One vocabulary accumulator per conversion, shared by the header and frame
	// paths so ConvertResult reports the whole file (VOCAB-001).
	vocab := &vocabulary{}
	v2Header := mapHeaderFromSession(v1Header, firstFrame.GetSession(), vocab)

	// A nil protobuf timestamp's AsTime() returns Unix epoch (1970-01-01),
	// not Go's zero time (year 0001), so IsZero() would never trigger.
	// Check the protobuf field directly instead.
	createdAt := v1Header.GetCreatedAt()
	var baseTime time.Time
	if createdAt == nil || createdAt.GetSeconds() == 0 {
		// Header has no creation timestamp; fall back to the first frame's
		// timestamp to avoid uint32 wrap in offset calculations.
		baseTime = firstFrame.GetTimestamp().AsTime()
	} else {
		baseTime = createdAt.AsTime()
	}
	mapper := &FrameMapper{BaseTime: baseTime, vocab: vocab}
	mapper.SeedMatchSettings(v2Header.GetEchoArena())

	writer, err := codec.NewWriter(outputPath)
	if err != nil {
		return nil, fmt.Errorf("create writer: %w", err)
	}

	// closeWriter is used on error paths to ensure the writer is closed.
	// On the success path, writer.Close() is called explicitly.
	writerClosed := false
	closeWriter := func() {
		if !writerClosed {
			writer.Close() //nolint:errcheck // best-effort cleanup on error path
			writerClosed = true
		}
	}

	if err := writer.WriteHeader(v2Header); err != nil {
		closeWriter()
		return nil, fmt.Errorf("write header: %w", err)
	}

	// Set up event detection.
	detector := events.NewWithDefaultSensors(events.WithSynchronousProcessing())
	processor := processing.NewWithDetector(detector)
	defer processor.Stop()

	result := &ConvertResult{
		InputPath:  inputPath,
		OutputPath: outputPath,
	}

	// Process first frame.
	if err := processAndWriteFrame(processor, writer, firstFrame, mapper, result); err != nil {
		closeWriter()
		return nil, err
	}

	// Process remaining frames.
	for {
		frame, err := reader.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			closeWriter()
			return nil, fmt.Errorf("read frame: %w", err)
		}

		if err := processAndWriteFrame(processor, writer, frame, mapper, result); err != nil {
			closeWriter()
			return nil, err
		}
	}

	// Record input the reader could not parse. Counted after the read loop so it
	// reflects the whole file.
	if skipper, ok := reader.(lineSkipper); ok {
		result.SkippedLines = skipper.SkippedFrames()
		result.DroppedBones = skipper.DroppedBones()
	}
	result.UnmappedValues = vocab.sorted()

	// Record event-detector drops. Under the drain-per-frame pattern these
	// are always zero; non-zero is a regression in the conversion pipeline.
	result.DroppedFrames = detector.DroppedFrames()
	result.DroppedEvents = detector.DroppedEvents()

	// Close writer (writes footer).
	writerClosed = true
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("close writer: %w", err)
	}

	// Verify output.
	if err := verifyOutput(outputPath, result.FrameCount); err != nil {
		return nil, fmt.Errorf("verify output: %w", err)
	}

	return result, nil
}

func processAndWriteFrame(
	processor *processing.Processor,
	writer *codec.Writer,
	v1Frame *telemetryv1.LobbySessionStateFrame,
	mapper *FrameMapper,
	result *ConvertResult,
) error {
	// Run event detection on the v1 frame.
	processor.DetectEvents(v1Frame)

	// Drain any detected events and attach them to the frame.
	drainEvents(processor, v1Frame)

	// Map v1 frame to v2 using the stateful mapper (tracks round number). An
	// error means the frame cannot be represented in v2 (e.g. a timestamp offset
	// past the uint32 range, R5) — abort rather than write a silently-corrupted
	// tape.
	v2Frame, err := mapper.MapFrame(v1Frame)
	if err != nil {
		return fmt.Errorf("map frame %d: %w", result.FrameCount, err)
	}

	// Count events on the mapped frame.
	if ea := v2Frame.GetEchoArena(); ea != nil {
		result.EventCount += uint32(len(ea.GetEvents())) //nolint:gosec // event count fits uint32
	}

	if err := writer.WriteFrame(v2Frame); err != nil {
		return fmt.Errorf("write frame %d: %w", result.FrameCount, err)
	}

	result.FrameCount++
	return nil
}

// drainEvents pulls all pending events from the processor channel and
// appends them to the v1 frame's event list (before mapping to v2).
//
// It runs after every DetectEvents call (see processAndWriteFrame), so the
// events channel is emptied once per frame and never holds more than the single
// batch that frame produced. The channel buffers 10 batches (pkg/events/events.go:117),
// so its depth never approaches capacity and the synchronous detector's
// full-channel drop branch (pkg/events/events.go:219-223) is unreachable during
// conversion: no events are lost. (Verified by
// TestSyncDetector_ConversionPatternNoEventLoss.)
func drainEvents(processor *processing.Processor, frame *telemetryv1.LobbySessionStateFrame) {
	for {
		select {
		case evts := <-processor.EventsChan():
			frame.Events = append(frame.Events, evts...)
		default:
			return
		}
	}
}

func verifyOutput(outputPath string, expectedFrames uint32) error {
	reader, err := codec.NewReader(outputPath)
	if err != nil {
		return fmt.Errorf("open for verification: %w", err)
	}
	defer reader.Close() //nolint:errcheck // best-effort cleanup; primary errors returned from pipeline

	if _, err := reader.ReadHeader(); err != nil {
		return fmt.Errorf("read header: %w", err)
	}

	var count uint32
	for {
		_, err := reader.ReadFrame()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return fmt.Errorf("read frame %d: %w", count, err)
		}
		count++
	}

	if count != expectedFrames {
		return fmt.Errorf("frame count mismatch: output has %d, expected %d", count, expectedFrames)
	}

	return nil
}
