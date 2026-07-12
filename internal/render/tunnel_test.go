package render

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

func TestCFRoutes(t *testing.T) {
	app := appconfig.App{
		Routes: []appconfig.Route{
			{Host: "yscale.sh", Public: true},
			{Host: "myapp-dev.yscale.sh", Public: true},
			{Host: "notyscale.sh", Public: true},
			{Host: "myapp-dev.yscale.sh.evil", Public: true},
			{Host: "private.yscale.sh", Public: false},
			{Host: "", Public: true},
		},
	}

	got := routeHosts(cfRoutes(app, &clusterenv.Config{CloudflareZone: "yscale.sh"}))
	want := []string{"yscale.sh", "myapp-dev.yscale.sh"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cfRoutes zone match = %#v, want %#v", got, want)
	}

	got = routeHosts(cfRoutes(app, &clusterenv.Config{}))
	want = []string{"yscale.sh", "myapp-dev.yscale.sh", "notyscale.sh", "myapp-dev.yscale.sh.evil"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("cfRoutes empty-zone passthrough = %#v, want %#v", got, want)
	}
}

// buildTunnel is omitted when the env does not provide a tunnel seam, so the
// default local dev fixture keeps its current byte-identical render.
func TestBuildTunnel_DevIsNil(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	if tn := buildTunnel(app, devCluster()); tn != nil {
		t.Fatalf("dev without the tunnel seam must not tunnel; got %+v", tn)
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

func TestBuildTunnel_DevCloudflareZoneOnly(t *testing.T) {
	app := appconfig.App{
		App:     "myapp",
		Runtime: appconfig.Runtime{Port: 8080},
		Routes: []appconfig.Route{
			{Host: "app.local", Public: true},
			{Host: "myapp-dev.yscale.sh", Public: true},
		},
	}
	c := devCluster()
	c.CloudflareZone = "yscale.sh"
	c.Seams = &clusterenv.Seams{Tunnel: boolPtr(true)}

	tn := buildTunnel(app, c)
	if tn == nil {
		t.Fatal("dev CF-zone route should render a tunnel")
	}
	if got, want := routeHostsFromIngress(tn.Ingress), []string{"myapp-dev.yscale.sh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("dev tunnel ingress hosts = %#v, want %#v", got, want)
	}
	if lan := buildLanExpose(app, c); lan == nil {
		t.Fatal("non-CF public route should still render LAN exposure")
	} else if lan.Host != "app.local" {
		t.Fatalf("LAN external-dns host = %q, want app.local", lan.Host)
	}
}

func routeHosts(routes []appconfig.Route) []string {
	hosts := make([]string, 0, len(routes))
	for _, r := range routes {
		hosts = append(hosts, r.Host)
	}
	return hosts
}

func routeHostsFromIngress(routes []TunnelIngressValues) []string {
	hosts := make([]string, 0, len(routes))
	for _, r := range routes {
		hosts = append(hosts, r.Hostname)
	}
	return hosts
}

// RemoveApp is the teardown inverse of UpsertApp: it deletes the app from the
// env umbrella so Flux prunes it. It must round-trip and be idempotent.
func TestRemoveApp_RoundTrip(t *testing.T) {
	root := t.TempDir()
	c := devCluster()
	app := loadTestApp(t, "carshowdb.deploy.yaml")

	res, err := Render(app, "dev", c, "ghcr.io/yscale-sh/carshowdb:abc123", "")
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

	_, removed, err := RemoveApp(root, "dev", "carshowdb", "", c)
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
	if _, removed2, err := RemoveApp(root, "dev", "carshowdb", "", c); err != nil {
		t.Fatalf("remove (idempotent): %v", err)
	} else if removed2 {
		t.Fatal("removing an absent app should report removed=false")
	}
}
