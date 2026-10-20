// Package git observes only saved working-tree bytes. No command mutates Git.
package git

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"golang.org/x/sys/unix"
)

type Backend struct{ Root, ID, Name string }

func (b *Backend) Info(context.Context) (workspace.Info, error) {
	return workspace.Info{ID: b.ID, DisplayName: b.Name, Backend: "git", Capabilities: workspace.Capabilities{SavedFiles: true}}, nil
}

type bounded struct {
	bytes.Buffer
	max      int
	exceeded bool
}

func (b *bounded) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		b.exceeded = true
		return 0, workspace.Err("CAPTURE_LIMIT_EXCEEDED")
	}
	return b.Buffer.Write(p)
}
func (b *Backend) command(ctx context.Context, args ...string) ([]byte, error) {
	return b.run(ctx, false, args...)
}

func (b *Backend) run(ctx context.Context, allowAbsent bool, args ...string) ([]byte, error) {
	fixed := []string{"--no-pager", "--no-optional-locks", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false", "-c", "core.hooksPath=/dev/null", "-c", "core.quotePath=false", "-c", "diff.external=", "-c", "status.renames=false", "-C", b.Root}
	cmd := exec.CommandContext(ctx, "git", append(fixed, args...)...)
	// Do not inherit GIT_DIR, alternate indexes, config injection or loader variables.
	cmd.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LC_ALL=C", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_OPTIONAL_LOCKS=0", "GIT_TERMINAL_PROMPT=0", "GIT_NO_LAZY_FETCH=1", "GIT_NO_REPLACE_OBJECTS=1", "GIT_ALLOW_PROTOCOL="}
	out := &bounded{max: 32 << 20}
	cmd.Stdout = out
	cmd.Stderr = io.Discard
	if e := cmd.Run(); e != nil {
		if out.exceeded {
			return nil, workspace.Err("CAPTURE_LIMIT_EXCEEDED")
		}
		var exit *exec.ExitError
		if allowAbsent && errors.As(e, &exit) && exit.ExitCode() == 1 {
			return nil, nil
		}
		return nil, workspace.Err("CAPTURE_UNAVAILABLE")
	}
	return out.Bytes(), nil
}

type state struct {
	paths       []string
	links       map[string]bool
	provenance  map[string]string
	fingerprint string
}

func (b *Backend) enumerate(ctx context.Context, p workspace.Policy) (state, error) {
	s := state{links: map[string]bool{}, provenance: map[string]string{}}
	raw, e := b.command(ctx, "ls-files", "--stage", "-z")
	if e != nil {
		return s, e
	}
	set := map[string]bool{}
	index := []string{}
	for _, entry := range bytes.Split(raw, []byte{0}) {
		if len(entry) == 0 {
			continue
		}
		meta, name, ok := strings.Cut(string(entry), "\t")
		if !ok {
			return s, workspace.Err("CAPTURE_UNAVAILABLE")
		}
		if !p.Allowed(name) {
			continue
		}
		set[name] = true
		index = append(index, meta+"\t"+name)
		if strings.HasPrefix(meta, "160000 ") {
			s.links[name] = true
		}
	}
	raw, e = b.command(ctx, "ls-files", "--others", "--exclude-standard", "-z")
	if e != nil {
		return s, e
	}
	for _, name := range bytes.Split(raw, []byte{0}) {
		if p.Allowed(string(name)) {
			set[string(name)] = true
		}
	}
	// Do not use status/diff: their content comparisons can execute clean/process
	// filters selected by repository attributes, even with optional locks off.
	raw, e = b.run(ctx, true, "rev-parse", "--verify", "--quiet", "HEAD")
	if e != nil {
		return s, e
	}
	s.provenance["git.head"] = strings.TrimSuffix(string(raw), "\n")
	if len(raw) == 0 {
		s.provenance["git.head"] = "(initial)"
	}
	raw, e = b.run(ctx, true, "symbolic-ref", "--quiet", "--short", "HEAD")
	if e != nil {
		return s, e
	}
	s.provenance["git.branch"] = strings.TrimSuffix(string(raw), "\n")
	if len(raw) == 0 {
		s.provenance["git.branch"] = "(detached)"
	}
	sort.Strings(index)
	s.provenance["git.index_digest"] = workspace.Digest([]byte(strings.Join(index, "\x00")))
	for name := range set {
		if len(name) > 4096 {
			continue
		}
		s.paths = append(s.paths, name)
	}
	sort.Strings(s.paths)
	if len(s.paths) > p.MaxFiles {
		return s, workspace.Err("CAPTURE_LIMIT_EXCEEDED")
	}
	s.fingerprint = workspace.Digest([]byte(strings.Join(s.paths, "\x00") + strings.Join(index, "\x00") + s.provenance["git.head"] + s.provenance["git.branch"]))
	return s, nil
}

// openFile walks directory descriptors with O_NOFOLLOW on every component.
// O_NONBLOCK prevents a raced-in FIFO from blocking before fstat rejects it.
func openFile(root int, name string) (*os.File, error) {
	parts := strings.Split(name, "/")
	dir, e := unix.Dup(root)
	if e != nil {
		return nil, e
	}
	defer func() { unix.Close(dir) }()
	for _, part := range parts[:len(parts)-1] {
		next, e := unix.Openat(dir, part, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
		if e != nil {
			return nil, e
		}
		unix.Close(dir)
		dir = next
	}
	fd, e := unix.Openat(dir, parts[len(parts)-1], unix.O_RDONLY|unix.O_NOFOLLOW|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if e != nil {
		return nil, e
	}
	return os.NewFile(uintptr(fd), "capture"), nil
}
func same(a, b os.FileInfo) bool {
	return os.SameFile(a, b) && a.Size() == b.Size() && a.ModTime() == b.ModTime() && a.Mode() == b.Mode()
}
func (b *Backend) Capture(ctx context.Context, p workspace.Policy) (workspace.Capture, error) {
	for attempt := 0; attempt < 3; attempt++ {
		c, e := b.capture(ctx, p)
		if e == nil {
			return c, nil
		}
		if workspace.Safe(e).Code != "CAPTURE_UNSTABLE" {
			return c, e
		}
		if ctx.Err() != nil {
			break
		}
	}
	return workspace.Capture{}, workspace.Err("CAPTURE_UNSTABLE")
}
func (b *Backend) capture(ctx context.Context, p workspace.Policy) (workspace.Capture, error) {
	c := workspace.Capture{Files: []workspace.File{}, Warnings: []string{"Saved files; best-effort consistency, not a filesystem-atomic snapshot. Symlinks are never followed."}}
	// Reject bare repositories and configured subdirectories of a repository.
	top, e := b.command(ctx, "rev-parse", "--show-toplevel")
	if e != nil || strings.TrimSuffix(string(top), "\n") != b.Root {
		return c, workspace.Err("CAPTURE_UNAVAILABLE")
	}
	s, e := b.enumerate(ctx, p)
	if e != nil {
		return c, e
	}
	c.Provenance = s.provenance
	root, e := unix.Open(b.Root, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0)
	if e != nil {
		return c, workspace.Err("CAPTURE_UNAVAILABLE")
	}
	defer unix.Close(root)
	identities := map[string]os.FileInfo{}
	absent := []string{}
	var total int64
	for _, name := range s.paths {
		if ctx.Err() != nil {
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		if s.links[name] {
			c.Files = append(c.Files, workspace.File{Path: name, Kind: "gitlink", Omission: "submodule"})
			continue
		}
		f, e := openFile(root, name)
		if e != nil {
			if errors.Is(e, unix.ENOENT) {
				absent = append(absent, name)
				continue
			}
			if errors.Is(e, unix.ELOOP) || errors.Is(e, unix.ENOTDIR) {
				c.Files = append(c.Files, workspace.File{Path: name, Kind: "omitted", Omission: "symlink_or_non_directory"})
				continue
			}
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		st, e := f.Stat()
		if e != nil {
			f.Close()
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		if !st.Mode().IsRegular() {
			f.Close()
			c.Files = append(c.Files, workspace.File{Path: name, Kind: "omitted", Omission: "non_regular"})
			continue
		}
		identities[name] = st
		entry := workspace.File{Path: name, Size: st.Size(), Executable: st.Mode()&0111 != 0, Kind: "text"}
		if st.Size() > p.MaxFileBytes {
			f.Close()
			entry.Omission = "oversize"
			entry.Kind = "omitted"
			c.Files = append(c.Files, entry)
			continue
		}
		data, e := io.ReadAll(io.LimitReader(f, p.MaxFileBytes+1))
		after, se := f.Stat()
		f.Close()
		if e != nil || se != nil || !same(st, after) || int64(len(data)) != st.Size() {
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		total += int64(len(data))
		if total > p.MaxTotalBytes {
			return c, workspace.Err("CAPTURE_LIMIT_EXCEEDED")
		}
		if !utf8.Valid(data) || bytes.IndexByte(data, 0) >= 0 {
			entry.Kind = "binary"
		}
		entry.Data = data
		entry.Digest = workspace.Digest(data)
		c.Files = append(c.Files, entry)
	}
	for name, st := range identities {
		f, e := openFile(root, name)
		if e != nil {
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		now, e := f.Stat()
		f.Close()
		if e != nil || !same(st, now) {
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
	}
	for _, name := range absent {
		f, e := openFile(root, name)
		if e == nil {
			f.Close()
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
		if !errors.Is(e, unix.ENOENT) {
			return c, workspace.Err("CAPTURE_UNSTABLE")
		}
	}
	end, e := b.enumerate(ctx, p)
	if e != nil {
		return c, e
	}
	if s.fingerprint != end.fingerprint {
		return c, workspace.Err("CAPTURE_UNSTABLE")
	}
	return c, nil
}
