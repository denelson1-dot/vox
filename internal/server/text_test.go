//go:build linux

package server

import "testing"

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
