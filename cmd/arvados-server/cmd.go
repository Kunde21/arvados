// Copyright (C) The Arvados Authors. All rights reserved.
//
// SPDX-License-Identifier: AGPL-3.0

package main

import (
	"os"

	"git.curoverse.com/arvados.git/lib/cmd"
	"git.curoverse.com/arvados.git/lib/config"
	"git.curoverse.com/arvados.git/lib/controller"
	"git.curoverse.com/arvados.git/lib/dispatchcloud"
)

var (
	version = "dev"
	handler = cmd.Multi(map[string]cmd.Handler{
		"version":   cmd.Version(version),
		"-version":  cmd.Version(version),
		"--version": cmd.Version(version),

		"config-check":   config.CheckCommand,
		"config-dump":    config.DumpCommand,
		"controller":     controller.Command,
		"dispatch-cloud": dispatchcloud.Command,
	})
)

func main() {
	os.Exit(handler.RunCommand(os.Args[0], os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
