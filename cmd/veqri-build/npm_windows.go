//go:build windows

package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func runWindowsNPMWrapper(directory, npmPath string, arguments ...string) error {
	cmdPath, err := exec.LookPath("cmd.exe")
	if err != nil {
		return errors.New("required command \"cmd.exe\" was not found")
	}
	npmCommand, err := windowsBatchCommandLine(npmPath, arguments...)
	if err != nil {
		return err
	}
	commandLine := `"` + cmdPath + `" /d /v:off /s /c "` + npmCommand + `"`
	fmt.Printf("\n> npm %s\n", strings.Join(arguments, " "))
	command := exec.Command(cmdPath)
	command.SysProcAttr = &syscall.SysProcAttr{CmdLine: commandLine}
	command.Dir = directory
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Run(); err != nil {
		return fmt.Errorf("npm failed: %w", err)
	}
	return nil
}
