package codec

import (
	"bytes"
	"os"
	"testing"
	"time"

	enginev1 "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/engine/v1"
	telemetry "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// richSampleFrame returns a realistic frame with session data, disc, teams, and
// player bones populated. Shared across codec benchmarks that need non-trivial
// payloads.
func richSampleFrame() *telemetry.LobbySessionStateFrame {
	return &telemetry.LobbySessionStateFrame{
		Timestamp: timestamppb.New(time.Date(2025, 3, 15, 14, 30, 0, 0, time.UTC)),
		Session: &enginev1.SessionResponse{
			SessionId:  "A1B2C3D4-E5F6-7890-ABCD-EF1234567890",
			GameStatus: "playing",
			GameClock:  180.5,
			BluePoints: 3, OrangePoints: 2,
			MapName:   "mpl_arena_a",
			MatchType: "Echo_Arena",
			Disc: &enginev1.Disc{
				Position: []float64{0, 5.1, 12.3},
				Forward:  []float64{0, 0, 1},
				Left:     []float64{1, 0, 0},
				Up:       []float64{0, 1, 0},
				Velocity: []float64{8.2, -1.3, 15.7},
			},
			LastThrow: &enginev1.LastThrowInfo{
				ArmSpeed:   12.5,
				TotalSpeed: 18.3,
				RotPerSec:  4.2,
			},
			Teams: []*enginev1.Team{
				{
					TeamName: "BLUE TEAM",
					Players: []*enginev1.TeamMember{
						{
							SlotNumber: 0, DisplayName: "Player1",
							AccountNumber: 4355631379520676917, Level: 50,
							Head: &enginev1.BodyPart{
								Position: []float64{1, 2, 3},
								Forward:  []float64{0, 0, 1},
								Left:     []float64{1, 0, 0},
								Up:       []float64{0, 1, 0},
							},
							Velocity:      []float64{0.5, 0.1, -0.3},
							HasPossession: true,
							Stats:         &enginev1.PlayerStats{Goals: 2, Stuns: 5, Assists: 1},
						},
						{
							SlotNumber: 1, DisplayName: "Player2",
							AccountNumber: 1234567890123456789, Level: 30,
							Head: &enginev1.BodyPart{
								Position: []float64{-3, 1.5, 8},
								Forward:  []float64{0, 0, 1},
								Left:     []float64{1, 0, 0},
								Up:       []float64{0, 1, 0},
							},
							Velocity: []float64{1.2, 0, 0.4},
							Stats:    &enginev1.PlayerStats{Goals: 1, Saves: 3},
						},
					},
				},
				{
					TeamName: "ORANGE TEAM",
					Players: []*enginev1.TeamMember{
						{
							SlotNumber: 2, DisplayName: "Player3",
							AccountNumber: 9876543210987654321, Level: 45,
							Head: &enginev1.BodyPart{
								Position: []float64{5, 2, -4},
								Forward:  []float64{0, 0, -1},
								Left:     []float64{-1, 0, 0},
								Up:       []float64{0, 1, 0},
							},
							Velocity: []float64{-0.8, 0.2, 1.1},
							Stats:    &enginev1.PlayerStats{Goals: 2, Stuns: 8, Steals: 2},
						},
					},
				},
			},
		},
		PlayerBones: &enginev1.PlayerBonesResponse{
			UserBones: []*enginev1.UserBones{
				{
					PlayerIndex: 0,
					BoneT:       []float32{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12},
					BoneO:       []float32{0, 0, 0, 1, 0, 0, 0, 1, 0, 0, 0, 1},
				},
			},
		},
	}
}

// BenchmarkEchoReplayReadFrame measures per-frame read throughput using ReadFrame
// (allocating path) from a real .echoreplay file when available.
func BenchmarkEchoReplayReadFrame(b *testing.B) {
	const realFile = "../../testdata/sample.echoreplay"
	inputFile := realFile

	if _, err := os.Stat(realFile); os.IsNotExist(err) {
		if testing.Short() {
			b.Skip("real echoreplay not available and -short set")
		}
		// Fall back to a synthetic file.
		inputFile = createSyntheticEchoreplay(b, 1000)
	}

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		reader, err := NewEchoReplayReader(inputFile)
		if err != nil {
			b.Fatalf("open reader: %v", err)
		}
		n := 0
		for {
			_, err := reader.ReadFrame()
			if err != nil {
				break
			}
			n++
		}
		reader.Close()
		b.ReportMetric(float64(n), "frames/op")
	}
}

// BenchmarkEchoReplayWriteFrame measures per-frame write throughput.
func BenchmarkEchoReplayWriteFrame(b *testing.B) {
	frame := richSampleFrame()

	b.ReportAllocs()
	b.ResetTimer()

	for b.Loop() {
		buf := &bytes.Buffer{}
		e := &EchoReplay{
			frameBuffer:  buf,
			scratchBuf:   make([]byte, 0, 1024),
			timestampBuf: [len(EchoReplayTimeFormat)]byte{},
		}
		e.WriteReplayFrame(buf, frame)
	}
}

// BenchmarkReadFrameTo measures the zero-alloc ReadFrameTo path.
func BenchmarkReadFrameTo(b *testing.B) {
	// Create a temporary echoreplay file with test data
	tmpFile := createSyntheticEchoreplay(b, 1000)

	// Create reader
	reader, err := NewEchoReplayReader(tmpFile)
	if err != nil {
		b.Fatalf("Failed to create reader: %v", err)
	}
	defer reader.Close()

	// Preallocate frame for reuse
	frame := &telemetry.LobbySessionStateFrame{
		Session:     &enginev1.SessionResponse{},
		PlayerBones: &enginev1.PlayerBonesResponse{},
		Timestamp:   &timestamppb.Timestamp{},
	}

	b.ReportAllocs()

	for i := 0; b.Loop(); i++ {
		// Reset reader position for each iteration
		if i%1000 == 0 && i > 0 {
			reader.Close()
			reader, err = NewEchoReplayReader(tmpFile)
			if err != nil {
				b.Fatalf("Failed to recreate reader: %v", err)
			}
		}

		ok, err := reader.ReadFrameTo(frame)
		if err != nil || !ok {
			// Recreate reader when EOF is reached
			reader.Close()
			reader, err = NewEchoReplayReader(tmpFile)
			if err != nil {
				b.Fatalf("Failed to recreate reader: %v", err)
			}
			ok, err = reader.ReadFrameTo(frame)
			if err != nil || !ok {
				b.Fatalf("Failed to read frame: %v", err)
			}
		}
	}
}

// BenchmarkNewEchoReplayReader measures reader creation overhead.
func BenchmarkNewEchoReplayReader(b *testing.B) {
	tmpFile := createSyntheticEchoreplay(b, 1000)

	b.ReportAllocs()

	for b.Loop() {
		reader, err := NewEchoReplayReader(tmpFile)
		if err != nil {
			b.Fatalf("Failed to create reader: %v", err)
		}
		reader.Close()
	}
}

func BenchmarkTimeParse(b *testing.B) {
	ts := "2023/11/27 15:04:05.123"
	format := "2006/01/02 15:04:05.000"

	b.ResetTimer()
	for b.Loop() {
		_, err := time.Parse(format, ts)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkFastParseTimestamp(b *testing.B) {
	tsBytes := []byte("2023/11/27 15:04:05.123")

	b.ResetTimer()
	for b.Loop() {
		_, err := fastParseTimestamp(tsBytes)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkTimeFormat(b *testing.B) {
	t := time.Now()
	format := "2006/01/02 15:04:05.000"

	b.ResetTimer()
	for b.Loop() {
		_ = t.Format(format)
	}
}

func BenchmarkFastFormatTimestamp(b *testing.B) {
	t := time.Now()
	buf := make([]byte, 23)

	b.ResetTimer()
	for b.Loop() {
		fastFormatTimestamp(buf, t)
	}
}

// createSyntheticEchoreplay builds a temporary .echoreplay with N rich frames.
func createSyntheticEchoreplay(tb testing.TB, n int) string {
	tb.Helper()
	tmpFile := tb.TempDir() + "/test.echoreplay"

	writer, err := NewEchoReplayWriter(tmpFile)
	if err != nil {
		tb.Fatalf("create writer: %v", err)
	}

	frame := richSampleFrame()
	for range n {
		if err := writer.WriteFrame(frame); err != nil {
			tb.Fatalf("write frame: %v", err)
		}
	}
	if err := writer.Close(); err != nil {
		tb.Fatalf("close writer: %v", err)
	}
	return tmpFile
}
