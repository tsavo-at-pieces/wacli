package transcribe

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// processWaitDelay bounds how long Run waits for pipes to close after the
// process is killed, so a grandchild holding stdout cannot hang a timeout.
const processWaitDelay = 2 * time.Second

type processTimeoutError struct {
	timeout time.Duration
}

func (e *processTimeoutError) Error() string {
	return fmt.Sprintf("timed out after %s", e.timeout)
}

// runProcess runs argv without a shell, stdin closed, and returns stdout.
// A non-zero exit includes the tail of stderr in the error.
func runProcess(ctx context.Context, argv []string, timeout time.Duration, maxStdout int) (string, error) {
	if len(argv) == 0 || strings.TrimSpace(argv[0]) == "" {
		return "", fmt.Errorf("no program to run")
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(runCtx, argv[0], argv[1:]...)
	stdout := &cappedBuffer{max: maxStdout}
	stderr := &tailBuffer{max: 16 * 1024}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.WaitDelay = processWaitDelay
	killProcessTreeOnCancel(cmd)

	err := cmd.Run()
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", ctxErr
		}
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			return "", &processTimeoutError{timeout: timeout}
		}
		return "", fmt.Errorf("%s: %w%s", programName(argv[0]), err, stderrDetail(stderr.String()))
	}
	if stdout.overflow {
		return "", fmt.Errorf("%s: output exceeded %d bytes", programName(argv[0]), maxStdout)
	}
	return stdout.String(), nil
}

func programName(path string) string {
	path = strings.TrimRight(path, `/\`)
	if i := strings.LastIndexAny(path, `/\`); i >= 0 {
		return path[i+1:]
	}
	return path
}

func stderrDetail(stderr string) string {
	stderr = strings.TrimSpace(stderr)
	if stderr == "" {
		return ""
	}
	runes := []rune(stderr)
	if len(runes) > maxStderrDetail {
		stderr = "…" + string(runes[len(runes)-maxStderrDetail:])
	}
	return ": " + strings.Join(strings.Fields(stderr), " ")
}

// cappedBuffer keeps the first max bytes and remembers whether more arrived.
type cappedBuffer struct {
	buf      []byte
	max      int
	overflow bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	room := b.max - len(b.buf)
	if room < len(p) {
		b.overflow = true
		if room > 0 {
			b.buf = append(b.buf, p[:room]...)
		}
		return len(p), nil
	}
	b.buf = append(b.buf, p...)
	return len(p), nil
}

func (b *cappedBuffer) String() string { return string(b.buf) }

// tailBuffer keeps the last max bytes written, where error messages usually are.
type tailBuffer struct {
	buf []byte
	max int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.buf = append(b.buf, p...)
	if over := len(b.buf) - b.max; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	return len(p), nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
