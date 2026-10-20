// Package store persists immutable manifests and events after durable blob writes.
package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

type Store struct {
	mu           sync.Mutex
	db           *sql.DB
	dir          string
	lock         *os.File
	BeforeCommit func() error
}

func Open(dir string) (*Store, error) {
	if e := os.MkdirAll(dir, 0700); e != nil {
		return nil, workspace.Err("STORAGE_UNAVAILABLE")
	}
	if e := os.Chmod(dir, 0700); e != nil {
		return nil, e
	}
	lock, e := os.OpenFile(filepath.Join(dir, "writer.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return nil, workspace.Err("STORAGE_UNAVAILABLE")
	}
	if e = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); e != nil {
		lock.Close()
		return nil, fmt.Errorf("state directory is already in use; stop the daemon before local administrator commands")
	}
	fail := func(e error) (*Store, error) { lock.Close(); return nil, e }
	if e = os.MkdirAll(filepath.Join(dir, "blobs"), 0700); e != nil {
		return fail(e)
	}
	dbfile := filepath.Join(dir, "bridge.sqlite")
	f, e := os.OpenFile(dbfile, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return fail(e)
	}
	f.Close()
	if e = os.Chmod(dbfile, 0600); e != nil {
		return fail(e)
	}
	db, e := sql.Open("sqlite", dbfile)
	if e != nil {
		return fail(e)
	}
	db.SetMaxOpenConns(1)
	_, e = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
 CREATE TABLE IF NOT EXISTS schema_version(version INTEGER NOT NULL);
 INSERT INTO schema_version SELECT 1 WHERE NOT EXISTS(SELECT 1 FROM schema_version);
 CREATE TABLE IF NOT EXISTS workspaces(id TEXT PRIMARY KEY, root TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS revisions(workspace TEXT NOT NULL REFERENCES workspaces(id), id TEXT PRIMARY KEY, stamp TEXT NOT NULL, fingerprint TEXT NOT NULL, manifest BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS revision_order ON revisions(workspace,stamp);
 CREATE TABLE IF NOT EXISTS events(workspace TEXT NOT NULL REFERENCES workspaces(id), seq INTEGER NOT NULL, revision TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(workspace,seq));
 CREATE TABLE IF NOT EXISTS health(workspace TEXT PRIMARY KEY REFERENCES workspaces(id), body BLOB NOT NULL);
 CREATE TABLE IF NOT EXISTS expired(workspace TEXT NOT NULL, revision TEXT PRIMARY KEY);
 `)
	if e != nil {
		db.Close()
		return fail(workspace.Err("STORAGE_UNAVAILABLE"))
	}
	var version int
	if e = db.QueryRow("SELECT version FROM schema_version").Scan(&version); e != nil || version != 1 {
		db.Close()
		return fail(fmt.Errorf("unsupported storage schema"))
	}
	return &Store{db: db, dir: dir, lock: lock}, nil
}
func (s *Store) Close() error { e := s.db.Close(); s.lock.Close(); return e }
func (s *Store) Register(ctx context.Context, id, root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	var existing string
	e := s.db.QueryRowContext(ctx, "SELECT root FROM workspaces WHERE id=?", id).Scan(&existing)
	if e == nil && existing != root {
		return fmt.Errorf("workspace root changed; delete its stored history before reusing its ID")
	}
	if e != nil && !errors.Is(e, sql.ErrNoRows) {
		return e
	}
	_, e = s.db.ExecContext(ctx, "INSERT OR IGNORE INTO workspaces(id,root) VALUES(?,?)", id, root)
	return e
}
func (s *Store) latest(ctx context.Context, w string) (workspace.Snapshot, error) {
	return s.get(ctx, w, "")
}
func (s *Store) get(ctx context.Context, w, r string) (workspace.Snapshot, error) {
	var snap workspace.Snapshot
	var data []byte
	var e error
	if r == "" {
		e = s.db.QueryRowContext(ctx, "SELECT manifest FROM revisions WHERE workspace=? ORDER BY rowid DESC LIMIT 1", w).Scan(&data)
	} else {
		e = s.db.QueryRowContext(ctx, "SELECT manifest FROM revisions WHERE workspace=? AND id=?", w, r).Scan(&data)
	}
	if errors.Is(e, sql.ErrNoRows) {
		var n int
		s.db.QueryRowContext(ctx, "SELECT count(*) FROM expired WHERE workspace=? AND revision=?", w, r).Scan(&n)
		if n > 0 {
			return snap, workspace.Err("REVISION_EXPIRED")
		}
		return snap, workspace.Err("REVISION_NOT_FOUND")
	}
	if e != nil {
		return snap, workspace.Err("STORAGE_UNAVAILABLE")
	}
	if json.Unmarshal(data, &snap) != nil {
		return snap, workspace.Err("STORAGE_UNAVAILABLE")
	}
	return snap, nil
}
func (s *Store) Get(ctx context.Context, w, r string) (workspace.Snapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.get(ctx, w, r)
}
func (s *Store) blobPath(d string) string { return filepath.Join(s.dir, "blobs", d[:2], d[2:]) }
func (s *Store) blob(d string) ([]byte, error) {
	if len(d) != 64 {
		return nil, workspace.Err("CONTENT_OMITTED")
	}
	if _, e := hex.DecodeString(d); e != nil {
		return nil, workspace.Err("STORAGE_UNAVAILABLE")
	}
	b, e := os.ReadFile(s.blobPath(d))
	if e != nil || workspace.Digest(b) != d {
		return nil, workspace.Err("STORAGE_UNAVAILABLE")
	}
	return b, nil
}
func syncDir(p string) error {
	f, e := os.Open(p)
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}
func (s *Store) writeBlob(f workspace.File) error {
	if f.Digest == "" {
		return nil
	}
	if workspace.Digest(f.Data) != f.Digest {
		return workspace.Err("STORAGE_UNAVAILABLE")
	}
	target := s.blobPath(f.Digest)
	if b, e := os.ReadFile(target); e == nil && workspace.Digest(b) == f.Digest {
		return nil
	}
	dir := filepath.Dir(target)
	if e := os.MkdirAll(dir, 0700); e != nil {
		return e
	}
	if e := syncDir(filepath.Dir(dir)); e != nil {
		return e
	}
	tmp, e := os.CreateTemp(dir, ".pending-")
	if e != nil {
		return e
	}
	defer os.Remove(tmp.Name())
	defer tmp.Close()
	if _, e = tmp.Write(f.Data); e != nil {
		return e
	}
	if e = tmp.Sync(); e != nil {
		return e
	}
	if e = tmp.Close(); e != nil {
		return e
	}
	if e = os.Rename(tmp.Name(), target); e != nil {
		return e
	}
	return syncDir(dir)
}
func (s *Store) Publish(ctx context.Context, w string, c workspace.Capture) (workspace.Snapshot, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sort.Slice(c.Files, func(i, j int) bool { return c.Files[i].Path < c.Files[j].Path })
	for i, f := range c.Files {
		if !workspace.ValidPath(f.Path) || (i > 0 && c.Files[i-1].Path == f.Path) {
			return workspace.Snapshot{}, false, workspace.Err("PATH_NOT_ALLOWED")
		}
	}
	encoded, e := json.Marshal(c)
	if e != nil {
		return workspace.Snapshot{}, false, e
	}
	finger := workspace.Digest(encoded)
	old, oe := s.latest(ctx, w)
	if oe != nil && workspace.Safe(oe).Code != "REVISION_NOT_FOUND" {
		return old, false, oe
	}
	if oe == nil {
		var previous string
		if e = s.db.QueryRowContext(ctx, "SELECT fingerprint FROM revisions WHERE id=?", old.Revision).Scan(&previous); e != nil {
			return old, false, e
		}
		if previous == finger {
			return old, false, nil
		}
	}
	for _, f := range c.Files {
		if e = s.writeBlob(f); e != nil {
			return old, false, workspace.Err("STORAGE_UNAVAILABLE")
		}
	}
	id := make([]byte, 16)
	if _, e = rand.Read(id); e != nil {
		return old, false, e
	}
	now := time.Now().UTC()
	snap := workspace.Snapshot{WorkspaceID: w, Revision: "rev_" + hex.EncodeToString(id), Previous: old.Revision, CapturedAt: now, Status: "complete", Capture: c}
	for i := range snap.Files {
		snap.Files[i].Data = nil
	}
	body, _ := json.Marshal(snap)
	_, sum := workspace.Changes(old, snap)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return old, false, e
	}
	defer tx.Rollback()
	var seq int64
	if e = tx.QueryRowContext(ctx, "SELECT COALESCE(MAX(seq),0)+1 FROM events WHERE workspace=?", w).Scan(&seq); e != nil {
		return old, false, e
	}
	event := workspace.Event{Cursor: cursor(seq), Kind: "revision_created", From: old.Revision, To: snap.Revision, Timestamp: now, Summary: sum}
	eventBody, _ := json.Marshal(event)
	if _, e = tx.ExecContext(ctx, "INSERT INTO revisions(workspace,id,stamp,fingerprint,manifest) VALUES(?,?,?,?,?)", w, snap.Revision, now.Format(time.RFC3339Nano), finger, body); e != nil {
		return old, false, e
	}
	if _, e = tx.ExecContext(ctx, "INSERT INTO events(workspace,seq,revision,body) VALUES(?,?,?,?)", w, seq, snap.Revision, eventBody); e != nil {
		return old, false, e
	}
	if s.BeforeCommit != nil {
		if e = s.BeforeCommit(); e != nil {
			return old, false, e
		}
	}
	if e = tx.Commit(); e != nil {
		return old, false, e
	}
	return snap, true, nil
}
func (s *Store) Read(ctx context.Context, w, r, p string) (workspace.File, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !workspace.ValidPath(p) {
		return workspace.File{}, nil, workspace.Err("PATH_NOT_ALLOWED")
	}
	snap, e := s.get(ctx, w, r)
	if e != nil {
		return workspace.File{}, nil, e
	}
	for _, f := range snap.Files {
		if f.Path == p {
			if f.Omission != "" {
				return f, nil, workspace.Err("CONTENT_OMITTED")
			}
			if f.Kind == "binary" {
				return f, nil, workspace.Err("BINARY_CONTENT")
			}
			b, e := s.blob(f.Digest)
			return f, b, e
		}
	}
	return workspace.File{}, nil, workspace.Err("FILE_NOT_FOUND")
}
func cursor(n int64) string { return fmt.Sprintf("evt_%012d", n) }
func (s *Store) Events(ctx context.Context, w, after string, limit int) (workspace.EventPage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := workspace.EventPage{Events: []workspace.Event{}}
	if limit < 1 || limit > 200 {
		return p, workspace.Err("INVALID_ARGUMENT")
	}
	var lo, hi int64
	if e := s.db.QueryRowContext(ctx, "SELECT COALESCE(MIN(seq),0),COALESCE(MAX(seq),0) FROM events WHERE workspace=?", w).Scan(&lo, &hi); e != nil {
		return p, e
	}
	p.Earliest = cursor(lo)
	p.Latest = cursor(hi)
	n := int64(0)
	if after != "" {
		var e error
		if !strings.HasPrefix(after, "evt_") {
			return p, workspace.Err("INVALID_ARGUMENT")
		}
		n, e = strconv.ParseInt(strings.TrimPrefix(after, "evt_"), 10, 64)
		if e != nil || n < 0 || n > hi {
			return p, workspace.Err("INVALID_ARGUMENT")
		}
		if n < lo-1 {
			e := workspace.Err("CURSOR_EXPIRED")
			e.Details = map[string]any{"earliest_cursor": p.Earliest, "latest_cursor": p.Latest}
			return p, e
		}
	}
	rows, e := s.db.QueryContext(ctx, "SELECT body FROM events WHERE workspace=? AND seq>? ORDER BY seq LIMIT ?", w, n, limit+1)
	if e != nil {
		return p, e
	}
	defer rows.Close()
	for rows.Next() {
		var data []byte
		if e = rows.Scan(&data); e != nil {
			return p, e
		}
		var ev workspace.Event
		if e = json.Unmarshal(data, &ev); e != nil {
			return p, e
		}
		p.Events = append(p.Events, ev)
	}
	if e = rows.Err(); e != nil {
		return p, e
	}
	rows.Close()
	if len(p.Events) > limit {
		p.HasMore = true
		p.Events = p.Events[:limit]
	}
	p.Next = cursor(n)
	if len(p.Events) > 0 {
		p.Next = p.Events[len(p.Events)-1].Cursor
	}
	if snap, e := s.latest(ctx, w); e == nil {
		p.LatestRevision = snap.Revision
	}
	return p, nil
}
func (s *Store) Health(ctx context.Context, w string) (workspace.Health, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.health(ctx, w)
}

// LatestSummary survives pruning of the preceding revision.
func (s *Store) LatestSummary(ctx context.Context, w string) (workspace.Summary, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var body []byte
	err := s.db.QueryRowContext(ctx, "SELECT body FROM events WHERE workspace=? ORDER BY seq DESC LIMIT 1", w).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return workspace.Summary{}, nil
	}
	if err != nil {
		return workspace.Summary{}, err
	}
	var event workspace.Event
	err = json.Unmarshal(body, &event)
	return event.Summary, err
}
func (s *Store) health(ctx context.Context, w string) (workspace.Health, error) {
	var h workspace.Health
	var b []byte
	e := s.db.QueryRowContext(ctx, "SELECT body FROM health WHERE workspace=?", w).Scan(&b)
	if errors.Is(e, sql.ErrNoRows) {
		return h, nil
	}
	if e != nil {
		return h, e
	}
	e = json.Unmarshal(b, &h)
	return h, e
}
func (s *Store) RecordAttempt(ctx context.Context, w string, attempt time.Time, r string, err error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, e := s.health(ctx, w)
	if e != nil {
		return e
	}
	h.LastAttempt = attempt
	if err != nil {
		h.Failures++
		h.ErrorCode = workspace.Safe(err).Code
	} else {
		h.Failures = 0
		h.ErrorCode = ""
		h.LastSuccess = time.Now().UTC()
		h.Latest = r
	}
	b, _ := json.Marshal(h)
	_, e = s.db.ExecContext(ctx, "INSERT INTO health VALUES(?,?) ON CONFLICT(workspace) DO UPDATE SET body=excluded.body", w, b)
	return e
}

// Prune removes oldest unpinned revisions by age, then by unique blob budget.
// Current revisions (and their newest events) always survive, even over budget.
func (s *Store) Prune(ctx context.Context, days int, maxBytes int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	rows, e := s.db.QueryContext(ctx, "SELECT manifest FROM revisions ORDER BY rowid")
	if e != nil {
		return e
	}
	snaps := []workspace.Snapshot{}
	latest := map[string]string{}
	refs := map[string]int{}
	sizes := map[string]int64{}
	for rows.Next() {
		var b []byte
		var snap workspace.Snapshot
		if e = rows.Scan(&b); e != nil {
			rows.Close()
			return e
		}
		if e = json.Unmarshal(b, &snap); e != nil {
			rows.Close()
			return e
		}
		snaps = append(snaps, snap)
		latest[snap.WorkspaceID] = snap.Revision
		for _, f := range snap.Files {
			if f.Digest != "" {
				refs[f.Digest]++
				sizes[f.Digest] = f.Size
			}
		}
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	var total int64
	for d := range refs {
		total += sizes[d]
	}
	cut := time.Now().Add(-time.Duration(days) * 24 * time.Hour)
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	keepSuffix := map[string]bool{}
	for _, snap := range snaps {
		if latest[snap.WorkspaceID] == snap.Revision {
			continue
		}
		if keepSuffix[snap.WorkspaceID] {
			continue
		}
		if !snap.CapturedAt.Before(cut) && total <= maxBytes {
			// Keep a contiguous event suffix even if the wall clock moved
			// backwards between captures. Paging must never skip interior events.
			keepSuffix[snap.WorkspaceID] = true
			continue
		}
		if _, e = tx.ExecContext(ctx, "INSERT OR IGNORE INTO expired VALUES(?,?)", snap.WorkspaceID, snap.Revision); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM revisions WHERE id=?", snap.Revision); e != nil {
			return e
		}
		if _, e = tx.ExecContext(ctx, "DELETE FROM events WHERE workspace=? AND revision=?", snap.WorkspaceID, snap.Revision); e != nil {
			return e
		}
		for _, f := range snap.Files {
			if f.Digest != "" {
				refs[f.Digest]--
				if refs[f.Digest] == 0 {
					total -= sizes[f.Digest]
				}
			}
		}
	}
	if e = tx.Commit(); e != nil {
		return e
	}
	return filepath.WalkDir(filepath.Join(s.dir, "blobs"), func(p string, d os.DirEntry, e error) error {
		if e != nil {
			return e
		}
		if d.IsDir() {
			return nil
		}
		rel, e := filepath.Rel(filepath.Join(s.dir, "blobs"), p)
		if e != nil {
			return e
		}
		digest := strings.ReplaceAll(rel, string(filepath.Separator), "")
		if refs[digest] == 0 {
			return os.Remove(p)
		}
		return nil
	})
}
func (s *Store) Forget(ctx context.Context, w string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, e := s.db.BeginTx(ctx, nil)
	if e != nil {
		return e
	}
	defer tx.Rollback()
	for _, table := range []string{"health", "events", "revisions", "expired"} {
		if _, e = tx.ExecContext(ctx, "DELETE FROM "+table+" WHERE workspace=?", w); e != nil {
			return e
		}
	}
	if _, e = tx.ExecContext(ctx, "DELETE FROM workspaces WHERE id=?", w); e != nil {
		return e
	}
	return tx.Commit()
}
