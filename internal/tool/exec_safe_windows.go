//go:build windows

package tool

import (
	"os/exec"
)

func setupProcessGroup(cmd *exec.Cmd) {
	// On Windows, CommandContext termination terminates the job object by default.
}
