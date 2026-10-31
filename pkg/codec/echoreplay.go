package codec

import (
	"archive/zip"
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"time"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	telemetry "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	EchoReplayTimeFormat = "2006/01/02 15:04:05.000"
)

var (
	ErrCodecNotConfiguredForWriting = fmt.Errorf("codec not configured for writing")

	// Byte patterns for converting protojson string-encoded uint64 to numbers.
	// protojson encodes uint64 as JSON strings per proto3 spec, but the original game engine
	// outputs them as raw numbers. Third-party parsers expect the original format.
	useridPattern         = []byte(`"userid":"`)
	rulesChangedAtPattern = []byte(`"rules_changed_at":"`)
)

// Use protojson marshaling for compatibility with existing format
var echoReplayerMarshaler = &protojson.MarshalOptions{
	UseProtoNames:   false,
	UseEnumNumbers:  true,
	EmitUnpopulated: true,
}

// EchoReplay handles .echoreplay file format (zip format)
type EchoReplay struct {
	filename    string
	zipWriter   *zip.Writer
	zipReader   *zip.ReadCloser
	file        *os.File
	frameBuffer *bytes.Buffer
	progress    io.Writer

	// Streaming state
	scanner     *bufio.Scanner
	frameIndex  uint32
	replayFile  io.ReadCloser
	unmarshaler *protojson.UnmarshalOptions
	// Reusable buffer for timestamp parsing to avoid allocations
	timestampBuf [len(EchoReplayTimeFormat)]byte
	// Scratch buffer for marshaling
	scratchBuf []byte
	// Flag to track if Finalize has been called
	finalized bool

	// skippedFrames counts lines that failed to parse and were skipped.
	skippedFrames uint32
	// droppedBones counts bones payloads present on a line but unparseable.
	// Distinct from skippedFrames: the session parsed, so the frame survives
	// and the line is not skipped — only the bones are gone.
	droppedBones uint32
	// eof is set when ReadFrame encounters io.EOF.
	eof bool

	// limits bounds hostile-input resource use (SEC-001); see Limits.
	limits Limits
	// framesRead counts frames returned, checked against limits.MaxFrameCount.
	framesRead int64
}

// SkippedFrames returns the number of frames skipped due to parse errors.
func (e *EchoReplay) SkippedFrames() uint32 {
	return e.skippedFrames
}

// DroppedBones returns the number of bones payloads that were present on a line
// but could not be parsed. Such a frame is still returned, with its session
// intact and its bones absent, so this loss is invisible to SkippedFrames.
// Non-zero means the capture's bone data is incomplete (CANONICAL-001 §3).
func (e *EchoReplay) DroppedBones() uint32 {
	return e.droppedBones
}

// EchoReplayFrame represents a frame in the .echoreplay format
type EchoReplayFrame struct {
	Timestamp   string                        `json:"timestamp"`
	Session     *enginev1.SessionResponse     `json:"session"`
	PlayerBones *enginev1.PlayerBonesResponse `json:"user_bones,omitempty"`
}

// NewEchoReplayWriter creates a new EchoReplay codec for writing
func NewEchoReplayWriter(filename string) (*EchoReplay, error) {
	file, err := os.Create(filename)
	if err != nil {
		return nil, err
	}

	zipWriter := zip.NewWriter(file)

	return &EchoReplay{
		filename:    filename,
		file:        file,
		zipWriter:   zipWriter,
		frameBuffer: &bytes.Buffer{},
		scratchBuf:  make([]byte, 0, 1024),
	}, nil
}

// NewEchoReplayReader creates a new EchoReplay codec for reading. Reads are
// bounded by DefaultLimits (SEC-001 decompression-bomb guard); pass
// ReaderOptions to raise or disable the budgets.
func NewEchoReplayReader(filename string, opts ...ReaderOption) (*EchoReplay, error) {
	return NewEchoReplayReaderWithProgress(filename, nil, opts...)
}

// NewEchoReplayReaderWithProgress opens an .echoreplay capture and reports
// read progress to progress, which may be nil to disable reporting. Reads are
// bounded by DefaultLimits unless opts raise or disable the budgets; see
// NewEchoReplayReader.
func NewEchoReplayReaderWithProgress(filename string, progress io.Writer, opts ...ReaderOption) (*EchoReplay, error) {
	zipped, err := fileStartsWith(filename, zipMagic)
	if err != nil {
		return nil, err
	}

	codec := &EchoReplay{
		filename: filename,
		progress: progress,
		limits:   applyReaderOptions(opts).limits,
		unmarshaler: &protojson.UnmarshalOptions{
			DiscardUnknown: true,
		},
	}

	if !zipped {
		// Some recorders wrote the NDJSON directly with no zip container. The
		// line format is identical, so only the source differs.
		file, openErr := os.Open(filename) //nolint:gosec // filename is caller-provided path
		if openErr != nil {
			return nil, fmt.Errorf("codec.NewEchoReplayReaderWithProgress: %w", openErr)
		}
		codec.replayFile = file
		codec.startScanner(file)
		return codec, nil
	}

	zipReader, err := zip.OpenReader(filename)
	if err != nil {
		return nil, err
	}
	codec.zipReader = zipReader

	// Initialize the scanner for streaming
	if err := codec.initScanner(); err != nil {
		zipReader.Close()
		return nil, err
	}

	return codec, nil
}

// initScanner initializes the scanner for streaming frame reads
func (e *EchoReplay) initScanner() error {
	var replayFile *zip.File

	// Look for files in order of preference:
	// 1. File with same name as zip (without .zip extension)
	// 4. Any .echoreplay file
	baseFilename := filepath.Base(e.filename)
	if ext := filepath.Ext(baseFilename); ext != "" {
		baseFilename = baseFilename[:len(baseFilename)-len(ext)]
	}

	for _, file := range e.zipReader.File {
		if file.Name == baseFilename {
			replayFile = file
			break
		}
	}

	if replayFile == nil {
		for _, file := range e.zipReader.File {
			if filepath.Ext(file.Name) == ".echoreplay" {
				replayFile = file
				break
			}
		}
	}

	if replayFile == nil {
		return fmt.Errorf("no `.echoreplay` file found in zip")
	}

	reader, err := replayFile.Open()
	if err != nil {
		return err
	}

	e.replayFile = reader
	e.startScanner(reader)

	return nil
}

// startScanner wires src into the line scanner, applying the progress tee and
// the SEC-001 decoded-bytes budget. Shared by the zip and uncompressed paths,
// which differ only in where the NDJSON comes from.
func (e *EchoReplay) startScanner(src io.Reader) {
	if e.progress != nil {
		src = io.TeeReader(src, e.progress)
	}

	// SEC-001: bound total decompressed bytes read from the source.
	src = newBudgetReader(src, e.limits.MaxDecodedBytes)

	e.scanner = bufio.NewScanner(src)
	// Set a larger buffer for long lines (some frames can be very large)
	// Default is 64KB, increase to 10MB
	const maxScannerBuffer = 10 * 1024 * 1024
	e.scanner.Buffer(make([]byte, 64*1024), maxScannerBuffer)
	e.frameIndex = 0
}

// WriteFrame writes a frame to the .echoreplay file using optimized buffer operations
func (e *EchoReplay) WriteFrame(frame *telemetry.LobbySessionStateFrame) error {
	if e.zipWriter == nil {
		return ErrCodecNotConfiguredForWriting
	}

	if n := e.WriteReplayFrame(e.frameBuffer, frame); n == 0 {
		return fmt.Errorf("failed to marshal frame")
	}
	return nil
}

// WriteFrameBatch writes multiple frames efficiently in a single operation
func (e *EchoReplay) WriteFrameBatch(frames []*telemetry.LobbySessionStateFrame) error {
	if e.zipWriter == nil {
		return ErrCodecNotConfiguredForWriting
	}

	for i, frame := range frames {
		if n := e.WriteReplayFrame(e.frameBuffer, frame); n == 0 {
			return fmt.Errorf("failed to marshal frame at index %d", i)
		}
	}
	return nil
}

// FlushBuffer forces a flush of the internal buffer (useful for periodic flushing)
func (e *EchoReplay) FlushBuffer() error {
	if e.zipWriter == nil {
		return ErrCodecNotConfiguredForWriting
	}

	// For this implementation, we buffer everything until Finalize()
	// This could be enhanced to support intermediate flushing if needed
	return nil
}

// GetBufferSize returns the current size of the internal buffer
func (e *EchoReplay) GetBufferSize() int {
	if e.frameBuffer == nil {
		return 0
	}
	return e.frameBuffer.Len()
}

// WriteReplayFrame appends one echoreplay record for frame to dst and returns
// the number of bytes written, or 0 if the frame carries no timestamp.
//
// The record is written straight into dst through reusable scratch buffers
// rather than via an intermediate allocation, because this runs once per
// frame on the write path.
func (e *EchoReplay) WriteReplayFrame(dst *bytes.Buffer, frame *telemetry.LobbySessionStateFrame) int {
	if frame == nil || frame.Timestamp == nil {
		return 0
	}

	startLen := dst.Len()

	// 1. Timestamp
	fastFormatTimestamp(e.timestampBuf[:], frame.Timestamp.AsTime())
	dst.Write(e.timestampBuf[:23])

	// 2. Separator
	dst.WriteByte('\t')

	// 3. Session - marshal and fix uint64 string encoding
	var err error
	e.scratchBuf = e.scratchBuf[:0]
	e.scratchBuf, err = echoReplayerMarshaler.MarshalAppend(e.scratchBuf, frame.GetSession())
	if err != nil {
		return 0
	}
	if isCombatMatchType(frame.GetSession().GetMatchType()) {
		shaped, shapeErr := shapeCombatSession(e.scratchBuf)
		if shapeErr != nil {
			return 0
		}
		e.scratchBuf = shaped
	}
	e.scratchBuf = FixProtojsonUint64Encoding(e.scratchBuf)
	e.scratchBuf = FixEngineFloatFormatting(e.scratchBuf)
	dst.Write(e.scratchBuf)

	// 4. Player Bones (optional) - only write if present and non-empty
	if frame.GetPlayerBones() != nil {
		// Check if PlayerBones has any actual data
		e.scratchBuf = e.scratchBuf[:0]
		e.scratchBuf, err = echoReplayerMarshaler.MarshalAppend(e.scratchBuf, frame.GetPlayerBones())
		if err == nil && len(e.scratchBuf) > 2 { // More than just "{}"
			// Separator and Space
			dst.WriteByte('\t')
			dst.WriteByte(' ')

			// Write Player Bones
			e.scratchBuf = FixProtojsonUint64Encoding(e.scratchBuf)
			e.scratchBuf = FixEngineFloatFormatting(e.scratchBuf)
			dst.Write(e.scratchBuf)
		}
	}

	// 5. Newline
	dst.WriteString("\r\n")

	return dst.Len() - startLen
}

// FixProtojsonUint64Encoding converts protojson string-encoded uint64 fields back to raw numbers.
// protojson encodes uint64 as JSON strings per proto3 spec (e.g., "userid":"123"),
// but the original game engine outputs them as numbers (e.g., "userid":123).
// This function transforms the JSON to match the original game format for compatibility
// with third-party echoreplay parsers.
//
// This implementation uses direct byte manipulation for performance, avoiding regex overhead.
func FixProtojsonUint64Encoding(data []byte) []byte {
	// Process all occurrences of "userid":"<digits>" -> "userid":<digits>
	data = fixStringEncodedNumber(data, useridPattern)
	// Process all occurrences of "rules_changed_at":"<digits>" -> "rules_changed_at":<digits>
	data = fixStringEncodedNumber(data, rulesChangedAtPattern)
	return data
}

// fixStringEncodedNumber finds pattern (e.g. `"userid":"`) and removes the quotes around the following number.
// Transforms: "userid":"123" -> "userid":123
//
// A single pass over data: each unchanged run is appended to the output once,
// and each valid match removes exactly two bytes (the quote before the digits,
// the last byte of pattern, and the quote after the digits). The output slice is
// allocated lazily on the first valid match with cap len(data), so a full fix is
// one allocation and no reallocation; input with no valid match is returned
// unchanged. This is linear in len(data) where the previous implementation
// re-copied the whole buffer per match (O(n^2)).
func fixStringEncodedNumber(data []byte, pattern []byte) []byte {
	lastCopied := 0 // start of the next unchanged run to append
	searchFrom := 0 // where to resume the pattern search
	var out []byte

	for {
		// Find the pattern starting from the current search position.
		idx := bytes.Index(data[searchFrom:], pattern)
		if idx == -1 {
			break
		}
		idx += searchFrom

		// Position after the pattern (start of the number string)
		numStart := idx + len(pattern)
		if numStart >= len(data) {
			break
		}

		// Find the closing quote of the number
		numEnd := numStart
		for numEnd < len(data) && data[numEnd] >= '0' && data[numEnd] <= '9' {
			numEnd++
		}

		// Skip empty strings (zero digits between quotes)
		if numEnd == numStart {
			searchFrom = numEnd
			continue
		}

		// Verify the number ends with a quote
		if numEnd >= len(data) || data[numEnd] != '"' {
			searchFrom = numEnd
			continue
		}

		// RFC 8259 forbids leading zeros in numbers. If the digit run
		// has more than one digit and starts with '0', skip this match
		// — the input is either a non-numeric string (already correct
		// with quotes) or adversarial input that would produce invalid
		// JSON if unquoted (GH #37).
		if numEnd-numStart > 1 && data[numStart] == '0' {
			searchFrom = numEnd + 1
			continue
		}

		// We found: pattern + digits + quote.
		// Remove: the quote before digits (last char of pattern) and the quote after digits.
		// Before: "userid":"123"
		// After:  "userid":123
		patternEndQuote := idx + len(pattern) - 1 // Position of quote before number
		closingQuote := numEnd                    // Position of quote after number

		if out == nil {
			// First valid match: allocate the output once. The result is
			// strictly shorter than the input, so cap at len(data) and append
			// without reallocating.
			out = make([]byte, 0, len(data))
		}
		out = append(out, data[lastCopied:patternEndQuote]...)
		out = append(out, data[numStart:closingQuote]...)

		// Resume after the closing quote. The unchanged suffix from
		// closingQuote+1 onward is exactly the search region the original
		// loop continued from after its offset advance.
		lastCopied = closingQuote + 1
		searchFrom = numEnd + 1
	}

	if out == nil {
		return data
	}
	out = append(out, data[lastCopied:]...)
	return out
}

func isEscaped(data []byte, i int) bool {
	backslashes := 0
	for j := i - 1; j >= 0 && data[j] == '\\'; j-- {
		backslashes++
	}
	return backslashes%2 == 1
}

// Deprecated: FixExponentNotation has no caller on the write path. The
// echoreplay writer uses FixEngineFloatFormatting, which reproduces the
// engine's float formatting in full; this function handles only exponent
// notation and is retained for compatibility.
//
// FixExponentNotation converts scientific notation floats to decimal notation
// This ensures JSON output uses decimal format (e.g., 0.000001 instead of 1e-6)
// while preserving full precision and not converting to strings
func FixExponentNotation(data []byte) []byte {
	result := make([]byte, 0, len(data))
	i := 0
	inString := false

	for i < len(data) {
		// Handle string boundaries - don't process content inside JSON strings
		if data[i] == '"' && !isEscaped(data, i) {
			inString = !inString
			result = append(result, data[i])
			i++
			continue
		}

		// If inside a string, copy as-is
		if inString {
			result = append(result, data[i])
			i++
			continue
		}

		// Look for a number in scientific notation
		// Pattern: optional '-', digit(s), optional '.', digit(s), 'e' or 'E', optional '+/-', digit(s)
		if i >= len(data) || (data[i] != '-' && (data[i] < '0' || data[i] > '9')) {
			if i < len(data) {
				result = append(result, data[i])
			}
			i++
			continue
		}

		// Found potential number - scan it
		numStart := i

		// Skip leading minus
		if data[i] == '-' {
			i++
		}

		// Skip integer part
		hasDigits := false
		for i < len(data) && data[i] >= '0' && data[i] <= '9' {
			hasDigits = true
			i++
		}

		// Skip decimal point and fraction
		if i < len(data) && data[i] == '.' {
			i++
			for i < len(data) && data[i] >= '0' && data[i] <= '9' {
				hasDigits = true
				i++
			}
		}

		// Check for exponent
		if hasDigits && i < len(data) && (data[i] == 'e' || data[i] == 'E') {
			// This is scientific notation - parse and reformat it
			// Continue scanning the exponent part
			i++ // skip 'e'/'E'
			if i < len(data) && (data[i] == '+' || data[i] == '-') {
				i++ // skip sign
			}
			for i < len(data) && data[i] >= '0' && data[i] <= '9' {
				i++ // skip exponent digits
			}

			// Parse the full number including exponent
			fullNum := data[numStart:i]
			if f, err := strconv.ParseFloat(string(fullNum), 64); err == nil {
				// Format without exponent - use 'f' format with enough precision
				formatted := strconv.FormatFloat(f, 'f', -1, 64)
				result = append(result, []byte(formatted)...)
			} else {
				// If parsing fails, keep original
				result = append(result, fullNum...)
			}
		} else {
			// Not scientific notation, keep as-is
			result = append(result, data[numStart:i]...)
		}
	}

	return result
}

// Finalize writes the buffered data to the zip file and closes it
func (e *EchoReplay) Finalize() error {
	if e.zipWriter == nil {
		return ErrCodecNotConfiguredForWriting
	}

	if e.finalized {
		return nil
	}
	e.finalized = true

	// Create the main replay file in the zip - strip extension to match
	// what the reader's initScanner expects when looking up by base name
	baseFilename := filepath.Base(e.filename)
	if ext := filepath.Ext(baseFilename); ext != "" {
		baseFilename = baseFilename[:len(baseFilename)-len(ext)]
	}
	replayFile, err := e.zipWriter.Create(baseFilename)
	if err != nil {
		return err
	}

	// Write the buffered frame data
	_, err = io.Copy(replayFile, e.frameBuffer)
	if err != nil {
		return err
	}

	return err
}

// ReadFrame reads the next frame from the .echoreplay file
func (e *EchoReplay) ReadFrame() (*telemetry.LobbySessionStateFrame, error) {
	if e.scanner == nil {
		return nil, fmt.Errorf("codec not configured for reading or already closed")
	}

	for e.scanner.Scan() {
		line := e.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		frame, err := e.parseFrameLine(line)
		if err != nil {
			e.skippedFrames++
			continue // Skip invalid lines
		}

		// SEC-001: bound the number of frames a capture may yield.
		e.framesRead++
		if err := e.limits.checkFrameBudget(e.framesRead); err != nil {
			return nil, fmt.Errorf("echoreplay: %w", err)
		}

		frame.FrameIndex = e.frameIndex
		e.frameIndex++
		return frame, nil
	}

	if err := e.scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanner error: %w", err)
	}

	e.eof = true
	return nil, io.EOF
}

// HasNext checks if there are more frames to read
func (e *EchoReplay) HasNext() bool {
	return e.scanner != nil && !e.eof && e.scanner.Err() == nil
}

// parseFrameLine parses a single line into a frame
func (e *EchoReplay) parseFrameLine(line []byte) (*telemetry.LobbySessionStateFrame, error) {
	parts := bytes.Split(line, []byte("\t"))
	if len(parts) < 2 {
		return nil, fmt.Errorf("invalid line format")
	}

	// Parse timestamp
	timestamp, err := fastParseTimestamp(parts[0])
	if err != nil {
		return nil, fmt.Errorf("invalid timestamp format: %s", string(parts[0]))
	}

	// Parse session data. The Spark-format recorder emits the
	// *_team_restart_request fields as JSON booleans, which protojson rejects
	// for their int32 proto type; coerce them to numbers first.
	sessionResponse := &enginev1.SessionResponse{}
	sessionJSON, unnestErr := unnestCombatPayload(parts[1])
	if unnestErr != nil {
		return nil, fmt.Errorf("failed to unnest combat payload: %w", unnestErr)
	}
	if err := e.unmarshaler.Unmarshal(FixBooleanInt32Fields(sessionJSON), sessionResponse); err != nil {
		return nil, fmt.Errorf("failed to unmarshal session data: %w", err)
	}

	// Create frame
	frame := &telemetry.LobbySessionStateFrame{
		Timestamp: timestamppb.New(timestamp),
		Session:   sessionResponse,
	}

	// Parse player bones if present (parts[2])
	if len(parts) > 2 && len(parts[2]) > 0 {
		// Skip leading space if present
		bonesData := parts[2]
		if bonesData[0] == ' ' {
			bonesData = bonesData[1:]
		}

		if len(bonesData) > 0 {
			userBones := &enginev1.PlayerBonesResponse{}
			// The frame is still returned when its bones do not parse — the
			// session is intact and the line is not skipped — so the loss is
			// invisible to skippedFrames. Count it (CANONICAL-001 §3).
			if err := e.unmarshaler.Unmarshal(bonesData, userBones); err != nil {
				e.droppedBones++
			} else {
				frame.PlayerBones = userBones
			}
		}
	}

	return frame, nil
}

// ReadTo reads frames into the provided slice and returns the number of frames read.
// This avoids allocations by reusing the caller's slice.
// Returns the number of frames read and any error encountered.
// If the slice is filled before EOF, it returns the count with no error.
func (e *EchoReplay) ReadTo(frames []*telemetry.LobbySessionStateFrame) (int, error) {
	if e.scanner == nil {
		return 0, fmt.Errorf("codec not configured for reading or already closed")
	}

	count := 0
	for count < len(frames) {
		frame, err := e.ReadFrame()
		if err != nil {
			if err == io.EOF {
				return count, io.EOF
			}
			return count, err
		}
		frames[count] = frame
		count++
	}

	return count, nil
}

// ReadFrames reads all frames from the .echoreplay file. Accumulation is
// bounded by the reader's Limits (SEC-001): past budget it returns an error
// wrapping ErrMaxFrameCount / ErrMaxDecodedBytes instead of heaping frames
// until OOM.
func (e *EchoReplay) ReadFrames() ([]*telemetry.LobbySessionStateFrame, error) {
	var frames []*telemetry.LobbySessionStateFrame

	for {
		frame, err := e.ReadFrame()
		if err != nil {
			if err == io.EOF {
				break
			}
			return nil, err
		}
		frames = append(frames, frame)

		// SEC-001 accumulation guard (belt to the reader's braces): never
		// grow the slice past the frame budget. Checked after the append with
		// > rather than >=, matching Limits.checkFrameBudget: a capture
		// holding exactly MaxFrameCount frames is AT budget, not over it.
		if max := e.limits.MaxFrameCount; max > 0 && int64(len(frames)) > max {
			return nil, fmt.Errorf("echoreplay: %d frames accumulated: %w (budget %d)", len(frames), ErrMaxFrameCount, max)
		}
	}

	return frames, nil
}

// Close closes the codec and underlying files
func (e *EchoReplay) Close() error {
	var err error

	if e.replayFile != nil {
		if closeErr := e.replayFile.Close(); closeErr != nil {
			err = closeErr
		}
		e.replayFile = nil
		e.scanner = nil
	}

	if e.zipWriter != nil {
		if finErr := e.Finalize(); finErr != nil && err == nil {
			err = finErr
		}
		if closeErr := e.zipWriter.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}

	if e.zipReader != nil {
		if closeErr := e.zipReader.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}

	if e.file != nil {
		if closeErr := e.file.Close(); closeErr != nil && err == nil {
			err = closeErr
		}
	}

	return err
}

// ReadFrameTo reads the next frame into the provided frame object to avoid allocations.
// Returns true if a frame was read, false if EOF or error.
// The frame parameter must be non-nil.
func (e *EchoReplay) ReadFrameTo(frame *telemetry.LobbySessionStateFrame) (bool, error) {
	if e.scanner == nil {
		return false, fmt.Errorf("codec not configured for reading or already closed")
	}

	for e.scanner.Scan() {
		line := e.scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		if err := e.parseFrameLineTo(line, frame); err != nil {
			e.skippedFrames++
			continue // Skip invalid lines
		}

		// SEC-001: bound the number of frames a capture may yield.
		e.framesRead++
		if err := e.limits.checkFrameBudget(e.framesRead); err != nil {
			return false, fmt.Errorf("echoreplay: %w", err)
		}

		frame.FrameIndex = e.frameIndex
		e.frameIndex++
		return true, nil
	}

	if err := e.scanner.Err(); err != nil {
		return false, fmt.Errorf("scanner error: %w", err)
	}

	return false, io.EOF
}

// parseFrameLineTo parses a single line into the provided frame object
func (e *EchoReplay) parseFrameLineTo(line []byte, frame *telemetry.LobbySessionStateFrame) error {
	// Reset sub-messages before unmarshaling to prevent protojson merge behavior
	// from accumulating repeated fields across calls.
	if frame.Session != nil {
		proto.Reset(frame.Session)
	}
	if frame.PlayerBones != nil {
		proto.Reset(frame.PlayerBones)
	}

	// Find tab positions to avoid bytes.Split allocation
	firstTab := bytes.IndexByte(line, '\t')
	if firstTab == -1 {
		return fmt.Errorf("invalid line format")
	}

	// Parse timestamp
	tsBytes := line[:firstTab]
	timestamp, err := fastParseTimestamp(tsBytes)
	if err != nil {
		return fmt.Errorf("invalid timestamp format")
	}

	// Find second tab (optional for Spark format compatibility)
	secondTab := bytes.IndexByte(line[firstTab+1:], '\t')

	var sessionBytes, bonesBytes []byte

	if secondTab == -1 {
		// Spark format: only timestamp and session (no player bones)
		sessionBytes = line[firstTab+1:]
		bonesBytes = nil
	} else {
		// Full format: timestamp, session, and player bones
		secondTab += firstTab + 1
		sessionBytes = line[firstTab+1 : secondTab]
		bonesBytes = line[secondTab+1:]

		// Skip leading space if present
		if len(bonesBytes) > 0 && bonesBytes[0] == ' ' {
			bonesBytes = bonesBytes[1:]
		}
	}

	// Parse session data. The Spark-format recorder emits the
	// *_team_restart_request fields as JSON booleans, which protojson rejects
	// for their int32 proto type; coerce them to numbers first.
	if frame.Session == nil {
		frame.Session = &enginev1.SessionResponse{}
	}
	sessionJSON, unnestErr := unnestCombatPayload(sessionBytes)
	if unnestErr != nil {
		return fmt.Errorf("failed to unnest combat payload: %w", unnestErr)
	}
	if err := e.unmarshaler.Unmarshal(FixBooleanInt32Fields(sessionJSON), frame.Session); err != nil {
		return fmt.Errorf("failed to unmarshal session data: %w", err)
	}

	// Parse player bones data if present. A bones payload that does not unmarshal
	// must not discard the line: the session is intact, so the frame is returned
	// with its bones absent and the loss is counted rather than silently dropped
	// (CANONICAL-001 §3). This mirrors parseFrameLine exactly — the reuse API and
	// the allocating API agree on every line (release-audit R4).
	if len(bonesBytes) > 0 {
		if frame.PlayerBones == nil {
			frame.PlayerBones = &enginev1.PlayerBonesResponse{}
		}
		if err := e.unmarshaler.Unmarshal(bonesBytes, frame.PlayerBones); err != nil {
			e.droppedBones++
			frame.PlayerBones = nil
		}
	} else {
		frame.PlayerBones = nil
	}

	// Set timestamp - reuse existing object to avoid allocation
	if frame.Timestamp == nil {
		frame.Timestamp = timestamppb.New(timestamp)
	} else {
		frame.Timestamp.Seconds = timestamp.Unix()
		frame.Timestamp.Nanos = int32(timestamp.Nanosecond())
	}

	return nil
}

// echoReplayTimestampShape is the engine's timestamp layout, one entry per
// byte position: 0 marks a position that must hold an ASCII digit, and any
// other value is the literal separator byte required there.
//
//	2006/01/02 15:04:05.000
//	0123456789012345678901
var echoReplayTimestampShape = [23]byte{
	0, 0, 0, 0, '/', 0, 0, '/', 0, 0, ' ',
	0, 0, ':', 0, 0, ':', 0, 0, '.', 0, 0, 0,
}

// fastParseTimestamp parses the engine's fixed-width timestamp without
// allocating.
//
// Every byte is validated against echoReplayTimestampShape before any
// arithmetic. Checking only the length would let arbitrary 23-byte input
// through: byte subtraction on a non-digit wraps, so garbage yielded a
// plausible-looking time.Date rather than an error, and the line was counted as
// a good frame instead of a skip. A fabricated timestamp is worse than a
// dropped line — it becomes the synthesized header's baseTime and shifts every
// frame offset in the capture.
func fastParseTimestamp(buf []byte) (time.Time, error) {
	fail := func(msg string) (time.Time, error) {
		return time.Time{}, &time.ParseError{Layout: EchoReplayTimeFormat, Value: string(buf), Message: msg}
	}

	if len(buf) != 23 {
		return fail("invalid length")
	}
	for i, want := range echoReplayTimestampShape {
		if want == 0 {
			if buf[i] < '0' || buf[i] > '9' {
				return fail("expected a digit")
			}
			continue
		}
		if buf[i] != want {
			return fail("expected a separator")
		}
	}

	year := int(buf[0]-'0')*1000 + int(buf[1]-'0')*100 + int(buf[2]-'0')*10 + int(buf[3]-'0')
	month := time.Month(int(buf[5]-'0')*10 + int(buf[6]-'0'))
	day := int(buf[8]-'0')*10 + int(buf[9]-'0')
	hour := int(buf[11]-'0')*10 + int(buf[12]-'0')
	min := int(buf[14]-'0')*10 + int(buf[15]-'0')
	sec := int(buf[17]-'0')*10 + int(buf[18]-'0')
	ms := int(buf[20]-'0')*100 + int(buf[21]-'0')*10 + int(buf[22]-'0')

	// Reject out-of-range components rather than letting time.Date normalize
	// them: "2026/13/40" would otherwise silently become a real date in the
	// following year, which is the same fabrication in a different disguise.
	if month < time.January || month > time.December ||
		day < 1 || day > 31 || hour > 23 || min > 59 || sec > 60 {
		return fail("component out of range")
	}

	return time.Date(year, month, day, hour, min, sec, ms*1000000, time.UTC), nil
}

func fastFormatTimestamp(dst []byte, t time.Time) {
	// 2006/01/02 15:04:05.000
	year, month, day := t.Date()
	hour, min, sec := t.Clock()
	ms := t.Nanosecond() / 1000000

	// Year
	dst[0] = byte(year/1000) + '0'
	dst[1] = byte((year/100)%10) + '0'
	dst[2] = byte((year/10)%10) + '0'
	dst[3] = byte(year%10) + '0'
	dst[4] = '/'

	// Month
	dst[5] = byte(month/10) + '0'
	dst[6] = byte(month%10) + '0'
	dst[7] = '/'

	// Day
	dst[8] = byte(day/10) + '0'
	dst[9] = byte(day%10) + '0'
	dst[10] = ' '

	// Hour
	dst[11] = byte(hour/10) + '0'
	dst[12] = byte(hour%10) + '0'
	dst[13] = ':'

	// Minute
	dst[14] = byte(min/10) + '0'
	dst[15] = byte(min%10) + '0'
	dst[16] = ':'

	// Second
	dst[17] = byte(sec/10) + '0'
	dst[18] = byte(sec%10) + '0'
	dst[19] = '.'

	// Millisecond
	dst[20] = byte(ms/100) + '0'
	dst[21] = byte((ms/10)%10) + '0'
	dst[22] = byte(ms%10) + '0'
}
