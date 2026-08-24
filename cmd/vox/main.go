//go:build linux

// Command vox is a system-wide dictation service.
//
// One service, one model, every application. It records the microphone,
// transcribes with whichever speech engine you have, and types the result into
// whatever currently has focus.
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"

	"github.com/denelson1-dot/vox/internal/audio"
	"github.com/denelson1-dot/vox/internal/inject"
	"github.com/denelson1-dot/vox/internal/server"
	"github.com/denelson1-dot/vox/internal/stt"
)

const usage = `vox - system-wide dictation for Linux

Usage:
  vox daemon             Run the service
  vox toggle             Start, or stop and type the result
  vox start | stop       Begin or end dictation explicitly
  vox cancel             Abandon a recording without transcribing
  vox state              ready | listening | transcribing
  vox doctor             Report what is installed and what is missing
  vox models             Show the model directory and what is in it
  vox models get NAME    Download a model, e.g. small.en, medium.en
  vox version

Daemon flags:
  -engine NAME   Speech engine profile (default: autodetect)
  -model PATH    Model path or name; bare names resolve under the model dir

Everything speaks to one long-lived service, so the model loads once and every
application on the machine shares it.
`

var version = "dev"

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "vox: %v\n", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return flag.ErrHelp
	}
	switch args[0] {
	case "daemon":
		return daemon(args[1:], stderr)
	case "toggle", "start", "stop", "cancel", "state", "info":
		return client(args[0], stdout)
	case "doctor":
		return doctor(stdout)
	case "models":
		if len(args) > 2 && args[1] == "get" {
			return fetchModel(args[2], stdout)
		}
		return models(stdout)
	case "version":
		fmt.Fprintln(stdout, version)
		return nil
	case "-h", "--help", "help":
		fmt.Fprint(stdout, usage)
		return nil
	default:
		fmt.Fprintf(stderr, "unknown command %q\n\n%s", args[0], usage)
		return flag.ErrHelp
	}
}

func daemon(args []string, stderr io.Writer) error {
	fs := flag.NewFlagSet("daemon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	engineName := fs.String("engine", "", "speech engine profile")
	model := fs.String("model", "", "model path or name")
	verbose := fs.Bool("v", false, "verbose logging")
	if err := fs.Parse(args); err != nil {
		return err
	}

	level := slog.LevelInfo
	if *verbose {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(stderr, &slog.HandlerOptions{Level: level}))

	var engine stt.Engine
	if *engineName != "" {
		p, err := stt.Find(*engineName)
		if err != nil {
			return err
		}
		if *model != "" {
			p.Model = *model
		}
		c := stt.New(p)
		if err := c.Check(); err != nil {
			return fmt.Errorf("engine %s: %w", p.Name, err)
		}
		engine = c
	} else {
		c, err := stt.Detect()
		if err != nil {
			return err
		}
		engine = c
	}

	srv, err := server.New(engine, log)
	if err != nil {
		return err
	}
	defer srv.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return srv.Serve(ctx, server.SocketPath())
}

// client sends one command to the running service.
func client(cmd string, stdout io.Writer) error {
	conn, err := net.Dial("unix", server.SocketPath())
	if err != nil {
		return fmt.Errorf("cannot reach the vox service at %s: %w\n\n"+
			"start it with: systemctl --user start vox", server.SocketPath(), err)
	}
	defer conn.Close()

	if _, err := fmt.Fprintln(conn, cmd); err != nil {
		return err
	}
	line, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		return err
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "error ") {
		return errors.New(strings.TrimPrefix(line, "error "))
	}
	if out := strings.TrimPrefix(line, "ok"); strings.TrimSpace(out) != "" {
		fmt.Fprintln(stdout, strings.TrimSpace(out))
	}
	return nil
}

func doctor(out io.Writer) error {
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)

	fmt.Fprintln(out, "SPEECH ENGINES")
	for _, p := range stt.BuiltinProfiles() {
		c := stt.New(p)
		status := "ready"
		if err := c.Check(); err != nil {
			status = err.Error()
		}
		fmt.Fprintf(w, "  %s\t%s\t%s\n", p.Name, status, p.Notes)
	}
	w.Flush()

	fmt.Fprintln(out, "\nAUDIO CAPTURE")
	rec, err := audio.Detect()
	if err != nil {
		fmt.Fprintf(out, "  none: %v\n", err)
	} else {
		fmt.Fprintf(out, "  %s (%s)\n", rec.Name, rec.Notes)
	}

	fmt.Fprintln(out, "\nTEXT INJECTION")
	inj, err := inject.Detect()
	if err != nil {
		fmt.Fprintf(out, "  none: %v\n", err)
	} else {
		fmt.Fprintf(out, "  %s\n", inj.Name())
		inj.Close()
	}

	fmt.Fprintf(out, "\nMODEL DIRECTORY\n  %s\n", stt.ModelDir())

	fmt.Fprintln(out, "\nSERVICE")
	if err := client("state", io.Discard); err != nil {
		fmt.Fprintf(out, "  not running (%v)\n", err)
	} else {
		fmt.Fprintln(out, "  running")
	}
	return nil
}

// knownModels are the faster-whisper models worth suggesting, with the
// tradeoff stated rather than left for the user to discover.
var knownModels = []struct{ name, size, note string }{
	{"tiny.en", "75 MB", "fastest, noticeably less accurate"},
	{"base.en", "142 MB", "the usual starting point"},
	{"small.en", "464 MB", "clearly better than base; still comfortably realtime on a laptop CPU"},
	{"medium.en", "1.5 GB", "better again, but slow on CPU without a GPU"},
}

// fetchModel downloads a model into the shared directory.
//
// Shelling out to the engine's own tooling rather than reimplementing a
// HuggingFace client: the layout, resumption and checksums are theirs to get
// right, and a bad model file fails in confusing ways.
func fetchModel(name string, out io.Writer) error {
	repo := name
	if !strings.Contains(repo, "/") {
		repo = "Systran/faster-whisper-" + name
	}
	dir := stt.ModelDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(out, "downloading %s into %s\n", repo, dir)

	script := `import sys
from huggingface_hub import snapshot_download
print(snapshot_download(sys.argv[1], cache_dir=sys.argv[2]))`
	py := findPython()
	if py == "" {
		return fmt.Errorf("no Python with huggingface_hub found; "+
			"install it, or download the model manually into %s", dir)
	}
	cmd := exec.Command(py, "-c", script, repo, dir)
	cmd.Stdout, cmd.Stderr = out, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("downloading %s: %w", repo, err)
	}
	fmt.Fprintf(out, "\ndone. use it with:  vox daemon -model %s\n", name)
	return nil
}

// findPython locates an interpreter that has huggingface_hub, preferring an
// existing virtualenv over asking the user to make another.
func findPython() string {
	home, _ := os.UserHomeDir()
	for _, c := range []string{
		os.Getenv("VOX_WHISPER_VENV") + "/bin/python",
		home + "/.local/share/vox/venv/bin/python",
		home + "/.local/share/voice-dictation/venv/bin/python",
		"python3",
	} {
		if c == "/bin/python" {
			continue
		}
		p, err := exec.LookPath(c)
		if err != nil {
			continue
		}
		if exec.Command(p, "-c", "import huggingface_hub").Run() == nil {
			return p
		}
	}
	return ""
}

func models(out io.Writer) error {
	dir := stt.ModelDir()
	fmt.Fprintf(out, "model directory: %s\n\n", dir)
	entries, err := os.ReadDir(dir)
	if os.IsNotExist(err) {
		fmt.Fprintln(out, "  (does not exist yet)")
		fmt.Fprintf(out, "\ncreate it with:  mkdir -p %s\n", dir)
		return nil
	}
	if err != nil {
		return err
	}
	if len(entries) == 0 {
		fmt.Fprintln(out, "  (empty)")
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(out, "  %s\n", e.Name())
	}
	fmt.Fprintln(out, "\navailable to download:")
	for _, m := range knownModels {
		fmt.Fprintf(out, "  %-11s %-8s %s\n", m.name, m.size, m.note)
	}
	fmt.Fprintln(out, "\n  vox models get small.en")
	return nil
}
