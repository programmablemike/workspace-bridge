package testutil

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func Git(t testing.TB, root string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.invalid", "GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.invalid")
	b, e := cmd.CombinedOutput()
	if e != nil {
		t.Fatalf("git %v: %v %s", args, e, b)
	}
	return string(b)
}
func Repo(t testing.TB) string {
	t.Helper()
	root, e := filepath.EvalSymlinks(t.TempDir())
	if e != nil {
		t.Fatal(e)
	}
	Git(t, root, "init", "-b", "main")
	return root
}
func Write(t testing.TB, root, p, body string) {
	t.Helper()
	if e := os.MkdirAll(filepath.Dir(filepath.Join(root, p)), 0700); e != nil {
		t.Fatal(e)
	}
	if e := os.WriteFile(filepath.Join(root, p), []byte(body), 0600); e != nil {
		t.Fatal(e)
	}
}
