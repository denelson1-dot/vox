//go:build linux

// Package server is the dictation state machine and its local API.
//
// vox runs as one long-lived user service rather than a process per
// invocation. That is the whole point of splitting it out: the speech model is
// loaded once, and every application on the machine dictates through the same
// service instead of each shipping its own copy of the same several hundred
// megabytes.
//
// The API is a Unix socket carrying newline-delimited commands. A socket
// rather than D-Bus because it adds no dependency, works with no session bus,
// and is trivial to drive from a shell script -- which matters when the
// callers are things like window-manager keybindings.
package server

import (
	"bufio"
	"context"
	"fmt"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/denelson1-dot/vox/internal/audio"
	"github.com/denelson1-dot/vox/internal/inject"
	"github.com/denelson1-dot/vox/internal/stt"
)

// State is what the service is currently doing. Clients poll or subscribe to
// this to render a microphone button.
type State string

const (
	StateReady        State = "ready"
	StateListening    State = "listening"
	StateTranscribing State = "transcribing"
)

// SocketPath returns the API socket location.
func SocketPath() string {
	if p := os.Getenv("VOX_SOCKET"); p != "" {
		return p
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return filepath.Join(dir, "vox.sock")
}

// Server is the dictation service.
type Server struct {
	engine   stt.Engine
	recorder audio.Recorder
	injector inject.Injector
	log      *slog.Logger

	streamCfg StreamConfig
	maxRecord time.Duration // 0 means no limit

	mu          sync.Mutex
	state       State
	session     *audio.Session
	audioIn     string
	streamDone  chan struct{} // closed to ask the streaming loop to finish
	streamRead  *audio.Reader
	streamedAny bool
	streamPrior string // the last chunk as the engine wrote it: its context for the next
	streamTyped string // the last chunk as it was typed, which may have lost a full stop

	// Every recording gets its own context. Cancelling it kills whatever
	// transcription is in flight and forbids any further typing, which is
	// what makes Cancel mean cancel even when a chunk is half done.
	sessCtx    context.Context
	sessCancel context.CancelFunc
	loopDone   chan struct{} // closed once the streaming loop has returned
	limitTimer *time.Timer

	// typeMu serialises everything that types. Without it a streamed chunk
	// and the final tail can type at the same moment and interleave their
	// keystrokes character by character.
	typeMu sync.Mutex

	// subscribers receive state changes, so a UI can show listening and
	// transcribing without polling.
	subsMu sync.Mutex
	subs   map[chan State]struct{}
}

// New builds a server, reporting clearly which piece is missing if any is.
func New(engine stt.Engine, log *slog.Logger) (*Server, error) {
	if log == nil {
		log = slog.Default()
	}
	rec, err := audio.Detect()
	if err != nil {
		return nil, err
	}
	inj, err := inject.Detect()
	if err != nil {
		return nil, err
	}
	log.Info("ready",
		"engine", engine.Name(), "recorder", rec.Name, "injector", inj.Name())

	return &Server{
		engine: engine, recorder: rec, injector: inj, log: log,
		state: StateReady, subs: map[chan State]struct{}{},
		streamCfg: DefaultStreamConfig(),
		maxRecord: DefaultMaxRecord,
	}, nil
}

// DefaultMaxRecord bounds a recording someone forgot to stop.
//
// Left running, dictation keeps typing into whatever has focus, and room noise
// that clears the silence threshold invites Whisper to invent phrases ("Thank
// you for watching."). Five minutes is far longer than anyone dictates in one
// breath and far shorter than a forgotten microphone does damage.
const DefaultMaxRecord = 5 * time.Minute

// SetStreaming enables incremental transcription.
func (s *Server) SetStreaming(c StreamConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCfg = c
}

// SetMaxRecord limits how long one recording may run before it is stopped and
// typed as though the user had stopped it. Zero removes the limit.
func (s *Server) SetMaxRecord(d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.maxRecord = d
}

// State returns the current state.
func (s *Server) State() State {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.state
}

func (s *Server) setState(st State) {
	s.mu.Lock()
	s.state = st
	s.mu.Unlock()
	s.notify(st)
}

func (s *Server) notify(st State) {
	s.subsMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- st:
		default: // never block the state machine on a slow subscriber
		}
	}
	s.subsMu.Unlock()
}

// typeText types text unless the recording it belongs to has been cancelled.
// It reports whether anything was typed.
//
// The check and the typing happen under typeMu, and Cancel takes typeMu after
// cancelling, so once Cancel returns nothing more from that recording can be
// typed.
func (s *Server) typeText(ctx context.Context, text string) (bool, error) {
	s.typeMu.Lock()
	defer s.typeMu.Unlock()
	if ctx.Err() != nil {
		return false, nil
	}
	return true, s.injector.Type(text)
}

// Start begins recording.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.state != StateReady {
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("busy: %s", st)
	}
	// Claim the state now, so two presses in quick succession cannot both
	// start a recorder.
	s.state = StateListening
	cfg, limit := s.streamCfg, s.maxRecord
	s.mu.Unlock()

	dir, err := os.MkdirTemp("", "vox-")
	if err != nil {
		s.setState(StateReady)
		return err
	}
	path := filepath.Join(dir, "speech.wav")

	sess, err := audio.Start(s.recorder, path)
	if err != nil {
		os.RemoveAll(dir)
		s.setState(StateReady)
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	var done, loopDone chan struct{}
	var reader *audio.Reader
	if cfg.Enabled {
		done, loopDone = make(chan struct{}), make(chan struct{})
		reader = audio.NewReader(path)
	}

	s.mu.Lock()
	s.session, s.audioIn = sess, path
	s.sessCtx, s.sessCancel = ctx, cancel
	s.streamedAny = false
	s.streamPrior = ""
	s.streamTyped = ""
	s.streamDone, s.streamRead, s.loopDone = done, reader, loopDone
	if limit > 0 {
		s.limitTimer = time.AfterFunc(limit, func() { s.limitReached(sess, limit) })
	}
	s.mu.Unlock()

	s.notify(StateListening)
	if cfg.Enabled {
		// Text appears while you speak, so the wait at the end is only for
		// whatever was said since the last chunk rather than the whole thing.
		go func() {
			defer close(loopDone)
			s.streamLoopShared(ctx, reader, path, done)
		}()
		s.log.Info("listening (streaming)")
	} else {
		s.log.Info("listening")
	}
	return nil
}

// limitReached stops a recording that has run for the maximum length. It is
// tied to the recording that set the timer, so a late timer can never stop a
// newer one.
func (s *Server) limitReached(sess *audio.Session, limit time.Duration) {
	s.log.Warn("recording limit reached; stopping", "limit", limit)
	if _, err := s.stop(context.Background(), sess); err != nil {
		s.log.Warn("stopping at the recording limit", "err", err)
	}
}

// Stop ends recording, transcribes, and types the result.
func (s *Server) Stop(ctx context.Context) (string, error) {
	return s.stop(ctx, nil)
}

// stop ends the given recording, or whichever is running when want is nil.
func (s *Server) stop(ctx context.Context, want *audio.Session) (string, error) {
	s.mu.Lock()
	if s.state != StateListening || (want != nil && s.session != want) {
		st := s.state
		s.mu.Unlock()
		return "", fmt.Errorf("not listening: %s", st)
	}
	sess, path := s.session, s.audioIn
	done, reader, loopDone := s.streamDone, s.streamRead, s.loopDone
	sessCtx := s.sessCtx
	s.streamDone = nil
	if s.limitTimer != nil {
		s.limitTimer.Stop()
	}
	// Transcribing from here on, claimed under the lock so that a second
	// stop (a double press, or the recording limit) is refused.
	s.state = StateTranscribing
	s.mu.Unlock()
	s.notify(StateTranscribing)

	defer os.RemoveAll(filepath.Dir(path))

	if done != nil {
		close(done)
	}
	if err := sess.Stop(); err != nil {
		s.reset(sess)
		return "", err
	}
	s.log.Info("transcribing", "engine", s.engine.Name())

	// A chunk may still be transcribing. Let it finish and be typed before
	// the tail is touched: the tail is later speech, and typing it first
	// would put the end of what you said before its middle.
	if loopDone != nil {
		<-loopDone
	}
	if sessCtx.Err() != nil {
		return "", nil // cancelled while the last chunk finished
	}

	// When streaming, only the audio since the last chunk is left. That is the
	// whole point: the wait at the end is a second or two rather than the
	// length of everything you said.
	source := path
	if reader != nil {
		tail, ok := s.remainder(reader, filepath.Dir(path))
		if !ok {
			s.mu.Lock()
			streamed := s.streamedAny
			s.mu.Unlock()
			s.log.Info("nothing left to transcribe", "streamed", streamed)
			s.reset(sess)
			return "", nil
		}
		source = tail
	}

	s.mu.Lock()
	prior, typedPrev := s.streamPrior, s.streamTyped
	s.mu.Unlock()

	// Cancel must reach this transcription too, not only the caller's ctx.
	tctx, stopT := context.WithCancel(ctx)
	defer stopT()
	defer context.AfterFunc(sessCtx, stopT)()

	start := time.Now()
	text, err := s.engine.TranscribeWithContext(tctx, source, prior)
	if sessCtx.Err() != nil {
		return "", nil // cancelled; Cancel has already reset
	}
	if err != nil {
		s.reset(sess)
		return "", err
	}
	s.log.Info("transcribed", "took", time.Since(start), "chars", len(text))

	if reader != nil {
		// The final piece is a chunk like any other and carries the same
		// boundary artefacts.
		text = cleanChunk(text)
	}
	// The tail is the likeliest place for one: it ends with the click of
	// the key that stopped the recording.
	if isPhantom(text) {
		s.log.Info("dropped a phrase Whisper invents from noise", "chars", len(text))
		text = ""
	}
	if text != "" {
		out := text + " "
		if reader != nil {
			out = joinChunk(typedPrev, text)
		}
		if _, err := s.typeText(sessCtx, out); err != nil {
			s.reset(sess)
			return text, fmt.Errorf("typing the transcript: %w", err)
		}
	}
	s.reset(sess)
	return text, nil
}

// Key presses a single named key in the focused window.
func (s *Server) Key(name string) error {
	if name == "" {
		return fmt.Errorf("no key named")
	}
	s.typeMu.Lock()
	defer s.typeMu.Unlock()
	return s.injector.Key(name)
}

// Cancel abandons a recording without transcribing.
//
// It also abandons work already under way: a chunk being transcribed is
// killed, and nothing from this recording is typed after Cancel returns.
func (s *Server) Cancel() error {
	s.mu.Lock()
	sess, path := s.session, s.audioIn
	cancel, done, loopDone := s.sessCancel, s.streamDone, s.loopDone
	s.streamDone = nil
	s.mu.Unlock()

	if cancel != nil {
		cancel()
	}
	if done != nil {
		close(done)
	}
	if sess != nil {
		_ = sess.Stop()
	}
	if loopDone != nil {
		<-loopDone
	}
	// Wait out a chunk that was already mid-keystroke when cancel landed.
	s.typeMu.Lock()
	s.typeMu.Unlock() //nolint:staticcheck // a barrier, not a critical section
	if path != "" {
		os.RemoveAll(filepath.Dir(path))
	}
	s.reset(sess)
	s.log.Info("cancelled")
	return nil
}

// reset ends a recording and returns to ready. It does nothing if a newer
// recording has already replaced sess, so a late reset from an old recording
// cannot end the current one. A nil sess resets whatever is there.
func (s *Server) reset(sess *audio.Session) {
	s.mu.Lock()
	if sess != nil && s.session != sess {
		s.mu.Unlock()
		return
	}
	if s.sessCancel != nil {
		s.sessCancel()
	}
	if s.limitTimer != nil {
		s.limitTimer.Stop()
	}
	s.session, s.audioIn = nil, ""
	s.sessCtx, s.sessCancel = nil, nil
	s.streamDone, s.streamRead, s.loopDone, s.limitTimer = nil, nil, nil, nil
	s.state = StateReady
	s.mu.Unlock()
	s.notify(StateReady)
}

// Toggle starts or stops, which is what a single button needs.
func (s *Server) Toggle(ctx context.Context) (string, error) {
	switch s.State() {
	case StateReady:
		return "", s.Start()
	case StateListening:
		return s.Stop(ctx)
	default:
		// Mid-transcription a toggle is ignored rather than queued: the user
		// pressed a button whose meaning is ambiguous while busy, and doing
		// nothing is the least surprising answer.
		return "", fmt.Errorf("busy transcribing")
	}
}

// Serve accepts clients until ctx is cancelled.
func (s *Server) Serve(ctx context.Context, socket string) error {
	s.removeStaleRecordings()

	// A stale socket from an unclean exit would otherwise block startup
	// forever with a confusing "address already in use".
	if err := os.Remove(socket); err != nil && !os.IsNotExist(err) {
		return err
	}
	ln, err := net.Listen("unix", socket)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", socket, err)
	}
	defer ln.Close()
	defer os.Remove(socket)

	// Owner-only: this socket types into the user's session.
	if err := os.Chmod(socket, 0o600); err != nil {
		return err
	}
	s.log.Info("listening on socket", "path", socket)

	go func() {
		<-ctx.Done()
		ln.Close()
	}()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go s.handle(ctx, conn)
	}
}

func (s *Server) handle(ctx context.Context, conn net.Conn) {
	defer conn.Close()
	sc := bufio.NewScanner(conn)
	for sc.Scan() {
		cmd := strings.TrimSpace(sc.Text())
		switch cmd {
		case "start":
			reply(conn, s.Start())
		case "stop":
			text, err := s.Stop(ctx)
			if err != nil {
				fmt.Fprintf(conn, "error %v\n", err)
			} else {
				fmt.Fprintf(conn, "ok %s\n", text)
			}
		case "toggle":
			text, err := s.Toggle(ctx)
			if err != nil {
				fmt.Fprintf(conn, "error %v\n", err)
			} else {
				fmt.Fprintf(conn, "ok %s\n", text)
			}
		case "cancel":
			reply(conn, s.Cancel())
		case "state":
			fmt.Fprintf(conn, "ok %s\n", s.State())
		case "info":
			s.mu.Lock()
			limit := s.maxRecord
			s.mu.Unlock()
			fmt.Fprintf(conn, "ok engine=%s recorder=%s injector=%s max_record=%s\n",
				s.engine.Name(), s.recorder.Name, s.injector.Name(), limit)
		case "subscribe":
			s.stream(ctx, conn)
			return
		case "":
		default:
			// "key NAME" presses a single key. A touch panel with no physical
			// keyboard needs Return to submit what was just dictated.
			if name, ok := strings.CutPrefix(cmd, "key "); ok {
				reply(conn, s.Key(strings.TrimSpace(name)))
				continue
			}
			fmt.Fprintf(conn, "error unknown command %q\n", cmd)
		}
	}
}

// stream pushes state changes until the client disconnects, so a microphone
// button can reflect listening and transcribing without polling.
func (s *Server) stream(ctx context.Context, conn net.Conn) {
	ch := make(chan State, 8)
	s.subsMu.Lock()
	s.subs[ch] = struct{}{}
	s.subsMu.Unlock()
	defer func() {
		s.subsMu.Lock()
		delete(s.subs, ch)
		s.subsMu.Unlock()
	}()

	fmt.Fprintf(conn, "ok %s\n", s.State())
	for {
		select {
		case <-ctx.Done():
			return
		case st := <-ch:
			if _, err := fmt.Fprintf(conn, "ok %s\n", st); err != nil {
				return
			}
		}
	}
}

func reply(conn net.Conn, err error) {
	if err != nil {
		fmt.Fprintf(conn, "error %v\n", err)
		return
	}
	fmt.Fprintln(conn, "ok")
}

// Close releases the injector.
//
// A recording still running is cancelled first, so its audio is deleted
// rather than left behind in the temp directory.
func (s *Server) Close() error {
	s.mu.Lock()
	active := s.session != nil
	s.mu.Unlock()
	if active {
		s.Cancel()
	}
	return s.injector.Close()
}

// removeStaleRecordings deletes recordings left by a previous run that died
// mid-dictation (a crash, a kill, a power cut). They hold whatever the
// microphone heard, and nothing will ever read them. Called before serving,
// when no recording of this run can exist yet.
func (s *Server) removeStaleRecordings() {
	dirs, _ := filepath.Glob(filepath.Join(os.TempDir(), "vox-*"))
	removed := 0
	for _, d := range dirs {
		info, err := os.Lstat(d)
		if err != nil || !info.IsDir() {
			continue
		}
		if st, ok := info.Sys().(*syscall.Stat_t); !ok || int(st.Uid) != os.Getuid() {
			continue // someone else's, or not knowably ours
		}
		if os.RemoveAll(d) == nil {
			removed++
		}
	}
	if removed > 0 {
		s.log.Info("removed recordings left by an earlier run", "count", removed)
	}
}
