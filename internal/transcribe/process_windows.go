//go:build windows

package transcribe

import "os/exec"

// killProcessTreeOnCancel keeps exec's default: the direct child is killed on
// timeout and WaitDelay bounds the wait for its pipes.
func killProcessTreeOnCancel(cmd *exec.Cmd) {}
