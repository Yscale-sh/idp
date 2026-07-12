// Package tenant models idp.yaml: the platform-instance identity file at the
// platform repo root. It carries the values that are unique to ONE installation
// of the platform — the image registry prefix and the platform repo itself —
// so that nothing identity-shaped is ever baked into Go code or chart defaults.
//
// The contract is FAIL-CLOSED: there are no fallback values. A fork that has
// not written its own idp.yaml gets a hard error pointing at this file, never
// a silent push to someone else's registry or a sync from someone else's repo.
package tenant

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

// File is the canonical filename, at the platform repo root.
const File = "idp.yaml"

// ErrNotFound marks a missing idp.yaml; errors.Is(err, ErrNotFound) detects it.
var ErrNotFound = errors.New("tenant config not found")

// Config is the parsed idp.yaml.
type Config struct {
	// Registry is the image registry prefix apps push to and pull from,
	// e.g. "ghcr.io/your-org". Scaffold derives image repos from it
	// (<registry>/<app>). Required.
	Registry string `json:"registry,omitempty"`

	// Repo is this platform repo (YOUR fork) — the repo Flux syncs desired
	// state from and the shipper commits rendered state to.
	Repo Repo `json:"repo,omitempty"`
}

// Repo identifies the platform git repo.
type Repo struct {
	// URL is the git URL of the platform repo, e.g.
	// "https://github.com/your-org/idp.git". Required.
	URL string `json:"url,omitempty"`

	// Branch is the default branch environments track. Defaults to "main";
	// per-env cluster.yaml flux.branch overrides.
	Branch string `json:"branch,omitempty"`
}

// Load reads <root>/idp.yaml. A missing file returns ErrNotFound (wrapped) with
// guidance; a present-but-invalid file is an error.
func Load(root string) (*Config, error) {
	path := filepath.Join(root, File)
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s — every platform instance declares its own identity; create it with at least:\n\n  registry: <your image registry prefix, e.g. ghcr.io/your-org>\n  repo:\n    url: <git URL of YOUR platform repo fork>\n", ErrNotFound, path)
		}
		return nil, fmt.Errorf("read tenant config %q: %w", path, err)
	}
	var c Config
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse tenant config %q: %w", path, err)
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("invalid tenant config %q: %w", path, err)
	}
	return &c, nil
}

// ApplyDefaults fills the non-identity conveniences (idempotent). Identity
// fields (registry, repo.url) are never defaulted.
func (c *Config) ApplyDefaults() {
	if c.Repo.Branch == "" {
		c.Repo.Branch = "main"
	}
}

// Validate checks the identity fields are present and plausible.
func (c *Config) Validate() error {
	switch {
	case c.Registry == "":
		return fmt.Errorf("registry is required (your image registry prefix, e.g. ghcr.io/your-org)")
	case strings.Contains(c.Registry, "://"):
		return fmt.Errorf("registry %q must be a bare registry prefix (no scheme), e.g. ghcr.io/your-org", c.Registry)
	case strings.HasSuffix(c.Registry, "/"):
		return fmt.Errorf("registry %q must not end with a slash", c.Registry)
	}
	switch {
	case c.Repo.URL == "":
		return fmt.Errorf("repo.url is required (the git URL of YOUR platform repo fork)")
	case !strings.HasPrefix(c.Repo.URL, "https://") && !strings.HasPrefix(c.Repo.URL, "ssh://") && !strings.HasPrefix(c.Repo.URL, "git@"):
		return fmt.Errorf("repo.url %q must be an https://, ssh:// or git@ URL", c.Repo.URL)
	}
	return nil
}
