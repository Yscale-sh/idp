package render

import (
	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"github.com/yscale-sh/idp/internal/secrets"
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
	hasTunnelRoutes := envProvidesTunnel(c) && len(cfRoutes(app, c)) > 0
	// An app with CF-eligible public routes in a tunnel env runs the cloudflared
	// sidecar, whose TUNNEL_TOKEN lives under the app root in the env's secret
	// backend — so it needs that Secret even with no db/cache/storage.
	if hasTunnelRoutes {
		needsSecret = true
	}
	// A non-local (prod) app with tailscaleEgress runs the tailscale sidecar, whose
	// TS_AUTHKEY comes from the shared SSM key — so it needs the runtime Secret even
	// with no db/cache/storage/route.
	if tailscaleEgressActive(app, c) {
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
	// Pin each declared secret key as an explicit per-key remoteRef under the app
	// SSM root. We do NOT use dataFrom extract/find: ESO's AWS SSM provider extract
	// is a single GetParameter (needs one JSON-blob param, which collides with the
	// per-app TUNNEL_TOKEN sub-param hierarchy) and find.path is unsupported on the
	// pinned external-secrets 0.14.4 ("unexpected find operator"). Explicit remoteRefs
	// are the only thing that works against individual /apps/<app>/<env>/* params, and
	// they also make a missing key fail loudly at the ExternalSecret. Apps therefore
	// DECLARE every key they need in deploy.yaml `secrets:` (including db URL keys like
	// DATABASE_URL — in prod those come from SSM, there is no in-cluster provisioner).
	for _, key := range app.Secrets {
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: key,
			RemoteRef: map[string]string{"key": spec.AppRoot + "/" + key},
		})
	}

	// Pin shared-group keys discovered from the app's declared capabilities.
	for _, ref := range spec.SharedRefs {
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: ref.SecretKey,
			RemoteRef: map[string]string{"key": ref.RemoteKey},
		})
	}
	// An app with CF-eligible public routes in a tunnel env runs the cloudflared
	// sidecar, whose TUNNEL_TOKEN is a per-app, PLATFORM-provisioned secret (the
	// developer never puts it in deploy.yaml). Pin it EXPLICITLY so the dependency
	// + backend key are visible and missing tokens fail loudly at the
	// ExternalSecret instead of as an opaque pod config error. The remoteRef shape
	// is BACKEND-SPECIFIC: SSM keys are paths (<appRoot>/TUNNEL_TOKEN), but the
	// local platform-local store is ESO's Kubernetes provider, where `key` is the
	// backing Secret's NAME (slashes are illegal) and `property` selects the entry —
	// per-app Secret "<app>" in ns platform-local, matching the litewindow/frigate
	// precedent (idpctl's setup-dev-tunnel-secret flow provisions it).
	if hasTunnelRoutes {
		ref := map[string]string{"key": spec.AppRoot + "/TUNNEL_TOKEN"}
		if isLocalBackend(c) {
			ref = map[string]string{"key": app.App, "property": "TUNNEL_TOKEN"}
		}
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: "TUNNEL_TOKEN",
			RemoteRef: ref,
		})
	}
	// The tailscale egress sidecar reads TS_AUTHKEY from the SHARED auth key in SSM
	// (not the per-app root) — pin it explicitly so the dependency + its SSM location
	// are visible and a missing key fails loudly at the ExternalSecret.
	if tailscaleEgressActive(app, c) {
		ev.RemoteRefs = append(ev.RemoteRefs, RemoteRefValues{
			SecretKey: "TS_AUTHKEY",
			RemoteRef: map[string]string{"key": sharedTailscaleAuthKey},
		})
	}
	return ev
}

// sharedTailscaleAuthKey is the SSM path of the shared Tailscale auth key every
// tailscaleEgress app pulls into its runtime Secret as TS_AUTHKEY (the on-prem
// homelab already provisions it; same /shared/<group>/* convention as stripe etc.).
const sharedTailscaleAuthKey = "/shared/tailscale/auth-key"

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
