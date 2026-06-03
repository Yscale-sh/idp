package render

import (
	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

// Application is the subset of argoproj.io/v1alpha1 Application we render. It is
// a typed shape so the output is stable and the contract is explicit; it
// serializes (via sigs.k8s.io/yaml) to the manifest Argo CD reconciles.
type Application struct {
	APIVersion string          `json:"apiVersion"`
	Kind       string          `json:"kind"`
	Metadata   ArgoMetadata    `json:"metadata"`
	Spec       ApplicationSpec `json:"spec"`
}

type ArgoMetadata struct {
	Name      string            `json:"name"`
	Namespace string            `json:"namespace"`
	Labels    map[string]string `json:"labels,omitempty"`
}

type ApplicationSpec struct {
	Project     string          `json:"project"`
	Source      ApplicationSrc  `json:"source"`
	Destination ApplicationDest `json:"destination"`
	SyncPolicy  SyncPolicy      `json:"syncPolicy"`
}

// ManagedNamespaceMetadata is the labels Argo stamps onto the namespace it
// creates (when CreateNamespace=true). The platform stamps its inventory labels
// so the created namespace is discoverable/cleanable like every other object.
type ManagedNamespaceMetadata struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type ApplicationSrc struct {
	RepoURL        string   `json:"repoURL"`
	Path           string   `json:"path,omitempty"`
	Chart          string   `json:"chart,omitempty"`
	TargetRevision string   `json:"targetRevision"`
	Helm           *HelmSrc `json:"helm,omitempty"`
}

// HelmSrc carries the rendered values inline (valuesObject), so the entire
// desired state lives in this one file (no sibling values file to keep in sync).
type HelmSrc struct {
	ReleaseName  string `json:"releaseName,omitempty"`
	ValuesObject any    `json:"valuesObject,omitempty"`
}

type ApplicationDest struct {
	Server    string `json:"server,omitempty"`
	Namespace string `json:"namespace"`
}

type SyncPolicy struct {
	Automated                *SyncAutomated            `json:"automated,omitempty"`
	SyncOptions              []string                  `json:"syncOptions,omitempty"`
	ManagedNamespaceMetadata *ManagedNamespaceMetadata `json:"managedNamespaceMetadata,omitempty"`
}

type SyncAutomated struct {
	Prune    bool `json:"prune"`
	SelfHeal bool `json:"selfHeal"`
}

// Argo CD defaults.
const (
	argoAPIVersion        = "argoproj.io/v1alpha1"
	argoKind              = "Application"
	argoProject           = "default"
	argoChartPath         = "charts/app"
	argoDestServer        = "https://kubernetes.default.svc"
	createNamespaceOption = "CreateNamespace=true"
)

// BuildApplication renders the Argo CD Application for an app in an env. The
// source points at charts/app in THIS platform repo with the rendered values
// inline; destination namespace is the app's OWN namespace <app>-<env>-<purpose>;
// syncPolicy is automated + prune + selfHeal with CreateNamespace=true so applying
// the YAML creates the namespace, plus managedNamespaceMetadata that stamps the
// platform inventory labels onto the created namespace. The platform label set is
// stamped on the Application itself too.
func BuildApplication(app appconfig.App, env string, c *clusterenv.Config, values Values) Application {
	repoURL := ""
	targetRev := clusterenv.DefaultTargetRevision
	argoNS := clusterenv.DefaultArgoNamespace
	if c != nil {
		repoURL = c.Argo.RepoURL
		if c.Argo.TargetRevision != "" {
			targetRev = c.Argo.TargetRevision
		}
		if c.Argo.Namespace != "" {
			argoNS = c.Argo.Namespace
		}
	}

	ns := app.Namespace(env)
	return Application{
		APIVersion: argoAPIVersion,
		Kind:       argoKind,
		Metadata: ArgoMetadata{
			Name:      app.ArgoAppName(),
			Namespace: argoNS,
			Labels:    app.Labels(env),
		},
		Spec: ApplicationSpec{
			Project: argoProject,
			Source: ApplicationSrc{
				RepoURL:        repoURL,
				Path:           argoChartPath,
				TargetRevision: targetRev,
				Helm: &HelmSrc{
					ReleaseName:  app.ReleaseName(),
					ValuesObject: values,
				},
			},
			Destination: ApplicationDest{
				Server:    argoDestServer,
				Namespace: ns,
			},
			SyncPolicy: createNamespaceSyncPolicy(namespaceLabels(app.App, env, app.Purpose())),
		},
	}
}

// namespaceLabels is the platform inventory label set stamped onto a
// platformctl-created namespace (via managedNamespaceMetadata): app, env,
// purpose, and managed-by.
func namespaceLabels(app, env, purpose string) map[string]string {
	return map[string]string{
		"platform/app":        app,
		"platform/env":        env,
		"platform/purpose":    purpose,
		"platform/managed-by": "platformctl",
	}
}

// createNamespaceSyncPolicy is the shared sync policy for every create-from-yaml
// Application: automated prune+selfHeal, CreateNamespace=true, and the platform
// labels on the namespace Argo creates.
func createNamespaceSyncPolicy(nsLabels map[string]string) SyncPolicy {
	return SyncPolicy{
		Automated:                &SyncAutomated{Prune: true, SelfHeal: true},
		SyncOptions:              []string{createNamespaceOption},
		ManagedNamespaceMetadata: &ManagedNamespaceMetadata{Labels: nsLabels},
	}
}
