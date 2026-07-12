package main

import (
	"reflect"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

func boolPtr(b bool) *bool { return &b }

// tunnel env: prod-shaped zones + an explicit seams.tunnel.
func envWithTunnel(on bool) *clusterenv.Config {
	return &clusterenv.Config{
		Zones: []string{"example.com", "*.example.com"},
		Seams: &clusterenv.Seams{Tunnel: boolPtr(on)},
	}
}

func appWithRoutes(routes ...appconfig.Route) appconfig.App {
	return appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/yscale-sh/carshowdb-api", Port: 8080},
		Routes:  routes,
	}
}

func TestPublicTunnelHosts(t *testing.T) {
	cases := []struct {
		name string
		app  appconfig.App
		cfg  *clusterenv.Config
		want []string
	}{
		{
			name: "bare label composes under the env zone",
			app:  appWithRoutes(appconfig.Route{Host: "carshowdb", Public: true}),
			cfg:  envWithTunnel(true),
			want: []string{"carshowdb.example.com"},
		},
		{
			name: "full host passes through unchanged",
			app:  appWithRoutes(appconfig.Route{Host: "carshowdb.example.com", Public: true}),
			cfg:  envWithTunnel(true),
			want: []string{"carshowdb.example.com"},
		},
		{
			name: "private routes are excluded",
			app: appWithRoutes(
				appconfig.Route{Host: "carshowdb", Public: true},
				appconfig.Route{Host: "internal", Public: false},
			),
			cfg:  envWithTunnel(true),
			want: []string{"carshowdb.example.com"},
		},
		{
			name: "cloudflareZone filters tunnel DNS hosts",
			app: appWithRoutes(
				appconfig.Route{Host: "api.local", Public: true},
				appconfig.Route{Host: "myapp-dev.yscale.sh", Public: true},
				appconfig.Route{Host: "notyscale.sh", Public: true},
			),
			cfg: &clusterenv.Config{
				Zones:          []string{"*.local", "yscale.sh", "*.yscale.sh"},
				CloudflareZone: "yscale.sh",
				Seams:          &clusterenv.Seams{Tunnel: boolPtr(true)},
			},
			want: []string{"myapp-dev.yscale.sh"},
		},
		{
			name: "no DNS step when the env provides no tunnel",
			app:  appWithRoutes(appconfig.Route{Host: "carshowdb", Public: true}),
			cfg:  envWithTunnel(false),
			want: nil,
		},
		{
			name: "no DNS step for a route-less app",
			app:  appWithRoutes(),
			cfg:  envWithTunnel(true),
			want: nil,
		},
		{
			name: "nil cluster config is a safe no-op",
			app:  appWithRoutes(appconfig.Route{Host: "carshowdb", Public: true}),
			cfg:  nil,
			want: nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := publicTunnelHosts(tc.app, tc.cfg)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("publicTunnelHosts() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPublicTunnelHosts_RealEnvs pins the behavior against the repo's ACTUAL
// environment files + the real carshowdb deploy.yaml: dev only tunnels hosts under
// cloudflareZone, so carshowdb's .local host stays off the Cloudflare DNS path,
// while prod composes the POC host.
func TestPublicTunnelHosts_RealEnvs(t *testing.T) {
	root := "../.." // cmd/idpctl -> repo root
	app, err := loadApp(root + "/examples/carshowdb/deploy.yaml")
	if err != nil {
		t.Fatalf("load carshowdb deploy.yaml: %v", err)
	}

	dev, err := loadCluster(root, "dev")
	if err != nil || dev == nil {
		t.Fatalf("load dev cluster.yaml: %v", err)
	}
	if got := publicTunnelHosts(app, dev); got != nil {
		t.Fatalf("dev carshowdb route must stay on the LAN path, got tunnel hosts %v", got)
	}

	prod, err := loadCluster(root, "prod")
	if err != nil || prod == nil {
		t.Fatalf("load prod cluster.yaml: %v", err)
	}
	// carshowdb's route is the bare label "api" -> api.example.com (matches
	// the live API host); composed under the prod wildcard zone.
	want := []string{"api.example.com"}
	if got := publicTunnelHosts(app, prod); !reflect.DeepEqual(got, want) {
		t.Fatalf("prod publicTunnelHosts() = %v, want %v", got, want)
	}
}
