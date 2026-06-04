package scaffold

import (
	"strings"
	"testing"

	"github.com/jakenesler/jdp/internal/appconfig"
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
	// Thin caller stub: it delegates build -> render -> commit to the reusable JDP
	// workflow rather than doing it inline.
	if !strings.Contains(ship, "uses: jakenesler/jdp/.github/workflows/ship.yml@") {
		t.Errorf("ship.yml should call the reusable JDP workflow:\n%s", ship)
	}
	// Teardown path is exposed (we must be able to tear it down too).
	if !strings.Contains(ship, "remove:") {
		t.Error("ship.yml should expose the teardown 'remove' input")
	}
	// GitHub ${{ }} expressions must survive verbatim (static emit, no templating).
	if !strings.Contains(ship, "${{ secrets.JDP_APP_ID }}") {
		t.Errorf("ship.yml lost the GitHub secret expression:\n%s", ship)
	}
	// The old never-built binary path must be gone (confirmed bug fix).
	if strings.Contains(ship, ".platform/bin/") {
		t.Error("ship.yml still references the never-built .platform/bin path")
	}
}

func TestGenerate_RequiresName(t *testing.T) {
	if _, err := Generate(Options{}); err == nil {
		t.Error("expected error for empty name")
	}
}
