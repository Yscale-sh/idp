package render

import "testing"

// TestSingleManifestRouting proves the headline contract: ONE deploy.yaml with a
// single bare-host public route, no env-specific fields, yields a MetalLB LAN
// LoadBalancer (auto IP + hostname) in dev and a Cloudflare Tunnel in prod —
// the exposure is derived entirely from the environment.
func TestSingleManifestRouting(t *testing.T) {
	// DEV: web -> web.local, fulfilled by a LAN LoadBalancer (pool-assigned IP). No tunnel.
	dev := loadTestApp(t, "web.deploy.yaml")
	dev.Routes = selectInZone(dev.Routes, devCluster())
	resDev, err := Render(dev, "dev", devCluster(), "ghcr.io/jakenesler/web:dev-1", "")
	if err != nil {
		t.Fatalf("dev render: %v", err)
	}
	if resDev.Values.Tunnel != nil {
		t.Errorf("dev: expected NO tunnel, got %+v", resDev.Values.Tunnel)
	}
	if resDev.Values.LanExpose == nil {
		t.Fatal("dev: expected a LAN LoadBalancer, got nil")
	}
	if got := resDev.Values.LanExpose.Host; got != "web.local" {
		t.Errorf("dev LAN host = %q, want web.local", got)
	}
	if got := resDev.Values.LanExpose.Pool; got != "lan-pool" {
		t.Errorf("dev LAN pool = %q, want lan-pool (auto-assigned from the env pool)", got)
	}
	if got := resDev.Values.LanExpose.Port; got != 80 {
		t.Errorf("dev LAN port = %d, want 80", got)
	}

	// PROD: the SAME route -> web.example.com, fulfilled by a Cloudflare Tunnel. No LAN LB.
	prod := loadTestApp(t, "web.deploy.yaml")
	prod.Routes = selectInZone(prod.Routes, prodCluster())
	resProd, err := Render(prod, "prod", prodCluster(), "ghcr.io/jakenesler/web:prod-1", "")
	if err != nil {
		t.Fatalf("prod render: %v", err)
	}
	if resProd.Values.LanExpose != nil {
		t.Errorf("prod: expected NO LAN LoadBalancer, got %+v", resProd.Values.LanExpose)
	}
	if resProd.Values.Tunnel == nil {
		t.Fatal("prod: expected a Cloudflare Tunnel, got nil")
	}
	if len(resProd.Values.Tunnel.Ingress) != 1 || resProd.Values.Tunnel.Ingress[0].Hostname != "web.example.com" {
		t.Errorf("prod tunnel ingress = %+v, want one entry for web.example.com", resProd.Values.Tunnel.Ingress)
	}
}
