package conversion

import (
	"archive/zip"
	"bufio"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/echotools/tape/v4/pkg/codec"
)

// TestReconstructFileWholeStreamLayout pins the layout nevr-spectator writes:
// one zstd frame over varint-delimited envelopes, opened by
// codec.WithWholeStreamCompression and carrying no seek table. The line count
// is taken from the raw zip, not from frames read back through the same
// reader, so a frame the reader drops cannot pass as a match.
func TestReconstructFileWholeStreamLayout(t *testing.T) {
	frames := loadCorpusProtos(t)
	if len(frames) == 0 {
		t.Fatal("corpus sample missing or empty: testdata/sample.echoreplay")
	}
	// Two keyframe intervals (codec.DefaultKeyframeInterval is 100), so the
	// footer's keyframe index is exercised.
	const n = 250
	if len(frames) < n {
		t.Fatalf("corpus has %d frames, need %d", len(frames), n)
	}
	frames = frames[:n]

	dir := t.TempDir()
	tape := filepath.Join(dir, "whole.tape")
	w, err := codec.NewWriterWithOptions(tape, codec.WithWholeStreamCompression())
	if err != nil {
		t.Fatalf("NewWriterWithOptions: %v", err)
	}
	if err := w.WriteHeader(shippedHeader()); err != nil {
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

	if _, err := codec.OpenBlockIndex(tape); !errors.Is(err, codec.ErrNoSeekTable) {
		t.Fatalf("OpenBlockIndex: %v, want ErrNoSeekTable (the whole-stream layout)", err)
	}

	recon := filepath.Join(dir, "recon.echoreplay")
	written, err := ReconstructFile(tape, recon)
	if err != nil {
		t.Fatalf("ReconstructFile: %v", err)
	}
	if written != n {
		t.Fatalf("ReconstructFile wrote %d frames, source has %d", written, n)
	}

	zr, err := zip.OpenReader(recon)
	if err != nil {
		t.Fatalf("open echoreplay zip: %v", err)
	}
	defer zr.Close() //nolint:errcheck // read-only test handle
	if len(zr.File) != 1 {
		t.Fatalf("echoreplay zip has %d entries, want 1", len(zr.File))
	}
	rc, err := zr.File[0].Open()
	if err != nil {
		t.Fatalf("open zip entry: %v", err)
	}
	defer rc.Close() //nolint:errcheck // read-only test handle

	scanner := bufio.NewScanner(rc)
	scanner.Buffer(make([]byte, 0, 1<<20), 64<<20)
	lines := 0
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.Contains(line, "\t") {
			t.Fatalf("line %d has no tab separator: %.60q", lines+1, line)
		}
		lines++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan echoreplay entry: %v", err)
	}
	if lines != n {
		t.Fatalf("echoreplay entry has %d lines, source has %d frames", lines, n)
	}
}
