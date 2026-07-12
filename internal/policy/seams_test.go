package policy

import (
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// prodNoStores is a prod-shaped env: PVC-free (no in-cluster stores), tunnel-only
// (no LAN), with zones + keda so publicRoutes/autoscale derive on.
func prodNoStores() *clusterenv.Config {
	no := false
	c := &clusterenv.Config{
		Env:   "prod",
		Zones: []string{"*.example.com"},
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
		Modules: map[string]clusterenv.Module{"keda": {Enabled: true}},
		Seams:   &clusterenv.Seams{StatefulStores: &no, LANExpose: &no},
	}
	c.ApplyDefaults()
	return c
}

func hasKind(vs Violations, k Kind) bool {
	for _, v := range vs {
		if v.Kind == k {
			return true
		}
	}
	return false
}

func TestSeams_DBAllowedViaManagedBackend(t *testing.T) {
	// A managed (ssm) prod env hosts no in-cluster stores, but supplies
	// DATABASE_URL/REDIS_URL from the secrets backend — so a declared db is ALLOWED
	// (the renderer wires the URL from SSM and skips the in-cluster chart).
	app := appconfig.App{App: "x", DB: []appconfig.DataStore{{Name: "primary", Type: "postgres"}}}
	vs := Check(Input{App: app, Env: "prod", Image: "x:prod-1", Cluster: prodNoStores()})
	if hasKind(vs, KindUnprovidedSeam) {
		t.Fatalf("db in a managed (ssm) env must be allowed (DATABASE_URL from SSM), got %v", vs)
	}
}

func TestSeams_DBRejectedWhenNoStoreAndNoManagedBackend(t *testing.T) {
	// No in-cluster stores AND a local backend (no managed store to supply the URL)
	// -> the store can't be provided either way, so db is rejected.
	no := false
	c := devCluster() // local backend
	c.Env = "prod"
	c.Seams = &clusterenv.Seams{StatefulStores: &no}
	app := appconfig.App{App: "x", DB: []appconfig.DataStore{{Name: "primary", Type: "postgres"}}}
	vs := Check(Input{App: app, Env: "prod", Image: "x:prod-1", Cluster: c})
	if !hasKind(vs, KindUnprovidedSeam) {
		t.Fatalf("db with no in-cluster store and no managed backend must be rejected, got %v", vs)
	}
	if !strings.Contains(vs.Error(), "statefulStores") {
		t.Errorf("violation should name the statefulStores seam, got %q", vs.Error())
	}
}

func TestSeams_DBAllowedWhenProvided(t *testing.T) {
	// dev derives statefulStores=true (no explicit Seams).
	app := appconfig.App{App: "x", DB: []appconfig.DataStore{{Name: "primary", Type: "postgres"}}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: devCluster()})
	if hasKind(vs, KindUnprovidedSeam) {
		t.Errorf("db in a store-providing env must pass, got %v", vs)
	}
}

func TestSeams_LANRejectedWhenNoLANExpose(t *testing.T) {
	app := appconfig.App{App: "x", Expose: &appconfig.Expose{LAN: true}}
	vs := Check(Input{App: app, Env: "prod", Image: "x:prod-1", Cluster: prodNoStores()})
	if !hasKind(vs, KindUnprovidedSeam) {
		t.Fatalf("expose.lan in a tunnel-only env must be rejected, got %v", vs)
	}
}

func TestSeams_AutoscaleRejectedWithoutKEDA(t *testing.T) {
	noScale := false
	c := devCluster()
	c.Seams = &clusterenv.Seams{Autoscale: &noScale}
	app := appconfig.App{App: "x"}
	app.Sizing.Autoscale.Enabled = true
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: c})
	if !hasKind(vs, KindUnprovidedSeam) {
		t.Fatalf("autoscale without the seam must be rejected, got %v", vs)
	}
}

func TestEffectiveSeams_Derivation(t *testing.T) {
	// no zones, no keda, no explicit Seams -> publicRoutes/autoscale derive false.
	c := &clusterenv.Config{Env: "bare"}
	s := c.EffectiveSeams()
	if s.PublicRoutes {
		t.Error("publicRoutes should derive false with no zones")
	}
	if s.Autoscale {
		t.Error("autoscale should derive false with no keda module")
	}
	if !s.StatefulStores || !s.LANExpose || !s.Volumes {
		t.Error("statefulStores/lanExpose/volumes should default true")
	}
	// zones + keda flip the derived ones on.
	c2 := &clusterenv.Config{Env: "full", Zones: []string{"*.x"}, Modules: map[string]clusterenv.Module{"keda": {Enabled: true}}}
	s2 := c2.EffectiveSeams()
	if !s2.PublicRoutes || !s2.Autoscale {
		t.Error("publicRoutes+autoscale should derive true from zones+keda")
	}
}
