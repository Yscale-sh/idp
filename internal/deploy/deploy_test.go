package deploy

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/clusterenv"
)

func devCluster() *clusterenv.Config {
	c := &clusterenv.Config{
		Env:              "dev",
		Zones:            []string{"carshowdb.local", "*.local"},
		AllowMutableTags: true, // dev tolerates :latest (mirrors environments/dev/cluster.yaml)
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendLocal,
			StoreRef: clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	c.ApplyDefaults()
	return c
}

func prodCluster() *clusterenv.Config {
	c := devCluster()
	c.Env = "prod"
	c.Zones = []string{"carshowdb.example.com", "*.example.com"}
	c.AllowMutableTags = false
	c.Secrets.Backend = clusterenv.BackendSSM
	c.Secrets.StoreRef = clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore}
	return c
}

// multiEnvApp carries hosts for several environments in one deploy.yaml. The
// dev-facing host (carshowdb.local) is PUBLIC and in the dev zone; the prod-only
// host (carshowdb.example.com) is INTERNAL (public:false) so it is allowed
// everywhere and simply narrowed out of envs whose zone it does not match. A
// public host must always be in-zone for the env it is built into (see
// strictDevCluster-style rejection in the policy package and the dedicated
// out-of-zone rejection test below).
func multiEnvApp() appconfig.App {
	return appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080},
		Routes: []appconfig.Route{
			{Host: "carshowdb.local", Public: true},
			{Host: "carshowdb.example.com", Public: false},
		},
		DB: []appconfig.DataStore{{Name: "primary", Type: "postgres"}},
	}
}

// prodPublicApp is built for prod: its public host is in the prod zone.
func prodPublicApp() appconfig.App {
	return appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080},
		Routes: []appconfig.Route{
			{Host: "carshowdb.local", Public: false},
			{Host: "carshowdb.example.com", Public: true},
		},
		DB: []appconfig.DataStore{{Name: "primary", Type: "postgres"}},
	}
}

func TestBuild_DevSelectsLocalRoute(t *testing.T) {
	plan, err := Build(Request{
		App: multiEnvApp(), Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:dev-abc", Cluster: devCluster(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	routes := plan.Result.App.Routes
	if len(routes) != 1 || routes[0].Host != "carshowdb.local" {
		t.Errorf("dev should keep only the .local route, got %+v", routes)
	}
	if plan.Result.Values.ExternalSecret.Backend != clusterenv.BackendLocal {
		t.Errorf("dev secret backend = %q", plan.Result.Values.ExternalSecret.Backend)
	}
}

func TestBuild_ProdSelectsPublicRoute(t *testing.T) {
	plan, err := Build(Request{
		App: prodPublicApp(), Env: "prod",
		Image: "ghcr.io/jakenesler/carshowdb-api:prod-abc", Cluster: prodCluster(),
	})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	routes := plan.Result.App.Routes
	if len(routes) != 1 || routes[0].Host != "carshowdb.example.com" {
		t.Errorf("prod should keep only the example.com route, got %+v", routes)
	}
	if !routes[0].Public {
		t.Errorf("prod route should be public, got %+v", routes[0])
	}
	if plan.Result.Values.ExternalSecret.Backend != clusterenv.BackendSSM {
		t.Errorf("prod secret backend = %q", plan.Result.Values.ExternalSecret.Backend)
	}
}

// Finding #1: a PUBLIC route whose host is outside the env's approved zones must
// be REJECTED with a clear policy error BEFORE any rendering/narrowing — never
// silently dropped.
func TestBuild_RejectsPublicOutOfZoneHost(t *testing.T) {
	app := appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/jakenesler/carshowdb-api", Port: 8080},
		Routes: []appconfig.Route{
			{Host: "carshowdb.local", Public: true},
			{Host: "evil.attacker.com", Public: true}, // public + in no approved zone
		},
	}
	_, err := Build(Request{
		App: app, Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:dev-abc", Cluster: devCluster(),
	})
	if err == nil || !strings.Contains(err.Error(), "RouteZone") {
		t.Fatalf("expected public out-of-zone host to be rejected, got %v", err)
	}
	if !strings.Contains(err.Error(), "evil.attacker.com") {
		t.Errorf("rejection should name the offending host, got %v", err)
	}
}

// An INTERNAL (non-public) out-of-zone host belongs to another env and is
// narrowed out silently — that is by design and must NOT error.
func TestBuild_AllowsInternalOutOfZoneHost(t *testing.T) {
	if _, err := Build(Request{
		App: multiEnvApp(), Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:dev-abc", Cluster: devCluster(),
	}); err != nil {
		t.Fatalf("internal out-of-zone host should be allowed, got %v", err)
	}
}

func TestBuild_RejectsMutableTagInProd(t *testing.T) {
	_, err := Build(Request{
		App: prodPublicApp(), Env: "prod",
		Image: "ghcr.io/jakenesler/carshowdb-api:latest", Cluster: prodCluster(),
	})
	if err == nil || !strings.Contains(err.Error(), "MutableTag") {
		t.Errorf("expected mutable-tag rejection, got %v", err)
	}
}

// Finding #5: a non-prod env with allowMutableTags=false rejects :latest just
// like prod does.
func TestBuild_RejectsMutableTagWhenEnvForbids(t *testing.T) {
	strict := devCluster()
	strict.AllowMutableTags = false
	_, err := Build(Request{
		App: multiEnvApp(), Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:latest", Cluster: strict,
	})
	if err == nil || !strings.Contains(err.Error(), "MutableTag") {
		t.Errorf("dev with allowMutableTags=false should reject :latest, got %v", err)
	}
}

func TestBuild_RejectsInvalidConfig(t *testing.T) {
	bad := appconfig.App{App: "", Runtime: appconfig.Runtime{Port: 8080}}
	_, err := Build(Request{App: bad, Env: "dev", Image: "x:y"})
	if err == nil || !strings.Contains(err.Error(), "validation failed") {
		t.Errorf("expected validation failure, got %v", err)
	}
}

// Finding #3: when --root + helm are available, Build runs a real `helm
// template charts/app` scan and feeds it to policy.CheckRenderedManifest. The
// shipped chart is ClusterIP-only, so the scan must PASS (proving the guardrail
// is wired and live, not dead code). Skipped when helm is absent.
func TestBuild_HelmTemplateScanRunsAndPasses(t *testing.T) {
	if _, err := exec.LookPath("helm"); err != nil {
		t.Skip("helm not on PATH")
	}
	root := repoRoot(t)
	plan, err := Build(Request{
		App: multiEnvApp(), Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:dev-abc", Cluster: devCluster(), Root: root,
	})
	if err != nil {
		t.Fatalf("build with helm scan should pass for the ClusterIP-only chart, got %v", err)
	}
	if plan == nil || plan.Result == nil {
		t.Fatal("expected a rendered plan")
	}
}

// repoRoot walks up from the test's working dir to the module root (go.mod).
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Skip("could not locate repo root (go.mod)")
		}
		dir = parent
	}
}

func TestBuild_Summary(t *testing.T) {
	plan, err := Build(Request{
		App: multiEnvApp(), Env: "dev",
		Image: "ghcr.io/jakenesler/carshowdb-api:dev-abc", Cluster: devCluster(),
	})
	if err != nil {
		t.Fatal(err)
	}
	s := plan.Summary()
	for _, want := range []string{"carshowdb", "ClusterIP", "/apps/carshowdb/dev", "DATABASE_URL", "backend=local"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary missing %q:\n%s", want, s)
		}
	}
}
