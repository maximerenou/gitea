// Copyright 2026 The Gitea Authors. All rights reserved.
// SPDX-License-Identifier: MIT

// agent-run runs one Gitea agent run inside an Actions job: "prepare", then "exec", then "finish".
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"gitea.dev/agents/runner-helper/internal/helper"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: agent-run prepare|exec|finish")
		os.Exit(2)
	}
	if err := helper.Run(ctx, os.Args[1], helper.EnvFromOS(), os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "agent-run:", err)
		os.Exit(1)
	}
}
