package main

import (
	"reflect"
	"testing"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
)

func boolPtr(b bool) *bool { return &b }

// tunnel env: prod-shaped zones + an explicit seams.tunnel.
func envWithTunnel(on bool) *clusterenv.Config {
	return &clusterenv.Config{
		Zones: []string{"carshowdatabase.com", "*.carshowdatabase.com"},
		Seams: &clusterenv.Seams{Tunnel: boolPtr(on)},
	}
}

func appWithRoutes(routes ...appconfig.Route) appconfig.App {
	return appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080},
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
			want: []string{"carshowdb.carshowdatabase.com"},
		},
		{
			name: "full host passes through unchanged",
			app:  appWithRoutes(appconfig.Route{Host: "carshowdb.carshowdatabase.com", Public: true}),
			cfg:  envWithTunnel(true),
			want: []string{"carshowdb.carshowdatabase.com"},
		},
		{
			name: "private routes are excluded",
			app: appWithRoutes(
				appconfig.Route{Host: "carshowdb", Public: true},
				appconfig.Route{Host: "internal", Public: false},
			),
			cfg:  envWithTunnel(true),
			want: []string{"carshowdb.carshowdatabase.com"},
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
// environment files + the real carshowdb deploy.yaml: promoting to dev must NEVER
// open a tunnel (dev: backend=local, no tunnel seam), while prod composes the POC
// host. Guards against someone flipping dev to a tunnel backend by accident.
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
		t.Fatalf("dev must not tunnel, got %v", got)
	}

	prod, err := loadCluster(root, "prod")
	if err != nil || prod == nil {
		t.Fatalf("load prod cluster.yaml: %v", err)
	}
	// carshowdb's route is the bare label "api" -> api.carshowdatabase.com (matches
	// the live API host); composed under the prod wildcard zone.
	want := []string{"api.carshowdatabase.com"}
	if got := publicTunnelHosts(app, prod); !reflect.DeepEqual(got, want) {
		t.Fatalf("prod publicTunnelHosts() = %v, want %v", got, want)
	}
}
