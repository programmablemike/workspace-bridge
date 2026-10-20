package store

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func input(text string) workspace.Capture {
	return workspace.Capture{Files: []workspace.File{{Path: "file", Digest: workspace.Digest([]byte(text)), Size: int64(len(text)), Kind: "text", Data: []byte(text)}}}
}
func setup(t *testing.T) (*Store, string) {
	t.Helper()
	dir := t.TempDir()
	s, e := Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	if e = s.Register(context.Background(), "w", "/example"); e != nil {
		t.Fatal(e)
	}
	return s, dir
}
func publish(t *testing.T, s *Store, text string) workspace.Snapshot {
	t.Helper()
	r, created, e := s.Publish(context.Background(), "w", input(text))
	if e != nil || !created {
		t.Fatalf("publish: %v %v", created, e)
	}
	return r
}
func TestHistoryAtomicityRestart(t *testing.T) {
	ctx := context.Background()
	s, dir := setup(t)
	a := publish(t, s, "a")
	if _, created, e := s.Publish(ctx, "w", input("a")); e != nil || created {
		t.Fatal(e)
	}
	s.BeforeCommit = func() error { return errors.New("injected") }
	if _, _, e := s.Publish(ctx, "w", input("b")); e == nil {
		t.Fatal("expected fault")
	}
	got, e := s.Get(ctx, "w", "")
	if e != nil || got.Revision != a.Revision {
		t.Fatal("partial commit")
	}
	s.BeforeCommit = nil
	b := publish(t, s, "b")
	c := publish(t, s, "a")
	if c.Revision == a.Revision {
		t.Fatal("revert lost")
	}
	_, old, e := s.Read(ctx, "w", a.Revision, "file")
	if e != nil || string(old) != "a" {
		t.Fatal(e)
	}
	page, e := s.Events(ctx, "w", "", 1)
	if e != nil || !page.HasMore || len(page.Events) != 1 {
		t.Fatal(page, e)
	}
	next, e := s.Events(ctx, "w", page.Next, 2)
	if e != nil || next.HasMore || len(next.Events) != 2 {
		t.Fatal(next, e)
	}
	if next.Events[0].To != b.Revision {
		t.Fatal("event order")
	}
	s.Close()
	s, e = Open(dir)
	if e != nil {
		t.Fatal(e)
	}
	defer s.Close()
	got, e = s.Get(ctx, "w", "")
	if e != nil || got.Revision != c.Revision {
		t.Fatal("restart lost latest")
	}
	_, old, e = s.Read(ctx, "w", a.Revision, "file")
	if e != nil || string(old) != "a" {
		t.Fatal("restart lost content")
	}
	if _, e = s.Get(ctx, "other", a.Revision); workspace.Safe(e).Code != "REVISION_NOT_FOUND" {
		t.Fatal(e)
	}
}
func TestConcurrentNoDuplicateAndWriterLock(t *testing.T) {
	s, dir := setup(t)
	defer s.Close()
	if other, e := Open(dir); e == nil {
		other.Close()
		t.Fatal("second writer admitted")
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, e := s.Publish(context.Background(), "w", input("a")); e != nil {
				t.Error(e)
			}
		}()
	}
	wg.Wait()
	p, e := s.Events(context.Background(), "w", "", 200)
	if e != nil || len(p.Events) != 1 {
		t.Fatal(p, e)
	}
}
func TestRetentionGCAndForget(t *testing.T) {
	s, _ := setup(t)
	defer s.Close()
	ctx := context.Background()
	a := publish(t, s, "one")
	publish(t, s, "two")
	c := publish(t, s, "three")
	if e := s.Prune(ctx, 7, 1); e != nil {
		t.Fatal(e)
	}
	if _, e := s.Get(ctx, "w", a.Revision); workspace.Safe(e).Code != "REVISION_EXPIRED" {
		t.Fatal(e)
	}
	if _, e := s.Events(ctx, "w", "evt_000000000000", 10); workspace.Safe(e).Code != "CURSOR_EXPIRED" {
		t.Fatal(e)
	}
	if _, b, e := s.Read(ctx, "w", c.Revision, "file"); e != nil || string(b) != "three" {
		t.Fatal(e)
	}
	if _, e := os.Stat(s.blobPath(workspace.Digest([]byte("one")))); !os.IsNotExist(e) {
		t.Fatal("unreferenced blob survived")
	}
	if e := s.Forget(ctx, "w"); e != nil {
		t.Fatal(e)
	}
	if e := s.Prune(ctx, 7, 1); e != nil {
		t.Fatal(e)
	}
	entries := 0
	filepath.WalkDir(filepath.Join(s.dir, "blobs"), func(_ string, d os.DirEntry, e error) error {
		if e == nil && !d.IsDir() {
			entries++
		}
		return e
	})
	if entries != 0 {
		t.Fatal("forgotten blobs remain")
	}
}
func TestMissingBlobNotPublished(t *testing.T) {
	s, _ := setup(t)
	defer s.Close()
	c := input("a")
	c.Files[0].Digest = workspace.Digest([]byte("wrong"))
	if _, _, e := s.Publish(context.Background(), "w", c); e == nil {
		t.Fatal("bad digest accepted")
	}
}

func TestRetentionKeepsContiguousEventsAcrossClockSkew(t *testing.T) {
	s, _ := setup(t)
	defer s.Close()
	ctx := context.Background()
	a := publish(t, s, "a")
	b := publish(t, s, "b")
	publish(t, s, "c")
	b.CapturedAt = b.CapturedAt.Add(-30 * 24 * time.Hour)
	body, e := json.Marshal(b)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = s.db.Exec("UPDATE revisions SET manifest=? WHERE id=?", body, b.Revision); e != nil {
		t.Fatal(e)
	}
	if e = s.Prune(ctx, 7, 1<<20); e != nil {
		t.Fatal(e)
	}
	p, e := s.Events(ctx, "w", "", 200)
	if e != nil || len(p.Events) != 3 {
		t.Fatal("retention created an interior event gap", p, e)
	}
	if _, e = s.Get(ctx, "w", a.Revision); e != nil {
		t.Fatal(e)
	}
}
