//go:build linux

package audio

import (
	"encoding/binary"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// tone builds PCM: `speech` seconds of signal, then `quiet` seconds of near
// silence, repeated.
func tone(pattern []struct {
	secs float64
	loud bool
}) []byte {
	var out []byte
	phase := 0.0
	for _, p := range pattern {
		n := Bytes(p.secs) / 2
		buf := make([]byte, n*2)
		for i := 0; i < n; i++ {
			v := 0.0
			if p.loud {
				phase += 2 * math.Pi * 220 / SampleRate
				v = math.Sin(phase) * 8000
			}
			binary.LittleEndian.PutUint16(buf[i*2:], uint16(int16(v)))
		}
		out = append(out, buf...)
	}
	return out
}

// The split must land in the pause, not mid-word. A cut through speech is what
// makes chunked transcription produce mangled words at boundaries.
func TestQuietestSplitFindsThePause(t *testing.T) {
	pcm := tone([]struct {
		secs float64
		loud bool
	}{
		{4.0, true},  // speech
		{0.5, false}, // the pause, at 4.0-4.5s
		{3.0, true},  // more speech
	})
	cut := QuietestSplit(pcm, Bytes(3.0))
	at := Seconds(cut)
	if at < 3.95 || at > 4.6 {
		t.Errorf("split at %.2fs, expected inside the pause at 4.0-4.5s", at)
	}
}

// With no pause at all it must still cut somewhere sane rather than refusing.
func TestQuietestSplitWithNoPause(t *testing.T) {
	pcm := tone([]struct {
		secs float64
		loud bool
	}{{8.0, true}})
	cut := QuietestSplit(pcm, Bytes(5.0))
	if cut <= 0 || cut > len(pcm) {
		t.Errorf("cut %d is outside the buffer of %d", cut, len(pcm))
	}
	if Seconds(cut) < 5.0 {
		t.Errorf("cut at %.2fs, before the search window started", Seconds(cut))
	}
}

func TestIsSilent(t *testing.T) {
	quiet := tone([]struct {
		secs float64
		loud bool
	}{{1.0, false}})
	loud := tone([]struct {
		secs float64
		loud bool
	}{{1.0, true}})
	if !IsSilent(quiet) {
		t.Error("silence not detected as silent")
	}
	if IsSilent(loud) {
		t.Error("speech detected as silent")
	}
}

func TestSecondsBytesRoundTrip(t *testing.T) {
	for _, s := range []float64{0.5, 1, 6, 14} {
		if got := Seconds(Bytes(s)); math.Abs(got-s) > 0.001 {
			t.Errorf("round trip %v -> %v", s, got)
		}
	}
}

// A reader tailing a growing file must never hand back the same audio twice,
// which would type every word a second time.
func TestReaderConsumesOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "r.wav")
	pcm := tone([]struct {
		secs float64
		loud bool
	}{{2.0, true}})
	if err := WriteWAV(path, pcm); err != nil {
		t.Fatal(err)
	}
	r := NewReader(path)

	first, err := r.Pending()
	if err != nil || len(first) != len(pcm) {
		t.Fatalf("first read: %d bytes, want %d (%v)", len(first), len(pcm), err)
	}
	r.Consume(len(first))
	again, _ := r.Pending()
	if len(again) != 0 {
		t.Errorf("consumed audio was returned again: %d bytes", len(again))
	}

	// Simulate the recorder appending more.
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	more := tone([]struct {
		secs float64
		loud bool
	}{{1.0, true}})
	f.Write(more)
	f.Close()

	grown, _ := r.Pending()
	if len(grown) != len(more) {
		t.Errorf("after append got %d bytes, want only the new %d", len(grown), len(more))
	}
}
