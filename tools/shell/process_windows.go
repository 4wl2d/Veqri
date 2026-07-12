//go:build windows

package shell

import (
	"os/exec"
	"time"
)

// Go's CommandContext terminates the direct process on Windows. Job-object
// process-tree ownership is not yet available in this adapter.
func configureProcessCancellation(command *exec.Cmd) { command.WaitDelay = 5 * time.Second }
