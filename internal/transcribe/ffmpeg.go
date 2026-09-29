package transcribe

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"
)

// FFmpegTimeout bounds one audio conversion.
const FFmpegTimeout = 60 * time.Second

// ResolveFFmpeg finds ffmpeg from WACLI_FFMPEG, PATH, or ~/.local/bin.
func ResolveFFmpeg(env Environment) (string, error) {
	return findBinary(env, EnvFFmpeg, "ffmpeg", "install ffmpeg (for example `brew install ffmpeg` or `apt install ffmpeg`)")
}

// FFmpegArgs is the argv that converts in to 16 kHz mono PCM16 WAV at out.
func FFmpegArgs(ffmpeg, in, out string) []string {
	return []string{ffmpeg, "-nostdin", "-loglevel", "error", "-i", in, "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", out}
}

// ConvertToWAV runs ffmpeg to produce the WAV every engine reads.
func ConvertToWAV(ctx context.Context, ffmpeg, in, out string) error {
	// Absolute paths keep ffmpeg from reading a leading "-" as an option or
	// a "name:" prefix as a protocol.
	absIn, err := filepath.Abs(in)
	if err != nil {
		return fmt.Errorf("resolve audio path: %w", err)
	}
	absOut, err := filepath.Abs(out)
	if err != nil {
		return fmt.Errorf("resolve wav path: %w", err)
	}
	if _, err := runProcess(ctx, FFmpegArgs(ffmpeg, absIn, absOut), FFmpegTimeout, 64*1024); err != nil {
		var timeoutErr *processTimeoutError
		if errors.As(err, &timeoutErr) {
			return fmt.Errorf("ffmpeg conversion timed out after %s", FFmpegTimeout)
		}
		return fmt.Errorf("convert audio with ffmpeg: %w", err)
	}
	return nil
}

// WAVDuration reads a PCM WAV header and returns the audio length.
func WAVDuration(path string) (time.Duration, error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return 0, err
	}

	var riff [12]byte
	if _, err := io.ReadFull(f, riff[:]); err != nil {
		return 0, fmt.Errorf("read wav header: %w", err)
	}
	if string(riff[0:4]) != "RIFF" || string(riff[8:12]) != "WAVE" {
		return 0, fmt.Errorf("not a RIFF/WAVE file")
	}
	offset := int64(12)
	var byteRate uint32
	for {
		var hdr [8]byte
		if _, err := io.ReadFull(f, hdr[:]); err != nil {
			return 0, fmt.Errorf("wav has no data chunk")
		}
		offset += 8
		id := string(hdr[0:4])
		size := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		switch id {
		case "fmt ":
			var fmtChunk [16]byte
			if size < 16 {
				return 0, fmt.Errorf("short wav fmt chunk")
			}
			if _, err := io.ReadFull(f, fmtChunk[:]); err != nil {
				return 0, fmt.Errorf("read wav fmt chunk: %w", err)
			}
			byteRate = binary.LittleEndian.Uint32(fmtChunk[8:12])
			if _, err := f.Seek(offset+size+size%2, io.SeekStart); err != nil {
				return 0, err
			}
		case "data":
			if byteRate == 0 {
				return 0, fmt.Errorf("wav data chunk before fmt chunk")
			}
			// Streamed writers leave 0 or 0xFFFFFFFF; fall back to the file size.
			if remaining := info.Size() - offset; size == 0 || size == 0xFFFFFFFF || size > remaining {
				size = remaining
			}
			return time.Duration(size) * time.Second / time.Duration(byteRate), nil
		default:
			if _, err := f.Seek(size+size%2, io.SeekCurrent); err != nil {
				return 0, err
			}
		}
		offset += size + size%2
	}
}
