// Package scaffold generates starter files for onboarding a new app: a minimal
// deploy.yaml (the developer shopping list). Output is deterministic and
// OSS-clean (no secrets).
//
// The image registry prefix is REQUIRED input (from idp.yaml or --registry):
// scaffold never assumes whose registry an app pushes to, so a fork cannot
// silently scaffold apps pointed at someone else's namespace.
package scaffold

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"text/template"
)

// Options drives `idpctl new app`.
type Options struct {
	Name     string // app name (DNS-1123); also the image repo suffix.
	Registry string // image registry prefix (e.g. ghcr.io/your-org). Required.
	Host     string // primary route host (optional).
	Port     int    // container port.
	Product  string // optional product group.
	WithDB   bool   // include a primary postgres db.
}

// Files is the generated file set, keyed by relative path.
type Files map[string][]byte

// Generate renders the starter files in memory. Callers Write them or print them.
func Generate(o Options) (Files, error) {
	if o.Name == "" {
		return nil, fmt.Errorf("app name is required")
	}
	if o.Registry == "" {
		return nil, fmt.Errorf("registry is required (set it in the platform repo's idp.yaml, or pass --registry)")
	}
	if o.Port == 0 {
		o.Port = 8080
	}
	deploy, err := render(deployTmpl, o)
	if err != nil {
		return nil, err
	}
	return Files{
		"deploy.yaml": deploy,
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
# Render:  idpctl render --env dev --file deploy.yaml --image {{.Registry}}/{{.Name}}:<tag>
# Ship:    register this app in the idp-shipper registry; every push to the
#          watched branch then builds, renders, and deploys it automatically.

app: {{.Name}}
{{- if .Product}}
product: {{.Product}}
{{- end}}

runtime:
  image: {{.Registry}}/{{.Name}}   # repo only; the platform injects the tag via --image
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
