package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"os"

	capturepb "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v2"
	"github.com/klauspost/compress/zstd"
)

// maxWriterWindow is the largest zstd window any capture this library wrote can
// carry, and it is FOUND rather than chosen. klauspost sets the encoder window
// from the level and nothing else: SpeedFastest gives 4 MiB, and SpeedDefault,
// SpeedBetterCompression and SpeedBestCompression each give 8 MiB
// (compress@v1.19.2 zstd/encoder_options.go:246-259; the package default is
// also 8 MiB at :42). Every encoder in this package is constructed with
// WithEncoderLevel and no window option — tape.go:133,137 @ bdf131f and
// tape_blocks.go:198,203 @ bdf131f — so 8 MiB is the ceiling our own writer can
// reach.
//
// THE TWO CITATIONS ABOVE ARE THE WHOLE DERIVATION, and they are pinned because
// the bound is only found for as long as they hold. The predicate that falsifies
// it is one command:
//
//	git grep -n 'WithWindowSize' <sha> -- pkg cmd
//
// EXPECTED OUTPUT IS NOT EMPTY, and saying so is the point: at bdf131f it was,
// but this comment and seekable_window_test.go now match it, so a reader running
// it today gets three hits and must read them rather than count them.
//
//	seekable.go:30                     this line
//	seekable_window_test.go:56,197     the hostile windows the tests craft
//
// Those three are the pass. A hit OUTSIDE a _test.go file is the failure: some
// writer chose its own window, and this constant must be re-derived from that
// call rather than left as a number somebody once measured.
//
// The same predicate answers the question this package cannot answer about
// itself — whether a PRODUCER can emit a file this reader would refuse. Surveyed
// 2026-09-07 across tape, nevr-stream and nevr-agent: no production code in any
// of the three sets a window; nevr-stream (internal/api/storage_manager.go:116)
// and nevr-agent (internal/agent/writer_tape.go:96) both write through the
// zero-option codec.NewWriter and inherit this ceiling, and the one direct
// encoder outside tape (nevr-agent internal/agent/legacy_writer.go:26) uses
// SpeedFastest, which is 4 MiB — half of it. That survey covers *.go in those
// three trees and nothing else; a producer written elsewhere, or in another
// language, is not covered by it.
//
// AND IT HAS AN EXPIRY, NOT ONLY A PERIMETER. The sentence above scopes WHERE I
// looked. It does not scope WHEN, and that is the half that rots: a survey
// enumerates the producers that existed on its date, so a producer created after
// it is outside the evidence no matter how thoroughly the trees were searched.
// This is not hypothetical as of 2026-09-07 — nevr-agent, one of the two
// surveyed producers, is being RETIRED in favour of a direct engine-memory
// reader that did not exist when the survey ran and has never been read. Nothing
// above is wrong; it is simply evidence about a producer set that is changing.
//
// So: the grep is repo-local and can never see a producer in another repository,
// and the survey is a snapshot and can never see a producer added after it. The
// failure mode both miss is the same one and it is the benign-looking direction —
// a LEGITIMATE capture refused with ErrWindowTooLarge, not a bomb let through.
// Whoever adds a producer re-runs the predicate against it; whoever hits an
// unexpected ErrWindowTooLarge on a file they trust should suspect this constant
// before suspecting the file.
//
// It is deliberately NOT read from the file. hdr.WindowSize is the hostile
// file's own number, and a cap computed from it is the attacker choosing their
// own ceiling: klauspost sizes the history buffer from the frame's window
// (zstd/framedec.go:255-266), so a frame declaring 256 MiB costs about that
// much before any other check can object. Measured on this path: a 54,303-byte
// crafted file drove 1433.4 MiB of allocation with the caller's budget set to
// 1 MiB, and the cost tracked the file's number linearly (39.5 / 191.6 / 788.7
// / 1433.4 MiB for declared payloads of 8 / 32 / 128 / 256 MiB).
//
// Nothing in docs/format-design.md specifies a window for the container, so
// this is a policy of THIS reader, not a conformance rule of the format: a
// third-party writer using a larger window would produce a file zstd considers
// valid and this reader refuses. The error says so rather than calling it
// corruption.
const maxWriterWindow = 8 << 20

// describeOversizeWindow says which of two shapes an over-ceiling window has, so
// that a refusal diagnoses itself instead of arriving as a support ticket.
//
// WHY IT EXISTS. maxWriterWindow is derived from a producer survey, and a survey
// has both a perimeter and an expiry (see above). The failure mode it cannot see
// is the benign-looking one: a LEGITIMATE capture, from a producer nobody
// surveyed, refused as though it were an attack. The reader cannot avoid that —
// it must refuse what it cannot bound — but it can say which of the two it thinks
// it is looking at, and that turns "why won't this file open" into a pointer at
// this constant.
//
// THE DISCRIMINATOR IS DERIVED, NOT PICKED. A zstd window is a back-reference
// distance, so it can never usefully exceed the data it references, and klauspost
// shrinks the window descriptor to fit short content. A legitimate block
// therefore satisfies window <= max(declared, MinWindowSize); the floor is zstd's
// own 1 KiB minimum window (zstd.MinWindowSize, compress@v1.19.2
// zstd/framedec.go:38-39), which even a tiny block still carries. Measured both
// ways: a real nevr-stream capture rewritten per-block carries a 1 KiB window
// against ~113 KiB blocks, while the hostile file in
// TestReadBlockRefusesAWindowThisLibraryCannotHaveWritten carries a 256 MiB
// window against 182 declared bytes — a frame asking to buy memory it has no data
// to reference.
//
// NEITHER BRANCH SAYS A FILE IS LEGITIMATE, and the wording keeps that honest.
// Both operands are numbers the file chose, so this can be lied to; it is a
// DIAGNOSTIC on a refusal that has already happened, never an input to it. The
// benign branch means "this does not have the attack's shape — go look at
// maxWriterWindow's producer survey", not "this file is fine".
//
// If nevr-stream begins serving byte ranges, the version to reach for is the
// stronger one this deliberately is not: a distinct sentinel or counter, so the
// ceiling biting real data is observable in aggregate rather than one error
// message at a time. That earns its own gate; this does not, because it changes
// no behaviour.
func describeOversizeWindow(window, declared uint64) string {
	if window > max(declared, zstd.MinWindowSize) {
		return fmt.Sprintf("the block decodes to %d bytes, so a %d-byte window "+
			"references data that is not there: this has the shape of a decompression bomb",
			declared, window)
	}
	return fmt.Sprintf("the block decodes to %d bytes, which this window is "+
		"proportionate to: this has the shape of a legitimate capture from a writer "+
		"using a larger window than %d — see maxWriterWindow and re-run its producer survey",
		declared, uint64(maxWriterWindow))
}

// The zstd seekable format — the index that makes a per-block .tape servable.
//
// A .tape written as one continuous zstd stream records keyframe byte offsets
// that nothing can seek to: the offsets are positions in the DECOMPRESSED
// stream, and reaching any of them means decompressing from the start. The
// container doc comment on Writer admits this in as many words. Splitting the
// container into independent zstd frames fixes the seeking, but only if
// something records where each frame begins in the COMPRESSED file.
//
// That index is not invented here. The zstd seekable format is the existing
// prior art for exactly this problem, and it is used verbatim:
//
//	facebook/zstd, contrib/seekable_format/zstd_seekable_compression_format.md
//
// Its shape, from that specification:
//
//	Seek_Table_Frame
//	  Skippable_Magic_Number   4 B LE  0x184D2A5E
//	  Frame_Size               4 B LE  byte length of everything after this field
//	  Seek_Table_Entries       N x (8 B, or 12 B with checksums)
//	  Seek_Table_Footer        9 B
//
//	Seek_Table_Entry
//	  Compressed_Size          4 B LE
//	  Decompressed_Size        4 B LE
//	  Checksum                 4 B LE  (present only if the descriptor says so)
//
//	Seek_Table_Footer
//	  Number_Of_Frames         4 B LE
//	  Seek_Table_Descriptor    1 B     bit 7 = checksums present; bits 6-2 reserved, must be 0
//	  Seekable_Magic_Number    4 B LE  0x8F92EAB1
//
// Two properties are why this shape and not a bespoke trailer, and both are
// verified against the pinned decoder in zstd_container_behavior_test.go rather
// than taken on faith:
//
//   - The table rides in a zstd SKIPPABLE frame, so a decoder that knows
//     nothing about seeking skips it instead of failing on it. The index costs
//     existing readers nothing.
//   - The footer is fixed-size and sits at EOF, so the table is locatable
//     backwards from the end of the file without decompressing anything.
//
// The table records block sizes; it does not record which capture frame starts
// which block. That mapping is already the job of CaptureFooter.keyframe_index,
// and the two are read together: the keyframe index answers "which block holds
// frame N", the seek table answers "where does that block begin and how big is
// it".

const (
	// seekableSkippableMagic marks the skippable frame that carries the seek
	// table. It is one of the 16 legal Skippable_Magic_Numbers
	// (0x184D2A50-0x184D2A5F); the seekable format reserves 0x184D2A5E.
	seekableSkippableMagic uint32 = 0x184D2A5E

	// seekableFooterMagic terminates the seek table footer, so a reader
	// scanning backwards from EOF can tell a seek table from arbitrary
	// trailing bytes.
	seekableFooterMagic uint32 = 0x8F92EAB1

	// seekTableFooterSize is Number_Of_Frames (4) + Seek_Table_Descriptor (1)
	// + Seekable_Magic_Number (4).
	seekTableFooterSize = 9

	// seekTableEntrySize is Compressed_Size (4) + Decompressed_Size (4). Tape
	// writes no per-entry checksums: zstd frames carry their own optional
	// content checksum, and a second one here would cost 50% more index for a
	// weaker guarantee.
	seekTableEntrySize = 8

	// skippableFrameHeaderSize is Skippable_Magic_Number (4) + Frame_Size (4).
	skippableFrameHeaderSize = 8

	// seekTableChecksumFlag is bit 7 of Seek_Table_Descriptor.
	seekTableChecksumFlag byte = 0x80

	// seekTableReservedMask covers descriptor bits 6-2, which the format
	// requires to be zero. A reader that ignored them would misparse a future
	// revision instead of refusing it.
	seekTableReservedMask byte = 0x7C
)

// Errors returned when a seek table is absent or unusable. They are distinct
// because the two states mean different things to a caller: ErrNoSeekTable
// means "this file was not written for seeking" (a legal state — the shipped
// whole-stream layout has no table), while the others mean "there is a table
// here and it is damaged".
var (
	// ErrNoSeekTable reports that the file does not end in a seek table. This
	// is not corruption: a whole-stream .tape is a valid capture with no index.
	ErrNoSeekTable = errors.New("tape: no zstd seek table at end of file")

	// ErrSeekTableCorrupt reports a seek table whose own fields disagree —
	// a declared frame count that does not fit the skippable frame it lives
	// in, or reserved descriptor bits this reader does not understand.
	ErrSeekTableCorrupt = errors.New("tape: seek table is malformed")
)

// seekTableEntry describes one independently decompressible block: how many
// bytes it occupies in the file, and how many bytes it yields when decoded.
type seekTableEntry struct {
	compressedSize   uint32
	decompressedSize uint32
}

// appendSeekTable appends a complete seek table skippable frame to dst and
// returns the extended slice. It is append-style rather than io.Writer-style
// because the table is built once, at Close, from sizes already in memory —
// there is no streaming case, and a []byte keeps the caller's error handling
// at one place instead of nine.
//
// An entry whose sizes exceed uint32 cannot be represented by the format at
// all, so this reports it rather than truncating: a silently wrapped size
// would produce a table that seeks to the wrong offset, which is worse than
// no table.
func appendSeekTable(dst []byte, entries []seekTableEntry) ([]byte, error) {
	if len(entries) > math.MaxUint32 {
		return nil, fmt.Errorf("tape: seek table: %d blocks exceeds the format's uint32 frame count: %w",
			len(entries), ErrSeekTableCorrupt)
	}

	contentSize := len(entries)*seekTableEntrySize + seekTableFooterSize
	if contentSize > math.MaxUint32 {
		return nil, fmt.Errorf("tape: seek table: %d bytes exceeds a skippable frame's uint32 size: %w",
			contentSize, ErrSeekTableCorrupt)
	}

	dst = binary.LittleEndian.AppendUint32(dst, seekableSkippableMagic)
	dst = binary.LittleEndian.AppendUint32(dst, uint32(contentSize)) //nolint:gosec // bounded above
	for _, e := range entries {
		dst = binary.LittleEndian.AppendUint32(dst, e.compressedSize)
		dst = binary.LittleEndian.AppendUint32(dst, e.decompressedSize)
	}
	dst = binary.LittleEndian.AppendUint32(dst, uint32(len(entries))) //nolint:gosec // bounded above
	dst = append(dst, 0)                                              // descriptor: no checksums, no reserved bits
	dst = binary.LittleEndian.AppendUint32(dst, seekableFooterMagic)
	return dst, nil
}

// readSeekTable locates and parses the seek table at the end of a file of the
// given size, reading backwards from EOF so nothing is decompressed to find it.
//
// It returns ErrNoSeekTable when the file simply does not carry one — a
// WithWholeStreamCompression capture, a truncated live capture, or any other
// zstd file — so a caller can fall back to sequential reading rather than treating
// the absence as damage. This mirrors the design chain's C′: an absent index
// means unfinished, not corrupt.
func readSeekTable(r io.ReaderAt, size int64) ([]seekTableEntry, error) {
	if size < seekTableFooterSize+skippableFrameHeaderSize {
		return nil, ErrNoSeekTable
	}

	var footer [seekTableFooterSize]byte
	if _, err := r.ReadAt(footer[:], size-seekTableFooterSize); err != nil {
		return nil, fmt.Errorf("tape: seek table: reading footer: %w", err)
	}
	if binary.LittleEndian.Uint32(footer[5:9]) != seekableFooterMagic {
		return nil, ErrNoSeekTable
	}

	numFrames := binary.LittleEndian.Uint32(footer[0:4])
	descriptor := footer[4]
	if descriptor&seekTableReservedMask != 0 {
		return nil, fmt.Errorf("tape: seek table descriptor %#02x sets reserved bits: %w",
			descriptor, ErrSeekTableCorrupt)
	}
	entrySize := int64(seekTableEntrySize)
	if descriptor&seekTableChecksumFlag != 0 {
		entrySize += 4 // Checksum
	}

	entriesSize := int64(numFrames) * entrySize
	frameSize := entriesSize + seekTableFooterSize
	tableStart := size - frameSize - skippableFrameHeaderSize
	if tableStart < 0 {
		return nil, fmt.Errorf("tape: seek table declares %d blocks (%d bytes) but the file is %d bytes: %w",
			numFrames, frameSize, size, ErrSeekTableCorrupt)
	}

	var header [skippableFrameHeaderSize]byte
	if _, err := r.ReadAt(header[:], tableStart); err != nil {
		return nil, fmt.Errorf("tape: seek table: reading skippable frame header: %w", err)
	}
	if magic := binary.LittleEndian.Uint32(header[0:4]); magic != seekableSkippableMagic {
		// The trailing bytes looked like a footer but the frame they claim to
		// end does not exist. Treating this as "no table" would hide a
		// damaged index behind a legal-sounding answer.
		return nil, fmt.Errorf("tape: seek table footer at EOF but skippable magic is %#x: %w",
			magic, ErrSeekTableCorrupt)
	}
	if declared := int64(binary.LittleEndian.Uint32(header[4:8])); declared != frameSize {
		return nil, fmt.Errorf("tape: skippable frame declares %d content bytes, footer implies %d: %w",
			declared, frameSize, ErrSeekTableCorrupt)
	}

	if numFrames == 0 {
		return nil, nil
	}

	buf := make([]byte, entriesSize)
	if _, err := r.ReadAt(buf, tableStart+skippableFrameHeaderSize); err != nil {
		return nil, fmt.Errorf("tape: seek table: reading %d entries: %w", numFrames, err)
	}

	entries := make([]seekTableEntry, numFrames)
	for i := range entries {
		off := int64(i) * entrySize
		entries[i] = seekTableEntry{
			compressedSize:   binary.LittleEndian.Uint32(buf[off : off+4]),
			decompressedSize: binary.LittleEndian.Uint32(buf[off+4 : off+8]),
		}
	}
	return entries, nil
}

// blockAt returns the file offset and compressed length of the block holding
// the given compressed-file offset, plus its index. It exists so a caller that
// has a KeyframeEntry.ByteOffset can turn it into a servable byte range
// without re-deriving the running sum every time.
//
// The offset must fall on a block boundary: a keyframe offset that lands in the
// middle of a block means the index and the table disagree, which is a defect
// worth reporting rather than rounding away.
func blockAt(entries []seekTableEntry, offset uint64) (index int, length uint64, err error) {
	var running uint64
	for i, e := range entries {
		if running == offset {
			return i, uint64(e.compressedSize), nil
		}
		running += uint64(e.compressedSize)
	}
	return 0, 0, fmt.Errorf("tape: offset %d is not a block boundary in a %d-block seek table: %w",
		offset, len(entries), ErrSeekTableCorrupt)
}

// BlockIndex is the seek table of a per-block capture, read from the end of the
// file without decompressing anything.
//
// It exists so a byte-range server can answer "give me the bytes for frame N"
// with a file offset and a length. That is the whole point of the per-block
// layout: a keyframe's KeyframeEntry.ByteOffset becomes a position in the
// compressed file, and this turns that position into a range.
type BlockIndex struct {
	path    string
	entries []seekTableEntry
	// limits bounds what this read path will spend on a hostile file, the same
	// budgets the sequential Reader enforces. Before v4.1.0 they were not
	// reachable from here at all (F4): a caller who bounded a Reader had no way
	// to bound a BlockIndex, and did not choose which path a library function
	// took.
	limits Limits
	// size is the file's length at open time, the only ground truth about how
	// large a block can possibly be. A seek table is data from the file; the
	// file's own size is not.
	size int64
	// skippedEnvelopes counts unknown-variant envelopes skipped while locating
	// the footer (F9). Surfaced by SkippedEnvelopes.
	skippedEnvelopes int64
}

// SkippedEnvelopes returns how many envelopes Footer skipped because they
// carried a variant this reader does not know (F9).
//
// It counts only what the seeking path skipped. A BlockIndex reads one block —
// the footer's — so this is not a census of the whole capture; the sequential
// Reader's counter of the same name is. Both exist because both paths can meet a
// variant from the future and neither may drop one silently (AGENTS.md §4).
//
// ITS ONLY CALLER IS IN ANOTHER REPOSITORY, AND THAT IS BY DESIGN — do not file
// this as an uncalled method. `grep -rn OpenBlockIndex cmd/ --include='*.go'`
// finds two hits, both in dict_test.go and none in production code: no tapedeck
// command uses the seeking path, because the per-block layout exists for
// nevr-stream's byte-range server rather than for the CLI.
// AGENTS.md §4's "an uncalled method is a defect, not instrumentation" is aimed
// at a counter nothing reads; an exported accessor whose consumer is the
// out-of-repo reader the layout was built for is a different thing. The
// sequential Reader's counter is the one with an in-repo consumer (`tapedeck
// show`), and it is there that a regression would surface.
//
// The follow-up that would close the gap properly, when it is worth it: a
// seek-table section in `tapedeck show` — block count, servability, and this
// counter. Deliberately not in this change.
func (i *BlockIndex) SkippedEnvelopes() int64 { return i.skippedEnvelopes }

// OpenBlockIndex reads the seek table of the capture at filename.
//
// It returns ErrNoSeekTable for a capture written with
// WithWholeStreamCompression, or for one still being written. That is a legal answer, not a
// failure: such a capture is read sequentially instead. A table that is
// present but internally inconsistent returns ErrSeekTableCorrupt, because
// answering "no index" there would hide damage behind a legal-sounding state.
//
// ReaderOptions bound what reads through the returned index will spend, exactly
// as they do for NewReader. Passing none applies DefaultLimits.
func OpenBlockIndex(filename string, opts ...ReaderOption) (*BlockIndex, error) {
	cfg := applyReaderOptions(opts)
	file, err := os.Open(filename) //nolint:gosec // filename is caller-provided path
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read-only

	info, err := file.Stat()
	if err != nil {
		return nil, fmt.Errorf("tape: stat %s: %w", filename, err)
	}

	entries, err := readSeekTable(file, info.Size())
	if err != nil {
		return nil, err
	}
	return &BlockIndex{entries: entries, path: filename, limits: cfg.limits, size: info.Size()}, nil
}

// Limits returns the resource budgets in effect for reads through this index.
func (i *BlockIndex) Limits() Limits { return i.limits }

// Blocks reports how many independently decompressible blocks the capture
// holds, including the header block and the footer block.
func (i *BlockIndex) Blocks() int { return len(i.entries) }

// ByteRange resolves a KeyframeEntry.ByteOffset to the file offset and length
// of the block opening at that keyframe. The returned range is one complete
// zstd frame and decompresses on its own, with no bytes before it.
//
// An offset that is not a block boundary returns ErrSeekTableCorrupt: it means
// the keyframe index and the seek table disagree, and serving the enclosing
// block instead would hand the caller a range that starts at the wrong frame.
func (i *BlockIndex) ByteRange(keyframeOffset uint64) (offset, length uint64, err error) {
	if _, blockLen, err := blockAt(i.entries, keyframeOffset); err != nil {
		return 0, 0, err
	} else {
		return keyframeOffset, blockLen, nil
	}
}

// DecompressedSize reports the uncompressed size of block n, which is what a
// caller needs to size a buffer before decoding a served range.
func (i *BlockIndex) DecompressedSize(n int) (uint32, error) {
	if n < 0 || n >= len(i.entries) {
		return 0, fmt.Errorf("tape: block %d out of range (%d blocks): %w", n, len(i.entries), ErrSeekTableCorrupt)
	}
	return i.entries[n].decompressedSize, nil
}

// BlockRange returns the file offset and length of block n. Block 0 is the
// header block and the last block is the footer block; the blocks between them
// each open at a keyframe.
func (i *BlockIndex) BlockRange(n int) (offset, length uint64, err error) {
	if n < 0 || n >= len(i.entries) {
		return 0, 0, fmt.Errorf("tape: block %d out of range (%d blocks): %w", n, len(i.entries), ErrSeekTableCorrupt)
	}
	for _, e := range i.entries[:n] {
		offset += uint64(e.compressedSize)
	}
	return offset, uint64(i.entries[n].compressedSize), nil
}

// ReadBlock reads and decompresses block n, using dict if the capture was
// written with a trained dictionary (nil otherwise). It reads only that block's
// bytes: nothing before it is touched, which is the property the per-block
// layout exists to provide.
//
// EVERY NUMBER IT ACTS ON COMES FROM THE FILE, so every one is checked before
// it is spent (F4, measured 2026-09-05). What this used to do:
//
//	compressedSize=0xFFFFFFFF: ReadBlock(1) -> "reading block 1 at 52: EOF"
//	                           AFTER HeapAlloc 4097 MiB
//	a 426 KB file whose block 1 is 2 GiB of zeros:
//	                           len=2147483648 err=nil, HeapAlloc 3699 MiB
//
// The first line is the defect exactly: the error was correct and it arrived
// after the allocation. A reader that refuses a hostile file only once it has
// allocated what the file asked for has not refused it (SEC-002). The second is
// SEC-001, which the sequential reader has enforced since it shipped and this
// path could not even be told about.
//
// Three checks, in the order the budget is actually spent:
//
//  1. The block must FIT IN THE FILE. The file's size is the one fact about a
//     capture that does not come out of the capture, so it bounds the buffer
//     before a byte is allocated.
//  2. The declared decompressed size must fit the caller's budget, checked
//     before decoding rather than after.
//  3. The FRAME HEADER is parsed before the frame is decoded. zstd states a
//     frame's content size in its own header, so a block whose header
//     disagrees with the seek table is refused having allocated nothing — this
//     is what catches the 2 GiB-of-zeros case, and it costs a header parse.
//     The decoder is then capped, and the decoded length must EQUAL the
//     declared size. Resolving a disagreement in the file's favour is how a
//     426 KB file becomes a 2 GB allocation.
//
// THE CAP IS NOT THE DECLARED SIZE, and the first attempt at this made it so:
// WithDecoderMaxMemory(182) rejected a legitimate 182-byte block with "window
// size exceeded", because klauspost checks the frame's WINDOW against that
// budget and the encoder's window is megabytes regardless of how little the
// block holds. The cap is therefore max(declared, the frame's own declared
// window) — still a bound the file cannot inflate past what it already
// committed to in its header, and check 3's equality is what makes a lie about
// that header useless.
func (i *BlockIndex) ReadBlock(n int, dict []byte) ([]byte, error) {
	offset, length, err := i.BlockRange(n)
	if err != nil {
		return nil, err
	}

	// 1. SEC-002: verify against the file's own size BEFORE allocating.
	if i.size < 0 || offset > uint64(i.size) || length > uint64(i.size)-offset {
		return nil, fmt.Errorf("tape: block %d claims %d bytes at offset %d in a %d-byte file: %w",
			n, length, offset, i.size, ErrSeekTableCorrupt)
	}

	declared := uint64(i.entries[n].decompressedSize)

	// 2. SEC-001: the budget is checked against what the table DECLARES,
	// before anything is decoded. A block that lies about this is caught by
	// check 3.
	if i.limits.MaxDecodedBytes > 0 && declared > uint64(i.limits.MaxDecodedBytes) {
		return nil, fmt.Errorf("tape: block %d declares %d decoded bytes: %w",
			n, declared, ErrMaxDecodedBytes)
	}

	file, err := os.Open(i.path) //nolint:gosec // path came from OpenBlockIndex
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck // read-only

	raw := make([]byte, length)
	if _, readErr := file.ReadAt(raw, int64(offset)); readErr != nil { //nolint:gosec // bounded by i.size above
		return nil, fmt.Errorf("tape: reading block %d at %d: %w", n, offset, readErr)
	}

	// 3. The frame states its own content size. Compare it with the index
	// BEFORE decoding: this is the check that refuses a bomb having allocated
	// nothing but the compressed bytes already read.
	var hdr zstd.Header
	if hdrErr := hdr.Decode(raw); hdrErr != nil {
		return nil, fmt.Errorf("tape: block %d frame header: %w", n, hdrErr)
	}
	if hdr.HasFCS && hdr.FrameContentSize != declared {
		return nil, fmt.Errorf("tape: block %d's frame declares %d bytes, seek table declares %d: %w",
			n, hdr.FrameContentSize, declared, ErrSeekTableCorrupt)
	}
	// 3c. The window is refused HERE, by our number, before a decoder exists.
	// Passing WithDecoderMaxWindow below would also refuse it, but klauspost's
	// error would arrive wrapped as "decompressing block N: window size
	// exceeded" — which reads as damage. This frame is not damaged; it is valid
	// zstd this reader declines. Checking it here also means the refusal costs
	// the header parse and nothing more.
	if hdr.WindowSize > maxWriterWindow {
		return nil, fmt.Errorf("tape: block %d's frame declares a %d-byte window, larger than this writer's %d (%s): %w",
			n, hdr.WindowSize, uint64(maxWriterWindow), describeOversizeWindow(hdr.WindowSize, declared),
			ErrWindowTooLarge)
	}

	// TWO CEILINGS, AND THEY NEED DIFFERENT NUMBERS. WithDecoderMaxMemory is
	// one knob doing two jobs — its doc reads "a maximum decoded size for
	// in-memory non-streaming operations OR maximum window size for streaming
	// operations" (zstd/decoder_options.go:85-88) — and newFrameDec then clamps
	// the window ceiling down to it (zstd/framedec.go:51-53). That is why the
	// earlier attempt here, WithDecoderMaxMemory(182), rejected a legitimate
	// 182-byte block with "window size exceeded": the one call had also capped
	// the window at 182 bytes. The previous fix for that was to widen the cap
	// with hdr.WindowSize, which fixed the false positive by handing the
	// attacker the ceiling.
	//
	// So set the two separately. WithDecoderMaxWindow
	// (zstd/decoder_options.go:143-160) is the window knob on its own, and it
	// gets maxWriterWindow — a number from our writer, which the file cannot
	// influence. It is not redundant with check 3c above: 3c parses ONE frame
	// header, and DecodeAll decodes every frame concatenated in the block, so
	// this is what bounds a window hidden behind an innocent first frame.
	//
	// WithDecoderMaxMemory then gets the DECODED ceiling. It must not sit below
	// maxWriterWindow or the framedec clamp reintroduces the false positive, and
	// it must admit declared, which check 2 has already bounded against the
	// caller's own MaxDecodedBytes. max(declared, maxWriterWindow) is both, and
	// both terms are ours: the seek table's declared size, already budget-checked,
	// and our writer's window. A declared zero cannot reach the option's
	// "must be at least 1" rejection because maxWriterWindow is the floor.
	dopts := []zstd.DOption{
		zstd.WithDecoderMaxWindow(maxWriterWindow),
		zstd.WithDecoderMaxMemory(max(declared, maxWriterWindow)),
	}
	if len(dict) > 0 {
		dopts = append(dopts, zstd.WithDecoderDicts(dict))
	}
	dec, err := zstd.NewReader(nil, dopts...)
	if err != nil {
		return nil, fmt.Errorf("tape: block %d decoder: %w", n, err)
	}
	defer dec.Close()

	out, err := dec.DecodeAll(raw, nil)
	if err != nil {
		// NAME THE DICTIONARY THE FRAME ASKED FOR. hdr.DictionaryID was decoded
		// above from the frame header (RFC 8878 §3.1.1.1.3) and costs nothing to
		// report; zstd's own errors say "dictionary" without saying WHICH, and an
		// operator holding several cannot act on that.
		//
		// WHAT THIS DOES AND DOES NOT DIAGNOSE, because the distinction is the
		// whole reason it is worded this way:
		//
		//   MISSING dictionary  — helped. The id says what to go and find.
		//   WRONG dictionary,
		//     DIFFERENT id      — helped. The ids visibly disagree.
		//   WRONG dictionary,
		//     SAME id (F3)      — NOT helped, and cannot be. Two dictionaries
		//                         trained at the same id are indistinguishable
		//                         here by construction; the number printed is
		//                         identical on both sides. It surfaces as a
		//                         checksum failure, and this line will name an id
		//                         that is not the problem.
		//
		// So this deliberately does NOT say anything like "content hash
		// disagrees". There is no stored dictionary hash to compare against —
		// storing one was rejected as duplicating bytes zstd already carries — and
		// describing a check that does not exist is the defect this codebase keeps
		// finding in its own comments. If F3's same-id collision is ever to be
		// detectable, it needs a content-derived id or a recorded hash; naming the
		// id here is not that and must not be mistaken for it.
		if hdr.DictionaryID != 0 {
			return nil, fmt.Errorf("tape: decompressing block %d (frame declares dictionary id %d): %w",
				n, hdr.DictionaryID, err)
		}
		return nil, fmt.Errorf("tape: decompressing block %d: %w", n, err)
	}
	if uint64(len(out)) != declared {
		return nil, fmt.Errorf("tape: block %d decoded to %d bytes, seek table declares %d: %w",
			n, len(out), declared, ErrSeekTableCorrupt)
	}
	return out, nil
}

// Footer reads the CaptureFooter out of the capture's final block.
//
// This is the reason the footer gets a block of its own. The footer carries the
// keyframe index, so a seeking reader needs it BEFORE it can resolve a frame to
// a byte range — and reading it through the sequential Reader means walking
// every frame in the capture, which costs exactly what seeking was supposed to
// save. Here it costs one block.
func (i *BlockIndex) Footer(dict []byte) (*capturepb.CaptureFooter, error) {
	if len(i.entries) == 0 {
		return nil, ErrNoSeekTable
	}
	block, err := i.ReadBlock(len(i.entries)-1, dict)
	if err != nil {
		return nil, err
	}

	r := &Reader{reader: bytes.NewReader(block), limits: Limits{}}
	// F9, and the seeking path needs it at least as much as the sequential one:
	// a future writer that puts a new envelope kind in the footer's block would
	// otherwise make the whole capture unseekable to this reader. The loop is
	// bounded by the block, which ReadBlock has already sized and verified.
	for {
		env, envErr := r.readEnvelope()
		if envErr != nil {
			return nil, fmt.Errorf("tape: reading footer envelope: %w", envErr)
		}
		if footer := env.GetFooter(); footer != nil {
			return footer, nil
		}
		if isUnknownEnvelope(env) {
			i.skippedEnvelopes++
			continue
		}
		return nil, fmt.Errorf("tape: final block does not hold a footer: %w", ErrUnexpectedEnvelope)
	}
}

// BlockForFrame returns the index of the block holding frameIndex, resolved
// against the footer's keyframe index. It is the lookup a byte-range server
// performs: frame number in, servable block out.
//
// A frame before the first keyframe cannot be resolved, because no block is
// known to contain it.
func (i *BlockIndex) BlockForFrame(footer *capturepb.CaptureFooter, frameIndex uint32) (int, error) {
	var offset uint64
	found := false
	for _, kf := range footer.GetKeyframeIndex() {
		if kf.GetFrameIndex() > frameIndex {
			break
		}
		offset, found = kf.GetByteOffset(), true
	}
	if !found {
		return 0, fmt.Errorf("tape: frame %d precedes every keyframe in the index: %w",
			frameIndex, ErrSeekTableCorrupt)
	}
	block, _, err := blockAt(i.entries, offset)
	if err != nil {
		return 0, err
	}
	return block, nil
}
