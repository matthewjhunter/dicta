#!/usr/bin/env bash
# Install dicta build and runtime dependencies on Arch / Manjaro.
#
# Run with sudo. Verifies /etc/os-release matches Arch before touching
# pacman.
set -euo pipefail

require_root() {
    if [[ $EUID -ne 0 ]]; then
        echo "This script must be run as root (sudo)." >&2
        exit 1
    fi
}

require_distro() {
    if [[ ! -r /etc/os-release ]]; then
        echo "Cannot read /etc/os-release; aborting." >&2
        exit 1
    fi
    . /etc/os-release
    case "${ID:-}${ID_LIKE:-}" in
        *arch*|*manjaro*|*endeavouros*) ;;
        *)
            echo "This script targets Arch-family distros. Detected: ${ID:-unknown}" >&2
            echo "Use install-deps-ubuntu.sh or install-deps-fedora.sh instead." >&2
            exit 1
            ;;
    esac
}

require_root
require_distro

# Build deps for dicta-preview's CGo build. Daemon + CLI are pure Go.
# Most "*-devel" Fedora/Ubuntu packages are split out on Arch — the
# main package usually carries the headers.
PACKAGES=(
    base-devel
    pkgconf

    # Gio + Wayland (nox11 build tag drops the X11 deps).
    wayland
    libxkbcommon
    mesa
    vulkan-headers
    vulkan-icd-loader
    libxcursor

    # Runtime: input synthesis + clipboard + audio.
    ydotool
    wl-clipboard
    pipewire
    pipewire-pulse

    # Go is in the official repos and current on Arch.
    go
)

echo "Installing: ${PACKAGES[*]}"
pacman -S --needed --noconfirm "${PACKAGES[@]}"

cat <<'EOF'

Done. Next steps:

  task build:all       # build dictad, dicta, dicta-preview
  task install:user    # install to ~/.local/bin and ~/.config/systemd/user

Then enable ydotoold (run as your user) per its upstream docs and
follow the README from "Bring up an ASR backend" onward.
EOF
