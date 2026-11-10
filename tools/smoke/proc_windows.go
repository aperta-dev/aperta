// SPDX-License-Identifier: FSL-1.1-ALv2

//go:build windows

package main

import (
	"os"
	"os/exec"
)

func ownGroup(cmd *exec.Cmd) {}

func killGroup(pid int) {
	if p, err := os.FindProcess(pid); err == nil {
		p.Kill()
	}
}
