package scaffold

import (
	"strings"
	"testing"

	"github.com/jakenesler/platformctl/internal/appconfig"
	"sigs.k8s.io/yaml"
)

func TestGenerate_DeployValidatesRoundTrip(t *testing.T) {
	files, err := Generate(Options{Name: "dummy-api", Host: "api.example.com", Port: 8080, WithDB: true})
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
	if len(app.DB) != 1 {
		t.Errorf("with-db should add one db, got %+v", app.DB)
	}
}

func TestGenerate_ShipWorkflow(t *testing.T) {
	files, err := Generate(Options{Name: "dummy-api"})
	if err != nil {
		t.Fatal(err)
	}
	ship := string(files[".github/workflows/ship.yml"])
	if !strings.Contains(ship, "ghcr.io/jakenesler/dummy-api") {
		t.Error("ship.yml should reference the ghcr image repo")
	}
	if !strings.Contains(ship, "platformctl render") {
		t.Error("ship.yml should call platformctl render")
	}
	// The ${{ secrets.* }} GitHub expression must survive templating intact.
	if !strings.Contains(ship, "${{ secrets.PLATFORM_REPO_TOKEN }}") {
		t.Errorf("ship.yml lost the GitHub secret expression:\n%s", ship)
	}
}

func TestGenerate_RequiresName(t *testing.T) {
	if _, err := Generate(Options{}); err == nil {
		t.Error("expected error for empty name")
	}
}
