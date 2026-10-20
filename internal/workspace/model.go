// Package workspace defines provider-independent immutable observation data.
package workspace

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/bmatcuk/doublestar/v4"
)

type Error struct {
	Code    string         `json:"code"`
	Message string         `json:"message"`
	Details map[string]any `json:"details,omitempty"`
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func Err(code string) *Error {
	return &Error{Code: code, Message: map[string]string{
		"WORKSPACE_NOT_FOUND": "Workspace is not configured", "REVISION_NOT_FOUND": "Revision was not found", "REVISION_EXPIRED": "Revision is no longer retained", "CURSOR_EXPIRED": "Cursor is no longer retained", "PATH_NOT_ALLOWED": "Path is not allowed", "FILE_NOT_FOUND": "File was not found", "BINARY_CONTENT": "File is not UTF-8 text", "CONTENT_OMITTED": "Content was omitted by capture policy", "CAPTURE_UNSTABLE": "Workspace changed during capture", "CAPTURE_LIMIT_EXCEEDED": "Capture exceeded configured limits", "STORAGE_UNAVAILABLE": "Storage is unavailable", "INVALID_ARGUMENT": "Invalid argument", "CAPTURE_UNAVAILABLE": "Workspace could not be observed",
	}[code]}
}
func Safe(err error) *Error {
	if e, ok := err.(*Error); ok {
		return e
	}
	return Err("STORAGE_UNAVAILABLE")
}
func Digest(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func ValidPath(p string) bool {
	return p != "" && len(p) <= 4096 && utf8.ValidString(p) && !strings.ContainsAny(p, "\\\x00%:") && !strings.HasPrefix(p, "/") && path.Clean(p) == p && p != "." && p != ".." && !strings.HasPrefix(p, "../")
}

type Policy struct {
	MaxFiles      int      `yaml:"max_files"`
	MaxFileBytes  int64    `yaml:"max_file_bytes"`
	MaxTotalBytes int64    `yaml:"max_total_bytes"`
	Exclude       []string `yaml:"exclude"`
	Allow         []string `yaml:"allow"`
}

var Denied = []string{".git", ".env", ".env.*", "*.pem", "*.key", "id_rsa*", "id_ed25519*", "*.p12", "*.pfx", "node_modules", ".venv", "venv", "dist", "build", "target", ".cache"}

func (p Policy) Allowed(name string) bool {
	if !ValidPath(name) {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		for _, pattern := range Denied {
			if ok, _ := path.Match(pattern, part); ok {
				return false
			}
		}
	}
	for _, pattern := range p.Exclude {
		if ok, _ := doublestar.Match(pattern, name); ok {
			return false
		}
	}
	if len(p.Allow) > 0 {
		for _, pattern := range p.Allow {
			if ok, _ := doublestar.Match(pattern, name); ok {
				return true
			}
		}
		return false
	}
	return true
}

type Capabilities struct {
	SavedFiles     bool `json:"saved_files"`
	UnsavedBuffers bool `json:"unsaved_buffers"`
	GitHistory     bool `json:"git_history"`
	Diagnostics    bool `json:"diagnostics"`
	TestExecution  bool `json:"test_execution"`
	LiveCursor     bool `json:"live_cursor"`
}
type Info struct {
	ID           string       `json:"id"`
	DisplayName  string       `json:"display_name"`
	Backend      string       `json:"backend"`
	Capabilities Capabilities `json:"capabilities"`
}
type File struct {
	Path       string `json:"path"`
	Digest     string `json:"digest,omitempty"`
	Size       int64  `json:"size"`
	Executable bool   `json:"executable,omitempty"`
	Kind       string `json:"kind"`
	Omission   string `json:"omission_reason,omitempty"`
	Data       []byte `json:"-"`
}
type Capture struct {
	Files      []File            `json:"files"`
	Provenance map[string]string `json:"provenance"`
	Warnings   []string          `json:"warnings"`
}
type Backend interface {
	Info(context.Context) (Info, error)
	Capture(context.Context, Policy) (Capture, error)
}
type Snapshot struct {
	WorkspaceID string    `json:"workspace_id"`
	Revision    string    `json:"revision"`
	Previous    string    `json:"previous_revision,omitempty"`
	CapturedAt  time.Time `json:"captured_at"`
	Status      string    `json:"status"`
	Capture
}
type Summary struct {
	Added    int `json:"added"`
	Modified int `json:"modified"`
	Deleted  int `json:"deleted"`
}
type Change struct {
	Path        string `json:"path"`
	Type        string `json:"change_type"`
	Before      string `json:"before_digest,omitempty"`
	After       string `json:"after_digest,omitempty"`
	UnifiedDiff string `json:"unified_diff,omitempty"`
	Omission    string `json:"omission_reason,omitempty"`
	Truncated   bool   `json:"truncated,omitempty"`
}

func Changes(a, b Snapshot) ([]Change, Summary) {
	out := []Change{}
	var sum Summary
	i, j := 0, 0
	for i < len(a.Files) || j < len(b.Files) {
		if j == len(b.Files) || (i < len(a.Files) && a.Files[i].Path < b.Files[j].Path) {
			f := a.Files[i]
			out = append(out, Change{Path: f.Path, Type: "deleted", Before: f.Digest})
			sum.Deleted++
			i++
		} else if i == len(a.Files) || b.Files[j].Path < a.Files[i].Path {
			f := b.Files[j]
			out = append(out, Change{Path: f.Path, Type: "added", After: f.Digest})
			sum.Added++
			j++
		} else {
			x, y := a.Files[i], b.Files[j]
			xb, _ := json.Marshal(x)
			yb, _ := json.Marshal(y)
			if string(xb) != string(yb) {
				out = append(out, Change{Path: y.Path, Type: "modified", Before: x.Digest, After: y.Digest})
				sum.Modified++
			}
			i++
			j++
		}
	}
	return out, sum
}

type Event struct {
	Cursor    string    `json:"cursor"`
	Kind      string    `json:"kind"`
	From      string    `json:"from_revision"`
	To        string    `json:"to_revision"`
	Timestamp time.Time `json:"timestamp"`
	Summary   Summary   `json:"summary"`
}
type EventPage struct {
	Events         []Event `json:"events"`
	Next           string  `json:"next_cursor"`
	HasMore        bool    `json:"has_more"`
	Earliest       string  `json:"earliest_cursor"`
	Latest         string  `json:"latest_cursor"`
	LatestRevision string  `json:"latest_revision"`
}
type Health struct {
	LastAttempt time.Time `json:"last_attempt_at"`
	LastSuccess time.Time `json:"last_success_at"`
	Latest      string    `json:"latest_revision"`
	ErrorCode   string    `json:"last_error_code,omitempty"`
	Failures    int       `json:"consecutive_failures"`
}
