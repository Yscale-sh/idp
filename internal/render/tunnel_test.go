package render

import (
	"fmt"
	"testing"
)

func TestHasPublicRoute(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	if !hasPublicRoute(app) {
		t.Fatal("carshowdb fixture declares a public route; hasPublicRoute should be true")
	}
	for i := range app.Routes {
		app.Routes[i].Public = false
	}
	if hasPublicRoute(app) {
		t.Fatal("with no public routes, hasPublicRoute should be false")
	}
}

// buildTunnel only applies to a non-local (prod) app with a public route — dev
// keeps its in-cluster routes and never tunnels, so the values field is omitted
// and the render stays byte-identical.
func TestBuildTunnel_DevIsNil(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	if tn := buildTunnel(app, devCluster()); tn != nil {
		t.Fatalf("dev (local backend) must not tunnel; got %+v", tn)
	}
}

func TestBuildTunnel_ProdNoPublicIsNil(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	for i := range app.Routes {
		app.Routes[i].Public = false
	}
	if tn := buildTunnel(app, prodCluster()); tn != nil {
		t.Fatalf("prod app with no public route must not tunnel; got %+v", tn)
	}
}

func TestBuildTunnel_ProdPublicRoute(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	tn := buildTunnel(app, prodCluster())
	if tn == nil {
		t.Fatal("prod app with a public route must tunnel; got nil")
	}
	if !tn.Enabled {
		t.Error("tunnel.Enabled should be true")
	}
	wantSvc := fmt.Sprintf("http://localhost:%d", app.Runtime.Port)
	var publicHosts int
	for _, r := range app.Routes {
		if r.Public && r.Host != "" {
			publicHosts++
		}
	}
	if len(tn.Ingress) != publicHosts {
		t.Fatalf("ingress rules = %d, want one per public host (%d)", len(tn.Ingress), publicHosts)
	}
	for _, ing := range tn.Ingress {
		if ing.Service != wantSvc {
			t.Errorf("ingress service = %q, want %q (the app's own Service on localhost)", ing.Service, wantSvc)
		}
		if ing.Hostname == "" {
			t.Error("ingress hostname must not be empty")
		}
	}
}

// RemoveApp is the teardown inverse of UpsertApp: it deletes the app from the
// env umbrella so Flux prunes it. It must round-trip and be idempotent.
func TestRemoveApp_RoundTrip(t *testing.T) {
	root := t.TempDir()
	c := devCluster()
	app := loadTestApp(t, "carshowdb.deploy.yaml")

	res, err := Render(app, "dev", c, "ghcr.io/jakenesler/carshowdb:abc123", "")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if _, err := res.UpsertApp(root, c); err != nil {
		t.Fatalf("upsert: %v", err)
	}
	pr, err := ReadPlatform(root, "dev", c)
	if err != nil {
		t.Fatalf("read after upsert: %v", err)
	}
	if len(pr.Spec.Values.Apps) != 1 {
		t.Fatalf("after upsert want 1 app, got %d", len(pr.Spec.Values.Apps))
	}

	_, removed, err := RemoveApp(root, "dev", "carshowdb", c)
	if err != nil {
		t.Fatalf("remove: %v", err)
	}
	if !removed {
		t.Fatal("remove should report removed=true for a present app")
	}
	pr2, err := ReadPlatform(root, "dev", c)
	if err != nil {
		t.Fatalf("read after remove: %v", err)
	}
	if len(pr2.Spec.Values.Apps) != 0 {
		t.Fatalf("after remove want 0 apps, got %d", len(pr2.Spec.Values.Apps))
	}

	// Idempotent: removing an absent app is a no-op.
	if _, removed2, err := RemoveApp(root, "dev", "carshowdb", c); err != nil {
		t.Fatalf("remove (idempotent): %v", err)
	} else if removed2 {
		t.Fatal("removing an absent app should report removed=false")
	}
}
