// SPDX-License-Identifier: FSL-1.1-ALv2

package main

import "runtime/debug"

func init() {
	if version != "dev" {
		return
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" && info.Main.Version != "(devel)" {
		version = info.Main.Version
	}
}
