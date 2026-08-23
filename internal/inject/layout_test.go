//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

package inject

import (
	"testing"
	"unsafe"
)

// Every character a speech engine can plausibly emit must be typeable, or it
// is silently dropped from someone's dictation.
func TestLayoutCoversTranscriptCharacters(t *testing.T) {
	const typical = "The quick brown fox jumps over the lazy dog! " +
		"It cost $42.50 (about 30%), didn't it? " +
		"Email: a.b-c_d@example.com; see http://x.io/y?z=1&w=2 " +
		"\"quoted\" 'single' [bracket] {brace} <angle> ~tilde` |pipe\\ +plus =equals\n\t"
	for _, r := range typical {
		if _, ok := layout[r]; !ok {
			t.Errorf("cannot type %q (U+%04X)", r, r)
		}
	}
}

func TestLayoutCaseIsDistinct(t *testing.T) {
	lower, okL := layout['a']
	upper, okU := layout['A']
	if !okL || !okU {
		t.Fatal("missing a/A")
	}
	if lower.code != upper.code {
		t.Error("a and A must share a keycode")
	}
	if lower.shift || !upper.shift {
		t.Error("only the uppercase form should use shift")
	}
}

func TestLayoutDigitsAndSymbolsShareCodes(t *testing.T) {
	for _, tc := range []struct{ digit, symbol rune }{
		{'1', '!'}, {'2', '@'}, {'5', '%'}, {'9', '('}, {'0', ')'},
	} {
		d, s := layout[tc.digit], layout[tc.symbol]
		if d.code != s.code {
			t.Errorf("%c and %c should share a keycode", tc.digit, tc.symbol)
		}
		if d.shift || !s.shift {
			t.Errorf("%c should be unshifted and %c shifted", tc.digit, tc.symbol)
		}
	}
}

// struct input_event must match the architecture, or every event is misparsed.
func TestInputEventSizeMatchesArch(t *testing.T) {
	want := 2*int(unsafe.Sizeof(uintptr(0))) + 8
	if inputEventSize != want {
		t.Errorf("inputEventSize = %d, want %d", inputEventSize, want)
	}
}
