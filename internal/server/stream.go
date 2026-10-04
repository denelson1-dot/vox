//go:build linux

package server

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/denelson1-dot/vox/internal/audio"
)

// Streaming defaults.
//
// ChunkSeconds is the target, not a hard cut: a chunk is emitted once at least
// this much audio has accumulated, split at the quietest point nearby. Six
// seconds is long enough for Whisper to have useful context and short enough
// that text keeps appearing while you speak.
const (
	defaultChunkSeconds = 6.0
	defaultMaxChunk     = 14.0 // force a cut even with no pause to be found
)

// pollInterval is how often the loop looks for a chunk's worth of audio.
// Each look reads only what is new since the last chunk, so looking often is
// cheap, and every interval is time a ready chunk can sit waiting before it is
// transcribed. A variable only so tests need not wait out real timings.
var pollInterval = 250 * time.Millisecond

// StreamConfig tunes incremental transcription.
type StreamConfig struct {
	Enabled      bool
	ChunkSeconds float64
	MaxSeconds   float64
}

// DefaultStreamConfig returns streaming turned on.
//
// On by default because waiting for a whole utterance to transcribe after you
// stop talking is the worst part of using dictation, and the wait grows with
// how much you said. Every chunk is given the preceding text as context, so
// the pieces join as one transcript rather than reading as fragments.
//
// Disable with -stream=false to transcribe in a single pass, which is slightly
// more accurate because the engine sees the whole utterance at once.
func DefaultStreamConfig() StreamConfig {
	return StreamConfig{Enabled: true, ChunkSeconds: defaultChunkSeconds, MaxSeconds: defaultMaxChunk}
}

// streamLoop transcribes and types the recording as it is captured.
//
// It never ends the recording. Silence is used only to choose where to cut a
// chunk, because a pause mid-sentence is a good split point and a terrible
// reason to stop listening -- people pause to think, and dictation that shuts
// off when they do is worse than dictation that is slow.
func (s *Server) streamLoopShared(ctx context.Context, reader *audio.Reader, path string, done <-chan struct{}) {
	s.mu.Lock()
	cfg := s.streamCfg
	s.mu.Unlock()
	if cfg.ChunkSeconds <= 0 {
		cfg.ChunkSeconds = defaultChunkSeconds
	}
	if cfg.MaxSeconds < cfg.ChunkSeconds {
		cfg.MaxSeconds = cfg.ChunkSeconds * 2
	}

	tmp := filepath.Join(filepath.Dir(path), "chunk.wav")
	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-done:
			// Stop() handles the remainder, so that the last words are
			// transcribed with whatever context is left rather than being
			// raced against by this loop.
			return
		case <-ticker.C:
			// select picks at random among ready cases. Once done is closed,
			// Stop is waiting on this loop, so never begin another chunk.
			select {
			case <-done:
				return
			default:
			}
			pcm, err := reader.Pending()
			if err != nil || audio.Seconds(len(pcm)) < cfg.ChunkSeconds {
				continue
			}

			// Look for a pause in the tail of the chunk. Searching only the
			// tail keeps chunks near the target length instead of collapsing
			// to the first quiet moment.
			searchFrom := audio.Bytes(cfg.ChunkSeconds * 0.7)
			cut := audio.QuietestSplit(pcm, searchFrom)
			if audio.Seconds(len(pcm)) > cfg.MaxSeconds {
				cut = audio.Bytes(cfg.MaxSeconds)
			}
			if cut <= 0 || cut > len(pcm) {
				continue
			}

			segment := pcm[:cut]
			// Measured before consuming: the audio after the cut is what
			// shows how long the pause runs.
			pause := audio.PauseAt(pcm, cut)
			reader.Consume(cut)
			if audio.IsSilent(segment) {
				// Nothing said. Transcribing silence wastes a second of CPU
				// and invites Whisper to invent a phrase.
				continue
			}
			if err := audio.WriteWAV(tmp, segment); err != nil {
				s.log.Warn("streaming: writing chunk", "err", err)
				continue
			}
			s.mu.Lock()
			prior, typedPrev := s.streamPrior, s.streamTyped
			s.mu.Unlock()

			text, err := s.engine.TranscribeWithContext(ctx, tmp, prior)
			if ctx.Err() != nil {
				return // cancelled; the engine was killed mid-chunk
			}
			if err != nil {
				s.log.Warn("streaming: transcribing chunk", "err", err)
				continue
			}
			text = cleanChunk(text)
			if isPhantom(text) {
				s.log.Info("dropped a phrase Whisper invents from noise", "chars", len(text))
				continue
			}
			if text == "" {
				continue
			}
			// The engine keeps its own view of the text, full stops and all,
			// so what it is prompted with is exactly what it would be without
			// softening; only what is typed changes.
			heard := strings.TrimSpace(joinChunk(prior, text))
			body, softened := softenEnd(strings.TrimSpace(joinChunk(typedPrev, text)), pause)
			out := body + " "
			s.log.Info("streaming chunk", "seconds", audio.Seconds(cut), "chars", len(out),
				"pause", pause, "end_dropped", softened)
			typed, err := s.typeText(ctx, out)
			if !typed {
				return // cancelled before this chunk could be typed
			}
			if err != nil {
				s.log.Warn("streaming: typing chunk", "err", err)
			}
			s.mu.Lock()
			s.streamedAny = true
			s.streamPrior = heard
			s.streamTyped = body
			s.mu.Unlock()
		}
	}
}

// remainder returns audio captured but not yet typed by the streaming loop.
func (s *Server) remainder(reader *audio.Reader, dir string) (string, bool) {
	pcm, err := reader.Pending()
	if err != nil || len(pcm) == 0 || audio.IsSilent(pcm) {
		return "", false
	}
	tmp := filepath.Join(dir, "tail.wav")
	if err := audio.WriteWAV(tmp, pcm); err != nil {
		return "", false
	}
	_ = os.Chmod(tmp, 0o600)
	return tmp, true
}
