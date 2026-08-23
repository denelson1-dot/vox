//go:build linux

// Package stt turns recorded audio into text.
//
// Every engine is driven as an external command. That is a deliberate choice
// rather than a shortcut: speech recognition moves fast, people have strong
// and legitimate preferences about the accuracy/size/latency tradeoff, and
// binding to one engine would make this project the bottleneck on all of it.
// A command contract means a new engine is a config file, not a release.
//
// It also means vox never bundles a model. Models are large, licensed
// variously, and most people who want dictation already have one somewhere.
package stt

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Engine transcribes an audio file.
type Engine interface {
	// Transcribe returns the text spoken in the given WAV file.
	Transcribe(ctx context.Context, audioPath string) (string, error)
	// Name identifies the engine, for logs and status.
	Name() string
	// Check reports whether the engine is usable, with an actionable reason
	// when it is not.
	Check() error
}

// Profile is a named recipe for driving one engine.
//
// Placeholders in Command are substituted at call time:
//
//	{audio}  path to the recorded WAV
//	{model}  resolved model path
type Profile struct {
	Name string

	// Command is an argv. Never a shell string: a transcript is untrusted
	// text and must never be able to become syntax.
	Command []string

	// Model is a path or a bare name resolved under the model directory.
	Model string

	// Timeout bounds a wedged engine.
	Timeout time.Duration

	// StripPrefixes removes engine chatter that some tools print before the
	// transcript.
	StripPrefixes []string

	// Notes describes what this profile needs installed.
	Notes string
}

// BuiltinProfiles are the engines vox knows how to drive out of the box.
//
// faster-whisper comes first because it is what a lot of people already have
// working, and reusing an existing install is the whole point of having one
// dictation service rather than a model per project.
func BuiltinProfiles() []Profile {
	return []Profile{
		{
			Name:    "faster-whisper",
			Command: []string{"vox-faster-whisper", "{model}", "{audio}"},
			Model:   "base.en",
			Timeout: 60 * time.Second,
			Notes:   "needs the faster-whisper Python package; vox ships a small wrapper script",
		},
		{
			Name:    "whisper-cpp",
			Command: []string{"whisper-cli", "-m", "{model}", "-f", "{audio}", "-nt", "--output-txt", "-of", "/dev/stdout"},
			Model:   "ggml-base.en.bin",
			Timeout: 60 * time.Second,
			Notes:   "whisper.cpp; models are .bin files from ggml-org/whisper.cpp",
		},
		{
			Name:    "whisper",
			Command: []string{"whisper", "--model", "{model}", "--output_format", "txt", "--output_dir", "/tmp", "{audio}"},
			Model:   "base.en",
			Timeout: 120 * time.Second,
			Notes:   "the reference OpenAI whisper CLI; slower than the alternatives",
		},
		{
			Name:    "vosk",
			Command: []string{"vosk-transcriber", "-m", "{model}", "-i", "{audio}"},
			Model:   "vosk-model-small-en-us",
			Timeout: 60 * time.Second,
			Notes:   "vosk; fully offline and small, lower accuracy than Whisper",
		},
	}
}

// ModelDir returns the one place vox looks for models.
//
// Having a single shared location is the point: a model downloaded once is
// usable by every project on the machine instead of each one pulling its own
// copy of the same several hundred megabytes.
func ModelDir() string {
	if d := os.Getenv("VOX_MODEL_DIR"); d != "" {
		return d
	}
	base := os.Getenv("XDG_DATA_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "/usr/share/vox/models"
		}
		base = filepath.Join(home, ".local", "share")
	}
	return filepath.Join(base, "vox", "models")
}

// Command is an Engine driven by an external program.
type Command struct{ p Profile }

// New returns an Engine for a profile.
func New(p Profile) *Command {
	if p.Timeout <= 0 {
		p.Timeout = 60 * time.Second
	}
	return &Command{p: p}
}

// Find returns a built-in profile by name.
func Find(name string) (Profile, error) {
	for _, p := range BuiltinProfiles() {
		if p.Name == name {
			return p, nil
		}
	}
	return Profile{}, fmt.Errorf("unknown engine %q; built-in profiles are: %s",
		name, strings.Join(ProfileNames(), ", "))
}

// ProfileNames lists the built-in profile names.
func ProfileNames() []string {
	var out []string
	for _, p := range BuiltinProfiles() {
		out = append(out, p.Name)
	}
	return out
}

// Detect returns the first built-in profile whose command exists and whose
// model is present, so a fresh install usually needs no configuration.
func Detect() (*Command, error) {
	var reasons []string
	for _, p := range BuiltinProfiles() {
		c := New(p)
		if err := c.Check(); err != nil {
			reasons = append(reasons, "  "+p.Name+": "+err.Error())
			continue
		}
		return c, nil
	}
	return nil, fmt.Errorf("no usable speech engine found:\n%s\n\nmodels are looked for in %s",
		strings.Join(reasons, "\n"), ModelDir())
}

// Name returns the profile name.
func (c *Command) Name() string { return c.p.Name }

// Model returns the resolved model path.
func (c *Command) Model() string { return resolveModel(c.p.Model) }

// Check reports whether this engine can run, and says what is missing if not.
func (c *Command) Check() error {
	if len(c.p.Command) == 0 {
		return fmt.Errorf("profile has no command")
	}
	if _, err := exec.LookPath(c.p.Command[0]); err != nil {
		return fmt.Errorf("%s is not on PATH", c.p.Command[0])
	}
	if c.p.Model != "" && !modelPresent(c.p.Model) {
		return fmt.Errorf("model %q not found in %s", c.p.Model, ModelDir())
	}
	return nil
}

// modelPresent reports whether a model is available, accepting both layouts
// that engines actually use.
//
// A literal path or directory is the simple case. faster-whisper instead takes
// a repository name and stores it in HuggingFace cache layout, so "base.en"
// lives at "models--Systran--faster-whisper-base.en". Checking only for a
// literal path would report a perfectly good model as missing, which is worse
// than not checking at all.
func modelPresent(model string) bool {
	if _, err := os.Stat(resolveModel(model)); err == nil {
		return true
	}
	matches, _ := filepath.Glob(filepath.Join(ModelDir(), "models--*"+model))
	return len(matches) > 0
}

// Transcribe runs the engine over one audio file.
func (c *Command) Transcribe(ctx context.Context, audioPath string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, c.p.Timeout)
	defer cancel()

	argv := make([]string, 0, len(c.p.Command))
	for _, a := range c.p.Command {
		a = strings.ReplaceAll(a, "{audio}", audioPath)
		a = strings.ReplaceAll(a, "{model}", resolveModel(c.p.Model))
		argv = append(argv, a)
	}

	cmd := exec.CommandContext(cctx, argv[0], argv[1:]...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr

	if err := cmd.Run(); err != nil {
		if cctx.Err() == context.DeadlineExceeded {
			return "", fmt.Errorf("%s timed out after %v", c.p.Name, c.p.Timeout)
		}
		return "", fmt.Errorf("%s failed: %w: %s", c.p.Name, err, strings.TrimSpace(stderr.String()))
	}
	return clean(stdout.String(), c.p.StripPrefixes), nil
}

// resolveModel turns a bare model name into a path under the model directory,
// and leaves an explicit path alone.
func resolveModel(m string) string {
	if m == "" {
		return ""
	}
	if strings.HasPrefix(m, "/") {
		return m
	}
	if strings.HasPrefix(m, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, m[2:])
		}
	}
	return filepath.Join(ModelDir(), m)
}

// clean tidies engine output into something typeable.
func clean(s string, strip []string) string {
	s = strings.TrimSpace(s)
	for _, p := range strip {
		s = strings.TrimPrefix(s, p)
	}
	// Some engines emit timestamps or blank lines around the text.
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			lines = append(lines, l)
		}
	}
	return strings.TrimSpace(strings.Join(lines, " "))
}
