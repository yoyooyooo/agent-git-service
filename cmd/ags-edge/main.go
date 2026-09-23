// ags-edge is a separate runtime: never call the primary server bootstrap here.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ngaut/agent-git-service/config"
	"github.com/ngaut/agent-git-service/internal/edge"
)

func main() {
	slog.SetDefault(slog.New(slog.NewJSONHandler(os.Stderr, nil)))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := run(ctx); err != nil {
		slog.Error("ags-edge stopped", "error", err)
		os.Exit(1)
	}
}

func run(ctx context.Context) (result error) {
	cfg, err := config.NewEdge()
	if err != nil {
		return err
	}
	var options []edge.Option
	if cfg.ReadConfigFile != "" {
		resources, err := edge.OpenReadResources(ctx, cfg.ID, cfg.ReadConfigFile)
		if err != nil {
			return err
		}
		defer func() { result = errors.Join(result, resources.Close()) }()
		options = append(options, edge.WithReadRuntime(resources.Runtime))
		if cfg.DiagnosticsAddr != "" {
			operations, err := edge.StartOperations(ctx, cfg, resources)
			if err != nil {
				return err
			}
			defer operations.Close()
			options = append(options, edge.WithOperations(operations))
		}
	}
	srv, err := edge.New(cfg, options...)
	if err != nil {
		return err
	}
	listener, err := net.Listen("tcp", cfg.ListenAddr)
	if err != nil {
		return err
	}
	defer listener.Close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if cfg.DiagnosticsAddr != "" {
		diagnosticListener, err := net.Listen("tcp", cfg.DiagnosticsAddr)
		if err != nil {
			return err
		}
		defer diagnosticListener.Close()
		diagnostics := &http.Server{Handler: srv.DiagnosticsHandler(), ReadHeaderTimeout: 5 * time.Second, WriteTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second, MaxHeaderBytes: 8 << 10}
		done := make(chan error, 1)
		go func() {
			err := diagnostics.Serve(diagnosticListener)
			done <- err
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				cancel()
			}
		}()
		defer func() {
			stop, stopCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer stopCancel()
			if err := diagnostics.Shutdown(stop); err != nil {
				_ = diagnostics.Close()
				result = errors.Join(result, err)
			}
			if err := <-done; err != nil && !errors.Is(err, http.ErrServerClosed) {
				result = errors.Join(result, err)
			}
		}()
	}
	slog.Info("ags-edge listening", "edge_id", cfg.ID, "address", listener.Addr().String(), "read_path_wired", cfg.ReadConfigFile != "", "operational_ready", false)
	return srv.Run(ctx, listener)
}
