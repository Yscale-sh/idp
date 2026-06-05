package render

import (
	"path/filepath"
	"testing"

	"github.com/jakenesler/jdp/internal/appconfig"
)

// loadDimComponent loads one of the examples/dim/*.deploy.yaml component specs.
func loadDimComponent(t *testing.T, file string) appconfig.App {
	t.Helper()
	app, err := appconfig.LoadDefaulted(filepath.Join("..", "..", "examples", "dim", file))
	if err != nil {
		t.Fatalf("load %s: %v", file, err)
	}
	return app
}

// TestDim_MultiComponentIngest is the end-to-end proof that the IDP ingests Dim's
// three components (api/scanner/ui sharing app: dim) into ONE umbrella without
// collision, and that each component's special needs render correctly:
// component-aware naming, a portless worker, shared stores, extended/GPU limits,
// NFS/emptyDir volumes, on-prem LAN expose, and cross-component DNS.
func TestDim_MultiComponentIngest(t *testing.T) {
	c := devCluster()
	c.Zones = nil // Dim is internal/LAN — no public-zone narrowing

	api := loadDimComponent(t, "dim-api.deploy.yaml")
	scanner := loadDimComponent(t, "dim-scanner.deploy.yaml")
	ui := loadDimComponent(t, "dim-ui.deploy.yaml")

	apiRes, err := Render(api, "dev", c, "ghcr.io/jakenesler/dim:dev-1", "")
	if err != nil {
		t.Fatalf("render api: %v", err)
	}
	scanRes, err := Render(scanner, "dev", c, "ghcr.io/jakenesler/dim:dev-1", "")
	if err != nil {
		t.Fatalf("render scanner: %v", err)
	}
	uiRes, err := Render(ui, "dev", c, "ghcr.io/jakenesler/dim-ui:dev-1", "")
	if err != nil {
		t.Fatalf("render ui: %v", err)
	}

	// 1. Component-aware naming: three distinct workload handles from app: dim.
	if got := api.Workload(); got != "dim-api" {
		t.Errorf("api workload = %q, want dim-api", got)
	}
	if got := scanner.Workload(); got != "dim-scanner" {
		t.Errorf("scanner workload = %q, want dim-scanner", got)
	}
	if got := ui.Workload(); got != "dim-ui" {
		t.Errorf("ui workload = %q, want dim-ui", got)
	}

	// 2. Worker: the scanner is portless — Worker flag set, no Service.
	if !scanRes.Values.Worker {
		t.Error("scanner should render as a worker (port 0)")
	}
	if apiRes.Values.Worker {
		t.Error("api should NOT be a worker")
	}

	// 3. Shared stores: api PROVISIONS postgres + redis; scanner/ui provision none.
	if len(apiRes.StoreReleases) != 2 {
		t.Fatalf("api should provision 2 stores (postgres+redis), got %d", len(apiRes.StoreReleases))
	}
	tools := map[string]bool{}
	for _, s := range apiRes.StoreReleases {
		tools[s.Tool] = true
	}
	if !tools["postgres"] || !tools["redis"] {
		t.Errorf("api stores = %v, want postgres+redis", tools)
	}
	if len(scanRes.StoreReleases) != 0 {
		t.Errorf("scanner should provision NO stores (shares api's), got %d", len(scanRes.StoreReleases))
	}
	// But the scanner still gets the URLs wired to the SAME app-level store.
	if len(scanRes.Values.DB) != 1 || scanRes.Values.DB[0].Connection == nil {
		t.Fatalf("scanner should still wire DATABASE_URL to the shared store")
	}
	if apiRes.Values.DB[0].Connection.URL != scanRes.Values.DB[0].Connection.URL {
		t.Errorf("api and scanner DATABASE_URL differ — stores not shared:\n api=%s\n scn=%s",
			apiRes.Values.DB[0].Connection.URL, scanRes.Values.DB[0].Connection.URL)
	}

	// 4. Extended/GPU limit on the api only.
	if apiRes.Values.Resources.ExtraLimits["gpu.intel.com/i915"] != "1" {
		t.Errorf("api should request the iGPU, got %v", apiRes.Values.Resources.ExtraLimits)
	}

	// 5. Volumes: api gets media(ro NFS) + metadata(NFS subpath) + transcode(emptyDir).
	if len(apiRes.Values.Volumes) != 3 || len(apiRes.Values.VolumeMounts) != 3 {
		t.Fatalf("api should have 3 volumes+mounts, got %d/%d", len(apiRes.Values.Volumes), len(apiRes.Values.VolumeMounts))
	}

	// 6. LAN expose on the ui only (the MetalLB LoadBalancer).
	if uiRes.Values.LanExpose == nil || !uiRes.Values.LanExpose.Enabled || uiRes.Values.LanExpose.IP != "10.0.0.206" {
		t.Errorf("ui should LAN-expose on 10.0.0.206, got %+v", uiRes.Values.LanExpose)
	}
	if apiRes.Values.LanExpose != nil {
		t.Error("api should NOT LAN-expose")
	}

	// 7. Cross-component DNS: ui's DIM_API_UPSTREAM -> the api Service on port 8000,
	// as a BARE host:port (scheme: none) for nginx's `upstream { server ...; }`.
	want := "dim-api.dim-dev-api.svc.cluster.local:8000"
	if got := uiRes.Values.Env.Extra["DIM_API_UPSTREAM"]; got != want {
		t.Errorf("DIM_API_UPSTREAM = %q, want %q", got, want)
	}
	// dim-api uses a TCP probe (no HTTP health route).
	if apiRes.Values.Probes.Type != "tcp" {
		t.Errorf("dim-api probes.type = %q, want tcp", apiRes.Values.Probes.Type)
	}

	// 8. Arbitrary env passthrough.
	if apiRes.Values.Env.Extra["DIM_ROLE"] != "web" || scanRes.Values.Env.Extra["DIM_ROLE"] != "scanner" {
		t.Errorf("DIM_ROLE not passed through (api=%q scanner=%q)",
			apiRes.Values.Env.Extra["DIM_ROLE"], scanRes.Values.Env.Extra["DIM_ROLE"])
	}

	// 9. Umbrella: all three coexist as DISTINCT entries (no collision).
	root := t.TempDir()
	for _, r := range []*Result{apiRes, scanRes, uiRes} {
		if _, err := r.UpsertApp(root, c); err != nil {
			t.Fatalf("upsert %s: %v", r.App.Workload(), err)
		}
	}
	pr, err := ReadPlatform(root, "dev", c)
	if err != nil {
		t.Fatal(err)
	}
	if len(pr.Spec.Values.Apps) != 3 {
		t.Fatalf("umbrella should hold 3 distinct workloads, got %d", len(pr.Spec.Values.Apps))
	}
	seen := map[string]bool{}
	for _, a := range pr.Spec.Values.Apps {
		if seen[a.ReleaseName] {
			t.Errorf("duplicate umbrella entry %q (collision)", a.ReleaseName)
		}
		seen[a.ReleaseName] = true
	}
	for _, want := range []string{"dim-api", "dim-scanner", "dim-ui"} {
		if !seen[want] {
			t.Errorf("umbrella missing %q", want)
		}
	}
}
