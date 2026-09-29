// Package transcribe turns stored WhatsApp audio into text with a local
// speech-to-text engine. Engines are external programs run without a shell:
// ffmpeg first converts the audio to 16 kHz mono PCM16 WAV, then the engine
// reads that WAV and prints the transcript on stdout.
package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"
)

// Built-in engine presets.
const (
	EngineNemoSpeech  = "nemo-speech"
	EngineFluidAudio  = "fluidaudio"
	EngineParakeetCpp = "parakeet-cpp"
	EngineWhisperCpp  = "whisper-cpp"
	EngineCommand     = "command"

	DefaultEngine  = EngineNemoSpeech
	DefaultTimeout = 300 * time.Second

	// WAVPlaceholder marks where the converted WAV path goes in an engine's
	// argv. The substitution never splits: a path with spaces stays one
	// argument.
	WAVPlaceholder = "{wav}"
)

// Environment variables read by engine and ffmpeg resolution.
const (
	EnvEngine      = "WACLI_TRANSCRIBE_ENGINE"
	EnvModel       = "WACLI_TRANSCRIBE_MODEL"
	EnvLanguage    = "WACLI_TRANSCRIBE_LANGUAGE"
	EnvCommand     = "WACLI_TRANSCRIBE_COMMAND"
	EnvTimeout     = "WACLI_TRANSCRIBE_TIMEOUT"
	EnvFFmpeg      = "WACLI_FFMPEG"
	EnvNemoSpeech  = "WACLI_NEMO_SPEECH"
	EnvFluidAudio  = "WACLI_FLUIDAUDIO"
	EnvParakeetCLI = "WACLI_PARAKEET_CLI"
	EnvWhisperCLI  = "WACLI_WHISPER_CLI"
)

const (
	maxTranscriptBytes = 1 << 20
	maxStderrDetail    = 600
)

// Environment is the process state engine resolution reads. Tests supply a
// fake; production uses OSEnvironment.
type Environment struct {
	Getenv   func(string) string
	LookPath func(string) (string, error)
	HomeDir  func() (string, error)
}

// OSEnvironment reads the real process environment and PATH.
func OSEnvironment() Environment {
	return Environment{Getenv: os.Getenv, LookPath: exec.LookPath, HomeDir: os.UserHomeDir}
}

func (env Environment) get(name string) string {
	if env.Getenv == nil {
		return ""
	}
	return strings.TrimSpace(env.Getenv(name))
}

// Engine is a resolved speech-to-text command.
type Engine struct {
	// Name is the preset name recorded with each transcript.
	Name string
	// Model is the model name or path recorded with each transcript. It may
	// be empty for a custom command.
	Model   string
	Timeout time.Duration
	argv    []string
}

// EngineNames lists the accepted --engine values.
func EngineNames() []string {
	names := []string{EngineNemoSpeech, EngineFluidAudio, EngineParakeetCpp, EngineWhisperCpp, EngineCommand}
	sort.Strings(names)
	return names
}

// SelectedEngineName applies the precedence flag > WACLI_TRANSCRIBE_ENGINE >
// default.
func SelectedEngineName(flagValue string, env Environment) string {
	if name := strings.ToLower(strings.TrimSpace(flagValue)); name != "" {
		return name
	}
	if name := strings.ToLower(env.get(EnvEngine)); name != "" {
		return name
	}
	return DefaultEngine
}

// ResolveEngine builds the engine selected by flagValue (or the environment)
// and checks that its program and any required model file exist.
func ResolveEngine(flagValue string, env Environment) (Engine, error) {
	name := SelectedEngineName(flagValue, env)
	timeout, err := engineTimeout(env)
	if err != nil {
		return Engine{}, err
	}
	engine := Engine{Name: name, Timeout: timeout}
	switch name {
	case EngineNemoSpeech:
		bin, err := findBinary(env, EnvNemoSpeech, "nemo-speech", "install NeMo-Speech.cpp and run `nemo-speech pull parakeet-tdt`")
		if err != nil {
			return Engine{}, err
		}
		engine.Model = firstNonEmpty(env.get(EnvModel), "parakeet-tdt")
		engine.argv = []string{bin, "--quiet", "transcribe", WAVPlaceholder, "--model", engine.Model}
	case EngineFluidAudio:
		bin, err := findBinary(env, EnvFluidAudio, "fluidaudiocli", "install the FluidAudio CLI (macOS, Apple silicon)")
		if err != nil {
			return Engine{}, err
		}
		engine.Model = firstNonEmpty(env.get(EnvModel), "ultra")
		engine.argv = []string{bin, "transcribe", WAVPlaceholder, "--model-version", engine.Model}
	case EngineParakeetCpp:
		bin, err := findBinary(env, EnvParakeetCLI, "parakeet-cli", "build parakeet.cpp")
		if err != nil {
			return Engine{}, err
		}
		model, err := requireModelFile(env, name, "a parakeet.cpp model file")
		if err != nil {
			return Engine{}, err
		}
		engine.Model = model
		engine.argv = []string{bin, "transcribe", "--model", model, "--input", WAVPlaceholder}
	case EngineWhisperCpp:
		bin, err := findBinary(env, EnvWhisperCLI, "whisper-cli", "build whisper.cpp or install it (e.g. brew install whisper-cpp)")
		if err != nil {
			return Engine{}, err
		}
		model, err := requireModelFile(env, name, "a whisper.cpp ggml model file such as ggml-base.en.bin")
		if err != nil {
			return Engine{}, err
		}
		engine.Model = model
		language := firstNonEmpty(env.get(EnvLanguage), "auto")
		engine.argv = []string{bin, "-m", model, "-f", WAVPlaceholder, "-l", language, "-nt", "-np"}
	case EngineCommand:
		argv, err := commandTemplateArgv(env)
		if err != nil {
			return Engine{}, err
		}
		engine.Model = env.get(EnvModel)
		engine.argv = argv
	default:
		return Engine{}, fmt.Errorf("unknown transcription engine %q (valid: %s)", name, strings.Join(EngineNames(), ", "))
	}
	return engine, nil
}

func commandTemplateArgv(env Environment) ([]string, error) {
	template := env.get(EnvCommand)
	if template == "" {
		return nil, fmt.Errorf("engine %q needs %s, a command template containing %s (for example: my-stt --input %s)", EngineCommand, EnvCommand, WAVPlaceholder, WAVPlaceholder)
	}
	argv, err := SplitCommand(template)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", EnvCommand, err)
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("%s is empty", EnvCommand)
	}
	hasWAV := false
	for _, arg := range argv[1:] {
		if strings.Contains(arg, WAVPlaceholder) {
			hasWAV = true
			break
		}
	}
	if !hasWAV {
		return nil, fmt.Errorf("%s must pass the audio file to the program with %s", EnvCommand, WAVPlaceholder)
	}
	program := argv[0]
	if strings.ContainsAny(program, `/\`) {
		if !isExecutableFile(program) {
			return nil, fmt.Errorf("%s program %q is not an executable file", EnvCommand, program)
		}
	} else {
		resolved, ok := lookupBinary(env, program)
		if !ok {
			return nil, fmt.Errorf("%s program %q not found on PATH or in ~/.local/bin", EnvCommand, program)
		}
		program = resolved
	}
	out := append([]string{program}, argv[1:]...)
	return out, nil
}

func requireModelFile(env Environment, engine, what string) (string, error) {
	model := env.get(EnvModel)
	if model == "" {
		return "", fmt.Errorf("engine %q needs a model: set %s to the path of %s", engine, EnvModel, what)
	}
	info, err := os.Stat(model)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("%s=%q: model file not found", EnvModel, model)
		}
		return "", fmt.Errorf("%s=%q: %w", EnvModel, model, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s=%q is a directory; engine %q needs a model file", EnvModel, model, engine)
	}
	return model, nil
}

func engineTimeout(env Environment) (time.Duration, error) {
	raw := env.get(EnvTimeout)
	if raw == "" {
		return DefaultTimeout, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q is not a Go duration (for example 10m or 90s)", EnvTimeout, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s must be positive, got %q", EnvTimeout, raw)
	}
	return d, nil
}

// findBinary resolves a program from its override variable, then PATH, then
// ~/.local/bin. The error names the variable to set.
func findBinary(env Environment, overrideVar, name, installHint string) (string, error) {
	if override := env.get(overrideVar); override != "" {
		if !isExecutableFile(override) {
			return "", fmt.Errorf("%s=%q is not an executable file", overrideVar, override)
		}
		return override, nil
	}
	if path, ok := lookupBinary(env, name); ok {
		return path, nil
	}
	return "", fmt.Errorf("%s not found on PATH or in ~/.local/bin: %s, or set %s to its path", name, installHint, overrideVar)
}

func lookupBinary(env Environment, name string) (string, bool) {
	if env.LookPath != nil {
		if path, err := env.LookPath(name); err == nil && path != "" {
			return path, true
		}
	}
	if env.HomeDir != nil {
		if home, err := env.HomeDir(); err == nil && home != "" {
			candidate := filepath.Join(home, ".local", "bin", name)
			if isExecutableFile(candidate) {
				return candidate, true
			}
		}
	}
	return "", false
}

func isExecutableFile(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0o111 != 0
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// Args returns the engine argv with the WAV path substituted.
func (e Engine) Args(wavPath string) []string {
	out := make([]string, len(e.argv))
	for i, arg := range e.argv {
		out[i] = strings.ReplaceAll(arg, WAVPlaceholder, wavPath)
	}
	return out
}

// Run transcribes wavPath and returns the normalized transcript. An empty
// transcript (silence) is not an error.
func (e Engine) Run(ctx context.Context, wavPath string) (string, error) {
	if len(e.argv) == 0 {
		return "", fmt.Errorf("transcription engine %q is not resolved", e.Name)
	}
	timeout := e.Timeout
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	stdout, err := runProcess(ctx, e.Args(wavPath), timeout, maxTranscriptBytes)
	if err != nil {
		var timeoutErr *processTimeoutError
		if errors.As(err, &timeoutErr) {
			return "", fmt.Errorf("engine %s timed out after %s (raise it with %s)", e.Name, timeout, EnvTimeout)
		}
		return "", fmt.Errorf("engine %s: %w", e.Name, err)
	}
	return NormalizeTranscript(stdout), nil
}

// NormalizeTranscript trims each line and drops blank lines, so engines that
// print one segment per line (whisper.cpp) or pad their output compare equal.
func NormalizeTranscript(raw string) string {
	lines := strings.Split(strings.ReplaceAll(raw, "\r\n", "\n"), "\n")
	kept := lines[:0]
	for _, line := range lines {
		if line = strings.TrimSpace(line); line != "" {
			kept = append(kept, line)
		}
	}
	return strings.Join(kept, "\n")
}
