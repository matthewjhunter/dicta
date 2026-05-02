// Package audio captures microphone input and produces 80 ms / 1280-sample /
// 2560-byte int16-LE mono frames at 16 kHz (D15). It owns the energy VAD,
// frame ring buffer, mic-cue tone generator, and the IsQuiet helper.
//
// Producers and consumers across asr, wyoming, and (v2) wake assume this
// exact frame shape — there is no resampling or re-chunking elsewhere.
package audio
