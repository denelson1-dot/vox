package stt

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestHelperEngine is not a test. It is the engine the worker tests run: the
// test binary re-executed, speaking the serve protocol the way
// vox-faster-whisper does. Audio paths double as instructions: "slow" takes
// five seconds, "crash" dies mid-request, "bad" gets an error reply.
func TestHelperEngine(t *testing.T) {
	mode := os.Getenv("VOX_FAKE_ENGINE")
	if mode == "" {
		t.Skip("helper process, run by the worker tests")
	}
	args := os.Args[len(os.Args)-2:]
	if args[0] != "--serve" {
		fmt.Println("oneshot:" + filepath.Base(args[1]))
		os.Exit(0)
	}
	if mode == "noserve" {
		fmt.Fprintln(os.Stderr, "usage: engine MODEL AUDIO")
		os.Exit(2)
	}
	f, _ := os.OpenFile(os.Getenv("VOX_FAKE_STARTS"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	fmt.Fprintln(f, "start")
	f.Close()

	fmt.Println(`{"ready": true}`)
	in := bufio.NewScanner(os.Stdin)
	for in.Scan() {
		var req request
		json.Unmarshal(in.Bytes(), &req)
		switch {
		case strings.Contains(req.Audio, "slow"):
			time.Sleep(5 * time.Second)
		case strings.Contains(req.Audio, "crash"):
			fmt.Fprintln(os.Stderr, "segmentation fault (pretend)")
			os.Exit(3)
		case strings.Contains(req.Audio, "bad"):
			fmt.Println(`{"error": "cannot read audio"}`)
			continue
		}
		out, _ := json.Marshal(reply{Text: "heard " + filepath.Base(req.Audio) + " after " + req.Prompt})
		fmt.Println(string(out))
	}
	os.Exit(0)
}

func fakeEngine(t *testing.T, mode string) (*Command, func() int) {
	t.Helper()
	starts := filepath.Join(t.TempDir(), "starts")
	t.Setenv("VOX_FAKE_ENGINE", mode)
	t.Setenv("VOX_FAKE_STARTS", starts)
	self := []string{os.Args[0], "-test.run=^TestHelperEngine$", "--"}
	c := New(Profile{
		Name:    "fake",
		Command: append(append([]string{}, self...), "{model}", "{audio}"),
		Serve:   append(append([]string{}, self...), "--serve", "{model}"),
		Timeout: 10 * time.Second,
	})
	t.Cleanup(c.Close)
	count := func() int {
		b, _ := os.ReadFile(starts)
		return strings.Count(string(b), "start")
	}
	return c, count
}

// The point of serving: the model is loaded once, not once per chunk.
func TestServedEngineStartsOnce(t *testing.T) {
	c, starts := fakeEngine(t, "serve")
	if served, err := c.Warm(); !served || err != nil {
		t.Fatalf("Warm() = %v, %v", served, err)
	}
	for i := range 3 {
		got, err := c.TranscribeWithContext(context.Background(), fmt.Sprintf("/tmp/chunk%d.wav", i), "before")
		if err != nil {
			t.Fatal(err)
		}
		if want := fmt.Sprintf("heard chunk%d.wav after before", i); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
	if n := starts(); n != 1 {
		t.Errorf("engine started %d times for 3 requests, want 1", n)
	}
}

// An inference cannot be interrupted, so cancelling kills the process. The
// next request must still work, and should find a model already loading.
func TestCancelKillsAndRecovers(t *testing.T) {
	c, starts := fakeEngine(t, "serve")
	c.Warm()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := c.TranscribeWithContext(ctx, "/tmp/slow.wav", ""); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want the context's", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("cancel took %v; it waited for the inference", took)
	}

	got, err := c.TranscribeWithContext(context.Background(), "/tmp/next.wav", "")
	if err != nil || got != "heard next.wav after" {
		t.Fatalf("after cancel: %q, %v", got, err)
	}
	if n := starts(); n != 2 {
		t.Errorf("engine started %d times, want 2", n)
	}
}

// A crash fails that request with the engine's own last words, and the next
// request gets a fresh process.
func TestCrashFailsOneRequestOnly(t *testing.T) {
	c, _ := fakeEngine(t, "serve")
	_, err := c.TranscribeWithContext(context.Background(), "/tmp/crash.wav", "")
	if err == nil || !strings.Contains(err.Error(), "segmentation fault") {
		t.Fatalf("err = %v, want one carrying the engine's stderr", err)
	}
	if got, err := c.TranscribeWithContext(context.Background(), "/tmp/after.wav", ""); err != nil || got != "heard after.wav after" {
		t.Fatalf("after crash: %q, %v", got, err)
	}
}

// An error about one request is not a reason to reload the model.
func TestErrorReplyKeepsTheProcess(t *testing.T) {
	c, starts := fakeEngine(t, "serve")
	if _, err := c.TranscribeWithContext(context.Background(), "/tmp/bad.wav", ""); err == nil {
		t.Fatal("want an error for a bad request")
	}
	if _, err := c.TranscribeWithContext(context.Background(), "/tmp/good.wav", ""); err != nil {
		t.Fatal(err)
	}
	if n := starts(); n != 1 {
		t.Errorf("engine started %d times, want 1", n)
	}
}

// An engine that cannot serve (an older wrapper, say) still works, one
// process per request, exactly as before serving existed.
func TestFallsBackWhenServingIsUnsupported(t *testing.T) {
	c, _ := fakeEngine(t, "noserve")
	served, err := c.Warm()
	if served || err == nil {
		t.Errorf("Warm() = %v, %v; want not served, with a reason", served, err)
	}
	got, err := c.TranscribeWithContext(context.Background(), "/tmp/chunk.wav", "")
	if err != nil || got != "oneshot:chunk.wav" {
		t.Fatalf("fallback: %q, %v", got, err)
	}
}
