#!/usr/bin/env bash
# Install dicta build and runtime dependencies on Ubuntu / Debian.
#
# Run with sudo. Verifies /etc/os-release matches Ubuntu or Debian
# before touching apt — refusing to run on the wrong distro keeps a
# misclick from installing 30 unwanted packages.
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
        *ubuntu*|*debian*) ;;
        *)
            echo "This script targets Ubuntu / Debian. Detected: ${ID:-unknown}" >&2
            echo "Use install-deps-fedora.sh or install-deps-arch.sh instead." >&2
            exit 1
            ;;
    esac
}

require_root
require_distro

# Build essentials for the dicta-preview CGo build (the daemon and CLI
# are pure Go and don't need these).
PACKAGES=(
    build-essential
    pkg-config

    # Gio + Wayland: the preview panel uses Gio with the nox11 build
    # tag. nox11 drops X11, so libxkbcommon-x11-dev / libxcb-* are NOT
    # needed. libvulkan-dev IS needed: Gio's Wayland code includes
    # vulkan headers unconditionally even when GLES is the runtime
    # fallback.
    libwayland-dev
    libxkbcommon-dev
    libgles-dev
    libegl1-mesa-dev
    libvulkan-dev
    libxcursor-dev

    # Runtime: input synthesis + clipboard + audio capture.
    ydotool
    wl-clipboard
    pipewire
    pipewire-pulse
)

echo "Updating apt metadata…"
apt-get update -qq

echo "Installing: ${PACKAGES[*]}"
DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends "${PACKAGES[@]}"

if ! command -v go >/dev/null; then
    cat <<'EOF'

Note: Go was not detected on PATH. Ubuntu's apt package (golang-go) is
often older than dicta requires. Install Go 1.25+ from
https://go.dev/dl/ or via your toolchain manager (g, asdf, etc.) before
running `task build:all`.
EOF
fi

cat <<'EOF'

Done. Next steps:

  task build:all       # build dictad, dicta, dicta-preview
  task install:user    # install to ~/.local/bin and ~/.config/systemd/user

Then enable ydotoold (run as your user) per its upstream docs and
follow the README from "Bring up an ASR backend" onward.
EOF
