package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeEnv builds an Environment from maps so tests never read the real
// environment (which may select an engine) or PATH.
type fakeEnv struct {
	vars map[string]string
	path map[string]string
	home string
}

func (f fakeEnv) env() Environment {
	return Environment{
		Getenv: func(name string) string { return f.vars[name] },
		LookPath: func(name string) (string, error) {
			if p, ok := f.path[name]; ok {
				return p, nil
			}
			return "", exec.ErrNotFound
		},
		HomeDir: func() (string, error) {
			if f.home == "" {
				return "", errors.New("no home")
			}
			return f.home, nil
		},
	}
}

func writeExecutable(t *testing.T, path string) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeFile(t *testing.T, path string) string {
	t.Helper()
	if err := os.WriteFile(path, []byte("model"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

const testWAV = "/tmp/voice notes/clip one.wav"

func TestResolveEngineDefaultsToNemoSpeech(t *testing.T) {
	env := fakeEnv{path: map[string]string{"nemo-speech": "/opt/bin/nemo-speech"}}.env()
	engine, err := ResolveEngine("", env)
	if err != nil {
		t.Fatalf("ResolveEngine: %v", err)
	}
	if engine.Name != EngineNemoSpeech || engine.Model != "parakeet-tdt" || engine.Timeout != DefaultTimeout {
		t.Fatalf("engine = %+v", engine)
	}
	want := []string{"/opt/bin/nemo-speech", "--quiet", "transcribe", testWAV, "--model", "parakeet-tdt"}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
}

func TestResolveEngineModelOverride(t *testing.T) {
	env := fakeEnv{
		vars: map[string]string{EnvModel: "nemotron-3.5"},
		path: map[string]string{"nemo-speech": "/opt/bin/nemo-speech"},
	}.env()
	engine, err := ResolveEngine("", env)
	if err != nil {
		t.Fatalf("ResolveEngine: %v", err)
	}
	want := []string{"/opt/bin/nemo-speech", "--quiet", "transcribe", testWAV, "--model", "nemotron-3.5"}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) || engine.Model != "nemotron-3.5" {
		t.Fatalf("args = %#v model = %q", got, engine.Model)
	}
}

func TestResolveEngineSelectionPrecedence(t *testing.T) {
	env := fakeEnv{
		vars: map[string]string{EnvEngine: "fluidaudio"},
		path: map[string]string{"nemo-speech": "/opt/bin/nemo-speech", "fluidaudiocli": "/opt/bin/fluidaudiocli"},
	}.env()
	for _, tc := range []struct{ flag, want string }{
		{flag: "", want: EngineFluidAudio},
		{flag: "nemo-speech", want: EngineNemoSpeech},
		{flag: " NEMO-SPEECH ", want: EngineNemoSpeech},
	} {
		engine, err := ResolveEngine(tc.flag, env)
		if err != nil {
			t.Fatalf("ResolveEngine(%q): %v", tc.flag, err)
		}
		if engine.Name != tc.want {
			t.Fatalf("ResolveEngine(%q) = %q, want %q", tc.flag, engine.Name, tc.want)
		}
	}
	if got := SelectedEngineName("", fakeEnv{}.env()); got != DefaultEngine {
		t.Fatalf("default engine = %q", got)
	}
}

func TestResolveEngineFluidAudio(t *testing.T) {
	env := fakeEnv{path: map[string]string{"fluidaudiocli": "/opt/bin/fluidaudiocli"}}.env()
	engine, err := ResolveEngine(EngineFluidAudio, env)
	if err != nil {
		t.Fatalf("ResolveEngine: %v", err)
	}
	want := []string{"/opt/bin/fluidaudiocli", "transcribe", testWAV, "--model-version", "ultra"}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) || engine.Model != "ultra" {
		t.Fatalf("args = %#v model = %q", got, engine.Model)
	}

	env = fakeEnv{
		vars: map[string]string{EnvModel: "v3"},
		path: map[string]string{"fluidaudiocli": "/opt/bin/fluidaudiocli"},
	}.env()
	engine, err = ResolveEngine(EngineFluidAudio, env)
	if err != nil {
		t.Fatalf("ResolveEngine model override: %v", err)
	}
	if got := engine.Args(testWAV); got[len(got)-1] != "v3" || engine.Model != "v3" {
		t.Fatalf("args = %#v model = %q", got, engine.Model)
	}
}

func TestResolveEngineBinaryOverrideAndLocalBin(t *testing.T) {
	dir := t.TempDir()
	override := writeExecutable(t, filepath.Join(dir, "custom", "nemo-speech"))
	env := fakeEnv{
		vars: map[string]string{EnvNemoSpeech: override},
		path: map[string]string{"nemo-speech": "/opt/bin/nemo-speech"},
	}.env()
	engine, err := ResolveEngine("", env)
	if err != nil {
		t.Fatalf("ResolveEngine override: %v", err)
	}
	if got := engine.Args(testWAV)[0]; got != override {
		t.Fatalf("binary = %q, want override %q", got, override)
	}

	home := filepath.Join(dir, "home")
	local := writeExecutable(t, filepath.Join(home, ".local", "bin", "fluidaudiocli"))
	engine, err = ResolveEngine(EngineFluidAudio, fakeEnv{home: home}.env())
	if err != nil {
		t.Fatalf("ResolveEngine ~/.local/bin: %v", err)
	}
	if got := engine.Args(testWAV)[0]; got != local {
		t.Fatalf("binary = %q, want %q", got, local)
	}

	notExec := writeFile(t, filepath.Join(dir, "plain-file"))
	_, err = ResolveEngine("", fakeEnv{vars: map[string]string{EnvNemoSpeech: notExec}}.env())
	if err == nil || !strings.Contains(err.Error(), EnvNemoSpeech) {
		t.Fatalf("non-executable override error = %v, want it to name %s", err, EnvNemoSpeech)
	}
}

func TestResolveEngineMissingBinaryNamesEnvVar(t *testing.T) {
	for engine, envVar := range map[string]string{
		EngineNemoSpeech:  EnvNemoSpeech,
		EngineFluidAudio:  EnvFluidAudio,
		EngineParakeetCpp: EnvParakeetCLI,
		EngineWhisperCpp:  EnvWhisperCLI,
	} {
		_, err := ResolveEngine(engine, fakeEnv{home: t.TempDir()}.env())
		if err == nil || !strings.Contains(err.Error(), envVar) || !strings.Contains(err.Error(), "not found") {
			t.Fatalf("%s missing binary error = %v, want it to name %s", engine, err, envVar)
		}
	}
	_, err := ResolveFFmpeg(fakeEnv{home: t.TempDir()}.env())
	if err == nil || !strings.Contains(err.Error(), EnvFFmpeg) {
		t.Fatalf("missing ffmpeg error = %v, want it to name %s", err, EnvFFmpeg)
	}
}

func TestResolveEngineModelFileEngines(t *testing.T) {
	dir := t.TempDir()
	model := writeFile(t, filepath.Join(dir, "model.bin"))
	bins := map[string]string{"parakeet-cli": "/opt/bin/parakeet-cli", "whisper-cli": "/opt/bin/whisper-cli"}

	for _, name := range []string{EngineParakeetCpp, EngineWhisperCpp} {
		_, err := ResolveEngine(name, fakeEnv{path: bins}.env())
		if err == nil || !strings.Contains(err.Error(), EnvModel) {
			t.Fatalf("%s without model error = %v, want it to name %s", name, err, EnvModel)
		}
		_, err = ResolveEngine(name, fakeEnv{path: bins, vars: map[string]string{EnvModel: filepath.Join(dir, "missing.bin")}}.env())
		if err == nil || !strings.Contains(err.Error(), "model file not found") {
			t.Fatalf("%s missing model file error = %v", name, err)
		}
		_, err = ResolveEngine(name, fakeEnv{path: bins, vars: map[string]string{EnvModel: dir}}.env())
		if err == nil || !strings.Contains(err.Error(), "directory") {
			t.Fatalf("%s model directory error = %v", name, err)
		}
	}

	engine, err := ResolveEngine(EngineParakeetCpp, fakeEnv{path: bins, vars: map[string]string{EnvModel: model}}.env())
	if err != nil {
		t.Fatalf("parakeet-cpp: %v", err)
	}
	want := []string{"/opt/bin/parakeet-cli", "transcribe", "--model", model, "--input", testWAV}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) || engine.Model != model {
		t.Fatalf("parakeet-cpp args = %#v model = %q", got, engine.Model)
	}

	engine, err = ResolveEngine(EngineWhisperCpp, fakeEnv{path: bins, vars: map[string]string{EnvModel: model}}.env())
	if err != nil {
		t.Fatalf("whisper-cpp: %v", err)
	}
	want = []string{"/opt/bin/whisper-cli", "-m", model, "-f", testWAV, "-l", "auto", "-nt", "-np"}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) {
		t.Fatalf("whisper-cpp args = %#v, want %#v", got, want)
	}
	engine, err = ResolveEngine(EngineWhisperCpp, fakeEnv{path: bins, vars: map[string]string{EnvModel: model, EnvLanguage: "de"}}.env())
	if err != nil {
		t.Fatalf("whisper-cpp language: %v", err)
	}
	if got := engine.Args(testWAV); got[6] != "de" {
		t.Fatalf("whisper-cpp language args = %#v", got)
	}
}

func TestResolveEngineCommandTemplate(t *testing.T) {
	dir := t.TempDir()
	program := writeExecutable(t, filepath.Join(dir, "My Tools", "stt"))
	env := fakeEnv{vars: map[string]string{
		EnvCommand: `'` + program + `' --lang "en us" --in={wav} --copy {wav}`,
		EnvModel:   "custom-model",
	}}.env()
	engine, err := ResolveEngine(EngineCommand, env)
	if err != nil {
		t.Fatalf("ResolveEngine: %v", err)
	}
	want := []string{program, "--lang", "en us", "--in=" + testWAV, "--copy", testWAV}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, want) {
		t.Fatalf("args = %#v, want %#v", got, want)
	}
	if engine.Name != EngineCommand || engine.Model != "custom-model" {
		t.Fatalf("engine = %+v", engine)
	}

	onPath := fakeEnv{
		vars: map[string]string{EnvCommand: "stt {wav}"},
		path: map[string]string{"stt": "/opt/bin/stt"},
	}.env()
	engine, err = ResolveEngine(EngineCommand, onPath)
	if err != nil {
		t.Fatalf("ResolveEngine PATH program: %v", err)
	}
	if got := engine.Args(testWAV); !reflect.DeepEqual(got, []string{"/opt/bin/stt", testWAV}) {
		t.Fatalf("args = %#v", got)
	}
}

func TestResolveEngineCommandTemplateErrors(t *testing.T) {
	for _, tc := range []struct {
		name     string
		template string
		want     string
	}{
		{name: "missing", template: "", want: EnvCommand},
		{name: "blank", template: "  ", want: EnvCommand},
		{name: "no wav", template: "stt --input audio.wav", want: WAVPlaceholder},
		{name: "wav only as program", template: "{wav}", want: WAVPlaceholder},
		{name: "unterminated", template: "stt '{wav}", want: "unterminated"},
		{name: "program missing", template: "no-such-stt {wav}", want: "not found"},
		{name: "program path missing", template: "/no/such/stt {wav}", want: "not an executable file"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ResolveEngine(EngineCommand, fakeEnv{vars: map[string]string{EnvCommand: tc.template}, home: t.TempDir()}.env())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestResolveEngineRejectsUnknownEngine(t *testing.T) {
	_, err := ResolveEngine("siri", fakeEnv{}.env())
	if err == nil || !strings.Contains(err.Error(), "unknown transcription engine") || !strings.Contains(err.Error(), EngineFluidAudio) {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveEngineTimeout(t *testing.T) {
	path := map[string]string{"nemo-speech": "/opt/bin/nemo-speech"}
	engine, err := ResolveEngine("", fakeEnv{path: path, vars: map[string]string{EnvTimeout: "90s"}}.env())
	if err != nil || engine.Timeout != 90*time.Second {
		t.Fatalf("timeout = %v, err = %v", engine.Timeout, err)
	}
	for _, bad := range []string{"soon", "0s", "-5m"} {
		_, err := ResolveEngine("", fakeEnv{path: path, vars: map[string]string{EnvTimeout: bad}}.env())
		if err == nil || !strings.Contains(err.Error(), EnvTimeout) {
			t.Fatalf("timeout %q error = %v", bad, err)
		}
	}
}

func TestNormalizeTranscript(t *testing.T) {
	got := NormalizeTranscript("  \n  first segment  \r\n\n second segment \n\t\n")
	if got != "first segment\nsecond segment" {
		t.Fatalf("NormalizeTranscript = %q", got)
	}
	if got := NormalizeTranscript(" \n\t "); got != "" {
		t.Fatalf("blank transcript = %q", got)
	}
}

// TestHelperProcess is re-executed by the engine tests as a fake engine. It
// does nothing when run as a normal test.
func TestHelperProcess(t *testing.T) {
	if os.Getenv("WACLI_TRANSCRIBE_HELPER") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	if len(args) < 2 {
		os.Exit(2)
	}
	switch args[1] {
	case "ok":
		fmt.Fprint(os.Stdout, "  \n  made-up words about a picnic  \r\n\n on saturday \n")
		fmt.Fprint(os.Stderr, "loading model...\n")
		os.Exit(0)
	case "echo-arg":
		fmt.Fprint(os.Stdout, args[2])
		os.Exit(0)
	case "silence":
		os.Exit(0)
	case "fail":
		fmt.Fprint(os.Stderr, strings.Repeat("noise ", 400)+"model file is corrupt")
		os.Exit(3)
	case "sleep":
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(2)
}

func helperEngine(t *testing.T, mode string, timeout time.Duration) Engine {
	t.Helper()
	t.Setenv("WACLI_TRANSCRIBE_HELPER", "1")
	return Engine{
		Name:    EngineCommand,
		Timeout: timeout,
		argv:    []string{os.Args[0], "-test.run=^TestHelperProcess$", "--", mode, WAVPlaceholder},
	}
}

func TestEngineRunReturnsNormalizedTranscript(t *testing.T) {
	text, err := helperEngine(t, "ok", time.Minute).Run(context.Background(), testWAV)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if text != "made-up words about a picnic\non saturday" {
		t.Fatalf("transcript = %q", text)
	}
}

func TestEngineRunPassesWAVAsOneArgument(t *testing.T) {
	t.Setenv("WACLI_TRANSCRIBE_HELPER", "1")
	env := fakeEnv{vars: map[string]string{
		EnvCommand: `'` + os.Args[0] + `' -test.run=^TestHelperProcess$ -- echo-arg {wav}`,
	}}.env()
	engine, err := ResolveEngine(EngineCommand, env)
	if err != nil {
		t.Fatalf("ResolveEngine: %v", err)
	}
	text, err := engine.Run(context.Background(), testWAV)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if text != testWAV {
		t.Fatalf("engine saw %q, want the whole path %q", text, testWAV)
	}
}

func TestEngineRunEmptyTranscriptIsNotAnError(t *testing.T) {
	text, err := helperEngine(t, "silence", time.Minute).Run(context.Background(), testWAV)
	if err != nil || text != "" {
		t.Fatalf("Run = %q, %v; want empty transcript", text, err)
	}
}

func TestEngineRunFailureIncludesTruncatedStderr(t *testing.T) {
	_, err := helperEngine(t, "fail", time.Minute).Run(context.Background(), testWAV)
	if err == nil {
		t.Fatal("expected failure")
	}
	msg := err.Error()
	if !strings.Contains(msg, "exit status 3") || !strings.Contains(msg, "model file is corrupt") {
		t.Fatalf("error = %q, want exit status and stderr tail", msg)
	}
	if len([]rune(msg)) > maxStderrDetail+200 {
		t.Fatalf("error not truncated (%d runes)", len([]rune(msg)))
	}
}

func TestEngineRunTimesOut(t *testing.T) {
	start := time.Now()
	_, err := helperEngine(t, "sleep", 300*time.Millisecond).Run(context.Background(), testWAV)
	if err == nil || !strings.Contains(err.Error(), "timed out") || !strings.Contains(err.Error(), EnvTimeout) {
		t.Fatalf("error = %v, want a timeout naming %s", err, EnvTimeout)
	}
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("timeout took %s", elapsed)
	}
}

func TestEngineRunHonorsCallerCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	_, err := helperEngine(t, "sleep", time.Minute).Run(ctx, testWAV)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context.Canceled", err)
	}
}
