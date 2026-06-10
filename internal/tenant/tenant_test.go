package tenant

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeTenant(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, File), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLoad_Valid(t *testing.T) {
	dir := writeTenant(t, "registry: ghcr.io/example-org\nrepo:\n  url: https://github.com/example-org/idp.git\n")
	c, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Registry != "ghcr.io/example-org" {
		t.Errorf("registry = %q", c.Registry)
	}
	if c.Repo.Branch != "main" {
		t.Errorf("branch should default to main, got %q", c.Repo.Branch)
	}
}

func TestLoad_MissingFileFailsClosed(t *testing.T) {
	_, err := Load(t.TempDir())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("missing idp.yaml must be ErrNotFound, got %v", err)
	}
	// The error must teach the fix, not just report the failure.
	if !strings.Contains(err.Error(), "registry:") {
		t.Errorf("error should include a starter snippet:\n%v", err)
	}
}

func TestValidate_IdentityRequired(t *testing.T) {
	cases := []struct {
		name, content, want string
	}{
		{"no registry", "repo:\n  url: https://github.com/x/y.git\n", "registry is required"},
		{"registry with scheme", "registry: https://ghcr.io/x\nrepo:\n  url: https://github.com/x/y.git\n", "no scheme"},
		{"registry trailing slash", "registry: ghcr.io/x/\nrepo:\n  url: https://github.com/x/y.git\n", "slash"},
		{"no repo url", "registry: ghcr.io/x\n", "repo.url is required"},
		{"bad repo url", "registry: ghcr.io/x\nrepo:\n  url: ftp://nope\n", "must be an https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Load(writeTenant(t, tc.content))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}
