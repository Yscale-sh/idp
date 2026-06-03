package render

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jakenesler/platformctl/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

// This file implements the "umbrella" delivery model: the rendered desired state
// for an environment is ONE Flux HelmRelease (clusters/<env>/platform.yaml) that
// installs the charts/cluster umbrella chart. That chart templates one isolated
// HelmRelease per app (+ its dedicated Postgres) and per enabled infra module,
// from the inline values maintained here. `platformctl render` upserts an app
// entry; `platformctl infra render` sets the module list. Flux installs the one
// umbrella; the helm-controller reconciles each inner release into its own ns.

// PlatformSource is the GitRepository the umbrella + inner HelmReleases pull their
// charts from (the source the Flux bootstrap provides, in flux-system).
type PlatformSource struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// PostgresEntry is an app's dedicated dev Postgres store; the umbrella chart
// renders a dev-postgres HelmRelease into its own namespace from it.
type PostgresEntry struct {
	Enabled     bool   `json:"enabled"`
	Namespace   string `json:"namespace"`
	ReleaseName string `json:"releaseName"`
	Values      any    `json:"values,omitempty"`
}

// AppEntry is one app in the umbrella values. The umbrella chart templates an app
// HelmRelease (chart ./charts/app, targetNamespace = Namespace) and, when present,
// its Postgres HelmRelease, from this entry.
type AppEntry struct {
	Name        string         `json:"name"`
	Namespace   string         `json:"namespace"`
	ReleaseName string         `json:"releaseName"`
	Component   string         `json:"component,omitempty"`
	Values      any            `json:"values"`
	Postgres    *PostgresEntry `json:"postgres,omitempty"`
}

// ModuleEntry is one enabled infra module in the umbrella values. The umbrella
// chart renders a HelmRelease (+ a HelmRepository for chartRepo modules) from it.
type ModuleEntry struct {
	Name      string         `json:"name"`
	Namespace string         `json:"namespace"`
	Source    string         `json:"source"` // localChart | chartRepo
	Chart     string         `json:"chart"`
	RepoURL   string         `json:"repoURL,omitempty"`
	Version   string         `json:"version,omitempty"`
	Values    map[string]any `json:"values,omitempty"`
}

// ClusterValues is the charts/cluster umbrella chart's values: the whole env.
type ClusterValues struct {
	Env     string         `json:"env"`
	Source  PlatformSource `json:"source"`
	Apps    []AppEntry     `json:"apps"`
	Modules []ModuleEntry  `json:"modules"`
}

// PlatformRelease is the single umbrella HelmRelease (clusters/<env>/platform.yaml).
// It is typed (Spec.Values is ClusterValues, not any) so render's read/modify/
// write upsert round-trips cleanly.
type PlatformRelease struct {
	APIVersion string       `json:"apiVersion"`
	Kind       string       `json:"kind"`
	Metadata   FluxMetadata `json:"metadata"`
	Spec       PlatformSpec `json:"spec"`
}

type PlatformSpec struct {
	Interval         string         `json:"interval"`
	ReleaseName      string         `json:"releaseName"`
	TargetNamespace  string         `json:"targetNamespace"`
	StorageNamespace string         `json:"storageNamespace"`
	Install          InstallSpec    `json:"install"`
	Upgrade          UpgradeSpec    `json:"upgrade"`
	Chart            ChartTemplate  `json:"chart"`
	Values           ClusterValues `json:"values"`
}

const (
	clusterChartPath = "./charts/cluster"
	platformRelease  = "platform"
)

// PlatformPath is clusters/<env>/platform.yaml under root — the one rendered file.
func PlatformPath(root, env string) string {
	return filepath.Join(root, "clusters", env, "platform.yaml")
}

// newPlatformRelease builds an empty umbrella HelmRelease for an env.
func newPlatformRelease(env string, c *clusterenv.Config) *PlatformRelease {
	srcName, srcNS := fluxSource(c)
	return &PlatformRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: FluxMetadata{
			Name:      platformRelease,
			Namespace: srcNS,
			Labels: map[string]string{
				"platform/env":        env,
				"platform/role":       "umbrella",
				"platform/managed-by": "platformctl",
			},
		},
		Spec: PlatformSpec{
			Interval:         fluxInterval,
			ReleaseName:      platformRelease,
			TargetNamespace:  srcNS,
			StorageNamespace: srcNS,
			Install:          InstallSpec{CreateNamespace: true, Remediation: &RemediationSpec{Retries: remediationRetries}},
			Upgrade:          UpgradeSpec{Remediation: &RemediationSpec{Retries: remediationRetries}},
			Chart: ChartTemplate{Spec: ChartSpec{
				Chart:             clusterChartPath,
				SourceRef:         SourceRef{Kind: sourceKindGitRepo, Name: srcName, Namespace: srcNS},
				ReconcileStrategy: reconcileRevision,
			}},
			Values: ClusterValues{
				Env:     env,
				Source:  PlatformSource{Name: srcName, Namespace: srcNS},
				Apps:    []AppEntry{},
				Modules: []ModuleEntry{},
			},
		},
	}
}

// ReadPlatform reads clusters/<env>/platform.yaml if present; otherwise returns a
// fresh empty umbrella HelmRelease so the first render starts clean.
func ReadPlatform(root, env string, c *clusterenv.Config) (*PlatformRelease, error) {
	p := PlatformPath(root, env)
	data, err := os.ReadFile(p)
	if os.IsNotExist(err) {
		return newPlatformRelease(env, c), nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", p, err)
	}
	var pr PlatformRelease
	if err := yaml.Unmarshal(data, &pr); err != nil {
		return nil, fmt.Errorf("parse %s: %w", p, err)
	}
	if pr.Spec.Values.Apps == nil {
		pr.Spec.Values.Apps = []AppEntry{}
	}
	if pr.Spec.Values.Modules == nil {
		pr.Spec.Values.Modules = []ModuleEntry{}
	}
	return &pr, nil
}

func platformHeader(env string) string {
	return fmt.Sprintf(
		"# Generated by platformctl. DO NOT EDIT BY HAND.\n"+
			"# Umbrella HelmRelease for env=%s: Flux installs charts/cluster, which templates\n"+
			"# one isolated HelmRelease per app (+ its Postgres) and per enabled module.\n"+
			"# Append apps with `platformctl render`; set modules with `platformctl infra render`.\n",
		env,
	)
}

// WritePlatform marshals the umbrella HelmRelease to clusters/<env>/platform.yaml.
func WritePlatform(root, env string, pr *PlatformRelease) (string, error) {
	p := PlatformPath(root, env)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", fmt.Errorf("create clusters dir: %w", err)
	}
	body, err := yaml.Marshal(pr)
	if err != nil {
		return "", fmt.Errorf("marshal platform: %w", err)
	}
	out := append([]byte(platformHeader(env)), body...)
	if err := os.WriteFile(p, out, 0o644); err != nil {
		return "", fmt.Errorf("write %s: %w", p, err)
	}
	return p, nil
}

// ToAppEntry builds the umbrella app entry from a rendered Result: the app-chart
// values plus its dedicated Postgres store (if the app declared db: postgres in a
// local-backend env). The value maps are reused verbatim from Render.
func (r *Result) ToAppEntry() AppEntry {
	e := AppEntry{
		Name:        r.App.App,
		Namespace:   r.App.Namespace(r.Env),
		ReleaseName: r.App.ReleaseName(),
		Component:   r.App.Purpose(),
		Values:      r.Values,
	}
	for _, s := range r.StoreReleases {
		if s.Tool == clusterenv.DevPostgresTool {
			e.Postgres = &PostgresEntry{
				Enabled:     true,
				Namespace:   s.Namespace,
				ReleaseName: s.HelmRelease.Spec.ReleaseName,
				Values:      s.HelmRelease.Spec.Values,
			}
			break
		}
	}
	return e
}

// UpsertApp inserts/replaces this Result's app in clusters/<env>/platform.yaml
// (keyed by app name, kept sorted for stable diffs) and writes the file back.
// Returns the path written.
func (r *Result) UpsertApp(root string, c *clusterenv.Config) (string, error) {
	pr, err := ReadPlatform(root, r.Env, c)
	if err != nil {
		return "", err
	}
	entry := r.ToAppEntry()
	apps := append([]AppEntry(nil), pr.Spec.Values.Apps...)
	replaced := false
	for i := range apps {
		if apps[i].Name == entry.Name {
			apps[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		apps = append(apps, entry)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].Name < apps[j].Name })
	pr.Spec.Values.Apps = apps
	return WritePlatform(root, r.Env, pr)
}

// SetModules replaces the modules list in clusters/<env>/platform.yaml and writes
// the file back. Modules are fully re-derived from cluster.yaml on each
// `infra render`, so this is a set (not an upsert).
func SetModules(root, env string, c *clusterenv.Config, mods []ModuleEntry) (string, error) {
	pr, err := ReadPlatform(root, env, c)
	if err != nil {
		return "", err
	}
	if mods == nil {
		mods = []ModuleEntry{}
	}
	sort.Slice(mods, func(i, j int) bool { return mods[i].Name < mods[j].Name })
	pr.Spec.Values.Modules = mods
	return WritePlatform(root, env, pr)
}
