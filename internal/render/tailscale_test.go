package render

import (
	"testing"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
)

func tsProdCluster() *clusterenv.Config {
	c := &clusterenv.Config{
		Env:   "prod",
		Zones: []string{"*.example.com"},
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	c.ApplyDefaults()
	return c
}

func tsDevCluster() *clusterenv.Config {
	c := &clusterenv.Config{
		Env:   "dev",
		Zones: []string{"*.local"},
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendLocal,
			StoreRef: clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	c.ApplyDefaults()
	return c
}

func tsTestApp() appconfig.App {
	return appconfig.App{
		App:             "carshowdb",
		Runtime:         appconfig.Runtime{Image: "ghcr.io/x/carshowdb", Port: 8080},
		TailscaleEgress: true,
	}
}

func TestBuildTailscale_ProdOptIn(t *testing.T) {
	app := tsTestApp()
	ts := buildTailscale(app, tsProdCluster())
	if ts == nil || !ts.Enabled {
		t.Fatalf("prod + tailscaleEgress should render the egress sidecar, got %+v", ts)
	}
	if want := app.Workload() + "-prod"; ts.Hostname != want {
		t.Errorf("hostname = %q, want %q", ts.Hostname, want)
	}
}

func TestBuildTailscale_DevSkips(t *testing.T) {
	// dev (local backend) reaches LAN services directly — no egress sidecar.
	if ts := buildTailscale(tsTestApp(), tsDevCluster()); ts != nil {
		t.Errorf("dev must not render the egress sidecar, got %+v", ts)
	}
}

func TestBuildTailscale_NotOptedIn(t *testing.T) {
	app := tsTestApp()
	app.TailscaleEgress = false
	if ts := buildTailscale(app, tsProdCluster()); ts != nil {
		t.Errorf("no tailscaleEgress must not render the sidecar, got %+v", ts)
	}
}

func TestExternalSecret_TailscaleAuthKeyRef(t *testing.T) {
	ev := BuildExternalSecret(tsTestApp(), "prod", tsProdCluster())
	if !ev.Enabled {
		t.Fatal("a tailscaleEgress app must need a runtime Secret")
	}
	var got string
	for _, r := range ev.RemoteRefs {
		if r.SecretKey == "TS_AUTHKEY" {
			got = r.RemoteRef["key"]
		}
	}
	if got != "/shared/tailscale/auth-key" {
		t.Errorf("TS_AUTHKEY remoteRef key = %q, want /shared/tailscale/auth-key (refs: %+v)", got, ev.RemoteRefs)
	}
}
