// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

func ownGroup(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} }

func killGroup(pid int) { syscall.Kill(-pid, syscall.SIGKILL) }
