package config

import (
	"fmt"
	"github.com/bmatcuk/doublestar/v4"
	"github.com/programmablemike/workspace-bridge/internal/workspace"
	"gopkg.in/yaml.v3"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

type Workspace struct {
	ID           string `yaml:"id"`
	Name         string `yaml:"name"`
	Backend      string `yaml:"backend"`
	Root         string `yaml:"root"`
	PollInterval string `yaml:"poll_interval"`
}
type Config struct {
	Server struct {
		Listen    string `yaml:"listen"`
		Transport string `yaml:"transport"`
		TokenEnv  string `yaml:"auth_token_env"`
		TokenFile string `yaml:"auth_token_file"`
	} `yaml:"server"`
	Storage struct {
		Path          string `yaml:"path"`
		RetentionDays int    `yaml:"retention_days"`
		MaxBytes      int64  `yaml:"max_bytes"`
	} `yaml:"storage"`
	Observer struct {
		PollInterval   string `yaml:"poll_interval"`
		CaptureTimeout string `yaml:"capture_timeout"`
	} `yaml:"observer"`
	Capture    workspace.Policy `yaml:"capture"`
	Workspaces []Workspace      `yaml:"workspaces"`
}

var idPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func ValidID(s string) bool { return idPattern.MatchString(s) }
func Load(p string) (Config, error) {
	var c Config
	c.Server.Listen = "127.0.0.1:8765"
	c.Server.Transport = "streamable-http"
	c.Server.TokenEnv = "WORKSPACE_BRIDGE_TOKEN"
	c.Storage.Path = "~/.local/state/workspace-bridge"
	c.Storage.RetentionDays = 7
	c.Storage.MaxBytes = 1 << 30
	c.Observer.PollInterval = "60s"
	c.Observer.CaptureTimeout = "20s"
	c.Capture = workspace.Policy{MaxFiles: 5000, MaxFileBytes: 1 << 20, MaxTotalBytes: 50 << 20}
	f, e := os.Open(p)
	if e != nil {
		return c, fmt.Errorf("cannot open administrator configuration")
	}
	defer f.Close()
	d := yaml.NewDecoder(f)
	d.KnownFields(true)
	if e = d.Decode(&c); e != nil {
		return c, fmt.Errorf("invalid administrator configuration")
	}
	return c, c.Validate()
}
func Canonical(p string) (string, error) {
	if strings.HasPrefix(p, "~/") {
		h, e := os.UserHomeDir()
		if e != nil {
			return "", e
		}
		p = filepath.Join(h, p[2:])
	}
	if !filepath.IsAbs(p) {
		return "", fmt.Errorf("absolute path required")
	}
	p = filepath.Clean(p)
	suffix := []string{}
	for {
		v, e := filepath.EvalSymlinks(p)
		if e == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				v = filepath.Join(v, suffix[i])
			}
			return v, nil
		}
		if !os.IsNotExist(e) {
			return "", e
		}
		suffix = append(suffix, filepath.Base(p))
		p = filepath.Dir(p)
	}
}
func Inside(root, p string) bool {
	r, e := filepath.Rel(root, p)
	return e == nil && (r == "." || r != ".." && !strings.HasPrefix(r, ".."+string(filepath.Separator)))
}
func (c *Config) Validate() error {
	bad := func() error {
		return fmt.Errorf("invalid configuration: check loopback address, absolute non-overlapping roots, IDs, durations and positive limits")
	}
	host, _, e := net.SplitHostPort(c.Server.Listen)
	ip := net.ParseIP(host)
	if e != nil || ip == nil || !ip.IsLoopback() || c.Server.Transport != "streamable-http" {
		return bad()
	}
	for _, s := range []string{c.Observer.PollInterval, c.Observer.CaptureTimeout} {
		d, e := time.ParseDuration(s)
		if e != nil || d <= 0 {
			return bad()
		}
	}
	if c.Capture.MaxFiles <= 0 || c.Capture.MaxFiles > 100000 || c.Capture.MaxFileBytes <= 0 || c.Capture.MaxFileBytes > 50<<20 || c.Capture.MaxTotalBytes <= 0 || c.Capture.MaxTotalBytes > 1<<30 || c.Storage.RetentionDays <= 0 || c.Storage.MaxBytes <= 0 {
		return bad()
	}
	for _, s := range append(append([]string{}, c.Capture.Exclude...), c.Capture.Allow...) {
		if !doublestar.ValidatePattern(s) {
			return bad()
		}
	}
	c.Storage.Path, e = Canonical(c.Storage.Path)
	if e != nil {
		return bad()
	}
	if c.Server.TokenFile != "" {
		c.Server.TokenFile, e = Canonical(c.Server.TokenFile)
		if e != nil {
			return bad()
		}
	}
	seen := map[string]bool{}
	if len(c.Workspaces) == 0 || len(c.Workspaces) > 1000 {
		return bad()
	}
	for i := range c.Workspaces {
		w := &c.Workspaces[i]
		if len(w.Name) > 256 || !utf8.ValidString(w.Name) {
			return bad()
		}
		if !ValidID(w.ID) || seen[w.ID] || w.Backend != "git" {
			return bad()
		}
		seen[w.ID] = true
		w.Root, e = Canonical(w.Root)
		if e != nil || Inside(w.Root, c.Storage.Path) || Inside(c.Storage.Path, w.Root) {
			return bad()
		}
		if c.Server.TokenFile != "" && Inside(w.Root, c.Server.TokenFile) {
			return fmt.Errorf("token file must be outside every observed workspace")
		}
		st, e := os.Stat(w.Root)
		if e != nil || !st.IsDir() {
			return bad()
		}
		if w.PollInterval != "" {
			d, e := time.ParseDuration(w.PollInterval)
			if e != nil || d <= 0 {
				return bad()
			}
		}
	}
	return nil
}
func (c Config) Token() (string, error) {
	s := ""
	if c.Server.TokenFile != "" {
		st, e := os.Lstat(c.Server.TokenFile)
		if e != nil || !st.Mode().IsRegular() || st.Mode().Perm()&0077 != 0 || st.Size() > 4096 {
			return "", fmt.Errorf("token file must be a private regular file")
		}
		b, e := os.ReadFile(c.Server.TokenFile)
		if e != nil {
			return "", fmt.Errorf("cannot read token file")
		}
		s = strings.TrimSpace(string(b))
	} else {
		s = os.Getenv(c.Server.TokenEnv)
	}
	if len(s) < 32 || len(s) > 4096 || strings.ContainsAny(s, "\r\n") {
		return "", fmt.Errorf("a bearer token of at least 32 bytes is required")
	}
	return s, nil
}
