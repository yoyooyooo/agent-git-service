package main

import (
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/joho/godotenv"

	"github.com/ngaut/agent-git-service/internal/buildinfo"
	applog "github.com/ngaut/agent-git-service/internal/logging"
	"github.com/ngaut/agent-git-service/server"
)

func main() {
	if handled, err := buildinfo.PrintVersion(os.Args[1:], "gh-server", os.Stdout); handled {
		if err != nil {
			os.Exit(1)
		}
		return
	}
	if len(os.Args) > 1 && os.Args[1] == "wiki-reindex" {
		_ = godotenv.Load()
		applog.Init()
		if err := server.RunWikiReindex(os.Args[2:]); err != nil {
			slog.Error("wiki reindex failed", "error", err)
			os.Exit(1)
		}
		return
	}

	_ = godotenv.Load()
	applog.Init()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	done := make(chan struct{})
	go func() {
		<-sigCh
		close(done)
	}()

	if err := server.Run(done); err != nil {
		slog.Error("bootstrap failed", "error", err)
		os.Exit(1)
	}
}
