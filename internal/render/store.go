package render

import (
	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

// devPostgresChartPath is the in-repo path to the per-app dev Postgres chart.
const devPostgresChartPath = "charts/infra/dev-postgres"

// StoreApplication is one rendered per-app data-store Argo CD Application (its own
// namespace, its own chart). It pairs the Application with the canonical output
// filename stem so the caller writes it to environments/<env>/apps/<file>.yaml.
type StoreApplication struct {
	// FileStem is the filename stem under environments/<env>/apps/ (no .yaml),
	// e.g. "carshowdb-postgres". The root app-of-apps directory-recurses
	// environments/<env>, so any file dropped there is picked up.
	FileStem string
	// Tool is the store engine/purpose ("postgres", "redis"), for the plan summary.
	Tool string
	// Namespace is the dedicated store namespace <app>-<env>-<tool>.
	Namespace string
	Application Application
}

// BuildStoreApplications renders the dedicated per-app data-store Argo CD
// Applications for an app in an env. Today this is the DEV per-app Postgres: when
// the env's backend is local (dev) and the app declares one or more db: postgres
// stores, each gets its OWN dev-postgres Application (source charts/infra/dev-postgres)
// into its own namespace <app>-<env>-postgres, with the per-app database name, a
// DEV PLACEHOLDER password, the optiplex-pg node pin, and CreateNamespace=true.
//
// In prod (backend=ssm) NO per-app Postgres is provisioned — the DB is
// external/managed and DATABASE_URL comes from SSM — so this returns nil.
// Redis dev provisioning lands here too once a dev-redis chart exists.
func BuildStoreApplications(app appconfig.App, env string, c *clusterenv.Config) []StoreApplication {
	if !isLocalBackend(c) {
		return nil
	}
	var out []StoreApplication

	// Per-app dev Postgres for each declared postgres db. The first/default db
	// owns the bare <app>-postgres name/namespace; a second postgres store
	// disambiguates with its store name (<app>-postgres-<name>).
	pgSeen := 0
	for _, s := range app.DB {
		if s.Type != appconfig.DefaultDBType { // only postgres provisions a dev chart today
			continue
		}
		secondary := pgSeen > 0
		pgSeen++
		out = append(out, buildDevPostgresApp(app, env, c, s.Name, secondary))
	}
	return out
}

// buildDevPostgresApp renders one dedicated dev-postgres Argo CD Application for
// the app's declared postgres store. release/service/namespace are derived from
// the app (and the store name for a secondary store); the inline Helm values pin
// the per-app database, the dev placeholder password, the optiplex-pg node, and
// the local-path PVC — mirroring what clusterenv.DevDatabaseURL assumes so the
// app's rendered DATABASE_URL points at exactly what this chart provisions.
func buildDevPostgresApp(app appconfig.App, env string, c *clusterenv.Config, storeName string, secondary bool) StoreApplication {
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

	release := clusterenv.DevPostgresReleaseName(app.App)
	ns := app.StoreNamespace(env, clusterenv.DevPostgresTool, storeName, secondary)
	fileStem := release
	if secondary && storeName != "" {
		release = release + "-" + appconfig.SanitizeDNSLabel(storeName)
		fileStem = release
	}

	labels := storeLabels(app.App, env, clusterenv.DevPostgresTool)
	values := map[string]any{
		"auth": map[string]any{
			"database": clusterenv.DevPostgresDatabase(app.App),
			"username": clusterenv.DevPostgresDefaultUser,
			"password": clusterenv.DevPostgresDefaultPassword,
		},
		"service": map[string]any{"port": 5432},
		// Pin to the homelab baseline node so the local-path PVC reschedules onto
		// the box that physically holds the data volume.
		"persistence": map[string]any{
			"enabled":      true,
			"storageClass": "local-path",
			"size":         "8Gi",
		},
	}
	// Pin Postgres to a baseline node only when one is configured. In dev the pin is
	// empty so the scheduler can place it on any node that can pull + run the image
	// (the homelab's bare-metal node currently can't reach external registries).
	if clusterenv.DevPostgresNode != "" {
		values["nodeSelector"] = map[string]any{"kubernetes.io/hostname": clusterenv.DevPostgresNode}
	}

	appl := Application{
		APIVersion: argoAPIVersion,
		Kind:       argoKind,
		Metadata: ArgoMetadata{
			Name:      release,
			Namespace: argoNS,
			Labels:    labels,
		},
		Spec: ApplicationSpec{
			Project: argoProject,
			Source: ApplicationSrc{
				RepoURL:        repoURL,
				Path:           devPostgresChartPath,
				TargetRevision: targetRev,
				Helm: &HelmSrc{
					ReleaseName:  release,
					ValuesObject: values,
				},
			},
			Destination: ApplicationDest{
				Server:    argoDestServer,
				Namespace: ns,
			},
			SyncPolicy: createNamespaceSyncPolicy(namespaceLabels(app.App, env, clusterenv.DevPostgresTool)),
		},
	}

	return StoreApplication{
		FileStem:    fileStem,
		Tool:        clusterenv.DevPostgresTool,
		Namespace:   ns,
		Application: appl,
	}
}

// storeLabels is the label set stamped on a per-app store Application. It mirrors
// the app label set but marks the purpose as the store tool (component=<tool>).
func storeLabels(app, env, tool string) map[string]string {
	return map[string]string{
		"app.kubernetes.io/name":     app + "-" + tool,
		"app.kubernetes.io/instance": app + "-" + tool,
		"platform/app":               app,
		"platform/env":               env,
		"platform/component":         tool,
		"platform/managed-by":        "platformctl",
	}
}
