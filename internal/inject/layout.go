//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

package inject

import (
	"encoding/binary"
	"unsafe"
)

// wordBytes is the size of a C long, so struct input_event is sized correctly
// on 32-bit as well as 64-bit targets.
const wordBytes = (32 << (^uint(0) >> 63)) / 8

const inputEventSize = 2*wordBytes + 8

const (
	offType  = 2 * wordBytes
	offCode  = offType + 2
	offValue = offCode + 2
)

func nativeOrder() binary.ByteOrder {
	var probe uint16 = 0x0102
	if *(*byte)(unsafe.Pointer(&probe)) == 0x02 {
		return binary.LittleEndian
	}
	return binary.BigEndian
}

var order = nativeOrder()

func putU16(b []byte, v uint16) { order.PutUint16(b, v) }
func putU32(b []byte, v uint32) { order.PutUint32(b, v) }

// key is a keycode plus whether shift is needed to produce the character.
type key struct {
	code  uint16
	shift bool
}

// layout maps characters to US-QWERTY keycodes from
// include/uapi/linux/input-event-codes.h.
//
// A virtual keyboard emits keycodes, and the compositor applies the user's
// keymap to them. So this produces the right characters only when the user's
// layout is US-QWERTY. On another layout the keycodes are still valid but the
// resulting characters differ.
//
// That limitation is inherent to the approach rather than an oversight: doing
// better means reading the active keymap (XKB on X11, a compositor-specific
// path on Wayland) and mapping through it, which is worth doing but is a
// project of its own. Until then vox reports characters it cannot type rather
// than typing something else, and the external injectors remain available for
// users on other layouts.
var layout = map[rune]key{}

func init() {
	// Letters. KEY_A..KEY_Z are not contiguous in the header; this is the
	// physical QWERTY row order they actually occupy.
	rows := []struct {
		chars string
		codes []uint16
	}{
		{"qwertyuiop", []uint16{16, 17, 18, 19, 20, 21, 22, 23, 24, 25}},
		{"asdfghjkl", []uint16{30, 31, 32, 33, 34, 35, 36, 37, 38}},
		{"zxcvbnm", []uint16{44, 45, 46, 47, 48, 49, 50}},
	}
	for _, row := range rows {
		for i, ch := range row.chars {
			layout[ch] = key{code: row.codes[i]}
			layout[ch-32] = key{code: row.codes[i], shift: true} // uppercase
		}
	}

	// Digits and their shifted symbols.
	digits := "1234567890"
	shifted := "!@#$%^&*()"
	for i, ch := range digits {
		code := uint16(2 + i)
		layout[ch] = key{code: code}
		layout[rune(shifted[i])] = key{code: code, shift: true}
	}

	// Punctuation and whitespace.
	for _, e := range []struct {
		plain, shift rune
		code         uint16
	}{
		{'-', '_', 12}, {'=', '+', 13}, {'[', '{', 26}, {']', '}', 27},
		{';', ':', 39}, {'\'', '"', 40}, {'`', '~', 41}, {'\\', '|', 43},
		{',', '<', 51}, {'.', '>', 52}, {'/', '?', 53},
	} {
		layout[e.plain] = key{code: e.code}
		layout[e.shift] = key{code: e.code, shift: true}
	}
	layout[' '] = key{code: 57}  // KEY_SPACE
	layout['\n'] = key{code: 28} // KEY_ENTER
	layout['\t'] = key{code: 15} // KEY_TAB
}
