package stt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// startTimeout bounds loading a model. Generous: a cold disk and a large
// model can take a while, and it happens once.
const startTimeout = 2 * time.Minute

// errNoServe means the engine's command does not support serving, which is
// not a failure: the per-call path works as it always has.
var errNoServe = errors.New("engine does not support serving")

// worker keeps one engine process alive with its model loaded and sends it
// one request at a time.
//
// Starting a process per chunk reloads the model every time: over a second
// on a laptop CPU, more than the transcription itself. A worker pays that
// once. It is restarted whenever it dies or is killed to abandon a request,
// and warmed again in the background so the next request does not wait for
// it.
type worker struct {
	argv    []string
	timeout time.Duration

	mu     sync.Mutex // one request at a time; also guards the fields below
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	lines  *bufio.Scanner
	stderr *tail
	broken error // set once the command proves unable to serve at all
}

type request struct {
	Audio  string `json:"audio"`
	Prompt string `json:"prompt,omitempty"`
}

type reply struct {
	Ready bool   `json:"ready"`
	Text  string `json:"text"`
	Error string `json:"error"`
}

// start launches the process and waits for it to report a loaded model.
// Called with w.mu held.
func (w *worker) start() error {
	if w.broken != nil {
		return w.broken
	}
	cmd := exec.Command(w.argv[0], w.argv[1:]...)
	// If vox dies without closing stdin, take the model down with it rather
	// than leave a few hundred megabytes orphaned.
	cmd.SysProcAttr = &syscall.SysProcAttr{Pdeathsig: syscall.SIGKILL}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	stderr := &tail{max: 2048}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	lines := bufio.NewScanner(stdout)
	lines.Buffer(make([]byte, 64*1024), 1024*1024)

	ready := make(chan error, 1)
	go func() {
		if !lines.Scan() {
			ready <- fmt.Errorf("exited while loading: %s", stderr)
			return
		}
		var r reply
		if err := json.Unmarshal(lines.Bytes(), &r); err != nil || !r.Ready {
			ready <- fmt.Errorf("did not report ready: %q", lines.Text())
			return
		}
		ready <- nil
	}()

	select {
	case err = <-ready:
	case <-time.After(startTimeout):
		err = fmt.Errorf("model did not load within %v", startTimeout)
	}
	if err != nil {
		cmd.Process.Kill()
		cmd.Wait()
		// An engine that never gets as far as ready (a wrapper too old to
		// know --serve, say) is not going to on the next try either.
		w.broken = fmt.Errorf("%w: %v", errNoServe, err)
		return w.broken
	}
	w.cmd, w.stdin, w.lines, w.stderr = cmd, stdin, lines, stderr
	return nil
}

// stop kills the process. Called with w.mu held.
func (w *worker) stop() {
	if w.cmd == nil {
		return
	}
	w.stdin.Close()
	w.cmd.Process.Kill()
	w.cmd.Wait()
	w.cmd, w.stdin, w.lines = nil, nil, nil
}

// warm starts the process now if it is not running, so the model is loaded
// before anyone speaks.
func (w *worker) warm() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd != nil {
		return nil
	}
	return w.start()
}

// transcribe sends one request. Cancelling ctx kills the process, since an
// inference cannot be interrupted any other way, and a fresh one is warmed in
// the background.
func (w *worker) transcribe(ctx context.Context, audio, prompt string) (string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.cmd == nil {
		if err := w.start(); err != nil {
			return "", err
		}
	}

	req, _ := json.Marshal(request{Audio: audio, Prompt: prompt})
	if _, err := w.stdin.Write(append(req, '\n')); err != nil {
		w.stop()
		return "", fmt.Errorf("engine process went away: %v: %s", err, w.stderr)
	}

	type result struct {
		r    reply
		dead bool
	}
	got := make(chan result, 1)
	lines := w.lines
	go func() {
		var res result
		if !lines.Scan() {
			res.dead = true
		} else if err := json.Unmarshal(lines.Bytes(), &res.r); err != nil {
			res.r.Error = fmt.Sprintf("unreadable reply %q", lines.Text())
		}
		got <- res
	}()

	timer := time.NewTimer(w.timeout)
	defer timer.Stop()
	select {
	case res := <-got:
		if res.dead {
			msg := w.stderr.String()
			w.stop()
			go w.warm()
			return "", fmt.Errorf("engine process exited mid-request: %s", msg)
		}
		if res.r.Error != "" {
			return "", errors.New(res.r.Error)
		}
		return res.r.Text, nil
	case <-ctx.Done():
		w.stop()
		go w.warm()
		return "", ctx.Err()
	case <-timer.C:
		w.stop()
		go w.warm()
		return "", fmt.Errorf("timed out after %v", w.timeout)
	}
}

// close stops the process for good.
func (w *worker) close() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.stop()
}

// tail keeps the last bytes written to it, for error messages.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return strings.TrimSpace(string(t.buf))
}
