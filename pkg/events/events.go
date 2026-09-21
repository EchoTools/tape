package events

import (
	"context"
	"sync"
	"sync/atomic"

	telemetry "buf.build/gen/go/echotools/nevr-api/protocolbuffers/go/telemetry/v1"
)

// Detector defines the behavior required to process frames and emit lobby events.
type Detector interface {
	// ProcessFrame processes a frame for event detection
	ProcessFrame(*telemetry.LobbySessionStateFrame)
	// EventsChan returns a channel to receive detected events
	EventsChan() <-chan []*telemetry.LobbySessionEvent
	// Reset clears the detector state
	Reset()
	// Stop gracefully shuts down the detector
	Stop()
}

// DefaultFrameBufferCapacity is the number of frames an AsyncDetector buffers
// before ProcessFrame starts dropping them and counting the drops. Override
// with WithFrameBufferSize.
const DefaultFrameBufferCapacity = 10

// Option configures the AsyncDetector
type Option func(*AsyncDetector)

// WithInputChannelSize sets the size of the input channel (minimum 1).
func WithInputChannelSize(size int) Option {
	return func(ed *AsyncDetector) {
		if size < 1 {
			size = 1
		}
		ed.inputChan = make(chan *telemetry.LobbySessionStateFrame, size)
	}
}

// WithEventsChannelSize sets the size of the events channel (minimum 1).
func WithEventsChannelSize(size int) Option {
	return func(ed *AsyncDetector) {
		if size < 1 {
			size = 1
		}
		ed.eventsChan = make(chan []*telemetry.LobbySessionEvent, size)
	}
}

// WithFrameBufferSize sets the size of the frame buffer (minimum 1).
func WithFrameBufferSize(size int) Option {
	return func(ed *AsyncDetector) {
		if size < 1 {
			size = 1
		}
		ed.frameBuffer = make([]*telemetry.LobbySessionStateFrame, size)
	}
}

// WithSensors adds sensors to the detector
func WithSensors(sensors ...Sensor) Option {
	return func(ed *AsyncDetector) {
		ed.sensors = append(ed.sensors, sensors...)
	}
}

// WithSynchronousProcessing enables synchronous processing of frames
func WithSynchronousProcessing() Option {
	return func(ed *AsyncDetector) {
		ed.synchronous = true
	}
}

// AsyncDetector detects events from frame streams
type AsyncDetector struct {
	// Ring buffer for frames
	frameBuffer []*telemetry.LobbySessionStateFrame
	writeIndex  int // Current write position
	frameCount  int // Number of frames currently in buffer

	sensors []Sensor

	// Channel-based processing
	inputChan  chan *telemetry.LobbySessionStateFrame
	eventsChan chan []*telemetry.LobbySessionEvent
	resetChan  chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	startOnce  sync.Once
	stopOnce   sync.Once
	stopped    atomic.Bool

	// Reusable buffer for events to reduce allocations
	eventBuffer []*telemetry.LobbySessionEvent

	synchronous bool

	// syncMu serializes the close(eventsChan) in Stop() with sends in
	// processFrameSync so a concurrent ProcessFrame cannot panic on a
	// channel that was closed between the ctx.Done() check and the send.
	syncMu sync.Mutex

	// Drop counters for monitoring channel back-pressure.
	droppedFrames atomic.Uint64
	droppedEvents atomic.Uint64
}

// DroppedFrames returns the number of frames dropped due to a full input channel.
func (ed *AsyncDetector) DroppedFrames() uint64 {
	return ed.droppedFrames.Load()
}

// DroppedEvents returns the number of event batches dropped due to a full events channel.
func (ed *AsyncDetector) DroppedEvents() uint64 {
	return ed.droppedEvents.Load()
}

var _ Detector = (*AsyncDetector)(nil)

// New creates a new event detector. In async mode it starts the background
// processing goroutine immediately, so a caller never needs to call Start.
func New(opts ...Option) *AsyncDetector {
	ctx, cancel := context.WithCancel(context.Background())
	ed := &AsyncDetector{
		inputChan:   make(chan *telemetry.LobbySessionStateFrame, 100),
		eventsChan:  make(chan []*telemetry.LobbySessionEvent, 10),
		resetChan:   make(chan struct{}),
		ctx:         ctx,
		cancel:      cancel,
		frameBuffer: make([]*telemetry.LobbySessionStateFrame, DefaultFrameBufferCapacity),
		eventBuffer: make([]*telemetry.LobbySessionEvent, 0, 10),
	}

	for _, opt := range opts {
		opt(ed)
	}

	if !ed.synchronous {
		ed.Start()
	}
	return ed
}

// Start launches the background processing goroutine. It is idempotent: the
// loop is started at most once, so calling Start more than once cannot spawn a
// second worker racing on the shared detector state. In synchronous mode there
// is no background loop, so Start is a no-op. A detector that has been Stop()ed
// cannot be restarted; Start after Stop has no effect.
func (ed *AsyncDetector) Start() {
	ed.startOnce.Do(func() {
		if ed.synchronous || ed.stopped.Load() {
			return
		}
		ed.wg.Add(1)
		go ed.processLoop()
	})
}

// Stop gracefully shuts down the event detector.
//
// In synchronous mode Stop first cancels the context so any in-flight
// ProcessFrame sees ctx.Done(), then acquires syncMu to wait for that
// ProcessFrame to finish before closing the events channel. This prevents
// a send-on-closed-channel panic when ProcessFrame and Stop are called from
// different goroutines.
func (ed *AsyncDetector) Stop() {
	ed.stopOnce.Do(func() {
		ed.stopped.Store(true)
		ed.cancel()
		if !ed.synchronous {
			ed.wg.Wait()
		} else {
			// In sync mode there is no background goroutine, but a
			// concurrent ProcessFrame may still be sending. Wait for it
			// to finish, then close the channel.
			ed.syncMu.Lock()
			defer ed.syncMu.Unlock()
		}
		close(ed.eventsChan)
	})
}

// Reset clears the event detector state
func (ed *AsyncDetector) Reset() {
	if ed.synchronous {
		ed.resetSync()
		return
	}
	select {
	case ed.resetChan <- struct{}{}:
	case <-ed.ctx.Done():
	}
}

func (ed *AsyncDetector) resetSync() {
	ed.writeIndex = 0
	ed.frameCount = 0
	for i := range ed.frameBuffer {
		ed.frameBuffer[i] = nil
	}
	for _, s := range ed.sensors {
		if r, ok := s.(Resettable); ok {
			r.Reset()
		}
	}
}

// ProcessFrame writes a frame to the processing channel (non-blocking)
func (ed *AsyncDetector) ProcessFrame(frame *telemetry.LobbySessionStateFrame) {
	if ed.synchronous {
		ed.processFrameSync(frame)
		return
	}

	select {
	case ed.inputChan <- frame:
		// Frame sent successfully
	case <-ed.ctx.Done():
		// Detector is stopping, ignore frame
	default:
		// Channel full, drop frame
		ed.droppedFrames.Add(1)
	}
}

func (ed *AsyncDetector) processFrameSync(frame *telemetry.LobbySessionStateFrame) {
	// Add frame to buffer
	ed.addFrameToBuffer(frame)

	// Detect events using the detection algorithm
	ed.eventBuffer = ed.eventBuffer[:0]
	ed.eventBuffer = ed.detectEvents(ed.eventBuffer)

	// Send events if any were detected
	if len(ed.eventBuffer) > 0 {
		// Copy events to avoid race conditions with the reused buffer
		eventsToSend := make([]*telemetry.LobbySessionEvent, len(ed.eventBuffer))
		copy(eventsToSend, ed.eventBuffer)

		// Serialize with Stop() so close(eventsChan) cannot interleave
		// with the send. stopOnce guarantees cancel() fires before
		// Stop acquires syncMu, so ctx.Done() is already ready when
		// Stop holds the lock.
		ed.syncMu.Lock()
		defer ed.syncMu.Unlock()

		select {
		case <-ed.ctx.Done():
			// Detector is stopping; return without sending.
			return
		default:
		}

		// In synchronous mode, use non-blocking send to avoid blocking ProcessFrame.
		// This ensures ProcessFrame completes immediately in the caller's goroutine.
		// Events are dropped if the channel is full, which is acceptable since
		// synchronous mode prioritizes immediate processing over guaranteed delivery.
		select {
		case ed.eventsChan <- eventsToSend:
			// Events sent successfully
		default:
			// Channel is full, drop events rather than blocking.
			// This maintains the synchronous processing guarantee.
			ed.droppedEvents.Add(1)
		}
	}
}

// EventsChan returns the channel for receiving detected events
func (ed *AsyncDetector) EventsChan() <-chan []*telemetry.LobbySessionEvent {
	return ed.eventsChan
}

// processLoop is the background goroutine that processes frames
func (ed *AsyncDetector) processLoop() {
	defer ed.wg.Done()

	for {
		select {
		case <-ed.resetChan:
			ed.writeIndex = 0
			ed.frameCount = 0
			for i := range ed.frameBuffer {
				ed.frameBuffer[i] = nil
			}
			for _, s := range ed.sensors {
				if r, ok := s.(Resettable); ok {
					r.Reset()
				}
			}

		case frame := <-ed.inputChan:
			// Add frame to buffer
			ed.addFrameToBuffer(frame)

			// Detect events using the detection algorithm
			ed.eventBuffer = ed.eventBuffer[:0]
			ed.eventBuffer = ed.detectEvents(ed.eventBuffer)

			// Send events if any were detected
			if len(ed.eventBuffer) > 0 {
				// Copy events to avoid race conditions with the reused buffer
				eventsToSend := make([]*telemetry.LobbySessionEvent, len(ed.eventBuffer))
				copy(eventsToSend, ed.eventBuffer)

				select {
				case ed.eventsChan <- eventsToSend:
					// Events sent successfully
				case <-ed.ctx.Done():
					// Context cancelled, drain inputChan and exit
					ed.drainInputChan()
					return
				default:
					// Channel full — drop events rather than blocking the frame
					// processing pipeline. Matches the sync path's behavior.
					ed.droppedEvents.Add(1)
				}
			}

		case <-ed.ctx.Done():
			// Context cancelled, drain inputChan before exiting
			ed.drainInputChan()
			return
		}
	}
}

// drainInputChan drains any remaining frames from inputChan to prevent resource leaks
func (ed *AsyncDetector) drainInputChan() {
	for {
		select {
		case <-ed.inputChan:
			// Discard frame
		default:
			// Channel is empty
			return
		}
	}
}

// addFrameToBuffer adds a frame to the buffer
func (ed *AsyncDetector) addFrameToBuffer(frame *telemetry.LobbySessionStateFrame) {
	// Write to current position
	ed.frameBuffer[ed.writeIndex] = frame

	// Advance write index (wrap around)
	ed.writeIndex = (ed.writeIndex + 1) % len(ed.frameBuffer)

	// Track frame count (max is buffer size)
	if ed.frameCount < len(ed.frameBuffer) {
		ed.frameCount++
	}
}

// lastFrame returns the most recently added frame
func (ed *AsyncDetector) lastFrame() *telemetry.LobbySessionStateFrame {
	if ed.frameCount == 0 {
		return nil
	}
	idx := ed.lastFrameIndex()
	return ed.frameBuffer[idx]
}

// lastFrameIndex returns the index of the most recently written frame
func (ed *AsyncDetector) lastFrameIndex() int {
	return (ed.writeIndex - 1 + len(ed.frameBuffer)) % len(ed.frameBuffer)
}

// detectEvents analyzes frames in the ring buffer and returns detected events
func (ed *AsyncDetector) detectEvents(dst []*telemetry.LobbySessionEvent) []*telemetry.LobbySessionEvent {
	// Use the newest frame available in the buffer
	if ed.frameCount == 0 {
		return dst
	}

	for _, s := range ed.sensors {
		// First call processes the frame; subsequent calls drain any
		// pending events the sensor queued (e.g. multiple joins in one
		// frame). Sensors check their pending queue before inspecting the
		// frame, so passing nil on drain calls is safe and avoids
		// re-processing.
		frame := ed.lastFrame()
		for {
			event := s.AddFrame(frame)
			if event == nil {
				break
			}
			dst = append(dst, event)
			frame = nil
		}
	}

	return dst
}
