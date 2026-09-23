// ags-replication is a remote operator client. It never boots the primary.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	"github.com/ngaut/agent-git-service/internal/replicationadmin"
)

func main() {
	if handled, err := buildinfo.PrintVersion(os.Args[1:], "ags-replication", os.Stdout); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := replicationadmin.Run(ctx, os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
