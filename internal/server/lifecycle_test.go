//go:build linux

package server

import (
	"context"
	"encoding/binary"
	"io"
	"log/slog"
	"math"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/denelson1-dot/vox/internal/audio"
)

// TestHelperRecorder is not a test. It is the recorder the other tests run:
// the test binary re-executed, writing a loud tone to the WAV path it is given
// until it is interrupted, the way pw-record does.
func TestHelperRecorder(t *testing.T) {
	if os.Getenv("VOX_TEST_RECORDER") != "1" {
		t.Skip("helper process, run by the lifecycle tests")
	}
	path := os.Args[len(os.Args)-1]
	f, err := os.Create(path)
	if err != nil {
		os.Exit(1)
	}
	f.Write(make([]byte, 44)) // a WAV header; the reader skips it unread

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt)

	// Faster than real time, so the tests do not wait out real speech.
	second := make([]byte, audio.Bytes(1))
	for i := 0; i < len(second)/2; i++ {
		v := int16(8000 * math.Sin(float64(i)*2*math.Pi*440/audio.SampleRate))
		binary.LittleEndian.PutUint16(second[2*i:], uint16(v))
	}
	tick := time.NewTicker(50 * time.Millisecond)
	for {
		select {
		case <-stop:
			f.Close()
			os.Exit(0)
		case <-tick.C:
			f.Write(second)
		}
	}
}

var helperRecorder = audio.Recorder{
	Name: "test-recorder",
	Args: func(path string) []string {
		return []string{os.Args[0], "-test.run=^TestHelperRecorder$", "--", path}
	},
}

// fakeEngine answers "One.", "Two.", ... and takes as long as delay says for
// each call, unless its context is cancelled first.
type fakeEngine struct {
	delay     func(call int) time.Duration
	calls     atomic.Int32
	cancelled atomic.Int32
}

func (e *fakeEngine) Name() string  { return "fake" }
func (e *fakeEngine) Check() error  { return nil }
func (e *fakeEngine) Model() string { return "" }
func (e *fakeEngine) Transcribe(ctx context.Context, p string) (string, error) {
	return e.TranscribeWithContext(ctx, p, "")
}

var words = []string{"One.", "Two.", "Three.", "Four.", "Five.", "Six.", "Seven.", "Eight."}

func (e *fakeEngine) TranscribeWithContext(ctx context.Context, _, _ string) (string, error) {
	n := int(e.calls.Add(1))
	select {
	case <-time.After(e.delay(n)):
		return words[(n-1)%len(words)], nil
	case <-ctx.Done():
		e.cancelled.Add(1)
		return "", ctx.Err()
	}
}

// fakeKeyboard records what was typed and whether two callers ever typed at
// the same moment, which on a real keyboard interleaves their keystrokes.
type fakeKeyboard struct {
	mu         sync.Mutex
	typed      []string
	inFlight   atomic.Int32
	overlapped atomic.Bool
}

func (k *fakeKeyboard) Type(text string) error {
	if k.inFlight.Add(1) > 1 {
		k.overlapped.Store(true)
	}
	defer k.inFlight.Add(-1)
	time.Sleep(20 * time.Millisecond) // typing takes time; let overlaps show
	k.mu.Lock()
	k.typed = append(k.typed, text)
	k.mu.Unlock()
	return nil
}
func (k *fakeKeyboard) Key(string) error { return nil }
func (k *fakeKeyboard) Name() string     { return "fake" }
func (k *fakeKeyboard) Close() error     { return nil }

func (k *fakeKeyboard) text() string {
	k.mu.Lock()
	defer k.mu.Unlock()
	return strings.Join(k.typed, "")
}

func newTestServer(t *testing.T, e *fakeEngine, streaming bool) (*Server, *fakeKeyboard) {
	t.Helper()
	t.Setenv("VOX_TEST_RECORDER", "1")
	// Under -race a Go program sleeps a second on exit, and the recorder is
	// this test binary; without this every stop would look a second slow.
	t.Setenv("GORACE", "atexit_sleep_ms=0")
	old := pollInterval
	pollInterval = 20 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })

	kb := &fakeKeyboard{}
	s := &Server{
		engine: e, recorder: helperRecorder, injector: kb,
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		state: StateReady, subs: map[chan State]struct{}{},
		streamCfg: StreamConfig{Enabled: streaming, ChunkSeconds: 1, MaxSeconds: 2},
	}
	t.Cleanup(func() { s.Cancel() })
	return s, kb
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// Stopping while a chunk is still transcribing must not type the tail first.
// The tail is later speech: typing it before the chunk puts the end of what
// was said ahead of its middle, and typing both at once interleaves them.
func TestStopWaitsForTheChunkInFlight(t *testing.T) {
	e := &fakeEngine{delay: func(call int) time.Duration {
		if call == 1 {
			return 400 * time.Millisecond // the chunk: slow
		}
		return 0 // the tail: instant, so it would win any race
	}}
	s, kb := newTestServer(t, e, true)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a chunk to start transcribing", func() bool { return e.calls.Load() == 1 })
	if _, err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}

	if got, want := kb.text(), "One. Two. "; got != want {
		t.Errorf("typed %q, want %q", got, want)
	}
	if kb.overlapped.Load() {
		t.Error("two pieces of text were typed at the same time")
	}
	if st := s.State(); st != StateReady {
		t.Errorf("state after stop = %s, want ready", st)
	}
}

// Cancel must stop a chunk that is already transcribing, not let it be typed
// a moment later into whatever has focus.
func TestCancelAbandonsTheChunkInFlight(t *testing.T) {
	e := &fakeEngine{delay: func(int) time.Duration { return 3 * time.Second }}
	s, kb := newTestServer(t, e, true)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "a chunk to start transcribing", func() bool { return e.calls.Load() == 1 })

	start := time.Now()
	if err := s.Cancel(); err != nil {
		t.Fatal(err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("cancel took %v: it waited for the engine instead of stopping it", took)
	}
	if e.cancelled.Load() != 1 {
		t.Error("the transcription in flight was not cancelled")
	}
	time.Sleep(200 * time.Millisecond)
	if got := kb.text(); got != "" {
		t.Errorf("typed %q after cancel", got)
	}
	if st := s.State(); st != StateReady {
		t.Errorf("state after cancel = %s, want ready", st)
	}
}

// Cancel during the final transcription, after stop, must not type either.
func TestCancelDuringTheFinalTranscription(t *testing.T) {
	e := &fakeEngine{delay: func(int) time.Duration { return 3 * time.Second }}
	s, kb := newTestServer(t, e, false)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	stopped := make(chan struct{})
	go func() {
		s.Stop(context.Background())
		close(stopped)
	}()
	waitFor(t, "the final transcription", func() bool { return e.calls.Load() == 1 })
	s.Cancel()

	select {
	case <-stopped:
	case <-time.After(time.Second):
		t.Fatal("stop kept transcribing after cancel")
	}
	if got := kb.text(); got != "" {
		t.Errorf("typed %q after cancel", got)
	}
}

// A recording left running stops itself and types what it heard, exactly as
// though stop had been pressed.
func TestRecordingLimitStopsAndTypes(t *testing.T) {
	e := &fakeEngine{delay: func(int) time.Duration { return 0 }}
	s, kb := newTestServer(t, e, false)
	s.SetMaxRecord(200 * time.Millisecond)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	waitFor(t, "the limit to stop the recording", func() bool {
		return s.State() == StateReady && kb.text() != ""
	})
	if got, want := kb.text(), "One. "; got != want {
		t.Errorf("typed %q, want %q", got, want)
	}
}

// The limit belongs to the recording that set it. A timer from an earlier
// recording must not stop the current one.
func TestStaleLimitLeavesANewerRecordingAlone(t *testing.T) {
	e := &fakeEngine{delay: func(int) time.Duration { return 0 }}
	s, _ := newTestServer(t, e, false)

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.mu.Lock()
	first := s.session
	s.mu.Unlock()
	s.Cancel()

	if err := s.Start(); err != nil {
		t.Fatal(err)
	}
	s.limitReached(first, time.Minute)
	if st := s.State(); st != StateListening {
		t.Errorf("state = %s: an old recording's limit stopped the new one", st)
	}
}

// Two quick presses must not start two recorders.
func TestConcurrentStartsStartOnce(t *testing.T) {
	e := &fakeEngine{delay: func(int) time.Duration { return 0 }}
	s, _ := newTestServer(t, e, false)

	var ok atomic.Int32
	var wg sync.WaitGroup
	for range 5 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if s.Start() == nil {
				ok.Add(1)
			}
		}()
	}
	wg.Wait()
	if n := ok.Load(); n != 1 {
		t.Errorf("%d starts succeeded, want 1", n)
	}
}
