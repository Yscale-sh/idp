package render

import (
	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

// HelmRelease is the subset of helm.toolkit.fluxcd.io/v2 HelmRelease we render.
// It is a typed shape so the output is stable and the contract is explicit; it
// serializes (via sigs.k8s.io/yaml) to the manifest Flux reconciles. It replaces
// the old argoproj.io/v1alpha1 Application: the chart values map content is the
// SAME (carried under spec.values instead of spec.source.helm.valuesObject) —
// only the wrapper object changed.
type HelmRelease struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   FluxMetadata    `json:"metadata"`
	Spec       HelmReleaseSpec `json:"spec"`
}

type FluxMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type HelmReleaseSpec struct {
	Interval string `json:"interval"`
	// ReleaseName is the Helm release name (= <app>).
	ReleaseName string `json:"releaseName,omitempty"`
	// TargetNamespace is where the chart's resources land: the app's OWN
	// namespace <app>-<env>-<purpose>.
	TargetNamespace string `json:"targetNamespace"`
	// StorageNamespace is where Flux stores the Helm release history Secret.
	StorageNamespace string         `json:"storageNamespace"`
	Install          InstallSpec    `json:"install"`
	Upgrade          UpgradeSpec    `json:"upgrade"`
	Chart            ChartTemplate  `json:"chart"`
	Values           any            `json:"values,omitempty"`
}

// InstallSpec sets createNamespace=true (replaces ArgoCD CreateNamespace=true)
// plus install remediation retries.
type InstallSpec struct {
	CreateNamespace bool             `json:"createNamespace"`
	Remediation     *RemediationSpec `json:"remediation,omitempty"`
}

type UpgradeSpec struct {
	Remediation *RemediationSpec `json:"remediation,omitempty"`
}

type RemediationSpec struct {
	Retries int `json:"retries"`
}

type ChartTemplate struct {
	Spec ChartSpec `json:"spec"`
}

// ChartSpec points the HelmRelease at a chart in a source. For an in-repo chart
// the sourceRef is the GitRepository the Flux bootstrap provides (cross-namespace
// in flux-system) and chart is the in-repo path; reconcileStrategy Revision so a
// new commit re-renders the chart.
type ChartSpec struct {
	Chart             string    `json:"chart"`
	Version           string    `json:"version,omitempty"`
	SourceRef         SourceRef `json:"sourceRef"`
	ReconcileStrategy string    `json:"reconcileStrategy,omitempty"`
}

// SourceRef is a cross-namespace reference to a Flux source (GitRepository or
// HelmRepository) in the flux-system namespace.
type SourceRef struct {
	Kind      string `json:"kind"`
	Name      string `json:"name"`
	Namespace string `json:"namespace"`
}

// Flux defaults / API shapes.
const (
	helmReleaseAPIVersion = "helm.toolkit.fluxcd.io/v2"
	helmReleaseKind       = "HelmRelease"
	fluxInterval          = "10m"
	fluxChartPath         = "./charts/app"
	sourceKindGitRepo     = "GitRepository"
	reconcileRevision     = "Revision"
	remediationRetries    = 3
)

// fluxSource resolves the per-env Flux source coordinates (the GitRepository the
// HelmReleases reference cross-namespace) from the cluster config, with defaults
// when c is nil.
func fluxSource(c *clusterenv.Config) (sourceName, sourceNS string) {
	sourceName = clusterenv.DefaultFluxSourceName
	sourceNS = clusterenv.DefaultFluxNamespace
	if c != nil {
		if c.Flux.SourceName != "" {
			sourceName = c.Flux.SourceName
		}
		if c.Flux.Namespace != "" {
			sourceNS = c.Flux.Namespace
		}
	}
	return sourceName, sourceNS
}

// BuildHelmRelease renders the Flux HelmRelease for an app in an env. The chart
// points at ./charts/app via the GitRepository source (cross-namespace in
// flux-system) with the rendered values inline; targetNamespace/storageNamespace
// are the app's OWN namespace <app>-<env>-<purpose>; install.createNamespace=true
// so reconciling creates the namespace (replacing ArgoCD CreateNamespace=true);
// install/upgrade remediation retries 3. The platform label set is stamped on the
// HelmRelease itself.
func BuildHelmRelease(app appconfig.App, env string, c *clusterenv.Config, values Values) HelmRelease {
	sourceName, sourceNS := fluxSource(c)
	ns := app.Namespace(env)
	return HelmRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: FluxMetadata{
			Name:      app.ReleaseName(),
			Namespace: sourceNS,
			Labels:    app.Labels(env),
		},
		Spec: HelmReleaseSpec{
			Interval:         fluxInterval,
			ReleaseName:      app.ReleaseName(),
			TargetNamespace:  ns,
			StorageNamespace: ns,
			Install: InstallSpec{
				CreateNamespace: true,
				Remediation:     &RemediationSpec{Retries: remediationRetries},
			},
			Upgrade: UpgradeSpec{
				Remediation: &RemediationSpec{Retries: remediationRetries},
			},
			Chart: ChartTemplate{
				Spec: ChartSpec{
					Chart: fluxChartPath,
					SourceRef: SourceRef{
						Kind:      sourceKindGitRepo,
						Name:      sourceName,
						Namespace: sourceNS,
					},
					ReconcileStrategy: reconcileRevision,
				},
			},
			Values: values,
		},
	}
}
