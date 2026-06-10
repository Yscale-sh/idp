package render

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
	"sigs.k8s.io/yaml"
)

// This file implements the "umbrella" delivery model: the rendered desired state
// for an environment is ONE Flux HelmRelease (clusters/<env>/platform.yaml) that
// installs the charts/cluster umbrella chart. That chart templates one isolated
// HelmRelease per app (+ its dedicated Postgres) and per enabled infra module,
// from the inline values maintained here. `idpctl render` upserts an app
// entry; `idpctl infra render` sets the module list. Flux installs the one
// umbrella; the helm-controller reconciles each inner release into its own ns.

// PlatformSource is the GitRepository the umbrella + inner HelmReleases pull their
// charts from (the source the Flux bootstrap provides, in flux-system).
type PlatformSource struct {
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// StoreEntry is one of an app's dedicated dev data stores (Postgres, Redis, ...);
// the umbrella chart renders a HelmRelease (from .chart) into .namespace from it.
type StoreEntry struct {
	Tool        string `json:"tool"`
	Namespace   string `json:"namespace"`
	ReleaseName string `json:"releaseName"`
	Chart       string `json:"chart"`
	Values      any    `json:"values,omitempty"`
}

// PostgresEntry is the LEGACY single-Postgres shape an older idpctl wrote. New
// renders use Stores[] instead; this type is retained ONLY so reading and
// re-writing a platform.yaml that still has a .postgres entry preserves it
// (without it, the unmarshal would silently drop the field and the next render
// would prune that app's Postgres). The cluster chart renders it for back-compat.
type PostgresEntry struct {
	Enabled     bool   `json:"enabled"`
	Namespace   string `json:"namespace"`
	ReleaseName string `json:"releaseName"`
	Values      any    `json:"values,omitempty"`
}

// AppEntry is one WORKLOAD in the umbrella values (a single app, or one component
// of a multi-component app). ReleaseName is the unique workload handle
// (<app>-<component> or <app>) — the umbrella key — so sibling components never
// collide. The umbrella chart templates an app HelmRelease (chart ./charts/app,
// targetNamespace = Namespace) plus a HelmRelease per dedicated data store.
type AppEntry struct {
	Name        string       `json:"name"`
	Namespace   string       `json:"namespace"`
	ReleaseName string       `json:"releaseName"`
	Component   string       `json:"component,omitempty"`
	Values      any          `json:"values"`
	Stores      []StoreEntry `json:"stores,omitempty"`
	// Postgres is the LEGACY single-store field. New renders leave it nil and use
	// Stores; it is kept so an old entry round-trips (read+write) without losing its
	// Postgres. Migrate an entry by re-rendering its app (moves it into Stores).
	Postgres *PostgresEntry `json:"postgres,omitempty"`
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

	// DisableWait turns off the Helm readiness wait on the module's
	// install/upgrade (see clusterenv.Module.DisableWait — e.g. a PVC on a
	// WaitForFirstConsumer StorageClass that only binds on first use).
	DisableWait bool `json:"disableWait,omitempty"`
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
			Install:          InstallSpec{CreateNamespace: true, DisableWait: true, Remediation: &RemediationSpec{Retries: remediationRetries}},
			Upgrade:          UpgradeSpec{DisableWait: true, Remediation: &RemediationSpec{Retries: remediationRetries}},
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
		"# Generated by idpctl. DO NOT EDIT BY HAND.\n"+
			"# Umbrella HelmRelease for env=%s: Flux installs charts/cluster, which templates\n"+
			"# one isolated HelmRelease per app (+ its Postgres) and per enabled module.\n"+
			"# Append apps with `idpctl render`; set modules with `idpctl infra render`.\n",
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
		e.Stores = append(e.Stores, StoreEntry{
			Tool:        s.Tool,
			Namespace:   s.Namespace,
			ReleaseName: s.HelmRelease.Spec.ReleaseName,
			Chart:       s.HelmRelease.Spec.Chart.Spec.Chart,
			Values:      s.HelmRelease.Spec.Values,
		})
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
	// Key by the unique workload handle (ReleaseName) so two components of one app
	// (e.g. dim-api / dim-scanner) coexist instead of overwriting each other.
	for i := range apps {
		if apps[i].ReleaseName == entry.ReleaseName {
			apps[i] = entry
			replaced = true
			break
		}
	}
	if !replaced {
		apps = append(apps, entry)
	}
	sort.Slice(apps, func(i, j int) bool { return apps[i].ReleaseName < apps[j].ReleaseName })
	pr.Spec.Values.Apps = apps
	return WritePlatform(root, r.Env, pr)
}

// RemoveApp deletes a workload from clusters/<env>/platform.yaml and writes the
// file back, so the umbrella re-renders WITHOUT it and Flux prunes the workload's
// HelmRelease, its dedicated data stores, and their namespaces. It is the inverse
// of UpsertApp — the teardown path (a git commit, not `kubectl delete`).
//
// When component is set, ONLY that component (handle <app>-<component>) is removed;
// when empty, EVERY component of the app is removed (tear down the whole app).
// Returns the path written and whether anything was present (false => no-op).
func RemoveApp(root, env, appName, component string, c *clusterenv.Config) (path string, removed bool, err error) {
	pr, err := ReadPlatform(root, env, c)
	if err != nil {
		return "", false, err
	}
	handle := appName
	if component != "" {
		handle = appconfig.SanitizeDNSLabel(appName + "-" + component)
	}
	kept := make([]AppEntry, 0, len(pr.Spec.Values.Apps))
	for _, a := range pr.Spec.Values.Apps {
		match := a.Name == appName // whole-app teardown
		if component != "" {
			match = a.ReleaseName == handle // single component
		}
		if match {
			removed = true
			continue
		}
		kept = append(kept, a)
	}
	if !removed {
		return PlatformPath(root, env), false, nil
	}
	pr.Spec.Values.Apps = kept
	path, err = WritePlatform(root, env, pr)
	return path, true, err
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
