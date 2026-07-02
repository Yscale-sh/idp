package catalog

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/render"
	"sigs.k8s.io/yaml"
)

// sampleRelease is a minimal umbrella with a provisioning api, a portless worker
// sharing its stores, and a LAN-exposed ui — the dim/yscale shape in miniature.
// Built from YAML so Values lands as the same map[string]any shape the real read
// path produces (numbers as float64, etc.).
func sampleRelease(t *testing.T) *render.PlatformRelease {
	t.Helper()
	var pr render.PlatformRelease
	if err := yaml.Unmarshal([]byte(sampleYAML), &pr); err != nil {
		t.Fatalf("unmarshal sample: %v", err)
	}
	return &pr
}

const sampleYAML = `
apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
metadata: { name: platform, namespace: flux-system }
spec:
  interval: 10m
  releaseName: platform
  values:
    env: dev
    source: { name: flux-system, namespace: flux-system }
    apps:
    - name: media
      component: api
      releaseName: media-api
      namespace: media-dev-api
      stores:
      - { tool: postgres, namespace: media-dev-postgres, releaseName: media-postgres, chart: ./charts/infra/dev-postgres }
      values:
        image: { repository: ghcr.io/x/media-api, tag: abc123 }
        port: 8000
        replicas: 1
        keda: { enabled: true, kind: HTTPScaledObject, minReplicas: 0, maxReplicas: 4 }
        routes:
        - { host: media.example.com, public: true, access: { humans: true, serviceToken: false } }
        db:
        - { name: primary, type: postgres, urlKeys: [DATABASE_URL, PRIMARY_DATABASE_URL] }
        cache:
        - { name: events, type: redis, urlKeys: [REDIS_URL] }
        externalSecret:
          enabled: true
          backend: local
          dataFrom: [ { extract: { key: /apps/media/dev } } ]
    - name: media
      component: scanner
      releaseName: media-scanner
      namespace: media-dev-scanner
      values:
        image: { repository: ghcr.io/x/media-api, tag: abc123 }
        port: 0
        replicas: 1
    - name: media
      component: ui
      releaseName: media-ui
      namespace: media-dev-ui
      values:
        image: { repository: ghcr.io/x/media-ui, tag: abc123 }
        port: 80
        replicas: 2
        lanExpose: { enabled: true, ip: 10.0.0.50, port: 80 }
    modules:
    - { name: keda, namespace: keda, source: chartRepo, chart: keda, version: 2.17.2 }
`

func TestBuild(t *testing.T) {
	c := Build(sampleRelease(t))

	if c.Env != "dev" || c.Source != "flux-system" {
		t.Fatalf("env/source = %q/%q", c.Env, c.Source)
	}
	if len(c.Apps) != 3 || len(c.Modules) != 1 {
		t.Fatalf("got %d apps, %d modules; want 3 apps, 1 module", len(c.Apps), len(c.Modules))
	}
	if c.Products() != 1 {
		t.Fatalf("Products() = %d, want 1 (one media product)", c.Products())
	}
	// Apps are sorted by workload handle: media-api, media-scanner, media-ui.
	api, scanner, ui := c.Apps[0], c.Apps[1], c.Apps[2]

	if api.Image != "ghcr.io/x/media-api:abc123" {
		t.Errorf("api image = %q", api.Image)
	}
	if api.Autoscale == nil || api.Autoscale.Min != 0 || api.Autoscale.Max != 4 {
		t.Errorf("api autoscale = %+v", api.Autoscale)
	}
	if len(api.Routes) != 1 || !api.Routes[0].Public || !api.Routes[0].Humans {
		t.Errorf("api routes = %+v", api.Routes)
	}
	if len(api.DBs) != 1 || api.DBs[0].Type != "postgres" || len(api.DBs[0].URLKeys) != 2 {
		t.Errorf("api dbs = %+v", api.DBs)
	}
	if len(api.Caches) != 1 || api.Caches[0].Name != "events" {
		t.Errorf("api caches = %+v", api.Caches)
	}
	if len(api.Stores) != 1 || api.Stores[0].Tool != "postgres" {
		t.Errorf("api stores = %+v", api.Stores)
	}
	if api.Secret == nil || api.Secret.Backend != "local" || api.Secret.Key != "/apps/media/dev" {
		t.Errorf("api secret = %+v", api.Secret)
	}
	if api.Worker {
		t.Error("api should not be a worker")
	}

	if !scanner.Worker || scanner.Port != 0 {
		t.Errorf("scanner should be a worker (port 0), got worker=%v port=%d", scanner.Worker, scanner.Port)
	}
	if len(scanner.Stores) != 0 {
		t.Errorf("scanner provisions nothing (shares api's), got %+v", scanner.Stores)
	}

	if ui.LAN == nil || ui.LAN.IP != "10.0.0.50" || ui.LAN.Port != 80 {
		t.Errorf("ui lan = %+v", ui.LAN)
	}
	if ui.Replicas != 2 {
		t.Errorf("ui replicas = %d", ui.Replicas)
	}
}

func TestRenderHTML(t *testing.T) {
	c := Build(sampleRelease(t))
	html, err := RenderHTML(c)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	s := string(html)
	for _, want := range []string{
		"<!DOCTYPE html>", "platform catalog", "media-api", "media-scanner",
		"ghcr.io/x/media-api:abc123", "autoscale 0–4", "worker", "10.0.0.50", "keda",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("HTML missing %q", want)
		}
	}
	// Deterministic: identical input → identical bytes (clean Pages diffs).
	html2, _ := RenderHTML(c)
	if !bytes.Equal(html, html2) {
		t.Error("RenderHTML is not deterministic")
	}
}

func TestRenderText(t *testing.T) {
	c := Build(sampleRelease(t))
	var buf bytes.Buffer
	c.WriteText(&buf)
	s := buf.String()
	for _, want := range []string{"env dev", "media-api", "worker (no Service)", "autoscale 0–4", "MODULES"} {
		if !strings.Contains(s, want) {
			t.Errorf("text missing %q", want)
		}
	}
}

func TestJSON(t *testing.T) {
	c := Build(sampleRelease(t))
	b, err := JSON(c)
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if !strings.Contains(string(b), `"workload": "media-api"`) {
		t.Errorf("JSON missing workload field:\n%s", b)
	}
}

func TestBuildSite(t *testing.T) {
	root := t.TempDir()
	for _, env := range []string{"dev", "prod"} {
		dir := filepath.Join(root, "clusters", env)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "platform.yaml"), []byte(sampleYAML), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	envs, err := DiscoverEnvs(root)
	if err != nil {
		t.Fatalf("DiscoverEnvs: %v", err)
	}
	if len(envs) != 2 || envs[0] != "dev" || envs[1] != "prod" {
		t.Fatalf("DiscoverEnvs = %v, want [dev prod]", envs)
	}

	outDir := filepath.Join(root, "public")
	if _, err := BuildSite(root, outDir); err != nil {
		t.Fatalf("BuildSite: %v", err)
	}
	for _, f := range []string{"index.html", "dev.html", "prod.html"} {
		if _, err := os.Stat(filepath.Join(outDir, f)); err != nil {
			t.Errorf("missing site file %s: %v", f, err)
		}
	}
	// The index must link each env page, and the prod page must carry the prod
	// identity even though the fixture's inner env field says dev (dir wins).
	index, _ := os.ReadFile(filepath.Join(outDir, "index.html"))
	for _, want := range []string{`href="dev.html"`, `href="prod.html"`} {
		if !strings.Contains(string(index), want) {
			t.Errorf("index.html missing link %q", want)
		}
	}
	prod, _ := os.ReadFile(filepath.Join(outDir, "prod.html"))
	if !strings.Contains(string(prod), `<title>idp catalog — prod</title>`) {
		t.Error("prod.html should carry the prod env identity from its directory")
	}
}

func TestDiscoverEnvsEmpty(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "clusters"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := DiscoverEnvs(root); err == nil {
		t.Error("DiscoverEnvs should error when no env has rendered state")
	}
}
