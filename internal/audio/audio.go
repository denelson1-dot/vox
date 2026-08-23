//go:build linux

// Package audio records microphone input to a WAV file.
//
// Linux has three plausible capture stacks and no single command that works
// everywhere, so the recorder is detected rather than assumed. There is no
// mature pure-Go PipeWire or PulseAudio client, and shelling out to the
// system's own tool is both more robust and more honest than a partial
// reimplementation.
package audio

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

// Whisper and most other engines expect 16 kHz mono signed 16-bit.
const (
	SampleRate = 16000
	Channels   = 1
)

// Recorder describes one capture backend.
type Recorder struct {
	Name string
	// Args builds the argv for recording to a path.
	Args  func(path string) []string
	Notes string
}

// Recorders lists capture backends in preference order.
//
// pw-record first: PipeWire is the default on current distributions, and using
// its native tool avoids depending on the PulseAudio compatibility shim being
// installed.
func Recorders() []Recorder {
	return []Recorder{
		{
			Name: "pw-record",
			Args: func(p string) []string {
				return []string{"pw-record", "--rate", fmt.Sprint(SampleRate),
					"--channels", fmt.Sprint(Channels), "--format", "s16", p}
			},
			Notes: "PipeWire",
		},
		{
			Name: "parec",
			Args: func(p string) []string {
				return []string{"parec", "--record", "--device=@DEFAULT_SOURCE@",
					"--rate=" + fmt.Sprint(SampleRate), "--channels=" + fmt.Sprint(Channels),
					"--format=s16le", "--file-format=wav", p}
			},
			Notes: "PulseAudio, or PipeWire's pulse shim",
		},
		{
			Name: "arecord",
			Args: func(p string) []string {
				return []string{"arecord", "-q", "-f", "S16_LE", "-r", fmt.Sprint(SampleRate),
					"-c", fmt.Sprint(Channels), "-t", "wav", p}
			},
			Notes: "ALSA directly; works with no sound server at all",
		},
	}
}

// Detect returns the first available recorder.
func Detect() (Recorder, error) {
	var tried []string
	for _, r := range Recorders() {
		if _, err := exec.LookPath(r.Args("")[0]); err == nil {
			return r, nil
		}
		tried = append(tried, r.Args("")[0])
	}
	return Recorder{}, fmt.Errorf(
		"no audio recorder found; install one of: %s", strings.Join(tried, ", "))
}

// Session is a recording in progress.
type Session struct {
	cmd  *exec.Cmd
	path string
	rec  Recorder
}

// Start begins recording to path.
func Start(rec Recorder, path string) (*Session, error) {
	argv := rec.Args(path)
	cmd := exec.Command(argv[0], argv[1:]...)
	// Own process group, so stopping the recorder cannot signal vox itself.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting %s: %w", rec.Name, err)
	}
	return &Session{cmd: cmd, path: path, rec: rec}, nil
}

// Stop ends the recording and flushes the file.
//
// SIGINT rather than SIGKILL, because the recorder has to write a valid WAV
// header on the way out. A killed recorder leaves a truncated file that every
// speech engine then rejects with a confusing error.
func (s *Session) Stop() error {
	if s.cmd == nil || s.cmd.Process == nil {
		return nil
	}
	_ = s.cmd.Process.Signal(os.Interrupt)

	done := make(chan error, 1)
	go func() { done <- s.cmd.Wait() }()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		_ = s.cmd.Process.Kill()
		<-done
		return fmt.Errorf("%s did not exit cleanly; the recording may be truncated", s.rec.Name)
	}

	info, err := os.Stat(s.path)
	if err != nil {
		return fmt.Errorf("no audio was captured: %w", err)
	}
	// A WAV header alone is 44 bytes; anything near that means silence or a
	// failed capture, which is worth saying plainly rather than handing an
	// empty file to the engine.
	if info.Size() < 1024 {
		return fmt.Errorf("captured only %d bytes; is the microphone muted or is the wrong source selected?", info.Size())
	}
	return nil
}

// Path returns the file being recorded to.
func (s *Session) Path() string { return s.path }
