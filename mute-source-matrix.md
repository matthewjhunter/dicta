# Mute-Source Compatibility Matrix

Hardware test results for `--unmute-to-dictate` mute detection. See
`mute-source-design.md` §8.2 for the test plan and the motivating
goal: figure out which `mute.Source` works on which mics, and use
that data to decide whether evdev needs to be the next source built.

## Summary

| Mic | USB ID | pcm-zero | pipewire | evdev | Verdict |
|-----|--------|----------|----------|-------|---------|
| MXL AC-44 TAP | 15dd:0010 | ✓ | ✗ | ✗ | pcm-zero only (baseline) |
| SteelSeries Arctis Pro Wireless | 1038:1290 + 1038:1294 | ✗ (noise-gate false-mute) | ✗ | ✗ (no mute key in HID descriptor) | **Unsupportable** by any v1 source |
| Sennheiser/EPOS GSP 370 | 1395:009a | — | — | — | **Deferred** (headset needs charging) |
| Blue Yeti (older, ×2) | — | — | — | — | Not currently surfaced from storage |
| Motherboard analog input | — | — | — | — | Not yet tested (host-controlled-mute baseline) |

Last updated: 2026-05-14.

## Per-mic results

### MXL AC-44 TAP

USB ID: `15dd:0010`. PipeWire node:
`alsa_input.usb-MXL_MXL_AC-44_TAP-00.mono-fallback`. Evdev: no input
device (audio-only USB device, no HID interface).

This is the baseline the whole feature was built against.

- **pcm-zero: ✓** — Empirically: muted state produces 100% literal
  zeros (peak=0), unmuted-silent produces ~95% nonzero samples at
  ~-69 dBFS ADC noise floor. The gap is wide enough that a
  byte-level "any nonzero?" check works without threshold tuning.
- **pipewire: ✗** — Confirmed via `pw-cli enum-params 57 Route`:
  the AC-44's Device.Route.props.mute does not flip when the touch
  button is pressed. The button gates audio inside the device
  firmware before the USB endpoint and the host sees nothing
  through any documented control channel.
- **evdev: ✗** — The AC-44 enumerates as a pure USB audio device
  with no HID interface; there's no `/dev/input/event*` node for
  it at all.

Use: `--unmute-source=pcm-zero` (or `auto`, which picks pcm-zero
once it observes a transition).

### SteelSeries Arctis Pro Wireless

USB IDs: `1038:1290` (transmitter) + `1038:1294` (receiver/dongle).
PipeWire node: `alsa_input.usb-SteelSeries_Arctis_Pro_Wireless-00.mono-chat`.
Evdev nodes: `event13` ("Consumer Control") and `event14`.

The Arctis turned out to be the worst-case mic in the lab — mute is
invisible through every documented channel.

- **pipewire: ✗** — `dicta probe-mute --device Arctis --seconds 15`
  saw the initial state but no transitions when the physical mute
  button was pressed. Confirmed: the button does not flip
  `Device.Route.props.mute`.

- **evdev: ✗** — `evtest /dev/input/event13` reports only these key
  codes in the device's HID descriptor:

  ```
  KEY_VOLUMEDOWN (114)
  KEY_VOLUMEUP (115)
  KEY_NEXTSONG (163)
  KEY_PLAYPAUSE (164)
  KEY_PREVIOUSSONG (165)
  ```

  No `KEY_MUTE` (113), no `KEY_MICMUTE` (248), no anything
  mute-shaped. The mute key code isn't even *exposed* by the
  device, let alone fired when the button is pressed. `event14`
  exposes only `EV_ABS / ABS_MISC` (range 0–65535), which is
  almost certainly a battery-level telemetry axis — not mute.

- **pcm-zero: ✗ (with caveat)** — `pw-record` captures of the chat
  input show all-zero PCM both when the OS-level mute is on AND
  when it's off but the mic is idle. The Arctis chat mic has a
  digital noise gate that outputs literal zeros below some
  threshold even when unmuted; sample data only appears when
  acoustic input crosses that threshold. pcm-zero would therefore:

  - Correctly detect mute when muted
  - Falsely detect mute every time the user pauses between
    sentences while unmuted
  - Flap to unmuted only during active speech

  That's worse than not working — the watcher would close the
  dictation session on every inter-utterance pause. **Do not use
  `--unmute-source=pcm-zero` on this mic.**

The mute button and LED state live entirely inside the wireless
dongle's firmware, with no Linux-visible signal. Supporting this
mic would require reverse-engineering SteelSeries' vendor protocol
(equivalent of the "SteelSeries GG" software on Windows). Not a v1
or v2 candidate.

Use: do not enable `--unmute-to-dictate` for this mic. The Pause
keystroke continues to work.

### Sennheiser/EPOS GSP 370

USB ID: `1395:009a` (DSEA A/S). Evdev: `event24` (kbd handler).

Testing deferred — the GSP 370 needed to charge before the test
pass. Predicted result based on EPOS family behavior:

- **evdev: likely ✓** — EPOS gaming/UC headsets typically implement
  the HID Telephony Usage Page (0x0B, usage 0x2F "Phone Mute") and
  emit `KEY_MICMUTE` when the boom arm is raised. The `event24`
  device having a kbd handler is consistent with that.
- **pipewire: unknown** — some EPOS firmware also flips UAC mute in
  parallel with the HID event; some doesn't.
- **pcm-zero: unknown** — probably no, since EPOS mute is typically
  a physical microphone mute relay (capsule disconnected) rather
  than a digital zero gate.

Test command for when the headset is charged:

```
sudo evtest /dev/input/event24
# raise/lower the boom arm; watch for KEY_MICMUTE / KEY_MUTE
dicta probe-mute --device GSP --seconds 15
```

If `KEY_MICMUTE` fires on `event24`, the GSP 370 is the confirmed
case for adding evdev as the next `mute.Source` implementation. Per
the design doc's promotion criterion (§8.2), two or more mics
needing evdev would move it from follow-up to next increment. The
Arctis already doesn't qualify (no key codes at all), so the GSP
370 carries the decision by itself.

### Blue Yeti (older, ×2)

Reported in storage; not currently surfaced. Older Yeti firmware is
expected to flip the ALSA capture switch on physical button press,
which would make `pipewire` route.mute fire. Worth verifying when
the units are dug out; the result will mostly matter for "is
pipewire actually useful for any of our mics" — none of the
currently-attached hardware exercises the pipewire path.

### Motherboard analog input

PipeWire node: `alsa_input.pci-0000_c3_00.6.analog-stereo`.

Not yet tested. Useful as a "host-controlled-mute only" baseline
since there's no physical button — `wpctl set-mute` is the only way
to mute it, which means pipewire `route.mute` should fire on every
mute change by definition. Confirms the pipewire source is healthy
even if no user-attached mic exercises it.

## Implications for the next feature increment

After two of three planned tests (AC-44 confirmed pcm-zero only,
Arctis confirmed unsupportable), the remaining test (GSP 370) is
the deciding data point for whether evdev gets built next:

- **If GSP 370 fires `KEY_MICMUTE` on `event24`**: build the evdev
  source. We have at least one mic in the lab that only works via
  evdev, which justifies the work even though the matrix would
  show "1 of 3 mics needs it" rather than "2+ of 3."
- **If GSP 370 also doesn't fire anything**: evdev gets reconsidered
  entirely. Three for three "vendor-only" mics in the lab would mean
  evdev wouldn't help any of them, and the effort would be
  speculative until different hardware shows up.

Recording the negative result on the Arctis was the surprise value
here — going in, we expected most non-AC-44 mics to expose mute
through *some* documented channel. The Arctis demonstrates the
"vendor firmware only" failure mode is real, not just theoretical,
and worth calling out for users who might otherwise spend time
debugging why `--unmute-to-dictate` doesn't work for them.
