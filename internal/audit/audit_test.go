package audit

import (
	"bufio"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestWriter returns a fileWriter rooted at t.TempDir() with the
// allowlist relaxed to that tempdir so tests don't have to mess with
// XDG_DATA_HOME.
func newTestWriter(t *testing.T, cfg Config) *fileWriter {
	t.Helper()
	dir := t.TempDir()
	cfg.Enabled = true
	cfg.Directory = dir
	cfg.PathAllowlist = []string{dir}
	w, err := New(cfg, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	fw, ok := w.(*fileWriter)
	if !ok {
		t.Fatalf("expected *fileWriter; got %T", w)
	}
	t.Cleanup(func() { _ = w.Close() })
	return fw
}

func TestNew_DisabledReturnsPassthrough(t *testing.T) {
	w, err := New(Config{Enabled: false}, discardLogger())
	if err != nil {
		t.Fatalf("New disabled: %v", err)
	}
	// Record + Close are no-ops; no disk activity at all.
	if err := w.Record(Record{RawText: "hello"}); err != nil {
		t.Errorf("passthrough Record: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("passthrough Close: %v", err)
	}
}

func TestNew_RejectsRelativeDirectory(t *testing.T) {
	_, err := New(Config{Enabled: true, Directory: "relative/path"}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("got %v want absolute-path error", err)
	}
}

func TestNew_RejectsOffAllowlistDirectory(t *testing.T) {
	_, err := New(Config{
		Enabled:       true,
		Directory:     "/etc/dicta",
		PathAllowlist: []string{"/var/lib/dicta"},
	}, discardLogger())
	if err == nil || !strings.Contains(err.Error(), "allowlist") {
		t.Errorf("got %v want allowlist error", err)
	}
}

func TestNew_DefaultsDirectoryToXDG(t *testing.T) {
	xdg := t.TempDir()
	t.Setenv("XDG_DATA_HOME", xdg)
	want := filepath.Join(xdg, "dicta")
	w, err := New(Config{Enabled: true}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	fw := w.(*fileWriter)
	if fw.dir != want {
		t.Errorf("dir: got %q want %q", fw.dir, want)
	}
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected MkdirAll to create %s; stat err %v", want, err)
	}
}

func TestRecord_WritesJSONLLine(t *testing.T) {
	fw := newTestWriter(t, Config{})
	rec := Record{
		Timestamp:        time.Date(2026, 5, 2, 12, 30, 45, 0, time.UTC),
		Mode:             "type",
		UtteranceID:      "u1714621234-1",
		Backend:          "wyoming",
		RawText:          "hello world",
		Language:         "en",
		ASRLatencyMs:     250,
		CleanupLatencyMs: 0,
	}
	if err := fw.Record(rec); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	jpath := filepath.Join(fw.dir, "2026-05-02", "transcripts.jsonl")
	data, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatalf("read jsonl: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v (line=%q)", err, string(data))
	}
	if got.Mode != "type" || got.RawText != "hello world" {
		t.Errorf("decoded record mismatch: %+v", got)
	}
	if got.UtteranceID != "u1714621234-1" {
		t.Errorf("UtteranceID lost: %+v", got)
	}
	// PCM tag is "-" so it must not appear in the JSONL.
	if strings.Contains(string(data), "PCM") || strings.Contains(string(data), "pcm") {
		t.Errorf("PCM field leaked into JSONL: %s", string(data))
	}
}

func TestRecord_AppendsMultipleRecords(t *testing.T) {
	fw := newTestWriter(t, Config{})
	for i := range 3 {
		if err := fw.Record(Record{
			Timestamp: time.Date(2026, 5, 2, 12, i, 0, 0, time.UTC),
			Mode:      "clip",
			RawText:   "line",
		}); err != nil {
			t.Fatalf("Record %d: %v", i, err)
		}
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	jpath := filepath.Join(fw.dir, "2026-05-02", "transcripts.jsonl")
	f, err := os.Open(jpath)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	lines := 0
	for scanner.Scan() {
		lines++
	}
	if lines != 3 {
		t.Errorf("got %d lines want 3", lines)
	}
}

func TestRecord_RotatesByDay(t *testing.T) {
	fw := newTestWriter(t, Config{})
	day1 := time.Date(2026, 5, 2, 23, 59, 59, 0, time.UTC)
	day2 := time.Date(2026, 5, 3, 0, 0, 1, 0, time.UTC)

	if err := fw.Record(Record{Timestamp: day1, Mode: "type", RawText: "before midnight"}); err != nil {
		t.Fatalf("Record day1: %v", err)
	}
	if err := fw.Record(Record{Timestamp: day2, Mode: "type", RawText: "after midnight"}); err != nil {
		t.Fatalf("Record day2: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	for _, day := range []string{"2026-05-02", "2026-05-03"} {
		jpath := filepath.Join(fw.dir, day, "transcripts.jsonl")
		if _, err := os.Stat(jpath); err != nil {
			t.Errorf("expected %s; stat err %v", jpath, err)
		}
	}
}

func TestRecord_FillsTimestampWhenZero(t *testing.T) {
	fw := newTestWriter(t, Config{})
	before := time.Now().Add(-time.Second)
	if err := fw.Record(Record{Mode: "type", RawText: "implicit ts"}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	after := time.Now().Add(time.Second)
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	day := time.Now().Format("2006-01-02")
	jpath := filepath.Join(fw.dir, day, "transcripts.jsonl")
	data, err := os.ReadFile(jpath)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var got Record
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Timestamp.Before(before) || got.Timestamp.After(after) {
		t.Errorf("Timestamp %v not in [%v,%v]", got.Timestamp, before, after)
	}
}

func TestRecord_KeepAudioFalseSkipsWAV(t *testing.T) {
	fw := newTestWriter(t, Config{KeepAudio: false})
	pcm := make([]byte, 1280)
	if err := fw.Record(Record{
		Timestamp:   time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		Mode:        "type",
		UtteranceID: "u1",
		RawText:     "x",
		PCM:         pcm,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	audioDir := filepath.Join(fw.dir, "2026-05-02", "audio")
	if _, err := os.Stat(audioDir); !os.IsNotExist(err) {
		t.Errorf("expected audio dir absent (KeepAudio=false); stat=%v", err)
	}
}

func TestRecord_KeepAudioTrueWritesWAV(t *testing.T) {
	fw := newTestWriter(t, Config{KeepAudio: true})
	// 80 ms / 1280-byte chunk per D15.
	pcm := make([]byte, 1280)
	for i := 0; i < len(pcm); i += 2 {
		binary.LittleEndian.PutUint16(pcm[i:], uint16(i))
	}
	if err := fw.Record(Record{
		Timestamp:   time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		Mode:        "clip",
		UtteranceID: "u1714621234-7",
		RawText:     "audio test",
		PCM:         pcm,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wavPath := filepath.Join(fw.dir, "2026-05-02", "audio", "u1714621234-7.wav")
	data, err := os.ReadFile(wavPath)
	if err != nil {
		t.Fatalf("read wav: %v", err)
	}
	if len(data) != 44+1280 {
		t.Errorf("wav size: got %d want %d", len(data), 44+1280)
	}
	if string(data[:4]) != "RIFF" || string(data[8:12]) != "WAVE" {
		t.Errorf("wav magic mismatch: %q %q", data[:4], data[8:12])
	}
	// SampleRate at offset 24, NumChannels at offset 22, BitsPerSample at 34.
	if rate := binary.LittleEndian.Uint32(data[24:28]); rate != 16000 {
		t.Errorf("SampleRate: got %d want 16000", rate)
	}
	if ch := binary.LittleEndian.Uint16(data[22:24]); ch != 1 {
		t.Errorf("NumChannels: got %d want 1", ch)
	}
	if bps := binary.LittleEndian.Uint16(data[34:36]); bps != 16 {
		t.Errorf("BitsPerSample: got %d want 16", bps)
	}

	// JSONL row should reference the relative path.
	jpath := filepath.Join(fw.dir, "2026-05-02", "transcripts.jsonl")
	jdata, _ := os.ReadFile(jpath)
	var got Record
	_ = json.Unmarshal(jdata, &got)
	if got.AudioPath != filepath.Join("audio", "u1714621234-7.wav") {
		t.Errorf("AudioPath: got %q", got.AudioPath)
	}
}

func TestRecord_SanitizesUtteranceIDForPath(t *testing.T) {
	fw := newTestWriter(t, Config{KeepAudio: true})
	pcm := make([]byte, 1280)
	if err := fw.Record(Record{
		Timestamp:   time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		UtteranceID: "../etc/passwd",
		RawText:     "x",
		PCM:         pcm,
	}); err != nil {
		t.Fatalf("Record: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// File should land inside audio/, not escape.
	expected := filepath.Join(fw.dir, "2026-05-02", "audio")
	entries, err := os.ReadDir(expected)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 audio file; got %d", len(entries))
	}
	name := entries[0].Name()
	if strings.Contains(name, "..") || strings.Contains(name, "/") {
		t.Errorf("filename %q not sanitized", name)
	}
}

func TestRecord_OddPCMRejectedButJSONLStillWritten(t *testing.T) {
	fw := newTestWriter(t, Config{KeepAudio: true})
	pcm := []byte{1, 2, 3} // odd length: not int16-aligned
	if err := fw.Record(Record{
		Timestamp:   time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		UtteranceID: "u1",
		RawText:     "x",
		PCM:         pcm,
	}); err != nil {
		t.Fatalf("Record should succeed (JSONL row valid even if WAV fails): %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// JSONL written, AudioPath empty.
	jpath := filepath.Join(fw.dir, "2026-05-02", "transcripts.jsonl")
	data, _ := os.ReadFile(jpath)
	var got Record
	_ = json.Unmarshal(data, &got)
	if got.AudioPath != "" {
		t.Errorf("AudioPath: got %q want empty (WAV write failed)", got.AudioPath)
	}
}

func TestClose_Idempotent(t *testing.T) {
	fw := newTestWriter(t, Config{})
	if err := fw.Record(Record{Mode: "type", RawText: "x"}); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Errorf("first Close: %v", err)
	}
	if err := fw.Close(); err != nil {
		t.Errorf("second Close: %v", err)
	}
	if err := fw.Record(Record{Mode: "type", RawText: "y"}); err == nil {
		t.Error("Record after Close should fail")
	}
}

func TestRetention_KeepsForeverWhenZero(t *testing.T) {
	fw := newTestWriter(t, Config{RetentionDays: 0})
	// Manufacture an old day-dir.
	oldDir := filepath.Join(fw.dir, "2024-01-01")
	if err := os.MkdirAll(oldDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Write something modern.
	if err := fw.Record(Record{
		Timestamp: time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC),
		Mode:      "type",
		RawText:   "x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := fw.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldDir); err != nil {
		t.Errorf("RetentionDays=0 should keep forever; old dir missing: %v", err)
	}
}

func TestRetention_DeletesOldDayDirs(t *testing.T) {
	fw := newTestWriter(t, Config{RetentionDays: 7})
	// Override now so the cutoff is deterministic.
	fixed := time.Date(2026, 5, 10, 12, 0, 0, 0, time.Local)
	fw.now = func() time.Time { return fixed }

	old := filepath.Join(fw.dir, "2026-05-01")    // 9 days old, prune
	keep := filepath.Join(fw.dir, "2026-05-05")   // 5 days old, keep
	notDay := filepath.Join(fw.dir, "extra-data") // not day-shaped, keep
	for _, d := range []string{old, keep, notDay} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	if err := fw.sweepRetention(); err != nil {
		t.Fatalf("sweepRetention: %v", err)
	}

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("old dir should be deleted; stat=%v", err)
	}
	if _, err := os.Stat(keep); err != nil {
		t.Errorf("recent dir should be kept; stat=%v", err)
	}
	if _, err := os.Stat(notDay); err != nil {
		t.Errorf("non-day-shaped dir should be left alone; stat=%v", err)
	}
}

func TestRetention_RunsAtStartup(t *testing.T) {
	dir := t.TempDir()
	old := filepath.Join(dir, "2024-01-01")
	if err := os.MkdirAll(old, 0o700); err != nil {
		t.Fatal(err)
	}
	w, err := New(Config{
		Enabled:       true,
		Directory:     dir,
		PathAllowlist: []string{dir},
		RetentionDays: 1,
	}, discardLogger())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer w.Close()
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("startup sweep should have removed %s; stat=%v", old, err)
	}
}

func TestPassthrough_DoesNotTouchFilesystem(t *testing.T) {
	// Even if a filesystem path that would be problematic is named,
	// passthrough never touches it because it's nil-side.
	w := Passthrough()
	if err := w.Record(Record{RawText: "x"}); err != nil {
		t.Errorf("Passthrough.Record: %v", err)
	}
	if err := w.Close(); err != nil {
		t.Errorf("Passthrough.Close: %v", err)
	}
}

func TestParseDayDir(t *testing.T) {
	for _, tc := range []struct {
		in string
		ok bool
	}{
		{"2026-05-02", true},
		{"2026-05-2", false},  // strict format
		{"2026-13-01", false}, // invalid month
		{"audio", false},
		{"", false},
		{"2026-05-02-extra", false},
	} {
		_, ok := parseDayDir(tc.in)
		if ok != tc.ok {
			t.Errorf("parseDayDir(%q): got ok=%v want %v", tc.in, ok, tc.ok)
		}
	}
}
