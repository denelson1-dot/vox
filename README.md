# vox

**System-wide dictation for Linux. One service, one model, every application.**

Written in Go. Pure static binary, no runtime dependencies beyond a speech
engine of your choosing.

> **Status: early.** The service, engines, capture and injection all work. See
> [Roadmap](#roadmap).

---

## Why one service

Most dictation setups end up per-project: a virtualenv here, a model there,
several hundred megabytes duplicated for each thing that wants to hear you.

vox is a single long-lived user service. The model loads once. Your editor,
your browser, your window manager keybinding and your tablet's microphone
button all talk to the same thing, and there is exactly one copy of the model
on disk.

```
                   ┌──────────────┐
  keybinding  ───► │              │
  tablet UI   ───► │  vox service │ ──► types into whatever has focus
  script      ───► │              │
                   └──────────────┘
                    one model, loaded once
```

## Quick start

```sh
./install.sh
vox doctor                          # what is present, what is missing
systemctl --user enable --now vox
vox toggle                          # speak; run again to stop and type
```

Bind `vox toggle` to a key for push-to-talk.

## Speech engines

vox drives every engine as an external command, and ships profiles for the
common ones. That means a new engine is a config entry rather than a release,
and you pick your own accuracy/size/latency tradeoff instead of inheriting
someone else's.

| Profile | Needs | Notes |
|---|---|---|
| `faster-whisper` | the `faster-whisper` Python package | Fast and accurate; a wrapper ships with vox |
| `whisper-cpp` | `whisper-cli` | Single binary, `ggml-*.bin` models |
| `whisper` | the reference `whisper` CLI | Slowest |
| `vosk` | `vosk-transcriber` | Small and fully offline, lower accuracy |

`vox daemon` autodetects the first usable one. Override with
`-engine NAME -model NAME`.

### Accuracy

Defaults favour accuracy over speed, because dictation has latency to spare.
Measured on an i5-8250U, transcribing a 22-second utterance:

| Model | Decoding | Speed | |
|---|---|---|---|
| `base.en` | greedy (`beam=1`) | 20x realtime | fast and noticeably worse |
| `base.en` | `beam=5`, all cores | 10x realtime | **default** |
| `small.en` | `beam=5`, all cores | 4x realtime | better; still imperceptible for dictation |

`beam_size=5` is faster-whisper's own default and cut word error rate by about
a fifth in testing against greedy decoding. Running 10x faster than realtime
means a ten-second utterance transcribes in one second, so there is no reason
to spend that headroom on speed you cannot feel.

If accuracy is not good enough, get a larger model -- that is the bigger lever:

```sh
vox models get small.en
vox daemon -model small.en
```

A caveat on the numbers above: they are speed measurements. Attempts to score
accuracy with synthetic speech were not meaningful -- every configuration
transcribed espeak output perfectly, because synthetic speech is far more
regular than the human speech these models are trained on. Only the
same-model beam comparison is a real accuracy result. Judge models with your
own voice; nothing else generalises.

### Already have faster-whisper?

vox looks for an existing virtualenv before asking you to make a new one — it
checks `$VOX_WHISPER_VENV`, then `~/.local/share/vox/venv`, then
`~/.local/share/voice-dictation/venv`. Point it at an existing model cache with:

```sh
export VOX_MODEL_DIR=~/.cache/faster-whisper
```

Nothing is re-downloaded.

## Models live in one place

```sh
vox models        # shows the directory and its contents
```

`$XDG_DATA_HOME/vox/models`, overridable with `VOX_MODEL_DIR`. Bare model names
resolve there; absolute paths are used as given. Downloading a model once and
having every tool on the machine use it is the entire point.

## How text gets typed

By default vox creates a **uinput virtual keyboard**. This is worth explaining,
because most tools reach for `xdotool` or `wtype`.

A virtual keyboard is a kernel input device, so text arrives by the same path as
a real keypress. It works identically on X11 and on every Wayland compositor,
needs no per-compositor protocol, and does not care which toolkit owns the
focused window. By contrast `xdotool` is X11-only, and `wtype` needs
`virtual-keyboard-unstable-v1`, which GNOME and KDE do not implement.

The cost is that a keyboard emits keycodes, not characters, so characters are
mapped through a **US-QWERTY layout**. On a different layout the keycodes are
still valid but the characters differ. vox reports characters it cannot type
rather than typing something else, and `wtype`/`ydotool`/`xdotool` remain
available as fallbacks. Reading the active keymap is on the roadmap.

**Security, plainly:** write access to `/dev/uinput` is the ability to
synthesize arbitrary keystrokes into your session. That is inherent to what
dictation does, but it is a real capability — the shipped udev rule scopes it
to the active local session via `uaccess` rather than granting it to a group
permanently. Skip it with `install.sh --no-udev` and vox falls back to an
external tool.

## Audio capture

Detected, not assumed: `pw-record` (PipeWire), then `parec` (PulseAudio), then
`arecord` (ALSA). Recording is 16 kHz mono s16, which is what the engines want.

The recorder is stopped with SIGINT rather than SIGKILL so it can finish writing
a valid WAV header — a truncated file makes every engine fail with a confusing
error.

## API

A Unix socket at `$XDG_RUNTIME_DIR/vox.sock`, newline-delimited. No D-Bus
dependency, works with no session bus, and is trivial to drive from a script.

| Command | |
|---|---|
| `toggle` | Start, or stop and type |
| `start` / `stop` | Explicit control |
| `cancel` | Abandon without transcribing |
| `state` | `ready` \| `listening` \| `transcribing` |
| `subscribe` | Stream state changes, for building a UI |
| `key NAME` | Press a single key: `Return`, `Tab`, `BackSpace`, `Escape`, arrows |
| `info` | Which engine, recorder and injector are in use |

```sh
echo toggle | nc -U $XDG_RUNTIME_DIR/vox.sock
```

`subscribe` is what a microphone button uses to show listening and transcribing
without polling.

`key` exists because a touch panel has no physical keyboard to submit with.
Pressing a key and typing text are the same capability -- synthetic input into
the focused window -- so it reuses the virtual keyboard rather than growing a
second mechanism with its own device and permissions.

## Roadmap

- [x] Pluggable engines, autodetection, shared model directory
- [x] uinput injection working on X11 and Wayland alike
- [x] Socket API with state subscription
- [ ] Read the active keymap instead of assuming US-QWERTY
- [ ] Streaming transcription, so text appears while you speak
- [ ] Voice commands (punctuation, newline, corrections)
- [ ] A model downloader, so `vox models get base.en` just works

## Related

- **[hinged](https://github.com/denelson1-dot/hinged-convertible)** — tablet mode
  for Linux convertibles. Its on-screen input panel calls vox for dictation,
  which is what a tablet with no keyboard needs.

## License

MIT
