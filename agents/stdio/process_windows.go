//go:build windows

package stdio

import (
	"os/exec"
	"time"
)

// CommandContext cancels the direct child. Windows job-object process-tree
// ownership is intentionally reported as unsupported until it can be added
// without relying on shell commands such as taskkill.
const processCancellationScope = "direct_process_only"

func configureProcess(command *exec.Cmd) { command.WaitDelay = 5 * time.Second }
