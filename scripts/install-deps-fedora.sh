#!/usr/bin/env bash
# Install dicta build and runtime dependencies on Fedora.
#
# Run with sudo. Verifies /etc/os-release matches Fedora before
# touching dnf.
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
        *fedora*|*rhel*|*centos*) ;;
        *)
            echo "This script targets Fedora / RHEL. Detected: ${ID:-unknown}" >&2
            echo "Use install-deps-ubuntu.sh or install-deps-arch.sh instead." >&2
            exit 1
            ;;
    esac
}

require_root
require_distro

# Build deps for dicta-preview's CGo build. Daemon + CLI are pure Go.
PACKAGES=(
    gcc
    pkgconf

    # Gio + Wayland (nox11 build tag drops the X11 deps).
    wayland-devel
    libxkbcommon-devel
    mesa-libGLES-devel
    mesa-libEGL-devel
    vulkan-loader-devel
    libXcursor-devel

    # Runtime: input synthesis + clipboard + audio.
    ydotool
    wl-clipboard
    pipewire
    pipewire-pulseaudio
)

echo "Installing: ${PACKAGES[*]}"
dnf install -y --setopt=install_weak_deps=False "${PACKAGES[@]}"

if ! command -v go >/dev/null; then
    cat <<'EOF'

Note: Go was not detected on PATH. Fedora's `golang` package is
usually current, but if you're on an older release install Go 1.25+
from https://go.dev/dl/ or via your toolchain manager.
EOF
fi

cat <<'EOF'

Done. Next steps:

  task build:all       # build dictad, dicta, dicta-preview
  task install:user    # install to ~/.local/bin and ~/.config/systemd/user

Then enable ydotoold (run as your user) per its upstream docs and
follow the README from "Bring up an ASR backend" onward.
EOF
