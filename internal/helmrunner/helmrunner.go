// Package helmrunner is a thin wrapper over the `helm` CLI for local rendering
// and linting. platformctl never runs `helm upgrade --install` on the default
// path (Flux is the only writer); helmrunner exists for preflight: rendering
// the chart from values (to scan output for guardrail violations) and linting.
package helmrunner

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"sigs.k8s.io/yaml"
)

// Runner executes helm. Bin defaults to "helm" on PATH.
type Runner struct {
	Bin string
}

// New returns a Runner using the helm binary on PATH.
func New() *Runner { return &Runner{Bin: "helm"} }

func (r *Runner) bin() string {
	if r.Bin != "" {
		return r.Bin
	}
	return "helm"
}

// Available reports whether the helm binary can be found. Callers degrade
// gracefully (skip template-based guardrails) when helm is absent.
func (r *Runner) Available() bool {
	_, err := exec.LookPath(r.bin())
	return err == nil
}

// Template runs `helm template <release> <chartDir> -f <valuesFile> -n <ns>` and
// returns the rendered multi-document YAML. values is marshaled to a temp file.
func (r *Runner) Template(ctx context.Context, release, chartDir, namespace string, values any) ([]byte, error) {
	valuesData, err := yaml.Marshal(values)
	if err != nil {
		return nil, fmt.Errorf("marshal values: %w", err)
	}
	tmp, err := os.CreateTemp("", "platformctl-values-*.yaml")
	if err != nil {
		return nil, fmt.Errorf("temp values file: %w", err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(valuesData); err != nil {
		tmp.Close()
		return nil, fmt.Errorf("write values: %w", err)
	}
	tmp.Close()

	args := []string{"template", release, chartDir, "-f", tmp.Name()}
	if namespace != "" {
		args = append(args, "-n", namespace)
	}
	return r.run(ctx, args...)
}

// Lint runs `helm lint <chartDir>`.
func (r *Runner) Lint(ctx context.Context, chartDir string) ([]byte, error) {
	return r.run(ctx, "lint", chartDir)
}

func (r *Runner) run(ctx context.Context, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, r.bin(), args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("helm %v: %w: %s", args, err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// ChartDir resolves the app chart directory under a platform repo root.
func ChartDir(root string) string {
	return filepath.Join(root, "charts", "app")
}
