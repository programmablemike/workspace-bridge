package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/programmablemike/workspace-bridge/internal/testutil"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"golang.org/x/sys/unix"
)

func policy() workspace.Policy {
	return workspace.Policy{MaxFiles: 5000, MaxFileBytes: 1 << 20, MaxTotalBytes: 50 << 20}
}
func capture(t *testing.T, b *Backend) workspace.Capture {
	t.Helper()
	c, e := b.Capture(context.Background(), policy())
	if e != nil {
		t.Fatal(e)
	}
	return c
}
func files(c workspace.Capture) map[string]workspace.File {
	out := map[string]workspace.File{}
	for _, f := range c.Files {
		out[f.Path] = f
	}
	return out
}
func TestGitScenarios(t *testing.T) {
	root := testutil.Repo(t)
	b := &Backend{Root: root}
	testutil.Write(t, root, "solution.go", "old\n")
	testutil.Write(t, root, "delete.go", "remove\n")
	testutil.Write(t, root, ".env", "DO_NOT_CAPTURE")
	testutil.Git(t, root, "add", ".")
	testutil.Git(t, root, "commit", "-m", "initial")
	clean := capture(t, b)
	if len(clean.Files) != 2 {
		t.Fatal(clean.Files)
	}
	testutil.Write(t, root, "solution.go", "staged\n")
	testutil.Git(t, root, "add", "solution.go")
	testutil.Write(t, root, "solution.go", "saved\n")
	testutil.Write(t, root, "added.go", "staged addition\n")
	testutil.Git(t, root, "add", "added.go")
	testutil.Git(t, root, "rm", "--cached", "delete.go")
	for _, p := range []string{"untracked.go", "space name.go", "tab\tfile.go", "new\nline.go", "日本語.go"} {
		testutil.Write(t, root, p, "new\n")
	}
	testutil.Write(t, root, ".gitignore", "ignored\n")
	testutil.Write(t, root, "ignored", "no")
	testutil.Write(t, root, "secrets.pem", "no")
	before := testutil.Git(t, root, "status", "--porcelain=v2", "-z")
	indexBefore, _ := os.ReadFile(filepath.Join(root, ".git/index"))
	headBefore, _ := os.ReadFile(filepath.Join(root, ".git/HEAD"))
	c := capture(t, b)
	m := files(c)
	if string(m["solution.go"].Data) != "saved\n" || string(m["delete.go"].Data) != "remove\n" || m["added.go"].Digest == "" {
		t.Fatal(m)
	}
	for _, p := range []string{"ignored", "secrets.pem", ".env"} {
		if _, ok := m[p]; ok {
			t.Fatalf("excluded path exposed: %q", p)
		}
	}
	after := testutil.Git(t, root, "status", "--porcelain=v2", "-z")
	indexAfter, _ := os.ReadFile(filepath.Join(root, ".git/index"))
	headAfter, _ := os.ReadFile(filepath.Join(root, ".git/HEAD"))
	if before != after || string(indexBefore) != string(indexAfter) || string(headBefore) != string(headAfter) {
		t.Fatal("observer modified git")
	}
	if e := os.Remove(filepath.Join(root, "delete.go")); e != nil {
		t.Fatal(e)
	}
	if _, ok := files(capture(t, b))["delete.go"]; ok {
		t.Fatal("deleted file captured")
	}
	testutil.Git(t, root, "mv", "added.go", "renamed.go")
	m = files(capture(t, b))
	if _, ok := m["added.go"]; ok || m["renamed.go"].Digest == "" {
		t.Fatal("rename failed")
	}
	testutil.Git(t, root, "checkout", "--detach")
	if capture(t, b).Provenance["git.branch"] != "(detached)" {
		t.Fatal("detached metadata")
	}
}
func TestUnbornAndSafeFiles(t *testing.T) {
	root := testutil.Repo(t)
	b := &Backend{Root: root}
	if len(capture(t, b).Files) != 0 {
		t.Fatal("empty")
	}
	testutil.Write(t, root, "binary", string([]byte{0, 1, 255}))
	testutil.Write(t, root, "big", strings.Repeat("x", (1<<20)+1))
	outside := t.TempDir()
	testutil.Write(t, outside, "private", "private")
	if e := os.Symlink(filepath.Join(outside, "private"), filepath.Join(root, "link")); e != nil {
		t.Fatal(e)
	}
	if e := os.Symlink(outside, filepath.Join(root, "nested")); e != nil {
		t.Fatal(e)
	}
	testutil.Write(t, root, "fifo", "placeholder")
	testutil.Git(t, root, "add", "fifo")
	os.Remove(filepath.Join(root, "fifo"))
	if e := unix.Mkfifo(filepath.Join(root, "fifo"), 0600); e != nil {
		t.Fatal(e)
	}
	c := capture(t, b)
	m := files(c)
	if m["binary"].Kind != "binary" || m["big"].Omission != "oversize" || m["link"].Digest != "" || m["fifo"].Omission != "non_regular" {
		t.Fatal(m)
	}
	fd, e := unix.Open(root, unix.O_RDONLY|unix.O_DIRECTORY, 0)
	if e != nil {
		t.Fatal(e)
	}
	defer unix.Close(fd)
	if f, e := openFile(fd, "nested/private"); e == nil {
		f.Close()
		t.Fatal("followed nested symlink")
	}
	p := policy()
	p.MaxFiles = 1
	if _, e = b.Capture(context.Background(), p); workspace.Safe(e).Code != "CAPTURE_LIMIT_EXCEEDED" {
		t.Fatal(e)
	}
	p = policy()
	p.MaxTotalBytes = 1
	if _, e = b.Capture(context.Background(), p); workspace.Safe(e).Code != "CAPTURE_LIMIT_EXCEEDED" {
		t.Fatal(e)
	}
}
func TestDisableFSMonitor(t *testing.T) {
	root := testutil.Repo(t)
	sentinel := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(t.TempDir(), "helper")
	if e := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+sentinel+"'\n"), 0700); e != nil {
		t.Fatal(e)
	}
	testutil.Write(t, root, "file", "safe")
	testutil.Git(t, root, "add", "file")
	testutil.Git(t, root, "config", "core.fsmonitor", script)
	capture(t, &Backend{Root: root})
	if _, e := os.Stat(sentinel); !os.IsNotExist(e) {
		t.Fatal("external helper executed")
	}
}
func TestSubmoduleOpaque(t *testing.T) {
	root := testutil.Repo(t)
	testutil.Write(t, root, "file", "a")
	testutil.Git(t, root, "add", "file")
	testutil.Git(t, root, "commit", "-m", "base")
	sha := strings.TrimSpace(testutil.Git(t, root, "rev-parse", "HEAD"))
	testutil.Git(t, root, "update-index", "--add", "--cacheinfo", "160000,"+sha+",sub")
	testutil.Write(t, root, "sub/inside", "hidden")
	m := files(capture(t, &Backend{Root: root}))
	if m["sub"].Omission != "submodule" {
		t.Fatal(m)
	}
	if _, ok := m["sub/inside"]; ok {
		t.Fatal("submodule traversed")
	}
}
func TestSymlinkSwapAndConcurrentWrites(t *testing.T) {
	root := testutil.Repo(t)
	outside := t.TempDir()
	testutil.Write(t, outside, "private", "never disclose")
	testutil.Write(t, root, "file", strings.Repeat("a", 65536))
	testutil.Git(t, root, "add", "file")
	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for ctx.Err() == nil {
			os.Remove(filepath.Join(root, "file"))
			os.Symlink(filepath.Join(outside, "private"), filepath.Join(root, "file"))
			os.Remove(filepath.Join(root, "file"))
			f, e := os.OpenFile(filepath.Join(root, "file"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
			if e == nil {
				f.Write([]byte("safe"))
				f.Close()
			}
		}
	}()
	for i := 0; i < 10; i++ {
		c, e := (&Backend{Root: root}).Capture(ctx, policy())
		if e != nil && workspace.Safe(e).Code != "CAPTURE_UNSTABLE" && workspace.Safe(e).Code != "CAPTURE_UNAVAILABLE" {
			t.Fatal(e)
		}
		for _, f := range c.Files {
			if strings.Contains(string(f.Data), "never disclose") {
				t.Fatal("symlink leaked")
			}
		}
	}
	cancel()
	wg.Wait()
}

func TestNeverRunCleanFilter(t *testing.T) {
	root := testutil.Repo(t)
	testutil.Write(t, root, "file", "aaaa\n")
	testutil.Write(t, root, ".gitattributes", "file filter=hostile\n")
	testutil.Git(t, root, "add", ".")
	testutil.Git(t, root, "commit", "-m", "base")
	sentinel := filepath.Join(t.TempDir(), "executed")
	script := filepath.Join(t.TempDir(), "filter")
	if e := os.WriteFile(script, []byte("#!/bin/sh\ntouch '"+sentinel+"'\ncat\n"), 0700); e != nil {
		t.Fatal(e)
	}
	testutil.Git(t, root, "config", "filter.hostile.clean", script)
	testutil.Write(t, root, "file", "bbbb\n")
	capture(t, &Backend{Root: root})
	if _, e := os.Stat(sentinel); !os.IsNotExist(e) {
		t.Fatal("capture executed repository-configured clean filter")
	}
}
