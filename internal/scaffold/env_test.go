package scaffold

import (
	"strings"
	"testing"

	"github.com/jakenesler/idp/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

// loadEnv parses + defaults + validates the generated cluster.yaml exactly as
// clusterenv.Load would, so a test failure means idpctl would reject it too.
func loadEnv(t *testing.T, files Files, env string) *clusterenv.Config {
	t.Helper()
	data, ok := files["environments/"+env+"/cluster.yaml"]
	if !ok {
		t.Fatalf("no cluster.yaml generated for env %q (got keys %v)", env, keysOf(files))
	}
	var c clusterenv.Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		t.Fatalf("generated cluster.yaml does not parse: %v", err)
	}
	if c.Env == "" {
		c.Env = env
	}
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("generated cluster.yaml is invalid: %v", err)
	}
	return &c
}

func keysOf(f Files) []string {
	var k []string
	for key := range f {
		k = append(k, key)
	}
	return k
}

func TestGenerateEnv_DevValidatesRoundTrip(t *testing.T) {
	files, err := GenerateEnv(EnvOptions{Env: "dev", RepoURL: "https://github.com/acme/idp.git", Branch: "main"})
	if err != nil {
		t.Fatal(err)
	}
	c := loadEnv(t, files, "dev")
	if c.Flux.RepoURL != "https://github.com/acme/idp.git" {
		t.Errorf("flux.repoURL should come from the caller, got %q", c.Flux.RepoURL)
	}
	if c.Secrets.Backend != "local" {
		t.Errorf("dev backend should default to local, got %q", c.Secrets.Backend)
	}
	if !c.AllowMutableTags {
		t.Error("dev should allow mutable tags")
	}
	// publicRoutes off so the template needs no zones to validate.
	if seams := c.EffectiveSeams(); seams.PublicRoutes {
		t.Error("dev publicRoutes should be off by default")
	}
}

func TestGenerateEnv_ProdDefaults(t *testing.T) {
	files, err := GenerateEnv(EnvOptions{Env: "prod", RepoURL: "https://github.com/acme/idp.git"})
	if err != nil {
		t.Fatal(err)
	}
	c := loadEnv(t, files, "prod")
	if c.Secrets.Backend != "ssm" {
		t.Errorf("prod backend should default to ssm, got %q", c.Secrets.Backend)
	}
	if c.AllowMutableTags {
		t.Error("prod must NOT allow mutable tags")
	}
	if c.Promotion == nil || c.Promotion.From != "stage" {
		t.Errorf("prod should promote from stage, got %+v", c.Promotion)
	}
}

func TestGenerateEnv_NoIdentityLeaks(t *testing.T) {
	files, err := GenerateEnv(EnvOptions{Env: "dev", RepoURL: "https://github.com/acme/idp.git"})
	if err != nil {
		t.Fatal(err)
	}
	out := string(files["environments/dev/cluster.yaml"])
	for _, leak := range []string{"jakenesler", "10.0.0.", "Yscale-sh", "Cartogopher"} {
		if strings.Contains(out, leak) {
			t.Errorf("generated cluster.yaml leaked an instance specific value: %q", leak)
		}
	}
}

func TestGenerateEnv_FailClosed(t *testing.T) {
	if _, err := GenerateEnv(EnvOptions{Env: "dev"}); err == nil {
		t.Error("missing repoURL must fail closed")
	}
	if _, err := GenerateEnv(EnvOptions{RepoURL: "https://github.com/acme/idp.git"}); err == nil {
		t.Error("missing env must fail closed")
	}
	if _, err := GenerateEnv(EnvOptions{Env: "dev", RepoURL: "x", Backend: "vault"}); err == nil {
		t.Error("invalid backend must fail closed")
	}
}

func TestGenerateEnv_BackendOverride(t *testing.T) {
	files, err := GenerateEnv(EnvOptions{Env: "dev", RepoURL: "https://github.com/acme/idp.git", Backend: "ssm", StoreName: "my-store"})
	if err != nil {
		t.Fatal(err)
	}
	c := loadEnv(t, files, "dev")
	if c.Secrets.Backend != "ssm" || c.Secrets.StoreRef.Name != "my-store" {
		t.Errorf("backend/store override not honored: %+v", c.Secrets)
	}
}
