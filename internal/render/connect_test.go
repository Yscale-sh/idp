package render

import (
	"testing"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/clusterenv"
)

func TestResolveConnections_Modes(t *testing.T) {
	app := appconfig.App{
		App:     "dummy-ui",
		Product: "dummy",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/dummy-ui", Port: 3000},
		ConnectsTo: []appconfig.Connection{
			{App: "dummy-api", Env: "API_BASE_URL", Mode: "clusterService"},
			{Component: "api", Env: "PUBLIC_API_URL", Mode: "publicRoute"},
			{App: "dummy-api", Env: "SECURE_API", Mode: "serviceToken"},
		},
	}
	app.ApplyDefaults()
	c := &clusterenv.Config{Env: "dev", Domain: "svc.cluster.local"}

	conns, err := ResolveConnections(app, "dev", c)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(conns) != 3 {
		t.Fatalf("expected 3 connections, got %d", len(conns))
	}

	// clusterService -> in-cluster DNS using the source port.
	if got, want := conns[0].Value, "http://dummy-api.dummy-api.svc.cluster.local:3000"; got != want {
		t.Errorf("clusterService value = %q, want %q", got, want)
	}

	// component publicRoute resolves target as <product>-<component>.
	if conns[1].Target != "dummy-api" {
		t.Errorf("component target = %q, want dummy-api", conns[1].Target)
	}
	if conns[1].Value == "" {
		t.Error("publicRoute value should be set")
	}

	// serviceToken -> publicRoute value + CF Access service-token keys.
	if len(conns[2].ServiceTokenKeys) != 2 {
		t.Errorf("serviceToken keys = %v, want 2", conns[2].ServiceTokenKeys)
	}
	if conns[2].ServiceTokenKeys[0] != "SECURE_API_CF_ACCESS_CLIENT_ID" {
		t.Errorf("serviceToken key[0] = %q", conns[2].ServiceTokenKeys[0])
	}
}

func TestResolveConnections_DefaultMode(t *testing.T) {
	app := appconfig.App{
		App:        "ui",
		Runtime:    appconfig.Runtime{Port: 8080},
		ConnectsTo: []appconfig.Connection{{App: "api", Env: "API_URL"}},
	}
	app.ApplyDefaults() // mode defaults to clusterService
	conns, err := ResolveConnections(app, "dev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if conns[0].Mode != appconfig.DefaultConnectsMode {
		t.Errorf("mode = %q, want %q", conns[0].Mode, appconfig.DefaultConnectsMode)
	}
}
