//go:build linux

package server

import (
	"testing"
	"time"
)

// The ellipses Whisper writes at a cut are artefacts of chunking, not speech.
// Left in, they make fluent dictation look full of hesitation -- and land
// exactly where the speaker was most fluent, since chunks cut at pauses.
func TestCleanChunkRemovesBoundaryArtefacts(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"so that it starts...", "so that it starts"},
		{"...putting stuff in after a few seconds", "putting stuff in after a few seconds"},
		{"…kind of like how Windows…", "kind of like how Windows"},
		{"  , and then it fills in  ", "and then it fills in"},
		{"- you start typing on it", "you start typing on it"},
		{"...", ""},
		{".", ""},
		{"  ", ""},
		{"normal sentence.", "normal sentence."},
		{"What about questions?", "What about questions?"},
	} {
		if got := cleanChunk(tc.in); got != tc.want {
			t.Errorf("cleanChunk(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// Every chunk looks like the start of an utterance to the engine, so it
// capitalises each one. Mid-sentence that is wrong.
func TestJoinChunkFixesMidSentenceCapitals(t *testing.T) {
	for _, tc := range []struct{ prev, next, want string }{
		{"so that it starts", "Putting stuff in", "putting stuff in "},
		{"a full stop here.", "Then a new sentence", "Then a new sentence "},
		{"", "First chunk", "First chunk "},
		// Things that must not be lowercased.
		{"and then", "I went home", "I went home "},
		{"we deployed to", "AWS yesterday", "AWS yesterday "},
		{"it runs on", "GitHub Actions", "GitHub Actions "},
		{"a question mark?", "Next one", "Next one "},
		// Punctuation after the first word does not make it a name.
		{"trying to figure out why it failed on the lap", "Top, even though", "top, even though "},
		{"I went", "Home.", "home. "},
		{"and then", "I, for one", "I, for one "},
		{"we deployed to", "AWS.", "AWS. "},
	} {
		if got := joinChunk(tc.prev, tc.next); got != tc.want {
			t.Errorf("joinChunk(%q, %q) = %q, want %q", tc.prev, tc.next, got, tc.want)
		}
	}
}

func TestEndsSentence(t *testing.T) {
	for in, want := range map[string]bool{
		"done.": true, "really?": true, "stop!": true, "as follows:": true,
		"not yet": false, "trailing ": false, "": true,
	} {
		if got := endsSentence(in); got != want {
			t.Errorf("endsSentence(%q) = %v", in, got)
		}
	}
}

// A full stop at a cut is a guess Whisper made without hearing what came
// next. The pause at the cut decides whether to keep it.
func TestSoftenEnd(t *testing.T) {
	short, long := 60*time.Millisecond, 600*time.Millisecond
	for _, tc := range []struct {
		in    string
		pause time.Duration
		want  string
	}{
		// A word gap: the stop is the cut, not the speaker.
		{"why the build kept failing on the laptop.", short, "why the build kept failing on the laptop"},
		{"is that the right one?", short, "is that the right one"},
		// A real pause: a real ending.
		{"it worked on the desktop.", long, "it worked on the desktop."},
		{"did the backup run?", long, "did the backup run?"},
		// No sentence ends on these, however long the speaker paused.
		{"I want to go to the.", long, "I want to go to the"},
		{"we could try it and.", long, "we could try it and"},
		// Sentence ends inside a chunk were heard whole, and stay.
		{"Short ones are easy. Long ones", short, "Short ones are easy. Long ones"},
		{"Short ones are easy. Long ones.", short, "Short ones are easy. Long ones"},
		// Deliberate punctuation is not a guess.
		{"we start at 9 a.m.", short, "we start at 9 a.m."},
		{"you did what?!", short, "you did what?!"},
		{"I spoke to Dr.", short, "I spoke to Dr."},
		{"no stop here", short, "no stop here"},
		{"", short, ""},
	} {
		got, dropped := softenEnd(tc.in, tc.pause)
		if got != tc.want {
			t.Errorf("softenEnd(%q, %v) = %q, want %q", tc.in, tc.pause, got, tc.want)
		}
		if dropped != (got != tc.in) {
			t.Errorf("softenEnd(%q) reported dropped=%v", tc.in, dropped)
		}
	}
}
