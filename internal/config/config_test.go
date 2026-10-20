package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestDefaultsAndValidation(t *testing.T) {
	root := t.TempDir()
	state := t.TempDir()
	f := filepath.Join(t.TempDir(), "config.yaml")
	write := func(listen, store, extra string) {
		t.Helper()
		body := "server:\n  listen: \"" + listen + "\"\nstorage:\n  path: \"" + store + "\"\nworkspaces:\n  - id: w\n    name: Test\n    backend: git\n    root: \"" + root + "\"\n" + extra
		if e := os.WriteFile(f, []byte(body), 0600); e != nil {
			t.Fatal(e)
		}
	}
	write("127.0.0.1:8765", state, "")
	c, e := Load(f)
	if e != nil || c.Capture.MaxFiles != 5000 {
		t.Fatal(e)
	}
	for _, host := range []string{"0.0.0.0:8765", "example.com:8765", "[::]:8765"} {
		write(host, state, "")
		if _, e := Load(f); e == nil {
			t.Fatal("non-loopback allowed")
		}
	}
	write("127.0.0.1:8765", filepath.Join(root, "state"), "")
	if _, e := Load(f); e == nil {
		t.Fatal("state in repo allowed")
	}
	write("127.0.0.1:8765", state, "unknown: true\n")
	if _, e := Load(f); e == nil {
		t.Fatal("unknown setting accepted")
	}
	if p, e := Canonical("~/state"); e != nil || !filepath.IsAbs(p) {
		t.Fatal(p, e)
	}
}
