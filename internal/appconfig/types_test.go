package appconfig

import (
	"os"
	"path/filepath"
	"testing"

	"sigs.k8s.io/yaml"
)

// TestLoadCarshowdbExample decodes the committed example through the production
// YAML codepath (sigs.k8s.io/yaml -> json tags), applies defaults, and asserts
// the derived names. This is the contract guard: if the example or the types
// drift, this fails.
func TestLoadCarshowdbExample(t *testing.T) {
	path := filepath.Join("..", "..", "examples", "carshowdb", "deploy.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read example: %v", err)
	}
	var app App
	if err := yaml.Unmarshal(raw, &app); err != nil {
		t.Fatalf("unmarshal example: %v", err)
	}
	app.ApplyDefaults()

	if app.App != "carshowdb" {
		t.Errorf("app = %q, want carshowdb", app.App)
	}
	if app.Runtime.Image != "ghcr.io/jakenesler/carshowdb-api" {
		t.Errorf("image = %q", app.Runtime.Image)
	}
	if app.Runtime.Port != 8080 {
		t.Errorf("port = %d, want 8080", app.Runtime.Port)
	}
	if len(app.DB) != 1 || app.DB[0].Name != "primary" || app.DB[0].Type != "postgres" {
		t.Errorf("db = %+v", app.DB)
	}
	if !app.Sizing.Autoscale.Enabled || app.Sizing.Autoscale.Max != 5 {
		t.Errorf("autoscale = %+v", app.Sizing.Autoscale)
	}
	// Autoscale kind defaulted.
	if app.Sizing.Autoscale.Kind != DefaultAutoscaleK {
		t.Errorf("autoscale kind = %q, want %q", app.Sizing.Autoscale.Kind, DefaultAutoscaleK)
	}
	if !app.LoggingEnabled() || !app.MetricsEnabled() {
		t.Errorf("logging/metrics should default on")
	}
}

func TestDerivedNames(t *testing.T) {
	a := App{App: "carshowdb"}
	cases := map[string]string{
		a.Namespace("dev"):  "carshowdb-dev-app", // no component => purpose "app"
		a.Namespace("prod"): "carshowdb-prod-app",
		a.ReleaseName():     "carshowdb",
		a.ServiceName():     "carshowdb",
		a.SecretName():      "carshowdb-runtime",
		a.ReleaseHandle():   "carshowdb",
		a.SSMRoot("prod"):   "/apps/carshowdb/prod",
		a.SSMCapabilityPath("dev", "db", "primary"): "/apps/carshowdb/dev/db/primary",
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// TestNamespaceScheme covers the {app}-{env}-{purpose} app namespace plus the
// per-store namespace helpers and the RFC1123 sanitizer.
func TestNamespaceScheme(t *testing.T) {
	api := App{App: "carshowdb", Component: "api"}
	if got := api.Namespace("dev"); got != "carshowdb-dev-api" {
		t.Errorf("app ns = %q, want carshowdb-dev-api", got)
	}
	if got := api.Namespace("prod"); got != "carshowdb-prod-api" {
		t.Errorf("prod app ns = %q, want carshowdb-prod-api", got)
	}
	if got := api.Purpose(); got != "api" {
		t.Errorf("purpose = %q, want api", got)
	}
	noComp := App{App: "anyrent"}
	if got := noComp.Purpose(); got != DefaultComponent {
		t.Errorf("default purpose = %q, want %q", got, DefaultComponent)
	}
	// Store namespaces: first/default store uses the bare tool; a second store of
	// the same tool disambiguates with the store name.
	if got := api.StoreNamespace("dev", "postgres", "primary", false); got != "carshowdb-dev-postgres" {
		t.Errorf("primary store ns = %q, want carshowdb-dev-postgres", got)
	}
	if got := api.StoreNamespace("dev", "postgres", "analytics", true); got != "carshowdb-dev-postgres-analytics" {
		t.Errorf("secondary store ns = %q, want carshowdb-dev-postgres-analytics", got)
	}
	if got := api.StoreNamespace("dev", "redis", "default", false); got != "carshowdb-dev-redis" {
		t.Errorf("redis store ns = %q, want carshowdb-dev-redis", got)
	}
}

func TestSanitizeDNSLabel(t *testing.T) {
	cases := map[string]string{
		"CarShow_DB":          "carshow-db",
		"a..b":                "a-b",
		"-lead-trail-":        "lead-trail",
		"already-good":        "already-good",
		"UPPER/case path":     "upper-case-path",
	}
	for in, want := range cases {
		if got := SanitizeDNSLabel(in); got != want {
			t.Errorf("SanitizeDNSLabel(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestEnvPrefix(t *testing.T) {
	cases := map[string]string{
		"publicAssets":    "PUBLIC_ASSETS",
		"private-uploads": "PRIVATE_UPLOADS",
		"primary":         "PRIMARY",
		"default":         "DEFAULT",
		"S3":              "S3",
	}
	for in, want := range cases {
		if got := EnvPrefix(in); got != want {
			t.Errorf("EnvPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestLabels(t *testing.T) {
	a := App{App: "carshowdb", Product: "carshow", Component: "api"}
	l := a.Labels("dev")
	want := map[string]string{
		"app.kubernetes.io/name":     "carshowdb",
		"app.kubernetes.io/instance": "carshowdb",
		"platform/app":               "carshowdb",
		"platform/env":               "dev",
		"platform/managed-by":        "platformctl",
		"platform/product":           "carshow",
		"platform/component":         "api",
	}
	for k, v := range want {
		if l[k] != v {
			t.Errorf("label %q = %q, want %q", k, l[k], v)
		}
	}
}

func TestResolve(t *testing.T) {
	a := App{App: "carshowdb", Runtime: Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080}}
	r := a.Resolve("dev")
	if r.Namespace != "carshowdb-dev-app" || r.Secret != "carshowdb-runtime" || r.SSMRoot != "/apps/carshowdb/dev" {
		t.Errorf("resolved = %+v", r)
	}
	if r.App.Sizing.Profile != DefaultProfile {
		t.Errorf("Resolve should apply defaults, profile = %q", r.App.Sizing.Profile)
	}
}
