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

// Options drives `platformctl new app`.
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
	ship, err := render(shipTmpl, o)
	if err != nil {
		return nil, err
	}
	return Files{
		"deploy.yaml":                deploy,
		".github/workflows/ship.yml": ship,
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
# Render:  platformctl render --env dev --file deploy.yaml --image ghcr.io/jakenesler/{{.Name}}:<tag>

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

const shipTmpl = `name: Ship
on:
  workflow_dispatch: {}
  push:
    branches: [main]

jobs:
  ship:
    runs-on: [self-hosted, k8s]
    steps:
      - uses: actions/checkout@v4
      - name: Build and push image
        run: |
          TAG=prod-${GITHUB_SHA::8}
          IMG=ghcr.io/jakenesler/{{.Name}}:$TAG
          docker buildx build --platform linux/amd64 -t "$IMG" --push .
          echo "IMG=$IMG" >> "$GITHUB_ENV"
      - uses: actions/checkout@v4
        with:
          repository: jakenesler/platformctl
          path: .platform
          token: ${{"{{"}} secrets.PLATFORM_REPO_TOKEN {{"}}"}}
      - name: Render desired state
        run: |
          .platform/bin/platformctl render \
            --env prod --file deploy.yaml --image "$IMG" \
            --root .platform
      # Commit/PR the generated environments/ change; Flux reconciles it.
`
