//go:build linux

package server

import (
	"strings"
	"time"
)

// cleanChunk removes the artefacts of having cut audio mid-sentence.
//
// Whisper writes an ellipsis when speech trails off or begins mid-utterance,
// which is precisely what a chunk boundary looks like to it. Left alone, this
// litters dictation with pauses the speaker never made -- and worse, they land
// exactly where the speaker was most fluent, since a chunk is cut at the
// quietest moment.
//
// The transcript should read as though it were transcribed in one pass. The
// boundaries are an implementation detail and should not be visible.
func cleanChunk(s string) string {
	s = strings.TrimSpace(s)

	// Both the ASCII and the typographic form; engines emit either.
	for _, e := range []string{"...", "…"} {
		s = strings.TrimPrefix(s, e)
		s = strings.TrimSuffix(s, e)
	}
	// A dangling connector at a cut is an artefact too, not speech.
	s = strings.TrimLeft(s, " ,-–—")
	s = strings.TrimRight(s, " ,-–—")
	s = strings.TrimSpace(s)

	// Whatever is left may still be only punctuation, which is silence that
	// the engine felt obliged to describe.
	if strings.Trim(s, ".,!?-–—…\"' ") == "" {
		return ""
	}
	return s
}

// joinChunk decides the spacing between what has already been typed and the
// next chunk, so the result reads continuously rather than as fragments.
func joinChunk(prev, next string) string {
	if next == "" {
		return ""
	}
	// Mid-sentence continuation: the engine capitalises the start of every
	// chunk because it believes each one begins an utterance. Lowering it
	// again only when the previous chunk did not end a sentence keeps proper
	// nouns and "I" intact.
	if prev != "" && !endsSentence(prev) {
		next = lowerFirstIfPlain(next)
	}
	return next + " "
}

func endsSentence(s string) bool {
	s = strings.TrimRight(s, " ")
	if s == "" {
		return true
	}
	switch s[len(s)-1] {
	case '.', '!', '?', ':', ';':
		return true
	}
	return false
}

// lowerFirstIfPlain lowercases a leading capital only when the word looks like
// an ordinary word. A fully capitalised or internally capitalised word is
// likely an acronym or a name, and "i" would be plainly wrong.
func lowerFirstIfPlain(s string) string {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return s
	}
	// Punctuation after the word is not part of it: "Top," is a plain word.
	w := strings.TrimRight(fields[0], ".,;:!?\"")
	if w == "" {
		return s
	}
	if w == "I" || strings.HasPrefix(w, "I'") {
		return s
	}
	if w == strings.ToUpper(w) {
		return s // acronym, or a single capital letter
	}
	if strings.ToUpper(w[1:]) == w[1:] && len(w) > 1 {
		return s
	}
	if rest := strings.TrimLeft(w[1:], "abcdefghijklmnopqrstuvwxyz'’-"); rest != "" {
		return s // mixed case, e.g. a product name
	}
	return strings.ToLower(s[:1]) + s[1:]
}

// sentencePause is the shortest pause at a chunk boundary that is taken as
// the end of a sentence.
//
// Whisper hears each chunk on its own, so audio that stops after a word sounds
// like an ending and it writes a full stop at almost every cut: about half of
// all mid-sentence boundaries, measured. The pause at the cut is the evidence
// it lacks. Between words it is a few tens of milliseconds, at a comma one or
// two hundred, and between sentences longer. Measured on synthetic speech,
// where sentence pauses are shorter than people's, a threshold of 0.3 s
// removed the false stops at the cost of an occasional missed one.
const sentencePause = 300 * time.Millisecond

// noEnd holds words that a sentence practically never ends on. A full stop
// after one of them is a cut, however long the pause: someone thinking
// mid-sentence ("I went to the... shop") pauses for as long as they like.
var noEnd = map[string]bool{}

func init() {
	for _, w := range strings.Fields(`a an the and or but nor so because if than
		that which who whose of to in on at by for from with into onto about as
		like my your his her its our their this these those is are was were be
		been being am has have had do does did will would shall should can could
		may might must very just`) {
		noEnd[w] = true
	}
}

// abbreviation holds titles whose full stop is part of the word. One lands at
// a cut whenever the cut falls between "Dr." and the name, a short pause.
var abbreviation = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "st": true, "vs": true,
	"etc": true, "prof": true, "jr": true, "sr": true,
}

// softenEnd removes a chunk's closing . ? or ! when the cut it ends at does
// not look like the end of a sentence. It reports whether it did.
//
// Only a single closing mark goes. "?!" or an abbreviation such as "a.m." is
// deliberate and stays, and so does everything inside the chunk: Whisper
// heard those sentence ends whole, so they are trustworthy.
func softenEnd(text string, pause time.Duration) (string, bool) {
	fields := strings.Fields(text)
	if len(fields) == 0 {
		return text, false
	}
	last := fields[len(fields)-1]
	mark := last[len(last)-1]
	if mark != '.' && mark != '?' && mark != '!' {
		return text, false
	}
	word := last[:len(last)-1]
	if word == "" || strings.ContainsAny(word, ".?!") || abbreviation[strings.ToLower(word)] {
		return text, false
	}
	if pause >= sentencePause && !noEnd[strings.ToLower(word)] {
		return text, false
	}
	trimmed := strings.TrimRight(text, " ")
	return trimmed[:len(trimmed)-1], true
}
