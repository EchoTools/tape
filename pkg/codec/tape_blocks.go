package codec

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"

	"github.com/klauspost/compress/zstd"
)

// Per-block container layout — THE DEFAULT.
//
// WHY THIS EXISTS. Writer's own doc comment concedes the defect: "Because the
// stream is Zstd-compressed, byte-offset seeking requires decompressing from
// the start." One zstd.NewWriter wraps the whole file, so every
// KeyframeEntry.ByteOffset is a position in the DECOMPRESSED stream and
// nothing can seek to it. The keyframe index shipped; the property it exists
// for did not. This file supplies the property.
//
// THE LAYOUT. Compression moves inside the container: instead of one zstd
// stream over the whole file, the file becomes a sequence of INDEPENDENT zstd
// frames, and a block boundary falls on a keyframe. One decision does two jobs
// — the keyframe interval is also the block size — rather than two numbers
// drifting apart.
//
//	[zstd frame]  header block                — the CaptureHeader alone
//	[zstd frame]  block 0                     — opens at keyframe, N frames
//	[zstd frame]  block 1                     — opens at the next keyframe
//	...
//	[zstd frame]  footer block                — the CaptureFooter alone
//	[skippable]   seek table (seekable.go)    — block sizes, locatable from EOF
//
// The header gets its own block so that rewriting it — the metadata edit that
// a strip or a re-derivation performs — recompresses one small block instead of
// the first N frames with it. The footer gets its own block for the same
// reason and because it is written last.
//
// WHAT THIS COSTS EXISTING READERS: nothing. zstd decoders are specified to
// decode concatenated frames as one continuous stream and to skip skippable
// frames, so the shipped sequential Reader reads a per-block tape unchanged.
// That is not inherited from the spec — it is verified against the pinned
// klauspost/compress in zstd_container_behavior_test.go, and those tests stay
// as gates.
//
// WHAT IT CHANGES ON DISK: the byte layout, and the meaning of
// KeyframeEntry.ByteOffset, which in this mode is the offset of the block in
// the COMPRESSED file rather than in the decompressed stream.
//
// WHY IT IS THE DEFAULT, AND WHEN THAT CHANGED. It shipped opt-in, on the
// reasoning that a format change must be asked for by name. That reasoning was
// protecting a release that had not happened — the format was unreleased, and
// v4.1.0 is the release — while costing every caller that did not know the
// option's name the seekability this layout exists for. Features default on;
// arguments opt out.
//
// The old reasoning protected a release that had not happened. What it cost
// was real: the seekability this layout exists for was off for every caller
// who did not know the option's name, which is every caller. So NewWriter,
// NewWriterWithKeyframeInterval and NewWriterWithOptions with no options all
// produce THIS layout, and WithWholeStreamCompression is how a caller opts
// back out.

// WriterOption configures a Writer. Options that change the on-disk layout say
// so in their own doc comment. The zero-option writer takes every feature that
// can be on by default: per-block compression at DefaultKeyframeInterval,
// compressed at zstd.SpeedFastest. Turning a feature OFF is what takes an
// argument.
type WriterOption func(*writerConfig)

// writerConfig is the folded result of the options passed to
// NewWriterWithOptions.
type writerConfig struct {
	dict             []byte
	level            zstd.EncoderLevel
	keyframeInterval uint32
	perBlock         bool
}

// WithKeyframeInterval sets how often a keyframe is recorded, in frames. Zero
// is clamped to DefaultKeyframeInterval (GH #23: it reached
// `frameIndex % keyframeInterval` and panicked with a divide by zero).
//
// Under the default per-block layout this interval is also the block size, so
// it sets seek granularity as well as index density. Under
// WithWholeStreamCompression it sets index density only.
func WithKeyframeInterval(n uint32) WriterOption {
	return func(c *writerConfig) { c.keyframeInterval = n }
}

// WithCompressionLevel sets the zstd encoder level. The default is
// zstd.SpeedFastest, which is what the shipped writer has always used.
func WithCompressionLevel(level zstd.EncoderLevel) WriterOption {
	return func(c *writerConfig) { c.level = level }
}

// WithPerBlockCompression writes the capture as independent per-block zstd
// frames plus a seek table, instead of one continuous zstd stream. What it
// buys: a keyframe offset becomes a servable byte range, and reaching frame N
// costs one block instead of the whole file.
//
// THIS IS THE DEFAULT as of v4.1.0 — the option is now a no-op that states an
// intent. It is kept, rather than deleted, because call sites and tests name it
// and because writing it makes a caller's requirement explicit at the call site
// instead of dependent on this package's default. It is idempotent and it wins
// over an earlier WithWholeStreamCompression, so options fold last-wins in
// either order.
func WithPerBlockCompression() WriterOption {
	return func(c *writerConfig) { c.perBlock = true }
}

// WithWholeStreamCompression writes the capture as ONE continuous zstd stream
// with no seek table — the whole-stream layout.
//
// THIS IS THE OPT-OUT, and it is the only way to get that layout now. What it
// costs, and the reason it is not the default: KeyframeEntry.ByteOffset goes
// back to being an offset in the DECOMPRESSED stream, which nothing can seek
// to, so reaching frame N means decompressing every byte before it and serving
// a range of the file is impossible. Integrity also gets later rather than
// absent: a per-block capture's zstd content checksum fires at every block
// boundary, so a flipped bit or a wrong dictionary is caught within one
// keyframe interval, where a whole-stream capture reaches its single checksum
// only at end of file.
//
// It exists for exactly two callers: one that must produce bytes an
// already-deployed whole-stream reader consumes byte-identically, and one
// streaming to a sink where the trailing seek table cannot be written. Anything
// else should take the default.
func WithWholeStreamCompression() WriterOption {
	return func(c *writerConfig) { c.perBlock = false }
}

// WithDictionary compresses with a trained zstd dictionary. Telemetry is close
// to a dictionary's ideal case — many small payloads with identical field
// structure and no shared window — which is what makes per-block compression
// cheap rather than costly.
//
// A dictionary is a PERMANENT OBLIGATION: a capture written with dictionary D
// needs D forever to be read. zstd records the dictionary's id in every frame
// header it writes, so a reader without it fails loudly rather than returning
// wrong bytes (verified in zstd_container_behavior_test.go), but the bytes
// themselves must be kept somewhere the reader can find. Passing nil is the
// same as not passing the option.
func WithDictionary(dict []byte) WriterOption {
	return func(c *writerConfig) { c.dict = dict }
}

// applyWriterOptions folds opts over the shipped defaults.
func applyWriterOptions(opts []WriterOption) writerConfig {
	// Every feature that CAN be on by default IS on by default; arguments opt
	// out. The one
	// exception is WithDictionary, and it is an exception of KIND rather than
	// of policy: a dictionary is trained bytes that do not exist at
	// construction time and cannot be conjured, and it is a permanent
	// obligation on every future reader. There is no dictionary to default TO.
	c := writerConfig{
		keyframeInterval: DefaultKeyframeInterval,
		level:            zstd.SpeedFastest,
		perBlock:         true,
	}
	for _, opt := range opts {
		opt(&c)
	}
	if c.keyframeInterval == 0 {
		c.keyframeInterval = DefaultKeyframeInterval
	}
	return c
}

// blockWriter accumulates envelope bytes for the block being built and emits
// each finished block as its own zstd frame.
//
// It compresses with EncodeAll on a reusable encoder rather than opening a
// zstd.Writer per block: a block is complete in memory before it is written,
// and constructing an encoder per keyframe would dominate the cost of a
// one-second interval.
type blockWriter struct {
	file *os.File
	enc  *zstd.Encoder
	// buf holds the uncompressed envelope bytes of the block in progress. It
	// is a pointer so the struct's pointer-bearing prefix stays compact
	// (govet fieldalignment); a Buffer by value drags the seek table entries
	// past it.
	buf *bytes.Buffer
	// entries is the seek table under construction, one entry per block
	// already written.
	entries []seekTableEntry
	// offset is the number of bytes already written to the file, which is
	// where the next block will begin. This is the number that makes a
	// keyframe offset servable.
	offset uint64
}

// newBlockWriter constructs the block machinery for a per-block capture.
func newBlockWriter(file *os.File, c writerConfig) (*blockWriter, error) {
	opts := []zstd.EOption{zstd.WithEncoderLevel(c.level)}
	if len(c.dict) > 0 {
		opts = append(opts, zstd.WithEncoderDict(c.dict))
	}
	// A nil destination means this encoder is used only through EncodeAll.
	enc, err := zstd.NewWriter(nil, opts...)
	if err != nil {
		return nil, fmt.Errorf("tape: per-block encoder: %w", err)
	}
	return &blockWriter{file: file, enc: enc, buf: &bytes.Buffer{}}, nil
}

// write appends raw bytes to the block in progress. It never fails: the bytes
// go to memory, and the only I/O happens in flush.
func (b *blockWriter) write(p []byte) {
	b.buf.Write(p) //nolint:errcheck // bytes.Buffer.Write never returns an error
}

// flush closes the block in progress, writing it as one independent zstd frame
// and recording its seek table entry. A block with nothing in it is not
// written: an empty zstd frame would occupy a seek table slot that maps to no
// frames, so a reader stepping blocks would see a hole.
func (b *blockWriter) flush() error {
	if b.buf.Len() == 0 {
		return nil
	}
	raw := b.buf.Bytes()
	if len(raw) > math.MaxUint32 {
		return fmt.Errorf("tape: block of %d uncompressed bytes exceeds the seek table's uint32 field: %w",
			len(raw), ErrSeekTableCorrupt)
	}
	compressed := b.enc.EncodeAll(raw, nil)
	if len(compressed) > math.MaxUint32 {
		return fmt.Errorf("tape: block of %d compressed bytes exceeds the seek table's uint32 field: %w",
			len(compressed), ErrSeekTableCorrupt)
	}
	n, err := b.file.Write(compressed)
	b.offset += uint64(max(n, 0)) //nolint:gosec // n is non-negative on success
	if err != nil {
		return fmt.Errorf("tape: writing block: %w", err)
	}
	b.entries = append(b.entries, seekTableEntry{
		compressedSize:   uint32(len(compressed)), //nolint:gosec // bounded above
		decompressedSize: uint32(len(raw)),        //nolint:gosec // bounded above
	})
	b.buf.Reset()
	return nil
}

// finish flushes any remaining block and appends the seek table. After it
// returns the file is a complete per-block capture.
func (b *blockWriter) finish() error {
	if err := b.flush(); err != nil {
		return err
	}
	table, err := appendSeekTable(nil, b.entries)
	if err != nil {
		return err
	}
	if _, err := b.file.Write(table); err != nil {
		return errors.Join(fmt.Errorf("tape: writing seek table: %w", err), b.enc.Close())
	}
	if err := b.enc.Close(); err != nil {
		return fmt.Errorf("tape: closing per-block encoder: %w", err)
	}
	return nil
}
