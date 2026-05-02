# systemd integration

`dictad` is a per-user daemon — install the unit under
`~/.config/systemd/user/` (not `/etc/systemd/system/`) so it runs as
your user with access to the graphical session and PipeWire.

## Install

```sh
install -d ~/.config/systemd/user
install -m 0644 packaging/systemd/dictad.service ~/.config/systemd/user/

systemctl --user daemon-reload
systemctl --user enable --now dictad.service
```

## Verify

```sh
systemctl --user status dictad.service
journalctl --user -u dictad.service -f
```

The daemon emits structured JSON logs to stdout; `journalctl` is the
expected reader.

## Hardening notes

The unit applies the §7.1 hardening posture. `MemoryDenyWriteExecute=true`
relies on `dictad` being pure Go (D13). If you build with CGo enabled,
or vendor a CGo dep into the daemon, the daemon will fail to start and
you'll see `Operation not permitted` in journal — that's the kernel
refusing to map writable+executable pages, not a bug.

Audit data (when `--audit-enabled`) is written to
`$XDG_DATA_HOME/dicta` (defaults to `~/.local/share/dicta`).
`ProtectHome=read-only` blocks writes everywhere else under `$HOME`,
which is the reason `ReadWritePaths=%h/.local/share/dicta` is in the
unit.

## Configuring flags

`ExecStart=/usr/local/bin/dictad` runs with no arguments by default.
To pass flags, override via `systemctl --user edit dictad.service`:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/dictad \
    --asr-backend wyoming \
    --asr-wyoming-addr tcp://localhost:10300 \
    --preview-binary /usr/local/bin/dicta-preview
```

Audit (debug) flags require explicit opt-in:

```ini
[Service]
ExecStart=
ExecStart=/usr/local/bin/dictad \
    --audit-enabled \
    --audit-keep-audio \
    --audit-retention-days 7
```
