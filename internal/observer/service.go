package observer

import (
	"context"
	gitbackend "github.com/programmablemike/workspace-bridge/internal/backends/git"
	"github.com/programmablemike/workspace-bridge/internal/config"
	"github.com/programmablemike/workspace-bridge/internal/store"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"log/slog"
	"sync"
	"time"
)

type entry struct {
	mu       sync.Mutex
	backend  workspace.Backend
	interval time.Duration
}
type Service struct {
	Config  config.Config
	Store   *store.Store
	entries map[string]*entry
}

func New(ctx context.Context, c config.Config, s *store.Store) (*Service, error) {
	v := &Service{Config: c, Store: s, entries: map[string]*entry{}}
	for _, w := range c.Workspaces {
		if e := s.Register(ctx, w.ID, w.Root); e != nil {
			return nil, e
		}
		interval := c.Observer.PollInterval
		if w.PollInterval != "" {
			interval = w.PollInterval
		}
		d, _ := time.ParseDuration(interval)
		v.entries[w.ID] = &entry{backend: &gitbackend.Backend{Root: w.Root, ID: w.ID, Name: w.Name}, interval: d}
	}
	return v, nil
}
func (s *Service) Info(ctx context.Context, w string) (workspace.Info, error) {
	if !config.ValidID(w) {
		return workspace.Info{}, workspace.Err("INVALID_ARGUMENT")
	}
	e, ok := s.entries[w]
	if !ok {
		return workspace.Info{}, workspace.Err("WORKSPACE_NOT_FOUND")
	}
	return e.backend.Info(ctx)
}
func (s *Service) Capture(ctx context.Context, w string) (workspace.Snapshot, bool, error) {
	e, ok := s.entries[w]
	if !ok {
		return workspace.Snapshot{}, false, workspace.Err("WORKSPACE_NOT_FOUND")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	timeout, _ := time.ParseDuration(s.Config.Observer.CaptureTimeout)
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	attempt := time.Now().UTC()
	c, err := e.backend.Capture(ctx, s.Config.Capture)
	var snap workspace.Snapshot
	var created bool
	if err == nil {
		snap, created, err = s.Store.Publish(ctx, w, c)
	}
	healthCtx, healthCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer healthCancel()
	he := s.Store.RecordAttempt(healthCtx, w, attempt, snap.Revision, err)
	if err == nil {
		err = he
	}
	if err != nil {
		slog.Warn("capture failed", "workspace_id", w, "code", workspace.Safe(err).Code)
	}
	return snap, created, err
}
func (s *Service) Run(ctx context.Context) {
	var wg sync.WaitGroup
	for id, e := range s.entries {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticker := time.NewTicker(e.interval)
			defer ticker.Stop()
			for {
				s.Capture(ctx, id)
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	wg.Add(1)
	go func() {
		defer wg.Done()
		timer := time.NewTicker(time.Hour)
		defer timer.Stop()
		for {
			if e := s.Store.Prune(ctx, s.Config.Storage.RetentionDays, s.Config.Storage.MaxBytes); e != nil && ctx.Err() == nil {
				slog.Warn("retention failed", "code", "STORAGE_UNAVAILABLE")
			}
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
		}
	}()
	wg.Wait()
}
