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

	mu          sync.Mutex
	state       State
	session     *audio.Session
	audioIn     string
	streamDone  chan struct{}
	streamRead  *audio.Reader
	streamedAny bool

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
	}, nil
}

// SetStreaming enables incremental transcription.
func (s *Server) SetStreaming(c StreamConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.streamCfg = c
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

	s.subsMu.Lock()
	for ch := range s.subs {
		select {
		case ch <- st:
		default: // never block the state machine on a slow subscriber
		}
	}
	s.subsMu.Unlock()
}

// Start begins recording.
func (s *Server) Start() error {
	s.mu.Lock()
	if s.state != StateReady {
		st := s.state
		s.mu.Unlock()
		return fmt.Errorf("busy: %s", st)
	}
	s.mu.Unlock()

	dir, err := os.MkdirTemp("", "vox-")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, "speech.wav")

	sess, err := audio.Start(s.recorder, path)
	if err != nil {
		os.RemoveAll(dir)
		return err
	}
	s.mu.Lock()
	s.session, s.audioIn = sess, path
	s.streamedAny = false
	s.streamDone = nil
	s.streamRead = nil
	if s.streamCfg.Enabled {
		s.streamDone = make(chan struct{})
		s.streamRead = audio.NewReader(path)
	}
	done, streaming := s.streamDone, s.streamCfg.Enabled
	s.mu.Unlock()

	s.setState(StateListening)
	if streaming {
		// Text appears while you speak, so the wait at the end is only for
		// whatever was said since the last chunk rather than the whole thing.
		go s.streamLoopWith(context.Background(), path, done)
		s.log.Info("listening (streaming)")
	} else {
		s.log.Info("listening")
	}
	return nil
}

// streamLoopWith shares the reader with Stop, so the two never transcribe the
// same audio and no words are typed twice.
func (s *Server) streamLoopWith(ctx context.Context, path string, done <-chan struct{}) {
	s.mu.Lock()
	r := s.streamRead
	s.mu.Unlock()
	if r == nil {
		return
	}
	s.streamLoopShared(ctx, r, path, done)
}

// Stop ends recording, transcribes, and types the result.
func (s *Server) Stop(ctx context.Context) (string, error) {
	s.mu.Lock()
	if s.state != StateListening {
		st := s.state
		s.mu.Unlock()
		return "", fmt.Errorf("not listening: %s", st)
	}
	sess, path := s.session, s.audioIn
	s.mu.Unlock()

	defer os.RemoveAll(filepath.Dir(path))

	// Stop the streaming loop before reading the tail, so both never
	// transcribe the same audio.
	s.mu.Lock()
	done, reader, streamed := s.streamDone, s.streamRead, s.streamedAny
	s.streamDone = nil
	s.mu.Unlock()
	if done != nil {
		close(done)
	}

	if err := sess.Stop(); err != nil {
		s.reset()
		return "", err
	}
	s.setState(StateTranscribing)
	s.log.Info("transcribing", "engine", s.engine.Name())

	// When streaming, only the audio since the last chunk is left. That is the
	// whole point: the wait at the end is a second or two rather than the
	// length of everything you said.
	source := path
	if reader != nil {
		tail, ok := s.remainder(reader, filepath.Dir(path))
		if !ok {
			s.log.Info("nothing left to transcribe", "streamed", streamed)
			s.reset()
			return "", nil
		}
		source = tail
	}

	start := time.Now()
	text, err := s.engine.Transcribe(ctx, source)
	if err != nil {
		s.reset()
		return "", err
	}
	s.log.Info("transcribed", "took", time.Since(start), "chars", len(text))

	if text != "" {
		// A trailing space, so consecutive dictations do not run together.
		if err := s.injector.Type(text + " "); err != nil {
			s.reset()
			return text, fmt.Errorf("typing the transcript: %w", err)
		}
	}
	s.reset()
	return text, nil
}

// Key presses a single named key in the focused window.
func (s *Server) Key(name string) error {
	if name == "" {
		return fmt.Errorf("no key named")
	}
	return s.injector.Key(name)
}

// Cancel abandons a recording without transcribing.
func (s *Server) Cancel() error {
	s.mu.Lock()
	sess, path := s.session, s.audioIn
	s.mu.Unlock()

	if sess != nil {
		_ = sess.Stop()
	}
	if path != "" {
		os.RemoveAll(filepath.Dir(path))
	}
	s.reset()
	s.log.Info("cancelled")
	return nil
}

func (s *Server) reset() {
	s.mu.Lock()
	s.session, s.audioIn = nil, ""
	s.mu.Unlock()
	s.setState(StateReady)
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
			fmt.Fprintf(conn, "ok engine=%s recorder=%s injector=%s\n",
				s.engine.Name(), s.recorder.Name, s.injector.Name())
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
func (s *Server) Close() error { return s.injector.Close() }
