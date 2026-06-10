package clusterenv

import (
	"strings"
	"testing"
)

func validConfig() *Config {
	c := &Config{
		Env: "dev",
		Secrets: SecretsConfig{
			Backend:  BackendLocal,
			StoreRef: StoreRef{Name: "platform-local", Kind: KindClusterSecretStore},
		},
		Flux: FluxConfig{RepoURL: "https://github.com/example-org/idp.git"},
	}
	c.ApplyDefaults()
	return c
}

func TestApplyDefaults_BackendByEnv(t *testing.T) {
	dev := &Config{Env: "dev", Secrets: SecretsConfig{StoreRef: StoreRef{Name: "s"}}}
	dev.ApplyDefaults()
	if dev.Secrets.Backend != BackendLocal {
		t.Errorf("dev backend = %q, want local", dev.Secrets.Backend)
	}
	prod := &Config{Env: "prod", Secrets: SecretsConfig{StoreRef: StoreRef{Name: "s"}}}
	prod.ApplyDefaults()
	if prod.Secrets.Backend != BackendSSM {
		t.Errorf("prod backend = %q, want ssm", prod.Secrets.Backend)
	}
}

func TestApplyDefaults_Fills(t *testing.T) {
	c := validConfig()
	if c.Domain != DefaultDomain {
		t.Errorf("domain = %q", c.Domain)
	}
	if c.Flux.Namespace != DefaultFluxNamespace {
		t.Errorf("flux ns = %q", c.Flux.Namespace)
	}
	if c.Flux.SourceName != DefaultFluxSourceName {
		t.Errorf("flux sourceName = %q", c.Flux.SourceName)
	}
	if c.Flux.Branch != DefaultBranch {
		t.Errorf("flux branch = %q", c.Flux.Branch)
	}
}

func TestValidate_RepoURLRequired(t *testing.T) {
	// Instance identity must fail closed: no jakenesler (or any) default.
	c := validConfig()
	c.Flux.RepoURL = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "flux.repoURL") {
		t.Errorf("empty flux.repoURL must be rejected, got %v", err)
	}
}

func TestBranchRef(t *testing.T) {
	if got := BranchRef("main"); got != "refs/heads/main" {
		t.Errorf("BranchRef(main) = %q", got)
	}
	if got := BranchRef(""); got != "refs/heads/main" {
		t.Errorf("BranchRef(empty) should default to main, got %q", got)
	}
	if got := BranchRef("release"); got != "refs/heads/release" {
		t.Errorf("BranchRef(release) = %q", got)
	}
}

func TestValidate_BackendAndStore(t *testing.T) {
	c := validConfig()
	c.Secrets.Backend = "vault"
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "backend") {
		t.Errorf("expected backend error, got %v", err)
	}

	c = validConfig()
	c.Secrets.StoreRef.Name = ""
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "storeRef.name") {
		t.Errorf("expected storeRef.name error, got %v", err)
	}
}

func TestValidate_ModuleRules(t *testing.T) {
	// chartRepo without version is invalid.
	c := validConfig()
	c.Modules = map[string]Module{
		"keda": {Enabled: true, Source: SourceChartRepo, Chart: "keda"},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "version") {
		t.Errorf("chartRepo without version should fail, got %v", err)
	}

	// localChart without chart path is invalid.
	c = validConfig()
	c.Modules = map[string]Module{
		"pg": {Enabled: true, Source: SourceLocalChart},
	}
	if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "chart path") {
		t.Errorf("localChart without chart should fail, got %v", err)
	}

	// disabled modules skip validation.
	c = validConfig()
	c.Modules = map[string]Module{
		"yscale": {Enabled: false, Source: "anything"},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("disabled module should not be validated, got %v", err)
	}

	// valid mix.
	c = validConfig()
	c.Modules = map[string]Module{
		"keda": {Enabled: true, Source: SourceChartRepo, Chart: "keda", Version: "2.17.2"},
		"pg":   {Enabled: true, Source: SourceLocalChart, Chart: "charts/infra/dev-postgres"},
	}
	if err := c.Validate(); err != nil {
		t.Errorf("valid modules should pass, got %v", err)
	}
}

func TestHostInZone(t *testing.T) {
	c := &Config{Zones: []string{"carshowdb.local", "*.example.com"}}
	cases := map[string]bool{
		"carshowdb.local":     true,  // exact zone match
		"api.example.com":     true,  // wildcard subdomain
		"example.com":         true,  // wildcard apex match
		"evil.attacker.com":   false, // outside every zone
		"sub.api.example.com": true,  // nested wildcard subdomain
		"sub.carshowdb.local": true,  // subdomain of an exact zone
	}
	for host, want := range cases {
		if got := c.HostInZone(host); got != want {
			t.Errorf("HostInZone(%q) = %v, want %v", host, got, want)
		}
	}

	// No zones configured -> unrestricted.
	open := &Config{}
	if !open.HostInZone("anything.com") {
		t.Error("no zones should be unrestricted")
	}
}

func TestConsoleLoggingValue(t *testing.T) {
	on := Observability{}
	if on.ConsoleLoggingValue() != "true" {
		t.Error("default console logging should be true")
	}
	f := false
	off := Observability{ConsoleLogging: &f}
	if off.ConsoleLoggingValue() != "false" {
		t.Error("explicit false should be false")
	}
}
