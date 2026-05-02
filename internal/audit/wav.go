package audit

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
)

// WAV format constants matching the locked PCM frame format (D15):
// 16 kHz, mono, int16 LE.
const (
	wavSampleRate    = 16000
	wavNumChannels   = 1
	wavBitsPerSample = 16
)

// writeWAVFile writes pcm to path as a 16 kHz mono int16 LE WAV file.
// The directory is created if missing (with 0700). The file is opened
// 0600 so audit data isn't world-readable on multi-user systems.
//
// Format: standard 44-byte RIFF/WAVE/fmt/data header followed by the
// raw PCM bytes. No metadata chunks (LIST/INFO) — the JSONL row
// already carries the metadata.
func writeWAVFile(path string, pcm []byte) error {
	if len(pcm)%2 != 0 {
		// int16 LE means an even number of bytes; an odd count would
		// produce a half-truncated last sample. Reject so we don't
		// silently corrupt the data field.
		return fmt.Errorf("audit: pcm length %d is not aligned to int16", len(pcm))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("audit: mkdir %s: %w", filepath.Dir(path), err)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("audit: create %s: %w", path, err)
	}
	defer f.Close()

	if err := writeWAVHeader(f, len(pcm)); err != nil {
		return err
	}
	if _, err := f.Write(pcm); err != nil {
		return fmt.Errorf("audit: write pcm: %w", err)
	}
	return nil
}

// writeWAVHeader writes a 44-byte canonical PCM/WAV header for the
// given data-section size in bytes.
//
//	Offset Field            Size  Value (for our locked format)
//	------ ---------------- ----- -------------------------------
//	0      "RIFF"           4
//	4      ChunkSize        4     36 + dataSize
//	8      "WAVE"           4
//	12     "fmt "           4
//	16     Subchunk1Size    4     16 (PCM)
//	20     AudioFormat      2     1  (PCM)
//	22     NumChannels      2     1  (mono)
//	24     SampleRate       4     16000
//	28     ByteRate         4     SampleRate * NumChannels * BitsPerSample/8
//	32     BlockAlign       2     NumChannels * BitsPerSample/8
//	34     BitsPerSample    2     16
//	36     "data"           4
//	40     Subchunk2Size    4     dataSize
func writeWAVHeader(f *os.File, dataSize int) error {
	byteRate := wavSampleRate * wavNumChannels * wavBitsPerSample / 8
	blockAlign := wavNumChannels * wavBitsPerSample / 8

	hdr := make([]byte, 44)
	copy(hdr[0:4], "RIFF")
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(36+dataSize))
	copy(hdr[8:12], "WAVE")
	copy(hdr[12:16], "fmt ")
	binary.LittleEndian.PutUint32(hdr[16:20], 16)
	binary.LittleEndian.PutUint16(hdr[20:22], 1)
	binary.LittleEndian.PutUint16(hdr[22:24], wavNumChannels)
	binary.LittleEndian.PutUint32(hdr[24:28], wavSampleRate)
	binary.LittleEndian.PutUint32(hdr[28:32], uint32(byteRate))
	binary.LittleEndian.PutUint16(hdr[32:34], uint16(blockAlign))
	binary.LittleEndian.PutUint16(hdr[34:36], wavBitsPerSample)
	copy(hdr[36:40], "data")
	binary.LittleEndian.PutUint32(hdr[40:44], uint32(dataSize))

	if _, err := f.Write(hdr); err != nil {
		return fmt.Errorf("audit: write wav header: %w", err)
	}
	return nil
}
