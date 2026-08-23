#!/usr/bin/env bash
#
# Install vox, the system-wide dictation service.

set -euo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
BIN="${XDG_BIN_HOME:-$HOME/.local/bin}"
UNIT="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
MODELS="${XDG_DATA_HOME:-$HOME/.local/share}/vox/models"
RULES=/etc/udev/rules.d

DO_UDEV=1
DRY=0
for arg in "$@"; do
    case "$arg" in
        --no-udev) DO_UDEV=0 ;;
        --dry-run) DRY=1 ;;
        -h|--help) echo "Usage: ./install.sh [--no-udev] [--dry-run]"; exit 0 ;;
        *) echo "unknown option: $arg" >&2; exit 2 ;;
    esac
done

say()  { printf '==> %s\n' "$*"; }
step() { if ((DRY)); then printf '    would run: %s\n' "$*"; else eval "$*"; fi; }

command -v go >/dev/null || { echo "Go is required to build vox" >&2; exit 1; }

say "Building"
step "CGO_ENABLED=0 go build -trimpath -o '$REPO/vox' ./cmd/vox"

say "Installing into $BIN"
step "mkdir -p '$BIN' '$MODELS'"
step "install -m755 '$REPO/vox' '$BIN/vox'"
step "install -m755 '$REPO/packaging/bin/vox-faster-whisper' '$BIN/vox-faster-whisper'"

if ((DO_UDEV)); then
    say "Installing the uinput rule (needs sudo)"
    echo "    This grants the ability to synthesize keystrokes into your session,"
    echo "    which is what dictation does. Skip with --no-udev to use xdotool/wtype."
    step "sudo install -m644 '$REPO/packaging/udev/70-vox-uinput.rules' '$RULES/'"
    step "sudo udevadm control --reload"
    step "sudo modprobe uinput || true"
fi

say "Installing the user service"
step "mkdir -p '$UNIT'"
step "install -m644 '$REPO/packaging/systemd/vox.service' '$UNIT/'"
step "systemctl --user daemon-reload"

say "Checking for an existing faster-whisper install to reuse"
if [[ -x "$HOME/.local/share/voice-dictation/venv/bin/python" ]]; then
    echo "    found ~/.local/share/voice-dictation/venv - vox will reuse it"
elif [[ -d "$HOME/.cache/faster-whisper" ]]; then
    echo "    found a faster-whisper model cache at ~/.cache/faster-whisper"
    echo "    point vox at it with:  export VOX_MODEL_DIR=$HOME/.cache/faster-whisper"
else
    echo "    none found; see the README for setting up an engine"
fi

cat <<MSG

Installed. Next:

  vox doctor                     what is present and what is missing
  systemctl --user enable --now vox
  vox toggle                     start dictating; run again to stop and type

Bind 'vox toggle' to a key in your desktop settings for push-to-talk.
MSG
