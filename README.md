# tape

High-performance telemetry codec library and CLI for Echo VR session data.

Module: `github.com/echotools/tape/v4`

## Installation

```bash
go get github.com/echotools/tape/v4
```

## tapedeck CLI

`tapedeck` is the command-line tool for working with tape files.

```bash
go install github.com/echotools/tape/v4/cmd/tapedeck@latest
```

### Commands

**convert** — Batch-convert `.echoreplay` files to `.tape` format.
Supports parallel workers, recursive directory scanning, glob patterns, dry-run mode, and progress bars.

```bash
tapedeck convert *.echoreplay          # convert files in place
tapedeck convert --recursive --output-dir out/ ./replays
tapedeck convert --workers 8 *.echoreplay
```

**export echoreplay** — Reconstruct a `.tape` as a `.echoreplay` file (one tab-separated `timestamp\tsessionJSON` line per frame) and print the frame count written.

```bash
tapedeck export echoreplay capture.tape capture.echoreplay
```

**show** — Display tape file contents (header, frames, footer).
Output formats: text, json, summary. Optional event filtering.

```bash
tapedeck show capture.tape
tapedeck show --format json capture.tape
tapedeck show --events capture.tape
```

**replay** — Serve a tape file over HTTP, emulating Echo VR API endpoints (`/session`, `/player_bones`) at the original capture rate.

```bash
tapedeck replay capture.tape
tapedeck replay --addr :6721 --rate 2.0 capture.tape
```

## Package Structure

```
pkg/
├── codec/       # File format readers/writers (.tape, .echoreplay)
├── conversion/  # Format conversion utilities
├── events/      # Event detection algorithms
└── processing/  # Frame processing pipeline
```

## Tape Formats

| Property | v2 (current) | v1 (legacy) |
|----------|-------------|-------------|
| Envelope | `telemetry.v2.Envelope` oneof | Raw protobuf messages |
| Footer | Frame count, duration, keyframe/event indexes | None |
| Spatial | `spatial.v1` float32 (~73.5% smaller) | `repeated double` (64-bit) |
| Extension | `.tape` | `.tape` / `.nevrcap` |

**EchoReplay** (`.echoreplay`) — ZIP-compressed JSON, legacy format from the game engine.

## Building

```bash
go build ./...                        # build all
go test -race ./...                   # tests with race detector
go test -bench=. -benchmem ./...      # benchmarks
go vet ./...                          # static analysis
```

## Related

- [nevr-proto](https://github.com/echotools/nevr-proto) — Protobuf definitions (BSR: `buf.build/echotools/nevr-api`)
- [nevr-agent](https://github.com/echotools/nevr-agent) — Recording and streaming CLI

## License

See [LICENSE](LICENSE) file for details.
