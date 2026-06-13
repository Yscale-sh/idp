// Package modules turns the per-env module registry (environments/<env>/
// cluster.yaml -> modules.<name>) into Flux desired state per ENABLED module,
// written to environments/<env>/infra/<module>.yaml. Disabled modules (e.g.
// yscale on a LAN cluster) are skipped — there is nothing to reconcile.
//
//   - source=localChart -> a HelmRelease whose chart is the in-repo chart path
//     (charts/infra/<x>) served by the GitRepository source; version comes from
//     the chart itself.
//   - source=chartRepo  -> a HelmRepository (the Helm repo) plus a HelmRelease
//     that references it cross-namespace with the pinned version; values flow
//     through spec.values.
package modules

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jakenesler/idp/internal/clusterenv"
	"github.com/jakenesler/idp/internal/helmrunner"
	"github.com/jakenesler/idp/internal/policy"
	"sigs.k8s.io/yaml"
)

// PlannedModule is one module resolved to its Flux desired state plus metadata
// for the plan summary. For a chartRepo module Repository is set (the
// HelmRepository emitted alongside the HelmRelease); for a localChart module
// Repository is nil (the in-repo GitRepository source is referenced directly).
type PlannedModule struct {
	Name        string
	Namespace   string
	Source      string
	Chart       string
	Version     string
	HelmRelease ModuleHelmRelease
	Repository  *HelmRepository
}

// ModuleHelmRelease mirrors render.HelmRelease but is kept here independently so
// infra rendering does not depend on the app render internals.
type ModuleHelmRelease struct {
	APIVersion string            `json:"apiVersion"`
	Kind       string            `json:"kind"`
	Metadata   ModuleMeta        `json:"metadata"`
	Spec       ModuleReleaseSpec `json:"spec"`
}

type ModuleMeta struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type ModuleReleaseSpec struct {
	Interval         string         `json:"interval"`
	ReleaseName      string         `json:"releaseName,omitempty"`
	TargetNamespace  string         `json:"targetNamespace"`
	StorageNamespace string         `json:"storageNamespace"`
	Install          ModuleInstall  `json:"install"`
	Upgrade          ModuleUpgrade  `json:"upgrade"`
	Chart            ModuleChart    `json:"chart"`
	Values           map[string]any `json:"values,omitempty"`
}

type ModuleInstall struct {
	CreateNamespace bool               `json:"createNamespace"`
	DisableWait     bool               `json:"disableWait,omitempty"`
	Remediation     *ModuleRemediation `json:"remediation,omitempty"`
}

type ModuleUpgrade struct {
	DisableWait bool               `json:"disableWait,omitempty"`
	Remediation *ModuleRemediation `json:"remediation,omitempty"`
}

type ModuleRemediation struct {
	Retries int `json:"retries"`
}

type ModuleChart struct {
	Spec ModuleChartSpec `json:"spec"`
}

type ModuleChartSpec struct {
	Chart             string          `json:"chart"`
	Version           string          `json:"version,omitempty"`
	SourceRef         ModuleSourceRef `json:"sourceRef"`
	ReconcileStrategy string          `json:"reconcileStrategy,omitempty"`
}

type ModuleSourceRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// HelmRepository is the source.toolkit.fluxcd.io/v1 HelmRepository emitted for a
// chartRepo module so its HelmRelease can pull the chart from a Helm repo.
type HelmRepository struct {
	APIVersion string             `json:"apiVersion"`
	Kind       string             `json:"kind"`
	Metadata   ModuleMeta         `json:"metadata"`
	Spec       HelmRepositorySpec `json:"spec"`
}

type HelmRepositorySpec struct {
	URL      string `json:"url"`
	Interval string `json:"interval"`
}

const (
	helmReleaseAPIVersion = "helm.toolkit.fluxcd.io/v2"
	helmReleaseKind       = "HelmRelease"
	helmRepoAPIVersion    = "source.toolkit.fluxcd.io/v1"
	helmRepoKind          = "HelmRepository"
	sourceKindGitRepo     = "GitRepository"
	sourceKindHelmRepo    = "HelmRepository"
	reconcileRevision     = "Revision"
	fluxInterval          = "10m"
	remediationRetries    = 3
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
		p, err := buildModule(c, name, m)
		if err != nil {
			return nil, fmt.Errorf("module %q: %w", name, err)
		}
		planned = append(planned, p)
	}
	return planned, nil
}

func moduleNamespace(name string, m clusterenv.Module) string {
	if m.Namespace != "" {
		return m.Namespace
	}
	return name
}

// fluxSource resolves the per-env GitRepository source coordinates (name +
// namespace) that localChart modules reference cross-namespace.
func fluxSource(c *clusterenv.Config) (name, ns string) {
	name = clusterenv.DefaultFluxSourceName
	ns = clusterenv.DefaultFluxNamespace
	if c.Flux.SourceName != "" {
		name = c.Flux.SourceName
	}
	if c.Flux.Namespace != "" {
		ns = c.Flux.Namespace
	}
	return name, ns
}

func buildModule(c *clusterenv.Config, name string, m clusterenv.Module) (PlannedModule, error) {
	ns := moduleNamespace(name, m)
	sourceName, sourceNS := fluxSource(c)

	labels := map[string]string{
		"platform/module":     name,
		"platform/env":        c.Env,
		"platform/managed-by": "platformctl",
	}

	chartSpec := ModuleChartSpec{ReconcileStrategy: reconcileRevision}
	var repo *HelmRepository

	switch m.Source {
	case clusterenv.SourceLocalChart:
		// In-repo chart served by the GitRepository source the Flux bootstrap
		// provides; chart is the path, sourceRef is the GitRepository.
		chartSpec.Chart = "./" + m.Chart
		chartSpec.SourceRef = ModuleSourceRef{Kind: sourceKindGitRepo, Name: sourceName, Namespace: sourceNS}
	case clusterenv.SourceChartRepo:
		// Emit a HelmRepository for the chart's Helm repo, referenced by name.
		repoName := helmRepoName(m.RepoURL, name)
		repo = &HelmRepository{
			APIVersion: helmRepoAPIVersion,
			Kind:       helmRepoKind,
			Metadata: ModuleMeta{
				Name:      repoName,
				Namespace: sourceNS,
				Labels:    labels,
			},
			Spec: HelmRepositorySpec{URL: m.RepoURL, Interval: fluxInterval},
		}
		chartSpec.Chart = m.Chart
		chartSpec.Version = m.Version
		chartSpec.SourceRef = ModuleSourceRef{Kind: sourceKindHelmRepo, Name: repoName, Namespace: sourceNS}
	default:
		return PlannedModule{}, fmt.Errorf("unknown source %q", m.Source)
	}

	hr := ModuleHelmRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: ModuleMeta{
			Name:      name,
			Namespace: sourceNS,
			Labels:    labels,
		},
		Spec: ModuleReleaseSpec{
			Interval:         fluxInterval,
			ReleaseName:      name,
			TargetNamespace:  ns,
			StorageNamespace: ns,
			Install: ModuleInstall{
				CreateNamespace: true,
				DisableWait:     m.DisableWait,
				Remediation:     &ModuleRemediation{Retries: remediationRetries},
			},
			Upgrade: ModuleUpgrade{
				DisableWait: m.DisableWait,
				Remediation: &ModuleRemediation{Retries: remediationRetries},
			},
			Chart:  ModuleChart{Spec: chartSpec},
			Values: nilIfEmpty(m.Values),
		},
	}

	return PlannedModule{
		Name:        name,
		Namespace:   ns,
		Source:      m.Source,
		Chart:       m.Chart,
		Version:     m.Version,
		HelmRelease: hr,
		Repository:  repo,
	}, nil
}

func nilIfEmpty(v map[string]any) map[string]any {
	if len(v) == 0 {
		return nil
	}
	return v
}

// helmRepoName derives a stable HelmRepository name from the chart repo URL host
// (e.g. https://kedacore.github.io/charts -> "kedacore"), falling back to the
// module name. Two modules from the same Helm repo therefore share one
// HelmRepository name (apply-idempotent).
func helmRepoName(repoURL, fallback string) string {
	u := repoURL
	for _, p := range []string{"https://", "http://", "oci://"} {
		u = trimPrefix(u, p)
	}
	if i := indexByte(u, '/'); i >= 0 {
		u = u[:i]
	}
	host := u
	if dot := indexByte(host, '.'); dot >= 0 {
		host = host[:dot]
	}
	host = sanitize(host)
	if host == "" {
		return sanitize(fallback)
	}
	return host
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func indexByte(s string, b byte) int {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return i
		}
	}
	return -1
}

// sanitize reduces s to a valid RFC1123 DNS label fragment (lowercase
// [a-z0-9-]). Mirrors appconfig.SanitizeDNSLabel without the import.
func sanitize(s string) string {
	out := make([]byte, 0, len(s))
	prevDash := false
	for i := 0; i < len(s); i++ {
		r := s[i]
		switch {
		case r >= 'A' && r <= 'Z':
			out = append(out, r+('a'-'A'))
			prevDash = false
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			out = append(out, r)
			prevDash = false
		default:
			if !prevDash && len(out) > 0 {
				out = append(out, '-')
				prevDash = true
			}
		}
	}
	for len(out) > 0 && out[len(out)-1] == '-' {
		out = out[:len(out)-1]
	}
	return string(out)
}

// CheckPolicy runs the platform guardrails over one planned module BEFORE it is
// written or applied. It enforces, in increasing depth:
//
//  1. inline Helm values must not request a LoadBalancer service type
//     (policy.CheckModuleValues) — the realistic leak surface for a chartRepo
//     module whose chart we cannot template offline;
//  2. the serialized Flux manifests (HelmRelease + any HelmRepository) must not
//     carry a LoadBalancer Service (policy.CheckRenderedManifest) — last-line
//     scan over the YAML;
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
	if len(p.HelmRelease.Spec.Values) > 0 {
		vs = append(vs, policy.CheckModuleValues(p.Name, p.HelmRelease.Spec.Values)...)
	}

	// (2) serialized Flux manifests scan.
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
					p.Name, chartDir, p.Namespace, p.HelmRelease.Spec.Values)
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

// YAML serializes a planned module's Flux desired state with a generated-file
// header: the HelmRepository (chartRepo only) first, then the HelmRelease.
func (p PlannedModule) YAML(env string) ([]byte, error) {
	header := fmt.Sprintf(
		"# Generated by platformctl. DO NOT EDIT BY HAND.\n"+
			"# Infra module %q for env=%s (source=%s).\n",
		p.Name, env, p.Source,
	)
	out := []byte(header)
	if p.Repository != nil {
		repoBody, err := yaml.Marshal(p.Repository)
		if err != nil {
			return nil, fmt.Errorf("marshal module %q repository: %w", p.Name, err)
		}
		out = append(out, repoBody...)
		out = append(out, []byte("---\n")...)
	}
	body, err := yaml.Marshal(p.HelmRelease)
	if err != nil {
		return nil, fmt.Errorf("marshal module %q: %w", p.Name, err)
	}
	out = append(out, body...)
	return out, nil
}

// OutputPath is environments/<env>/infra/<module>.yaml under root.
func OutputPath(root, env, module string) string {
	return filepath.Join(root, "environments", env, "infra", module+".yaml")
}

// Write writes a planned module's Flux desired state to its canonical path under
// root.
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
