package render

import (
	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
	"github.com/jakenesler/platformctl/internal/secrets"
)

// BuildExternalSecret renders the values.externalSecret block from the env's
// secrets plan. dataFrom pulls the whole app SSM root into the runtime Secret;
// remoteRefs pin shared-group keys. Enabled only when the app actually needs
// secrets (any db/cache/storage/connectsTo-serviceToken, or an explicit shared
// reference). Apps with zero secret needs render externalSecret.enabled=false.
func BuildExternalSecret(app appconfig.App, env string, c *clusterenv.Config) ExternalSecretValues {
	spec := secrets.Plan(app, env, c)

	// An app needs a runtime Secret when it has any secret-backed capability OR,
	// on the local (dev) backend, whenever it gets dev placeholders so it can boot
	// operator-free. The chart renders a plain Secret (local) or an ExternalSecret
	// (ssm) accordingly, and the Deployment envFroms it.
	needsSecret := len(app.DB) > 0 || len(app.Cache) > 0 || len(app.Storage) > 0 || hasServiceToken(app)
	if isLocalBackend(c) && len(buildDevPlaceholders(app, c)) > 0 {
		needsSecret = true
	}

	ev := ExternalSecretValues{
		Enabled:         needsSecret,
		Backend:         spec.Backend,
		RefreshInterval: spec.RefreshInterval,
		StoreRef: StoreRefValues{
			Name: spec.StoreName,
			Kind: spec.StoreKind,
		},
		DataFrom:   []DataFromValues{},
		RemoteRefs: []RemoteRefValues{},
	}
	if !needsSecret {
		return ev
	}
	// Pull every key under the app's SSM root.
	ev.DataFrom = []DataFromValues{{Extract: map[string]string{"key": spec.AppRoot}}}

	// Pin shared-group keys discovered from the app's declared capabilities.
	for _, ref := range spec.SharedRefs {
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: ref.SecretKey,
			RemoteRef: map[string]string{"key": ref.RemoteKey},
		})
	}
	return ev
}

func hasServiceToken(app appconfig.App) bool {
	for _, r := range app.Routes {
		if r.Access.ServiceToken {
			return true
		}
	}
	for _, conn := range app.ConnectsTo {
		if conn.Mode == "serviceToken" {
			return true
		}
	}
	return false
}
