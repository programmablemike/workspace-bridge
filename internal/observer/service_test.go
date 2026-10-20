package observer

import (
	"context"
	"github.com/programmablemike/workspace-bridge/internal/config"
	"github.com/programmablemike/workspace-bridge/internal/store"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"testing"
)

type fakeBackend struct{ fail bool }

func (b *fakeBackend) Info(context.Context) (workspace.Info, error) {
	return workspace.Info{ID: "w"}, nil
}
func (b *fakeBackend) Capture(context.Context, workspace.Policy) (workspace.Capture, error) {
	if b.fail {
		return workspace.Capture{}, workspace.Err("CAPTURE_UNSTABLE")
	}
	return workspace.Capture{Files: []workspace.File{{Path: "file", Kind: "text", Size: 1, Digest: workspace.Digest([]byte("a")), Data: []byte("a")}}}, nil
}
func TestFailureRecoveryPreservesRevision(t *testing.T) {
	ctx := context.Background()
	st, e := store.Open(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	defer st.Close()
	if e = st.Register(ctx, "w", "/example"); e != nil {
		t.Fatal(e)
	}
	c := config.Config{}
	c.Observer.CaptureTimeout = "1s"
	backend := &fakeBackend{}
	s := &Service{Config: c, Store: st, entries: map[string]*entry{"w": {backend: backend}}}
	first, created, e := s.Capture(ctx, "w")
	if e != nil || !created {
		t.Fatal(e)
	}
	backend.fail = true
	if _, _, e = s.Capture(ctx, "w"); workspace.Safe(e).Code != "CAPTURE_UNSTABLE" {
		t.Fatal(e)
	}
	h, e := st.Health(ctx, "w")
	if e != nil || h.Latest != first.Revision || h.Failures != 1 || h.ErrorCode != "CAPTURE_UNSTABLE" {
		t.Fatal(h, e)
	}
	backend.fail = false
	again, created, e := s.Capture(ctx, "w")
	if e != nil || created || again.Revision != first.Revision {
		t.Fatal("recovery duplicated content", e)
	}
	h, e = st.Health(ctx, "w")
	if e != nil || h.Failures != 0 || h.ErrorCode != "" {
		t.Fatal(h, e)
	}
}
