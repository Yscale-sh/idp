package policy

import (
	"errors"
	"testing"

	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

func baseApp() appconfig.App {
	a := appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080},
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
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/jakenesler/carshowdb-api:latest"})
	if err := vs.AsError(); err == nil {
		t.Fatal("expected mutable-tag violation in prod")
	}
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("expected ErrMutableTag, got %v", vs)
	}
}

func TestCheck_EmptyTagProd(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/jakenesler/carshowdb-api"})
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("empty prod tag should violate, got %v", vs)
	}
}

func TestCheck_MutableTagAllowedInDev(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/jakenesler/carshowdb-api:latest", Cluster: devCluster()})
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
	vs := Check(Input{App: app, Env: "prod", Image: "ghcr.io/jakenesler/carshowdb-api:prod-abc", Cluster: c})
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
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/jakenesler/carshowdb-api:latest", Cluster: strictDevCluster()})
	if !errors.Is(vs, ErrMutableTag) {
		t.Errorf("strict dev (allowMutableTags=false) must reject :latest, got %v", vs)
	}
}

// allowMutableTags=false also rejects an EMPTY tag in a non-prod env.
func TestCheck_EmptyTagRejectedWhenNotAllowed(t *testing.T) {
	app := baseApp()
	vs := Check(Input{App: app, Env: "dev", Image: "ghcr.io/jakenesler/carshowdb-api", Cluster: strictDevCluster()})
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

func TestCheckArgoDestination_Mismatch(t *testing.T) {
	manifest := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: carshowdb
spec:
  destination:
    namespace: othersapp
    server: https://kubernetes.default.svc
`)
	vs := CheckArgoDestination(manifest, "carshowdb")
	if !errors.Is(vs, ErrNamespace) {
		t.Errorf("expected ErrNamespace for cross-namespace destination, got %v", vs)
	}
}

func TestCheckArgoDestination_OwnNamespaceClean(t *testing.T) {
	manifest := []byte(`apiVersion: argoproj.io/v1alpha1
kind: Application
metadata:
  name: carshowdb
spec:
  destination:
    namespace: carshowdb
    server: https://kubernetes.default.svc
`)
	if vs := CheckArgoDestination(manifest, "carshowdb"); len(vs) != 0 {
		t.Errorf("own-namespace destination should be clean, got %v", vs)
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
		"ghcr.io/jakenesler/app:prod-abc": "prod-abc",
		"ghcr.io/jakenesler/app:latest":   "latest",
		"ghcr.io/jakenesler/app":          "",
		"registry:5000/app:v1":            "v1",
		"registry:5000/app":               "",
		"app@sha256:deadbeef":             "sha256:deadbeef",
	}
	for in, want := range cases {
		if got := imageTag(in); got != want {
			t.Errorf("imageTag(%q) = %q, want %q", in, got, want)
		}
	}
}
