//go:build windows

package local_events

import (
	"os/exec"
	"time"
)

// Windows job-object process-tree ownership is intentionally unsupported in
// this adapter; CommandContext still cancels the direct child without shelling
// out to taskkill.
const localProcessCancellationScope = "direct_process_only"

func configureLocalProcess(command *exec.Cmd) { command.WaitDelay = 5 * time.Second }
