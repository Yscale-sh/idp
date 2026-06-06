package render

import (
	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clusterenv"
	"github.com/jakenesler/idp/internal/secrets"
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
	// A non-local (prod) app with a public route is exposed via the cloudflared
	// sidecar, whose TUNNEL_TOKEN lives under the app's SSM root and is pulled into
	// the runtime Secret by dataFrom — so it needs that Secret even with no
	// db/cache/storage. (Dev never tunnels, so no dev placeholder is needed.)
	if !isLocalBackend(c) && hasPublicRoute(app) {
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
	// A non-local (prod) app with a public route runs the cloudflared sidecar, whose
	// TUNNEL_TOKEN is a per-app, PLATFORM-provisioned secret at <appRoot>/TUNNEL_TOKEN
	// (the developer never puts it in deploy.yaml; the platform provisions it in SSM
	// when the public route is onboarded). Pin it EXPLICITLY — although dataFrom also
	// pulls the whole app root, the explicit remoteRef makes the dependency + its SSM
	// location visible in the rendered ExternalSecret and makes a missing token fail
	// loudly at the ExternalSecret instead of as an opaque pod CreateContainerConfigError.
	if !isLocalBackend(c) && hasPublicRoute(app) {
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: "TUNNEL_TOKEN",
			RemoteRef: map[string]string{"key": spec.AppRoot + "/TUNNEL_TOKEN"},
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
