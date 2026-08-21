package tool

import (
	"context"
	"os"
	"os/exec"
)

// SafeCommandContext prepares an exec.Cmd configured with non-interactive environment variables,
// isolated process group (PGID), and clean process-tree termination on cancellation/timeout.
func SafeCommandContext(ctx context.Context, name string, args ...string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = append(os.Environ(),
		"CI=true",
		"DEBIAN_FRONTEND=noninteractive",
		"TERM=dumb",
		"GIT_TERMINAL_PROMPT=0",
		"PAGER=cat",
		"FORCE_COLOR=0",
		"NO_COLOR=1",
	)
	setupProcessGroup(cmd)
	return cmd
}
