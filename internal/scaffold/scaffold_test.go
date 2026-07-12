package scaffold

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"sigs.k8s.io/yaml"
)

func TestGenerate_DeployValidatesRoundTrip(t *testing.T) {
	files, err := Generate(Options{Name: "dummy-api", Registry: "ghcr.io/example-org", Host: "api.example.com", Port: 8080, WithDB: true})
	if err != nil {
		t.Fatal(err)
	}
	deploy, ok := files["deploy.yaml"]
	if !ok {
		t.Fatal("no deploy.yaml generated")
	}
	var app appconfig.App
	if err := yaml.Unmarshal(deploy, &app); err != nil {
		t.Fatalf("generated deploy.yaml does not parse: %v", err)
	}
	app.ApplyDefaults()
	if err := app.Validate(); err != nil {
		t.Fatalf("generated deploy.yaml is invalid: %v", err)
	}
	if app.App != "dummy-api" || app.Runtime.Port != 8080 {
		t.Errorf("unexpected app: %+v", app)
	}
	if app.Runtime.Image != "ghcr.io/example-org/dummy-api" {
		t.Errorf("image should derive from the tenant registry, got %q", app.Runtime.Image)
	}
	if len(app.DB) != 1 {
		t.Errorf("with-db should add one db, got %+v", app.DB)
	}
}

func TestGenerate_NoIdentityLeaks(t *testing.T) {
	// The scaffold must contain ONLY the caller-supplied identity: no baked-in
	// registry/org, and no ship.yml stub (the reusable workflow it referenced
	// does not exist — the shipper is the CD path).
	files, err := Generate(Options{Name: "dummy-api", Registry: "ghcr.io/example-org"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := files[".github/workflows/ship.yml"]; ok {
		t.Error("scaffold must not emit the phantom ship.yml workflow stub")
	}
	if strings.Contains(string(files["deploy.yaml"]), "legacy-owner") {
		t.Error("scaffold output leaked a hardcoded identity")
	}
}

func TestGenerate_RequiresName(t *testing.T) {
	if _, err := Generate(Options{Registry: "ghcr.io/example-org"}); err == nil {
		t.Error("expected error for empty name")
	}
}

func TestGenerate_RequiresRegistry(t *testing.T) {
	_, err := Generate(Options{Name: "dummy-api"})
	if err == nil || !strings.Contains(err.Error(), "registry is required") {
		t.Errorf("missing registry must fail closed, got %v", err)
	}
}
