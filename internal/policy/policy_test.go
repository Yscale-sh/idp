package policy

import (
	"errors"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

func baseApp() appconfig.App {
	a := appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/yscale-sh/carshowdb-api", Port: 8080},
	}
	a.ApplyDefaults()
	return a
}

// devCluster mirrors the real dev env: allowMutableTags=true (fast iteration).
func devCluster() *clusterenv.Config {
	c := &clusterenv.Config{
		Env:              "dev",
		Zones:            []string{"carshowdb.local", "*.local"},
		AllowMutableTags: true,
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendLocal,
			StoreRef: clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
		ResourceBounds: &clusterenv.ResourceBounds{MaxCPU: "2", MaxMemory: "2Gi"},
	}
	c.ApplyDefaults()
	return c
}

// strictDevCluster is a non-prod env that FORBIDS mutable tags
// (allowMutableTags=false). Prod always forbids; this proves an opted-in
// non-prod env honors the same rejection.
func strictDevCluster() *clusterenv.Config {
	c := devCluster()
	c.AllowMutableTags = false
	return c
}

func TestCheck_MutableTagProd(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/yscale-sh/carshowdb-api:latest"})
	if err := vs.AsError(); err == nil {
		t.Fatal("expected mutable-tag violation in prod")
	}
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("expected ErrMutableTag, got %v", vs)
	}
}

func TestCheck_EmptyTagProd(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/yscale-sh/carshowdb-api"})
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("empty prod tag should violate, got %v", vs)
	}
}

func TestCheck_MutableTagAllowedInDev(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/yscale-sh/carshowdb-api:latest", Cluster: devCluster()})
	if err := vs.AsError(); err != nil {
		t.Errorf("dev should tolerate :latest, got %v", err)
	}
}

func TestCheck_ImmutableTagProdOK(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "carshowdb.example.com"}}
	c := &clusterenv.Config{
		Env:   "prod",
		Zones: []string{"carshowdb.example.com", "*.example.com"},
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	c.ApplyDefaults()
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/yscale-sh/carshowdb-api:prod-abc", Cluster: c})
	if err := vs.AsError(); err != nil {
		t.Errorf("immutable prod tag in zone should pass, got %v", err)
	}
}

// A PUBLIC out-of-zone host must be a hard policy error — never silently
// dropped (finding #1).
func TestCheck_PublicOutOfZoneRejected(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "evil.attacker.com", Public: true}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: devCluster()})
	if !errors.Is(vs, ErrRouteZone) {
		t.Errorf("expected ErrRouteZone for public out-of-zone host, got %v", vs)
	}
}

// A NON-public (internal) out-of-zone host is allowed by design: a single
// deploy.yaml may carry hosts for several envs, and the caller narrows by zone.
func TestCheck_InternalOutOfZoneAllowed(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "carshowdb.example.com", Public: false}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: devCluster()})
	if errors.Is(vs, ErrRouteZone) {
		t.Errorf("internal out-of-zone host should NOT violate, got %v", vs)
	}
}

func TestCheck_ZoneOKInZone(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "carshowdb.local", Public: true}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: devCluster()})
	if errors.Is(vs, ErrRouteZone) {
		t.Errorf("in-zone public host should not violate, got %v", vs)
	}
}

// allowMutableTags=false in a NON-prod env must reject :latest (finding #5).
func TestCheck_MutableTagRejectedWhenNotAllowed(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/yscale-sh/carshowdb-api:latest", Cluster: strictDevCluster()})
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("strict dev (allowMutableTags=false) must reject :latest, got %v", vs)
	}
}

// allowMutableTags=false also rejects an EMPTY tag in a non-prod env.
func TestCheck_EmptyTagRejectedWhenNotAllowed(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/yscale-sh/carshowdb-api", Cluster: strictDevCluster()})
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("strict dev (allowMutableTags=false) must reject empty tag, got %v", vs)
	}
}

func TestCheck_InvalidProfile(t *testing.T) {
	app := baseApp()
	app.Sizing.Profile = "ginormous"
	vs := Check(Input{App: app, Env: "dev", Image: "x:y"})
	if !errors.Is(vs, ErrInvalidProfile) {
		t.Errorf("expected ErrInvalidProfile, got %v", vs)
	}
}

func TestCheck_ResourceBoundsExceeded(t *testing.T) {
	app := baseApp()
	app.Sizing.Profile = "large" // limits cpu=2,mem=2Gi
	tight := devCluster()
	tight.ResourceBounds = &clusterenv.ResourceBounds{MaxCPU: "1", MaxMemory: "1Gi"}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: tight})
	if !errors.Is(vs, ErrResourceBounds) {
		t.Errorf("expected ErrResourceBounds, got %v", vs)
	}
}

func TestCheckRenderedValues_LoadBalancer(t *testing.T) {
	vs := CheckRenderedValues(map[string]any{
		"service": map[string]any{"type": "LoadBalancer", "port": 8080},
	})
	if !errors.Is(vs, ErrLoadBalancer) {
		t.Errorf("expected ErrLoadBalancer, got %v", vs)
	}
}

func TestCheckRenderedManifest_LoadBalancer(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Service
metadata:
  name: carshowdb
spec:
  type: LoadBalancer
  ports:
    - port: 8080
`)
	vs := CheckRenderedManifest(manifest)
	if !errors.Is(vs, ErrLoadBalancer) {
		t.Errorf("expected ErrLoadBalancer from manifest scan, got %v", vs)
	}
}

func TestCheckRenderedManifest_ClusterIPClean(t *testing.T) {
	manifest := []byte(`apiVersion: v1
kind: Service
metadata:
  name: carshowdb
spec:
  type: ClusterIP
  ports:
    - port: 8080
`)
	if vs := CheckRenderedManifest(manifest); len(vs) != 0 {
		t.Errorf("ClusterIP service should be clean, got %v", vs)
	}
}

func TestCheckHelmReleaseTarget_Mismatch(t *testing.T) {
	manifest := []byte(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: carshowdb
  namespace: flux-system
spec:
  targetNamespace: othersapp
  chart:
    spec:
      chart: ./charts/app
`)
	vs := CheckHelmReleaseTarget(manifest, "carshowdb-dev-api")
	if !errors.Is(vs, ErrNamespace) {
		t.Errorf("expected ErrNamespace for cross-namespace targetNamespace, got %v", vs)
	}
}

func TestCheckHelmReleaseTarget_OwnNamespaceClean(t *testing.T) {
	manifest := []byte(`apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata:
  name: carshowdb
  namespace: flux-system
spec:
  targetNamespace: carshowdb-dev-api
  chart:
    spec:
      chart: ./charts/app
`)
	if vs := CheckHelmReleaseTarget(manifest, "carshowdb-dev-api"); len(vs) != 0 {
		t.Errorf("own-namespace targetNamespace should be clean, got %v", vs)
	}
}

func TestCheckModuleValues_LoadBalancer(t *testing.T) {
	values := map[string]any{
		"service": map[string]any{"type": "LoadBalancer", "port": 80},
	}
	vs := CheckModuleValues("bad-module", values)
	if !errors.Is(vs, ErrLoadBalancer) {
		t.Errorf("expected ErrLoadBalancer for module LoadBalancer values, got %v", vs)
	}
}

func TestCheckModuleValues_NestedLoadBalancer(t *testing.T) {
	values := map[string]any{
		"controller": map[string]any{
			"service": map[string]any{"type": "LoadBalancer"},
		},
	}
	vs := CheckModuleValues("bad-module", values)
	if !errors.Is(vs, ErrLoadBalancer) {
		t.Errorf("expected ErrLoadBalancer for nested LoadBalancer, got %v", vs)
	}
}

func TestCheckModuleValues_ClusterIPClean(t *testing.T) {
	values := map[string]any{
		"service": map[string]any{"type": "ClusterIP", "port": 80},
	}
	if vs := CheckModuleValues("good-module", values); len(vs) != 0 {
		t.Errorf("ClusterIP module values should be clean, got %v", vs)
	}
}

func TestImageTag(t *testing.T) {
	cases := map[string]string{
		"ghcr.io/yscale-sh/app:prod-abc": "prod-abc",
		"ghcr.io/yscale-sh/app:latest":   "latest",
		"ghcr.io/yscale-sh/app":          "",
		"registry:5000/app:v1":           "v1",
		"registry:5000/app":              "",
		"app@sha256:deadbeef":            "sha256:deadbeef",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestCheckSeams_StatefulStoreGate(t *testing.T) {
	dbApp := appconfig.App{
		App:     "needsdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/x/needsdb", Port: 8080},
		DB:      []appconfig.DataStore{{Name: "primary", Type: "postgres"}},
	}
	// default env (no seams declared) -> stores derive to allowed.
	if vs := checkSeams(dbApp, devCluster()); len(vs) != 0 {
		t.Errorf("dev (default seams) should allow db, got %v", vs)
	}
	// env with NO in-cluster stores AND a local backend (no managed store) -> db
	// is rejected: the store can't be provided either way.
	no := false
	prod := devCluster() // local backend
	prod.Env = "prod"
	prod.Seams = &clusterenv.Seams{StatefulStores: &no}
	vs := checkSeams(dbApp, prod)
	if len(vs) != 1 || vs[0].Kind != KindUnprovidedSeam {
		t.Fatalf("statefulStores:false + local backend should reject db with UnprovidedSeam, got %v", vs)
	}
	// real prod: no in-cluster stores BUT a managed (ssm) backend supplies
	// DATABASE_URL/REDIS_URL from SSM -> db is ALLOWED (managed/external).
	ssmProd := devCluster()
	ssmProd.Env = "prod"
	ssmProd.Secrets.Backend = clusterenv.BackendSSM
	ssmProd.Seams = &clusterenv.Seams{StatefulStores: &no}
	if vs := checkSeams(dbApp, ssmProd); len(vs) != 0 {
		t.Errorf("statefulStores:false + ssm backend should allow managed db, got %v", vs)
	}
	// nil cluster degrades to no-op.
	if vs := checkSeams(dbApp, nil); vs != nil {
		t.Errorf("nil cluster should be a no-op, got %v", vs)
	}
}

// cfDevCluster mirrors environments/dev/cluster.yaml after the Cloudflare
// zone-split: yscale.sh routes tunnel (Access-gated by contract), .local
// routes stay on the LAN LoadBalancer.
func cfDevCluster() *clusterenv.Config {
	c := devCluster()
	c.Zones = append(c.Zones, "yscale.sh", "*.yscale.sh")
	c.CloudflareZone = "yscale.sh"
	return c
}

// A dev public route under the Cloudflare zone WITHOUT an access declaration
// must be denied: dev CF exposure is Access-gated internal exposure by
// contract, never open (finding: the whole point of the dev tunnel feature).
func TestCheck_CFRouteWithoutAccessDenied(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "myapp-dev.yscale.sh", Public: true}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: cfDevCluster()})
	if !errors.Is(vs, ErrCFAccess) {
		t.Errorf("expected ErrCFAccess for CF-zone route without access, got %v", vs)
	}
}

// Declaring access.humans satisfies the rail (access.serviceToken would too).
func TestCheck_CFRouteWithAccessAllowed(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{
		Host: "myapp-dev.yscale.sh", Public: true,
		Access: appconfig.Access{Humans: true},
	}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: cfDevCluster()})
	if errors.Is(vs, ErrCFAccess) {
		t.Errorf("access.humans should satisfy the CF Access rail, got %v", vs)
	}
}

// A .local LAN route needs no access declaration — the rail is scoped to the
// Cloudflare zone only.
func TestCheck_LANRouteWithoutAccessAllowed(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "myapp.local", Public: true}}
	vs := Check(Input{App: app, Env: "dev", Image: "x:y", Cluster: cfDevCluster()})
	if errors.Is(vs, ErrCFAccess) {
		t.Errorf(".local route should not trip the CF Access rail, got %v", vs)
	}
}

// Prod (no cloudflareZone) is untouched by the rail: public routes without
// access declarations remain valid exactly as before this feature.
func TestCheck_ProdWithoutCloudflareZoneUnchanged(t *testing.T) {
	app := baseApp()
	app.Routes = []appconfig.Route{{Host: "carshowdb.example.com", Public: true}}
	c := &clusterenv.Config{
		Env:   "prod",
		Zones: []string{"carshowdb.example.com", "*.example.com"},
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	c.ApplyDefaults()
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/yscale-sh/x:prod-abc", Cluster: c})
	if errors.Is(vs, ErrCFAccess) {
		t.Errorf("prod without cloudflareZone must not trip the CF Access rail, got %v", vs)
	}
}
