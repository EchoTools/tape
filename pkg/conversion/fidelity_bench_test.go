package conversion

import (
	"os"
	"strconv"
	"testing"

	telemetryv1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	"github.com/echotools/tape/v4/pkg/codec"
)

// BenchmarkFidelityCompareFrame measures the per-frame cost of the reflective
// comparison on REAL frames, because that is the cost that decides whether a
// 100k-frame recording can be verified exhaustively instead of sampled.
//
// Run: go test ./pkg/conversion/ -run XXX -bench FidelityCompareFrame -benchmem
func BenchmarkFidelityCompareFrame(b *testing.B) {
	src := "../../testdata/sample.echoreplay"
	if _, err := os.Stat(src); err != nil {
		b.Skipf("no sample: %v", err)
	}
	r, err := codec.NewEchoReplayReader(src)
	if err != nil {
		b.Fatalf("open: %v", err)
	}
	frames, err := r.ReadFrames()
	_ = r.Close()
	if err != nil {
		b.Fatalf("read: %v", err)
	}
	if len(frames) == 0 {
		b.Fatal("no frames")
	}

	sessionS, bonesS, tsS, err := echoSchemas()
	if err != nil {
		b.Fatalf("schemas: %v", err)
	}
	// Compare each frame against itself: the identical case is the one the walk
	// pays in full on every field without short-circuiting into diff recording,
	// so it is the honest per-frame floor.
	tsC := tsS.NewComparator()
	sessionC := sessionS.NewComparator()
	bonesC := bonesS.NewComparator()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		f := frames[i%len(frames)]
		label := "frame " + strconv.Itoa(i)
		tsC.Compare(f.GetTimestamp(), f.GetTimestamp(), label)
		sessionC.Compare(f.GetSession(), f.GetSession(), label)
		bonesC.Compare(f.GetPlayerBones(), f.GetPlayerBones(), label)
	}
	b.StopTimer()
	if d := sessionC.Diffs(); len(d) != 0 {
		b.Fatalf("a frame compared against itself reported %d difference(s): %v", len(d), d)
	}
	var _ *telemetryv1.LobbySessionStateFrame = frames[0]
}
