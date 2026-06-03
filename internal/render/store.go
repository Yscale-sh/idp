package render

import (
	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
)

// devPostgresChartPath is the in-repo path to the per-app dev Postgres chart,
// referenced by the GitRepository source.
const devPostgresChartPath = "./charts/infra/dev-postgres"

// StoreRelease is one rendered per-app data-store Flux HelmRelease (its own
// namespace, its own chart). It pairs the HelmRelease with the canonical output
// filename stem so the caller writes it to environments/<env>/apps/<file>.yaml.
type StoreRelease struct {
	// FileStem is the filename stem under environments/<env>/apps/ (no .yaml),
	// e.g. "carshowdb-postgres". The FluxInstance Kustomization applies the whole
	// environments/<env> tree, so any file dropped there is picked up.
	FileStem string
	// Tool is the store engine/purpose ("postgres", "redis"), for the plan summary.
	Tool string
	// Namespace is the dedicated store namespace <app>-<env>-<tool>.
	Namespace string
	HelmRelease HelmRelease
}

// BuildStoreReleases renders the dedicated per-app data-store Flux HelmReleases
// for an app in an env. Today this is the DEV per-app Postgres: when the env's
// backend is local (dev) and the app declares one or more db: postgres stores,
// each gets its OWN dev-postgres HelmRelease (chart ./charts/infra/dev-postgres)
// with targetNamespace <app>-<env>-postgres, the per-app database name, a DEV
// PLACEHOLDER password, the optiplex-pg node pin, and install.createNamespace=true.
//
// In prod (backend=ssm) NO per-app Postgres is provisioned — the DB is
// external/managed and DATABASE_URL comes from SSM — so this returns nil.
// Redis dev provisioning lands here too once a dev-redis chart exists.
func BuildStoreReleases(app appconfig.App, env string, c *clusterenv.Config) []StoreRelease {
	if !isLocalBackend(c) {
		return nil
	}
	var out []StoreRelease

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
		out = append(out, buildDevPostgresRelease(app, env, c, s.Name, secondary))
	}
	return out
}

// buildDevPostgresRelease renders one dedicated dev-postgres Flux HelmRelease for
// the app's declared postgres store. release/service/namespace are derived from
// the app (and the store name for a secondary store); the inline Helm values pin
// the per-app database, the dev placeholder password, the optiplex-pg node, and
// the local-path PVC — mirroring what clusterenv.DevDatabaseURL assumes so the
// app's rendered DATABASE_URL points at exactly what this chart provisions.
func buildDevPostgresRelease(app appconfig.App, env string, c *clusterenv.Config, storeName string, secondary bool) StoreRelease {
	sourceName, sourceNS := fluxSource(c)

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

	hr := HelmRelease{
		APIVersion: helmReleaseAPIVersion,
		Kind:       helmReleaseKind,
		Metadata: FluxMetadata{
			Name:      release,
			Namespace: sourceNS,
			Labels:    labels,
		},
		Spec: HelmReleaseSpec{
			Interval:         fluxInterval,
			ReleaseName:      release,
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
					Chart: devPostgresChartPath,
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

	return StoreRelease{
		FileStem:    fileStem,
		Tool:        clusterenv.DevPostgresTool,
		Namespace:   ns,
		HelmRelease: hr,
	}
}

// storeLabels is the label set stamped on a per-app store HelmRelease. It mirrors
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
