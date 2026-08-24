//go:build linux

package audio

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
)

// wavHeaderSize is the canonical 44-byte RIFF header the recorders emit.
//
// The length fields in it are only correct once recording finishes, which is
// why streaming reads the PCM directly and builds its own header per chunk
// rather than trying to parse a file still being written.
const wavHeaderSize = 44

// Reader tails a recording that is still being written.
type Reader struct {
	path   string
	offset int64 // bytes of PCM already consumed
}

// NewReader tails the file a Session is recording to.
func NewReader(path string) *Reader { return &Reader{path: path} }

// Pending returns PCM captured but not yet consumed.
func (r *Reader) Pending() ([]byte, error) {
	f, err := os.Open(r.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	start := wavHeaderSize + r.offset
	if info.Size() <= start {
		return nil, nil
	}
	buf := make([]byte, info.Size()-start)
	if _, err := f.ReadAt(buf, start); err != nil {
		return nil, err
	}
	// Whole samples only; a partial frame would be read as noise.
	buf = buf[:len(buf)-len(buf)%2]
	return buf, nil
}

// Consume advances past n bytes of PCM.
func (r *Reader) Consume(n int) { r.offset += int64(n) }

// Seconds converts a PCM byte count to duration.
func Seconds(n int) float64 { return float64(n) / float64(SampleRate*2*Channels) }

// Bytes converts a duration to a PCM byte count, aligned to a whole sample.
func Bytes(seconds float64) int {
	n := int(seconds * float64(SampleRate*2*Channels))
	return n - n%2
}

// QuietestSplit finds the best place to cut a run of PCM.
//
// This is silence detection used to choose *where* to cut, never *whether* to
// stop. A pause in the middle of a sentence is a good place to split a chunk
// and a terrible reason to end a recording -- people pause to think, and
// having dictation shut off when they do is the single most irritating
// behaviour a voice tool can have.
//
// It searches only the tail of the buffer, so the chunk stays near the target
// length, and returns the midpoint of the quietest window found.
func QuietestSplit(pcm []byte, searchFrom int) int {
	const windowMS = 40
	win := Bytes(windowMS / 1000.0)
	if win <= 0 || len(pcm) < win*2 {
		return len(pcm)
	}
	if searchFrom < 0 || searchFrom >= len(pcm)-win {
		searchFrom = len(pcm) / 2
	}

	best, bestRMS := len(pcm), math.MaxFloat64
	for off := searchFrom; off+win <= len(pcm); off += win / 2 {
		if e := rms(pcm[off : off+win]); e < bestRMS {
			bestRMS, best = e, off+win/2
		}
	}
	return best - best%2
}

// rms is the loudness of a span of 16-bit samples.
func rms(pcm []byte) float64 {
	if len(pcm) < 2 {
		return 0
	}
	var sum float64
	n := len(pcm) / 2
	for i := 0; i < n; i++ {
		s := float64(int16(binary.LittleEndian.Uint16(pcm[i*2:])))
		sum += s * s
	}
	return math.Sqrt(sum / float64(n))
}

// IsSilent reports whether a span is quiet enough to be worth skipping.
// Transcribing pure silence wastes a second of CPU and tends to make Whisper
// hallucinate a stray phrase.
func IsSilent(pcm []byte) bool { return rms(pcm) < 120 }

// WriteWAV writes PCM as a standalone WAV file for an engine to read.
func WriteWAV(path string, pcm []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	var h [wavHeaderSize]byte
	copy(h[0:], "RIFF")
	binary.LittleEndian.PutUint32(h[4:], uint32(36+len(pcm)))
	copy(h[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(h[16:], 16)
	binary.LittleEndian.PutUint16(h[20:], 1) // PCM
	binary.LittleEndian.PutUint16(h[22:], Channels)
	binary.LittleEndian.PutUint32(h[24:], SampleRate)
	binary.LittleEndian.PutUint32(h[28:], SampleRate*Channels*2)
	binary.LittleEndian.PutUint16(h[32:], Channels*2)
	binary.LittleEndian.PutUint16(h[34:], 16)
	copy(h[36:], "data")
	binary.LittleEndian.PutUint32(h[40:], uint32(len(pcm)))

	if _, err := f.Write(h[:]); err != nil {
		return fmt.Errorf("writing wav header: %w", err)
	}
	_, err = f.Write(pcm)
	return err
}
