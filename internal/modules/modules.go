// Package modules turns the per-env module registry (environments/<env>/
// cluster.yaml -> modules.<name>) into one Argo CD Application per ENABLED
// module, written to environments/<env>/infra/<module>.yaml. Disabled modules
// (e.g. yscale on a LAN cluster) are skipped — there is nothing to reconcile.
//
//   - source=localChart -> Application.spec.source.path = the in-repo chart path
//     (charts/infra/<x>); version comes from the chart itself.
//   - source=chartRepo  -> Application.spec.source.{repoURL,chart,targetRevision}
//     with the pinned version; values flow through helm.valuesObject.
package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jakenesler/platformctl/internal/clusterenv"
	"github.com/jakenesler/platformctl/internal/helmrunner"
	"github.com/jakenesler/platformctl/internal/policy"
	"sigs.k8s.io/yaml"
)

// PlannedModule is one module resolved to its Argo Application plus metadata for
// the plan summary.
type PlannedModule struct {
	Name        string
	Namespace   string
	Source      string
	Chart       string
	Version     string
	Application ModuleApplication
}

// ModuleApplication mirrors the app renderer's Application shape but is kept here
// independently so infra rendering does not depend on the app render internals.
type ModuleApplication struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Metadata   ModuleMeta `json:"metadata"`
	Spec       ModuleSpec `json:"spec"`
}

type ModuleMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type ModuleSpec struct {
	Project     string        `json:"project"`
	Source      ModuleSource  `json:"source"`
	Destination ModuleDest    `json:"destination"`
	SyncPolicy  ModuleSyncPol `json:"syncPolicy"`
}

type ModuleSource struct {
	RepoURL        string      `json:"repoURL"`
	Path           string      `json:"path,omitempty"`
	Chart          string      `json:"chart,omitempty"`
	TargetRevision string      `json:"targetRevision"`
	Helm           *ModuleHelm `json:"helm,omitempty"`
}

type ModuleHelm struct {
	ValuesObject any `json:"valuesObject,omitempty"`
}

type ModuleDest struct {
	Server    string `json:"server,omitempty"`
	Namespace string `json:"namespace"`
}

type ModuleSyncPol struct {
	Automated   *ModuleAutomated `json:"automated,omitempty"`
	SyncOptions []string         `json:"syncOptions,omitempty"`
}

type ModuleAutomated struct {
	Prune    bool `json:"prune"`
	SelfHeal bool `json:"selfHeal"`
}

const (
	argoAPIVersion = "argoproj.io/v1alpha1"
	argoKind       = "Application"
	argoProject    = "default"
	destServer     = "https://kubernetes.default.svc"
	createNS       = "CreateNamespace=true"
)

// Plan resolves every ENABLED module in the env config into a PlannedModule.
// Modules are returned sorted by name for deterministic output.
func Plan(c *clusterenv.Config) ([]PlannedModule, error) {
	names := make([]string, 0, len(c.Modules))
	for name := range c.Modules {
		names = append(names, name)
	}
	sort.Strings(names)

	var planned []PlannedModule
	for _, name := range names {
		m := c.Modules[name]
		if !m.Enabled {
			continue
		}
		app, err := buildModuleApp(c, name, m)
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", name, err)
		}
		planned = append(planned, PlannedModule{
			Name:        name,
			Namespace:   moduleNamespace(name, m),
			Source:      m.Source,
			Chart:       m.Chart,
			Version:     m.Version,
			Application: app,
		})
	}
	return planned, nil
}

func moduleNamespace(name string, m clusterenv.Module) string {
	if m.Namespace != "" {
		return m.Namespace
	}
	return name
}

func buildModuleApp(c *clusterenv.Config, name string, m clusterenv.Module) (ModuleApplication, error) {
	ns := moduleNamespace(name, m)
	targetRev := c.Argo.TargetRevision
	if targetRev == "" {
		targetRev = clusterenv.DefaultTargetRevision
	}

	src := ModuleSource{TargetRevision: targetRev}
	switch m.Source {
	case clusterenv.SourceLocalChart:
		src.RepoURL = c.Argo.RepoURL
		src.Path = m.Chart
	case clusterenv.SourceChartRepo:
		src.RepoURL = m.RepoURL
		src.Chart = m.Chart
		src.TargetRevision = m.Version // chartRepo pins via version
	default:
		return ModuleApplication{}, fmt.Errorf("unknown source %q", m.Source)
	}
	if len(m.Values) > 0 {
		src.Helm = &ModuleHelm{ValuesObject: m.Values}
	}

	labels := map[string]string{
		"platform/module":     name,
		"platform/env":        c.Env,
		"platform/managed-by": "platformctl",
	}

	return ModuleApplication{
		APIVersion: argoAPIVersion,
		Kind:       argoKind,
		Metadata: ModuleMeta{
			Name:      name,
			Namespace: c.Argo.Namespace,
			Labels:    labels,
		},
		Spec: ModuleSpec{
			Project:     argoProject,
			Source:      src,
			Destination: ModuleDest{Server: destServer, Namespace: ns},
			SyncPolicy: ModuleSyncPol{
				Automated:   &ModuleAutomated{Prune: true, SelfHeal: true},
				SyncOptions: []string{createNS},
			},
		},
	}, nil
}

// CheckPolicy runs the platform guardrails over one planned module BEFORE it is
// written or applied. It enforces, in increasing depth:
//
//  1. inline Helm values must not request a LoadBalancer service type
//     (policy.CheckModuleValues) — the realistic leak surface for a chartRepo
//     module whose chart we cannot template offline;
//  2. the serialized Argo Application manifest must not carry a LoadBalancer
//     Service (policy.CheckRenderedManifest) — last-line scan over the YAML;
//  3. for a localChart module (chart lives in this repo), when root is set and
//     helm is on PATH, `helm template` the chart with the module's inline values
//     and scan the rendered manifests for a LoadBalancer Service.
//
// root is the platform repo root (for localChart template scans). It may be ""
// (skips step 3). A chartRepo chart is remote and not templated here; steps 1–2
// still guard it. Returns the collected violations (empty when clean).
func (p PlannedModule) CheckPolicy(root, env string) (policy.Violations, error) {
	var vs policy.Violations

	// (1) inline values override scan.
	if len(p.Application.Spec.Source.helmValues()) > 0 {
		vs = append(vs, policy.CheckModuleValues(p.Name, p.Application.Spec.Source.helmValues())...)
	}

	// (2) serialized Application manifest scan.
	manifest, err := p.YAML(env)
	if err != nil {
		return nil, err
	}
	vs = append(vs, policy.CheckRenderedManifest(manifest)...)

	// (3) localChart: template the in-repo chart and scan the real manifests.
	if p.Source == clusterenv.SourceLocalChart && root != "" && p.Chart != "" {
		hr := helmrunner.New()
		if hr.Available() {
			chartDir := filepath.Join(root, p.Chart)
			if _, statErr := os.Stat(chartDir); statErr == nil {
				rendered, terr := hr.Template(context.Background(),
					p.Name, chartDir, p.Namespace, p.Application.Spec.Source.helmValues())
				if terr == nil {
					vs = append(vs, policy.CheckRenderedManifest(rendered)...)
				}
			}
		}
	}

	return vs, nil
}

// CheckAll runs CheckPolicy over every planned module and returns the first
// non-empty set of violations as an error, so callers fail closed before any
// write or apply. root is the platform repo root (for localChart template scans).
func CheckAll(planned []PlannedModule, root, env string) error {
	for _, p := range planned {
		vs, err := p.CheckPolicy(root, env)
		if err != nil {
			return fmt.Errorf("module %q: %w", p.Name, err)
		}
		if e := vs.AsError(); e != nil {
			return fmt.Errorf("module %q policy violations: %w", p.Name, e)
		}
	}
	return nil
}

// helmValues returns the module's inline Helm values override as a map (nil when
// none / not a map), so policy can scan it without reaching into render types.
func (s ModuleSource) helmValues() map[string]any {
	if s.Helm == nil {
		return nil
	}
	if m, ok := s.Helm.ValuesObject.(map[string]any); ok {
		return m
	}
	return nil
}

// YAML serializes a planned module's Application with a generated-file header.
func (p PlannedModule) YAML(env string) ([]byte, error) {
	body, err := yaml.Marshal(p.Application)
	if err != nil {
		return nil, fmt.Errorf("marshal module %q: %w", p.Name, err)
	}
	header := fmt.Sprintf(
		"# Generated by platformctl. DO NOT EDIT BY HAND.\n"+
			"# Infra module %q for env=%s (source=%s).\n",
		p.Name, env, p.Source,
	)
	return append([]byte(header), body...), nil
}

// OutputPath is environments/<env>/infra/<module>.yaml under root.
func OutputPath(root, env, module string) string {
	return filepath.Join(root, "environments", env, "infra", module+".yaml")
}

// Write writes a planned module's Application to its canonical path under root.
func (p PlannedModule) Write(root, env string) (string, error) {
	out := OutputPath(root, env, p.Name)
	if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
		return "", fmt.Errorf("create infra dir: %w", err)
	}
	data, err := p.YAML(env)
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", out, err)
	}
	return out, nil
}
