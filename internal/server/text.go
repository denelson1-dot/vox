//go:build linux

package server

import "strings"

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
	w := fields[0]
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
