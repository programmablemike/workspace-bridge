package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/programmablemike/workspace-bridge/internal/config"
	"github.com/programmablemike/workspace-bridge/internal/mcpserver"
	"github.com/programmablemike/workspace-bridge/internal/observer"
	"github.com/programmablemike/workspace-bridge/internal/store"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
)

func main() {
	if e := run(); e != nil {
		slog.Error("command failed", "error", e.Error())
		os.Exit(1)
	}
}
func run() error {
	args := os.Args[1:]
	if len(args) == 0 {
		return fmt.Errorf("usage: workspace-bridge {serve|capture ID|status ID|forget ID|doctor|version} --config FILE")
	}
	command := args[0]
	if command == "version" {
		fmt.Println("workspace-bridge 0.1.0")
		return nil
	}
	configPath := "bridge.yaml"
	id := ""
	for i := 1; i < len(args); i++ {
		if args[i] == "--config" && i+1 < len(args) {
			i++
			configPath = args[i]
		} else if id == "" && config.ValidID(args[i]) {
			id = args[i]
		} else {
			return workspace.Err("INVALID_ARGUMENT")
		}
	}
	if command != "serve" && command != "capture" && command != "status" && command != "forget" && command != "doctor" {
		return workspace.Err("INVALID_ARGUMENT")
	}
	c, e := config.Load(configPath)
	if e != nil {
		return e
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var token string
	if command == "serve" || command == "doctor" {
		token, e = c.Token()
		if e != nil {
			return e
		}
	}
	st, e := store.Open(c.Storage.Path)
	if e != nil {
		return e
	}
	defer st.Close()
	svc, e := observer.New(ctx, c, st)
	if e != nil {
		return e
	}
	emit := func(v any) error { return json.NewEncoder(os.Stdout).Encode(v) }
	switch command {
	case "capture":
		snap, created, e := svc.Capture(ctx, id)
		if e != nil {
			return workspace.Safe(e)
		}
		if e = st.Prune(ctx, c.Storage.RetentionDays, c.Storage.MaxBytes); e != nil {
			return workspace.Safe(e)
		}
		return emit(map[string]any{"revision": snap.Revision, "created": created})
	case "status":
		if _, e = svc.Info(ctx, id); e != nil {
			return e
		}
		h, e := st.Health(ctx, id)
		if e != nil {
			return workspace.Safe(e)
		}
		return emit(h)
	case "forget":
		if _, e = svc.Info(ctx, id); e != nil {
			return e
		}
		if e = st.Forget(ctx, id); e != nil {
			return workspace.Safe(e)
		}
		if e = st.Prune(ctx, c.Storage.RetentionDays, c.Storage.MaxBytes); e != nil {
			return workspace.Safe(e)
		}
		return emit(map[string]bool{"deleted": true})
	case "doctor":
		for _, w := range c.Workspaces {
			if _, _, e = svc.Capture(ctx, w.ID); e != nil {
				return workspace.Safe(e)
			}
		}
		return emit(map[string]bool{"configuration": true, "storage": true, "capture": true, "authentication_configured": true})
	}
	listener, e := net.Listen("tcp", c.Server.Listen)
	if e != nil {
		return fmt.Errorf("cannot bind configured loopback listener")
	}
	defer listener.Close()
	server := &http.Server{Handler: mcpserver.Handler(mcpserver.New(svc), token, listener.Addr().String()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, WriteTimeout: 30 * time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	done := make(chan struct{})
	go func() { svc.Run(ctx); close(done) }()
	go func() {
		<-ctx.Done()
		shutdownCtx, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		server.Shutdown(shutdownCtx)
	}()
	slog.Info("workspace bridge listening", "address", listener.Addr().String())
	e = server.Serve(listener)
	cancel()
	<-done
	if e != nil && e != http.ErrServerClosed {
		return fmt.Errorf("HTTP service failed")
	}
	return nil
}
