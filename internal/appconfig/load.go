package appconfig

import (
	"fmt"
	"os"

	"sigs.k8s.io/yaml"
)

// Load reads a deploy.yaml from disk and unmarshals it through the production
// codepath (sigs.k8s.io/yaml -> json struct tags). It does NOT apply defaults or
// validate; callers chain ApplyDefaults() and Validate() explicitly so each step
// is observable (and so `plan` can report exactly which step failed).
func Load(path string) (App, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return App{}, fmt.Errorf("read deploy file %q: %w", path, err)
	}
	return Parse(raw)
}

// Parse unmarshals deploy.yaml bytes into an App. sigs.k8s.io/yaml converts YAML
// to JSON first, so it honors the json struct tags and rejects unknown-typed
// values the same way the production renderer will.
func Parse(raw []byte) (App, error) {
	var app App
	if err := yaml.Unmarshal(raw, &app); err != nil {
		return App{}, fmt.Errorf("parse deploy yaml: %w", err)
	}
	return app, nil
}

// LoadDefaulted is the common convenience: Load + ApplyDefaults. Validation is
// still the caller's responsibility (policy package owns guardrails).
func LoadDefaulted(path string) (App, error) {
	app, err := Load(path)
	if err != nil {
		return App{}, err
	}
	app.ApplyDefaults()
	return app, nil
}
