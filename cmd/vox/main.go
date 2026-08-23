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
		info, _ := e.Info()
		size := ""
		if info != nil && !e.IsDir() {
			size = fmt.Sprintf("  %.0f MB", float64(info.Size())/(1<<20))
		}
		fmt.Fprintf(out, "  %s%s\n", e.Name(), size)
	}
	return nil
}
