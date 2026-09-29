//go:build !windows

package transcribe

import (
	"os/exec"
	"syscall"
)

// killProcessTreeOnCancel runs the program in its own process group and kills
// the whole group on timeout, so wrapper scripts cannot leave the real engine
// running after wacli gives up on it.
func killProcessTreeOnCancel(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			return cmd.Process.Kill()
		}
		return nil
	}
}
