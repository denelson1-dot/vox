//go:build linux && (amd64 || arm64 || 386 || arm || riscv64 || s390x)

// Package inject types text into whatever currently has focus.
//
// The default is a uinput virtual keyboard. That is worth explaining, because
// most tools reach for xdotool or wtype instead.
//
// A virtual keyboard is a kernel-level input device, so the text arrives by
// exactly the same path as a real keypress. It works identically on X11 and on
// every Wayland compositor, needs no per-compositor protocol, and does not care
// which toolkit owns the focused window. xdotool is X11-only; wtype needs
// virtual-keyboard-unstable-v1, which GNOME and KDE do not implement; ydotool
// is the same mechanism as this but as a separate root daemon. Doing it
// in-process removes a dependency and a privilege boundary.
//
// The cost is that a keyboard emits keycodes, not characters, so text has to be
// mapped through a layout. That mapping is the bulk of this file.
package inject

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
	"unsafe"
)

// Injector types text and presses named keys.
//
// Pressing a key is the same capability as typing text -- both are synthetic
// input into the focused window -- so it lives here rather than growing a
// second mechanism with its own device and permissions.
type Injector interface {
	Type(text string) error
	// Key presses a named key, e.g. "Return", "Tab", "BackSpace", "Escape".
	Key(name string) error
	Name() string
	Close() error
}

// namedKeys are the keys worth exposing: the ones a touch panel needs when
// there is no physical keyboard, from
// include/uapi/linux/input-event-codes.h.
var namedKeys = map[string]uint16{
	"Return": 28, "Enter": 28,
	"Tab": 15, "BackSpace": 14, "Escape": 1, "Delete": 111,
	"Up": 103, "Down": 108, "Left": 105, "Right": 106,
	"Home": 102, "End": 107, "space": 57,
}

// Detect returns the best available injector.
//
// uinput first: it is the only option that works everywhere. The external
// tools are fallbacks for machines where /dev/uinput is not writable.
func Detect() (Injector, error) {
	if inj, err := NewUinput(); err == nil {
		return inj, nil
	} else {
		uinputErr := err
		if inj, err := detectExternal(); err == nil {
			return inj, nil
		}
		return nil, fmt.Errorf(
			"no way to type text.\n"+
				"  uinput: %v\n"+
				"  and none of wtype, ydotool or xdotool are installed\n\n"+
				"the uinput path is preferred: it works on X11 and Wayland alike.\n"+
				"install packaging/udev/70-vox-uinput.rules to enable it", uinputErr)
	}
}

func detectExternal() (Injector, error) {
	// wtype: Wayland, wlroots-family compositors.
	if _, err := exec.LookPath("wtype"); err == nil {
		return &external{name: "wtype", argv: func(s string) []string { return []string{"wtype", s} }}, nil
	}
	// ydotool: uinput-based, same mechanism but via a root daemon.
	if _, err := exec.LookPath("ydotool"); err == nil {
		return &external{name: "ydotool", argv: func(s string) []string { return []string{"ydotool", "type", s} }}, nil
	}
	// xdotool: X11 only.
	if _, err := exec.LookPath("xdotool"); err == nil && os.Getenv("DISPLAY") != "" {
		return &external{name: "xdotool", argv: func(s string) []string {
			return []string{"xdotool", "type", "--delay", "1", "--clearmodifiers", "--", s}
		}}, nil
	}
	return nil, fmt.Errorf("no external typing tool found")
}

type external struct {
	name string
	argv func(string) []string
}

func (e *external) Key(name string) error {
	var argv []string
	switch e.name {
	case "wtype":
		argv = []string{"wtype", "-k", name}
	case "ydotool":
		// ydotool takes keycodes rather than names.
		code, ok := namedKeys[name]
		if !ok {
			return fmt.Errorf("ydotool: unknown key %q", name)
		}
		argv = []string{"ydotool", "key", fmt.Sprintf("%d:1", code), fmt.Sprintf("%d:0", code)}
	default:
		argv = []string{"xdotool", "key", "--clearmodifiers", name}
	}
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s key %s: %w: %s", e.name, name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (e *external) Name() string { return e.name }
func (e *external) Close() error { return nil }
func (e *external) Type(text string) error {
	argv := e.argv(text)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%s: %w: %s", e.name, err, strings.TrimSpace(string(out)))
	}
	return nil
}

// --- uinput virtual keyboard ------------------------------------------------

const (
	uiSetEvBit   = (1 << 30) | (4 << 16) | (0x55 << 8) | 100
	uiSetKeyBit  = (1 << 30) | (4 << 16) | (0x55 << 8) | 101
	uiDevSetup   = (1 << 30) | (uinputSetupSize << 16) | (0x55 << 8) | 3
	uiDevCreate  = (0x55 << 8) | 1
	uiDevDestroy = (0x55 << 8) | 2

	evSyn     = 0x00
	evKey     = 0x01
	synReport = 0x00

	keyLeftShift = 42

	nameSize        = 80
	uinputSetupSize = 8 + nameSize + 4
)

// Uinput is a virtual keyboard.
type Uinput struct {
	fd     int
	closed bool
}

// NewUinput creates the virtual keyboard.
func NewUinput() (*Uinput, error) {
	fd, err := syscall.Open("/dev/uinput", syscall.O_WRONLY|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("/dev/uinput: %w", err)
	}
	u := &Uinput{fd: fd}
	if err := u.setup(); err != nil {
		syscall.Close(fd)
		return nil, err
	}
	// udev needs a moment to create the node and for compositors to notice the
	// new device; typing immediately would drop the first characters.
	time.Sleep(300 * time.Millisecond)
	return u, nil
}

func (u *Uinput) setup() error {
	if err := ioctl(u.fd, uiSetEvBit, evKey); err != nil {
		return fmt.Errorf("UI_SET_EVBIT: %w", err)
	}
	// Declare every keycode the layout can produce, plus shift and the named
	// keys. A keycode not declared here is silently dropped by the kernel.
	declared := map[uint16]bool{keyLeftShift: true}
	for _, k := range layout {
		declared[k.code] = true
	}
	for _, code := range namedKeys {
		declared[code] = true
	}
	for code := range declared {
		if err := ioctl(u.fd, uiSetKeyBit, uintptr(code)); err != nil {
			return fmt.Errorf("UI_SET_KEYBIT(%d): %w", code, err)
		}
	}

	var setup [uinputSetupSize]byte
	putU16(setup[0:], 0x06) // BUS_VIRTUAL
	putU16(setup[2:], 0)
	putU16(setup[4:], 0)
	putU16(setup[6:], 1)
	copy(setup[8:8+nameSize], "vox virtual keyboard")

	if err := ioctlPtr(u.fd, uiDevSetup, unsafe.Pointer(&setup[0])); err != nil {
		return fmt.Errorf("UI_DEV_SETUP: %w", err)
	}
	return ioctl(u.fd, uiDevCreate, 0)
}

// Name identifies the injector.
func (u *Uinput) Name() string { return "uinput" }

// Key presses and releases a named key.
func (u *Uinput) Key(name string) error {
	if u.closed {
		return fmt.Errorf("virtual keyboard is closed")
	}
	code, ok := namedKeys[name]
	if !ok {
		return fmt.Errorf("unknown key %q", name)
	}
	return u.press(key{code: code})
}

// Type sends text as keystrokes.
//
// Characters the layout cannot produce are skipped rather than mistyped: a
// wrong character silently inserted into someone's document is worse than a
// missing one, and the caller is told how many were dropped.
func (u *Uinput) Type(text string) error {
	if u.closed {
		return fmt.Errorf("virtual keyboard is closed")
	}
	var skipped []rune
	for _, r := range text {
		k, ok := layout[r]
		if !ok {
			skipped = append(skipped, r)
			continue
		}
		if err := u.press(k); err != nil {
			return err
		}
		// A short gap: some applications drop keystrokes delivered faster than
		// a human could produce them.
		time.Sleep(2 * time.Millisecond)
	}
	if len(skipped) > 0 {
		return fmt.Errorf("could not type %d character(s) not in the layout: %q",
			len(skipped), string(skipped))
	}
	return nil
}

func (u *Uinput) press(k key) error {
	if k.shift {
		if err := u.emit(evKey, keyLeftShift, 1); err != nil {
			return err
		}
	}
	if err := u.emit(evKey, k.code, 1); err != nil {
		return err
	}
	if err := u.emit(evKey, k.code, 0); err != nil {
		return err
	}
	if k.shift {
		if err := u.emit(evKey, keyLeftShift, 0); err != nil {
			return err
		}
	}
	return u.emit(evSyn, synReport, 0)
}

func (u *Uinput) emit(typ, code uint16, value int32) error {
	buf := make([]byte, inputEventSize)
	putU16(buf[offType:], typ)
	putU16(buf[offCode:], code)
	putU32(buf[offValue:], uint32(value))
	for {
		_, err := syscall.Write(u.fd, buf)
		if err == syscall.EINTR {
			continue
		}
		return err
	}
}

// Close destroys the virtual keyboard.
func (u *Uinput) Close() error {
	if u.closed {
		return nil
	}
	u.closed = true
	_ = ioctl(u.fd, uiDevDestroy, 0)
	return syscall.Close(u.fd)
}

func ioctl(fd int, req uintptr, arg uintptr) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, arg)
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}

func ioctlPtr(fd int, req uintptr, p unsafe.Pointer) error {
	for {
		_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, uintptr(fd), req, uintptr(p))
		if errno == syscall.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
