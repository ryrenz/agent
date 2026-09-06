//go:build !windows

package vendor

import "os/exec"

// hideConsole is a no-op outside Windows, which is the only platform where a
// child process can pop a window.
func hideConsole(_ *exec.Cmd) {}
