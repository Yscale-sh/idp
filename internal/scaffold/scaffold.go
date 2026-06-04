// Package scaffold generates starter files for onboarding a new app: a minimal
// deploy.yaml (the developer shopping list) and a thin .github/workflows/ship.yml
// that builds the image and renders desired state. Output is deterministic and
// OSS-clean (no secrets).
package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// Options drives `jdpctl new app`.
type Options struct {
	Name    string // app name (DNS-1123); also the image repo suffix.
	Host    string // primary route host (optional).
	Port    int    // container port.
	Product string // optional product group.
	WithDB  bool   // include a primary postgres db.
}

// Files is the generated file set, keyed by relative path.
type Files map[string][]byte

// Generate renders the starter files in memory. Callers Write them or print them.
func Generate(o Options) (Files, error) {
	if o.Name == "" {
		return nil, fmt.Errorf("app name is required")
	}
	if o.Port == 0 {
		o.Port = 8080
	}
	deploy, err := render(deployTmpl, o)
	if err != nil {
		return nil, err
	}
	// ship.yml is a STATIC thin caller stub (literal GitHub Actions ${{ }} syntax,
	// no Go-template vars), so it is emitted verbatim rather than through render().
	return Files{
		"deploy.yaml":                deploy,
		".github/workflows/ship.yml": []byte(shipTmpl),
	}, nil
}

// Write writes the generated files under dir, creating subdirectories. It will
// not overwrite existing files (onboarding should be explicit). Returns the
// written paths.
func Write(dir string, files Files) ([]string, error) {
	var written []string
	for rel, data := range files {
		out := filepath.Join(dir, rel)
		if _, err := os.Stat(out); err == nil {
			return written, fmt.Errorf("refusing to overwrite existing file %s", out)
		}
		if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
			return written, err
		}
		if err := os.WriteFile(out, data, 0o644); err != nil {
			return written, err
		}
		written = append(written, out)
	}
	return written, nil
}

func render(tmpl string, o Options) ([]byte, error) {
	t, err := template.New("scaffold").Parse(tmpl)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, o); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

const deployTmpl = `# {{.Name}} — deploy.yaml (developer shopping list).
# Render:  jdpctl render --env dev --file deploy.yaml --image ghcr.io/jakenesler/{{.Name}}:<tag>

app: {{.Name}}
{{- if .Product}}
product: {{.Product}}
{{- end}}

runtime:
  image: ghcr.io/jakenesler/{{.Name}}   # repo only; CI injects the tag via --image
  port: {{.Port}}
{{- if .Host}}

routes:
  - host: {{.Host}}
    public: true
    access:
      humans: false
      serviceToken: false
{{- end}}

sizing:
  profile: minimal
  replicas: 1
{{- if .WithDB}}

db:
  - name: primary
    type: postgres
    size: minimal
{{- end}}

logging:
  enabled: true
metrics:
  enabled: true
`

// shipTmpl is the app-repo .github/workflows/ship.yml — a THIN caller stub for the
// reusable JDP workflow. On push it builds + pushes this repo's image, renders its
// deploy.yaml into the platform umbrella, and commits it (Flux reconciles). It is
// STATIC (literal Actions ${{ }} syntax, no Go-template vars) so it is emitted as
// raw bytes, not rendered. Teardown: run it manually with remove=true (or
// `jdpctl remove`) to delete the app from the platform.
const shipTmpl = `# Ship — onboards/updates (or tears down) this app on JDP via the reusable
# workflow in jakenesler/jdp. The platform owns build -> render -> commit; Flux
# reconciles. No deploy.yaml -> render details here; just the call.
name: Ship
on:
  push:
    branches: [main]
  workflow_dispatch:
    inputs:
      remove:
        description: "Tear down: remove this app from the platform instead of deploying"
        type: boolean
        default: false

jobs:
  ship:
    uses: jakenesler/jdp/.github/workflows/ship.yml@v1   # pin a released major
    permissions:
      contents: read
      packages: write
    with:
      env: dev                      # prod apps set env: prod
      jdpctl-tag: v1
      remove: ${{ github.event.inputs.remove == 'true' }}
      # manage-dns: true            # also upsert Cloudflare DNS for public routes
                                    # (proxied CNAME -> the app's tunnel). Off by
                                    # default; the Cloudflare Tunnel is the exposure
                                    # either way. Set the domain by hand otherwise.
    secrets:
      # Prefer a GitHub App (org secrets => zero-config onboarding); PAT fallback.
      JDP_APP_ID: ${{ secrets.JDP_APP_ID }}
      JDP_APP_PRIVATE_KEY: ${{ secrets.JDP_APP_PRIVATE_KEY }}
      # PLATFORM_REPO_TOKEN: ${{ secrets.PLATFORM_REPO_TOKEN }}
      # For manage-dns: pass the Cloudflare token directly, OR give AWS creds and the
      # step reads CLOUDFLARE_API_TOKEN + TUNNEL_TOKEN from SSM (existing prod creds).
      # CLOUDFLARE_API_TOKEN: ${{ secrets.CLOUDFLARE_API_TOKEN }}
      # TUNNEL_TOKEN: ${{ secrets.TUNNEL_TOKEN }}
      # AWS_ROLE_ARN: ${{ secrets.AWS_ROLE_ARN }}
`
