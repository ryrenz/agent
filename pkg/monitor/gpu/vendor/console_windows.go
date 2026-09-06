//go:build windows

package vendor

import (
	"os/exec"
	"syscall"
)

// hideConsole keeps nvidia-smi from allocating a console. A service already
// runs in session 0, where nothing it spawns can reach an interactive desktop,
// but the agent is also run by hand -- and this polls every few seconds.
func hideConsole(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}
}
