package transcribe

import (
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestFFmpegArgs(t *testing.T) {
	got := FFmpegArgs("/opt/bin/ffmpeg", "/tmp/in put.ogg", "/tmp/out put.wav")
	want := []string{"/opt/bin/ffmpeg", "-nostdin", "-loglevel", "error", "-i", "/tmp/in put.ogg", "-ar", "16000", "-ac", "1", "-c:a", "pcm_s16le", "-y", "/tmp/out put.wav"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("FFmpegArgs = %#v, want %#v", got, want)
	}
}

func TestResolveFFmpegPrefersOverride(t *testing.T) {
	override := writeExecutable(t, filepath.Join(t.TempDir(), "ffmpeg-custom"))
	got, err := ResolveFFmpeg(fakeEnv{
		vars: map[string]string{EnvFFmpeg: override},
		path: map[string]string{"ffmpeg": "/opt/bin/ffmpeg"},
	}.env())
	if err != nil || got != override {
		t.Fatalf("ResolveFFmpeg = %q, %v; want %q", got, err, override)
	}
	got, err = ResolveFFmpeg(fakeEnv{path: map[string]string{"ffmpeg": "/opt/bin/ffmpeg"}}.env())
	if err != nil || got != "/opt/bin/ffmpeg" {
		t.Fatalf("ResolveFFmpeg PATH = %q, %v", got, err)
	}
}

// writeTestWAV writes a PCM16 WAV with an extra chunk before the data chunk,
// as some encoders do.
func writeTestWAV(t *testing.T, path string, sampleRate, channels int, samples int) {
	t.Helper()
	data := make([]byte, samples*channels*2)
	list := []byte("INFOISFT\x05\x00\x00\x00test\x00")
	le := binary.LittleEndian
	var buf []byte
	buf = append(buf, "RIFF"...)
	buf = le.AppendUint32(buf, uint32(4+8+16+8+len(list)+len(list)%2+8+len(data)))
	buf = append(buf, "WAVE"...)
	buf = append(buf, "fmt "...)
	buf = le.AppendUint32(buf, 16)
	buf = le.AppendUint16(buf, 1)
	buf = le.AppendUint16(buf, uint16(channels))
	buf = le.AppendUint32(buf, uint32(sampleRate))
	buf = le.AppendUint32(buf, uint32(sampleRate*channels*2))
	buf = le.AppendUint16(buf, uint16(channels*2))
	buf = le.AppendUint16(buf, 16)
	buf = append(buf, "LIST"...)
	buf = le.AppendUint32(buf, uint32(len(list)))
	buf = append(buf, list...)
	if len(list)%2 == 1 {
		buf = append(buf, 0)
	}
	buf = append(buf, "data"...)
	buf = le.AppendUint32(buf, uint32(len(data)))
	buf = append(buf, data...)
	if err := os.WriteFile(path, buf, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestWAVDuration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "clip.wav")
	writeTestWAV(t, path, 16000, 1, 24000)
	got, err := WAVDuration(path)
	if err != nil {
		t.Fatalf("WAVDuration: %v", err)
	}
	if got != 1500*time.Millisecond {
		t.Fatalf("duration = %s, want 1.5s", got)
	}

	notWAV := filepath.Join(t.TempDir(), "clip.ogg")
	if err := os.WriteFile(notWAV, []byte("OggS not a wav file"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := WAVDuration(notWAV); err == nil {
		t.Fatal("expected an error for a non-WAV file")
	}
}

// TestConvertToWAVWithRealFFmpeg is the one optional integration test: it
// runs only when ffmpeg is installed, synthesizes a tone with lavfi (no real
// audio involved), and checks the conversion output.
func TestConvertToWAVWithRealFFmpeg(t *testing.T) {
	ffmpeg, err := exec.LookPath("ffmpeg")
	if err != nil {
		t.Skip("ffmpeg not on PATH")
	}
	dir := t.TempDir()
	ctx := context.Background()

	// Prefer Opus-in-Ogg like a WhatsApp voice note; fall back to WAV when
	// this ffmpeg has no libopus.
	src := filepath.Join(dir, "tone.ogg")
	gen := exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-c:a", "libopus", "-y", src)
	if out, err := gen.CombinedOutput(); err != nil {
		t.Logf("libopus unavailable (%v: %s); using 44.1 kHz stereo WAV", err, out)
		src = filepath.Join(dir, "tone.wav")
		gen = exec.Command(ffmpeg, "-nostdin", "-loglevel", "error", "-f", "lavfi", "-i", "sine=frequency=440:duration=1", "-ar", "44100", "-ac", "2", "-y", src)
		if out, err := gen.CombinedOutput(); err != nil {
			t.Fatalf("generate tone: %v: %s", err, out)
		}
	}

	wav := filepath.Join(dir, "out dir", "converted.wav")
	if err := os.MkdirAll(filepath.Dir(wav), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := ConvertToWAV(ctx, ffmpeg, src, wav); err != nil {
		t.Fatalf("ConvertToWAV: %v", err)
	}
	raw, err := os.ReadFile(wav)
	if err != nil {
		t.Fatal(err)
	}
	if len(raw) < 44 || string(raw[0:4]) != "RIFF" || string(raw[8:12]) != "WAVE" {
		t.Fatalf("output is not a WAV file")
	}
	le := binary.LittleEndian
	if channels, rate, bits := le.Uint16(raw[22:24]), le.Uint32(raw[24:28]), le.Uint16(raw[34:36]); channels != 1 || rate != 16000 || bits != 16 {
		t.Fatalf("format = %d ch, %d Hz, %d bit; want mono 16 kHz PCM16", channels, rate, bits)
	}
	d, err := WAVDuration(wav)
	if err != nil {
		t.Fatalf("WAVDuration: %v", err)
	}
	if d < 900*time.Millisecond || d > 1100*time.Millisecond {
		t.Fatalf("duration = %s, want about 1s", d)
	}

	if err := ConvertToWAV(ctx, ffmpeg, filepath.Join(dir, "missing.ogg"), wav); err == nil {
		t.Fatal("expected ffmpeg to fail on a missing input")
	}
}
