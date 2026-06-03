package render

import (
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite golden testdata files")

// loadTestApp reads a deploy.yaml from testdata, defaults it, and returns it.
func loadTestApp(t *testing.T, name string) appconfig.App {
	t.Helper()
	path := filepath.Join("testdata", name)
	app, err := appconfig.LoadDefaulted(path)
	if err != nil {
		t.Fatalf("load %s: %v", name, err)
	}
	return app
}

// devCluster / prodCluster are fixed env configs so golden output is stable and
// independent of the on-disk environments/ files.
func devCluster() *clusterenv.Config {
	c := &clusterenv.Config{
		Env:   "dev",
		Zones: []string{"carshowdb.local", "*.local"},
		Secrets: clusterenv.SecretsConfig{
			Backend:         clusterenv.BackendLocal,
			RefreshInterval: "1h",
			StoreRef:        clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
		Observability: clusterenv.Observability{
			LokiURL:      "http://loki.monitoring.svc.cluster.local:3100",
			OTLPEndpoint: "http://otel-collector.monitoring.svc.cluster.local:4317",
		},
		Domain: "svc.cluster.local",
		Argo: clusterenv.ArgoConfig{
			Namespace:      "argocd",
			RepoURL:        "https://github.com/jakenesler/platformctl.git",
			TargetRevision: "HEAD",
		},
		// keda is the only SHARED infra module in dev now; the per-app dev Postgres
		// is rendered by the app render path (BuildStoreApplications), NOT a shared
		// module. The renderer's clusterenv.DevDatabaseURL computes the per-app
		// Postgres namespace/service from the app name + env, so no dev-postgres
		// module fixture is needed here.
		Modules: map[string]clusterenv.Module{
			"keda": {
				Enabled: true, Source: clusterenv.SourceChartRepo, Chart: "keda",
				RepoURL: "https://kedacore.github.io/charts", Version: "2.17.2", Namespace: "keda",
			},
		},
	}
	c.ApplyDefaults()
	return c
}

func prodCluster() *clusterenv.Config {
	c := devCluster()
	c.Env = "prod"
	c.Zones = []string{"carshowdb.example.com", "*.example.com"}
	c.Secrets.Backend = clusterenv.BackendSSM
	c.Secrets.StoreRef = clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore}
	c.Observability.ConsoleLogging = boolPtr(false)
	return c
}

func boolPtr(b bool) *bool { return &b }

// goldenCase renders an app for an env and compares the Argo Application YAML to
// a golden file. The DEPLOY_TIME is fixed for determinism.
func goldenCase(t *testing.T, deployFile, env string, c *clusterenv.Config, image, golden string) {
	t.Helper()
	app := loadTestApp(t, deployFile)
	// Mimic the deploy orchestration's per-env route selection so the golden
	// matches what the CLI writes.
	app.Routes = selectInZone(app.Routes, c)

	res, err := Render(app, env, c, image, "2026-06-02T00:00:00Z")
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	got, err := res.ApplicationYAML()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	goldenPath := filepath.Join("testdata", golden)
	if *updateGolden {
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden %s", goldenPath)
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("read golden %s (run with -update-golden first): %v", goldenPath, err)
	}
	if string(got) != string(want) {
		t.Errorf("golden mismatch for %s.\n--- got ---\n%s\n--- want ---\n%s", golden, got, want)
	}
}

func selectInZone(routes []appconfig.Route, c *clusterenv.Config) []appconfig.Route {
	if c == nil || len(c.Zones) == 0 {
		return routes
	}
	kept := make([]appconfig.Route, 0, len(routes))
	for _, r := range routes {
		if c.HostInZone(r.Host) {
			kept = append(kept, r)
		}
	}
	return kept
}

func TestGolden_CarshowdbDev(t *testing.T) {
	goldenCase(t, "carshowdb.deploy.yaml", "dev", devCluster(),
		"ghcr.io/jakenesler/carshowdb-api:dev-abc123", "carshowdb.dev.golden.yaml")
}

func TestGolden_CarshowdbProd(t *testing.T) {
	goldenCase(t, "carshowdb.deploy.yaml", "prod", prodCluster(),
		"ghcr.io/jakenesler/carshowdb-api:prod-abc123", "carshowdb.prod.golden.yaml")
}

func TestGolden_DummyUIConnectsTo(t *testing.T) {
	goldenCase(t, "dummy-ui.deploy.yaml", "dev", devCluster(),
		"ghcr.io/jakenesler/dummy-ui:dev-1", "dummy-ui.dev.golden.yaml")
}

func TestBuildValues_TierAAndStores(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	v, err := BuildValues(app, "dev", devCluster(), "ghcr.io/jakenesler/carshowdb-api:dev", "")
	if err != nil {
		t.Fatal(err)
	}
	if v.Env.TierA["ENVIRONMENT"] != "dev" {
		t.Errorf("ENVIRONMENT = %q", v.Env.TierA["ENVIRONMENT"])
	}
	if v.Env.TierA["PORT"] != "8080" {
		t.Errorf("PORT = %q", v.Env.TierA["PORT"])
	}
	if v.Env.TierA["LOKI_URL"] == "" {
		t.Error("LOKI_URL should be injected from cluster observability")
	}
	if _, ok := v.Env.TierA["IMAGE_NAME"]; ok {
		t.Error("IMAGE_NAME must NOT be injected (Helm sets the image)")
	}
	if len(v.DB) != 1 || !v.DB[0].Default {
		t.Errorf("first db should be default: %+v", v.DB)
	}
	if v.ExternalSecret.Backend != clusterenv.BackendLocal {
		t.Errorf("dev backend should be local, got %q", v.ExternalSecret.Backend)
	}
	if v.ServiceMonitor.ReleaseLabel != DefaultReleaseLabel {
		t.Errorf("serviceMonitor releaseLabel = %q, want %q", v.ServiceMonitor.ReleaseLabel, DefaultReleaseLabel)
	}
}

// TestNamespaceAndStoreApplications verifies the create-from-yaml namespace
// scheme end to end: the app Application targets <app>-<env>-<component> with
// CreateNamespace=true, a dedicated dev-postgres Application is rendered into
// <app>-<env>-postgres, and the app's DATABASE_URL host is the cross-namespace
// FQDN of that per-app Postgres.
func TestNamespaceAndStoreApplications(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	app.Component = "api"

	res, err := Render(app, "dev", devCluster(), "ghcr.io/jakenesler/carshowdb-api:dev-abc", "")
	if err != nil {
		t.Fatal(err)
	}

	// App Application: own namespace + CreateNamespace + namespace labels.
	dest := res.Application.Spec.Destination.Namespace
	if dest != "carshowdb-dev-api" {
		t.Errorf("app dest namespace = %q, want carshowdb-dev-api", dest)
	}
	sp := res.Application.Spec.SyncPolicy
	if !contains(sp.SyncOptions, "CreateNamespace=true") {
		t.Errorf("app syncOptions missing CreateNamespace=true: %v", sp.SyncOptions)
	}
	if sp.ManagedNamespaceMetadata == nil || sp.ManagedNamespaceMetadata.Labels["platform/purpose"] != "api" {
		t.Errorf("app managedNamespaceMetadata purpose label = %+v", sp.ManagedNamespaceMetadata)
	}

	// Dedicated per-app dev Postgres Application.
	if len(res.StoreApplications) != 1 {
		t.Fatalf("expected 1 store application, got %d", len(res.StoreApplications))
	}
	pg := res.StoreApplications[0]
	if pg.FileStem != "carshowdb-postgres" {
		t.Errorf("store file stem = %q, want carshowdb-postgres", pg.FileStem)
	}
	if pg.Application.Spec.Destination.Namespace != "carshowdb-dev-postgres" {
		t.Errorf("store dest namespace = %q, want carshowdb-dev-postgres", pg.Application.Spec.Destination.Namespace)
	}
	if pg.Application.Spec.Source.Path != "charts/infra/dev-postgres" {
		t.Errorf("store source path = %q", pg.Application.Spec.Source.Path)
	}
	if !contains(pg.Application.Spec.SyncPolicy.SyncOptions, "CreateNamespace=true") {
		t.Errorf("store syncOptions missing CreateNamespace=true")
	}

	// DATABASE_URL host is the cross-namespace FQDN of the per-app Postgres.
	if len(res.Values.DB) != 1 || res.Values.DB[0].Connection == nil {
		t.Fatalf("expected a resolved db connection, got %+v", res.Values.DB)
	}
	url := res.Values.DB[0].Connection.URL
	wantHost := "carshowdb-postgres-dev-postgres.carshowdb-dev-postgres.svc.cluster.local"
	if !strings.Contains(url, "@"+wantHost+":5432/carshowdb?") {
		t.Errorf("DATABASE_URL = %q, want host %q", url, wantHost)
	}
}

// TestProdNoPerAppPostgres confirms prod (backend=ssm) renders NO per-app
// Postgres Application and NO dev connection URL (DATABASE_URL comes from SSM).
func TestProdNoPerAppPostgres(t *testing.T) {
	app := loadTestApp(t, "carshowdb.deploy.yaml")
	app.Component = "api"
	res, err := Render(app, "prod", prodCluster(), "ghcr.io/jakenesler/carshowdb-api:prod-abc", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.StoreApplications) != 0 {
		t.Errorf("prod should render no per-app store applications, got %d", len(res.StoreApplications))
	}
	if res.Application.Spec.Destination.Namespace != "carshowdb-prod-api" {
		t.Errorf("prod app namespace = %q, want carshowdb-prod-api", res.Application.Spec.Destination.Namespace)
	}
	if len(res.Values.DB) == 1 && res.Values.DB[0].Connection != nil {
		t.Errorf("prod db should have no dev connection URL, got %+v", res.Values.DB[0].Connection)
	}
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}

func TestDataStoreEnvKeys(t *testing.T) {
	app := appconfig.App{
		DB: []appconfig.DataStore{
			{Name: "primary", Type: "postgres"},
			{Name: "analytics", Type: "postgres"},
		},
		Cache: []appconfig.DataStore{
			{Name: "sessions", Type: "redis"},
		},
	}
	keys := DataStoreEnvKeys(app)
	want := []string{"DATABASE_URL", "PRIMARY_DATABASE_URL", "ANALYTICS_DATABASE_URL", "REDIS_URL", "SESSIONS_REDIS_URL"}
	if len(keys) != len(want) {
		t.Fatalf("keys = %v, want %v", keys, want)
	}
	for i := range want {
		if keys[i] != want[i] {
			t.Errorf("keys[%d] = %q, want %q", i, keys[i], want[i])
		}
	}
}
