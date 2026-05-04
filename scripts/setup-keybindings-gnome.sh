#!/usr/bin/env bash
# Configure GNOME custom keybindings for dicta:
#   Pause       -> toggle type-mode session
#   Scroll Lock -> toggle clip-mode panel
#
# Each binding wraps the dicta CLI in `systemctl --user start dictad` so
# the daemon auto-launches on first press if it isn't already running
# (subsequent presses are instant; `systemctl start` is a no-op when the
# unit is active).
#
# Idempotent: re-running just re-applies the same settings. Existing
# user-defined custom-keybindings under other paths are preserved.
#
# Usage:
#   scripts/setup-keybindings-gnome.sh           # install
#   scripts/setup-keybindings-gnome.sh --uninstall
set -euo pipefail

SCHEMA_LIST='org.gnome.settings-daemon.plugins.media-keys'
SCHEMA_KB='org.gnome.settings-daemon.plugins.media-keys.custom-keybinding'
PREFIX='/org/gnome/settings-daemon/plugins/media-keys/custom-keybindings'
KEYS=(dicta-type dicta-clip)

require_gnome() {
    case "${XDG_CURRENT_DESKTOP:-}" in
        *GNOME*) ;;
        *)
            echo "This script targets GNOME. Detected XDG_CURRENT_DESKTOP=${XDG_CURRENT_DESKTOP:-unset}." >&2
            echo "For other compositors, bind Pause and Scroll_Lock manually — see CONFIGURATION.md." >&2
            exit 1
            ;;
    esac
    if ! command -v gsettings >/dev/null; then
        echo "gsettings not found on PATH. Install glib2 tooling and retry." >&2
        exit 1
    fi
}

require_dicta() {
    DICTA_BIN="${HOME}/.local/bin/dicta"
    if [[ ! -x "$DICTA_BIN" ]]; then
        echo "dicta CLI not found at $DICTA_BIN — run 'task install:user' first." >&2
        exit 1
    fi
}

# Read the current custom-keybindings list as one path per line. The
# gsettings get returns a GVariant array literal like @as [] or
# ['/path/a/', '/path/b/']; parse it without sourcing the literal.
read_list() {
    gsettings get "$SCHEMA_LIST" custom-keybindings \
        | sed -E "s/^@as //; s/^\[//; s/\]$//; s/'//g; s/, /\n/g" \
        | grep -v '^[[:space:]]*$' || true
}

# Write the list back as a GVariant array literal. Accepts paths via
# stdin, one per line.
write_list() {
    local entries
    entries=$(awk 'NF{printf "%s'"'"'%s'"'"'", (NR>1?", ":""), $0}')
    gsettings set "$SCHEMA_LIST" custom-keybindings "[${entries}]"
}

set_binding() {
    local key=$1 name=$2 command=$3 binding=$4
    local path="${PREFIX}/${key}/"
    gsettings set "${SCHEMA_KB}:${path}" name "$name"
    gsettings set "${SCHEMA_KB}:${path}" command "$command"
    gsettings set "${SCHEMA_KB}:${path}" binding "$binding"
}

reset_binding() {
    local key=$1
    local path="${PREFIX}/${key}/"
    # `gsettings reset-recursively` clears every key under the relocatable
    # schema path, so the entry disappears from dconf entirely.
    gsettings reset-recursively "${SCHEMA_KB}:${path}" 2>/dev/null || true
}

merge_paths() {
    # Merge our two paths into the existing list, deduplicating, and
    # write the result back.
    local existing
    existing=$(read_list)
    {
        printf '%s\n' "$existing"
        for k in "${KEYS[@]}"; do printf '%s/%s/\n' "$PREFIX" "$k"; done
    } | awk 'NF && !seen[$0]++' | write_list
}

remove_paths() {
    local existing keep
    existing=$(read_list)
    keep=$(printf '%s\n' "$existing" | awk -v p="$PREFIX" '
        $0 != p"/dicta-type/" && $0 != p"/dicta-clip/" && NF
    ')
    if [[ -z "$keep" ]]; then
        gsettings set "$SCHEMA_LIST" custom-keybindings '@as []'
    else
        printf '%s\n' "$keep" | write_list
    fi
}

install_bindings() {
    require_gnome
    require_dicta

    local cmd_type cmd_clip
    cmd_type="sh -c 'systemctl --user start dictad; exec ${DICTA_BIN} toggle_talk -mode type'"
    cmd_clip="sh -c 'systemctl --user start dictad; exec ${DICTA_BIN} toggle_talk -mode clip'"

    set_binding dicta-type 'Dicta toggle type-mode' "$cmd_type" 'Pause'
    set_binding dicta-clip 'Dicta toggle clip-mode panel' "$cmd_clip" 'Scroll_Lock'
    merge_paths

    cat <<EOF
Installed GNOME custom keybindings:
  Pause       -> ${cmd_type}
  Scroll_Lock -> ${cmd_clip}

Both wrappers auto-start dictad if it isn't running. To keep it warm at
login instead, run: systemctl --user enable --now dictad.service
EOF
}

uninstall_bindings() {
    require_gnome
    for k in "${KEYS[@]}"; do reset_binding "$k"; done
    remove_paths
    echo "Removed dicta keybindings."
}

case "${1:-install}" in
    install)   install_bindings ;;
    --uninstall|uninstall) uninstall_bindings ;;
    -h|--help)
        sed -n '2,/^set -euo/p' "$0" | sed -e '$d' -e 's/^# \?//'
        ;;
    *)
        echo "Unknown argument: $1" >&2
        echo "Usage: $0 [install|--uninstall]" >&2
        exit 2
        ;;
esac
