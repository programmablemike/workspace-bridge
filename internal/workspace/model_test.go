package workspace

import "testing"

func TestPolicy(t *testing.T) {
	p := Policy{Exclude: []string{"**/credentials/**", "**/*.secret"}, Allow: []string{"**/*.go", "README.md"}}
	for _, name := range []string{"../x", "/a", "a/../b", "a\\b", "%2e%2e/x", "a\x00b", ".git/config", "foo/.env", "tracked.key", "foo/credentials/a.go", "a.secret", "node_modules/a.go", "a.txt"} {
		if p.Allowed(name) {
			t.Errorf("allowed %q", name)
		}
	}
	for _, name := range []string{"a.go", "dir/a.go", "new\nline.go", "README.md"} {
		if !p.Allowed(name) {
			t.Errorf("denied %q", name)
		}
	}
}
