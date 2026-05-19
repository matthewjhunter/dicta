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

## ydotoold fd-leak workaround

`ydotoold` (the input-synthesis daemon dicta talks to in type-mode)
leaks accept'd client sockets — roughly one per dictation invocation.
With the default `LimitNOFILE=1024`, a moderately active user wedges
the daemon in about a week: new `ydotool` clients block forever in
`unix_wait_for_peer` with the listen backlog full. From dicta's side
this looks like "audio captures and processes, but nothing is typed";
the audit log (when enabled) still records transcripts.

Confirmed on Ubuntu 25.10 with ydotool 1.0.4. Tracked upstream is on
the to-do list; the symptom is well-defined enough to paper over.

Two example units in this directory implement the workaround:

```sh
install -m 0644 packaging/systemd/ydotoold.service ~/.config/systemd/user/
install -m 0644 packaging/systemd/ydotoold-restart.service ~/.config/systemd/user/
install -m 0644 packaging/systemd/ydotoold-restart.timer ~/.config/systemd/user/

systemctl --user daemon-reload
systemctl --user enable --now ydotoold.service
systemctl --user enable --now ydotoold-restart.timer
```

What this does:

- `ydotoold.service` bumps `LimitNOFILE` to 65536 — enough headroom
  that even heavy daily use stays well below the ceiling.
- `ydotoold-restart.timer` fires daily (with up to 15 min jitter) and
  triggers `ydotoold-restart.service`, which restarts ydotoold to drop
  accumulated leaked sockets. `Persistent=true` so a missed run on a
  suspended laptop catches up on resume.

Verify:

```sh
systemctl --user list-timers ydotoold-restart.timer
systemctl --user show ydotoold.service -p LimitNOFILE
ls /proc/$(systemctl --user show -p MainPID --value ydotoold.service)/fd | wc -l
```

If you already wedged ydotoold (typing silently broken), restart it
once by hand — `systemctl --user restart ydotoold.service`. The timer
prevents it from happening again.

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
