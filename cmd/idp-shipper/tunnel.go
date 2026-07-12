package main

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"strings"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clouddns"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"github.com/yscale-sh/idp/internal/kube"
)

// shouldProvisionTunnel is the pure gate: the CF pipeline is enabled in the
// registry config, the required API credentials are present, and the app has
// at least one zone-filtered CF public route. Pure (no network, no cluster
// calls) so the unit tests cover it without external dependencies.
func shouldProvisionTunnel(app appconfig.App, cfEnabled bool, apiToken, accountID string, cluster *clusterenv.Config) bool {
	if !cfEnabled || strings.TrimSpace(apiToken) == "" || strings.TrimSpace(accountID) == "" {
		return false
	}
	return len(clouddns.PublicTunnelHosts(app, cluster)) > 0
}

// tunnelFrontend enforces the current connector topology: one component owns
// every Cloudflare-public hostname and may route paths to sibling components.
// Multiple public components would register connectors for the same tunnel,
// allowing Cloudflare to send a hostname to the wrong component-local origin.
func tunnelFrontend(apps []appconfig.App) (appconfig.App, error) {
	if len(apps) == 0 {
		return appconfig.App{}, fmt.Errorf("no Cloudflare-public component")
	}
	if len(apps) > 1 {
		return appconfig.App{}, fmt.Errorf("%d Cloudflare-public components: expose one router/front component and route to siblings internally", len(apps))
	}
	return apps[0], nil
}

// maybeProvisionTunnel auto-provisions the Cloudflare Tunnel for app after a
// successful ship. It is BEST-EFFORT: every error is logged and the function
// always returns — a tunnel failure never aborts or rolls back the ship (the
// app is deployed; the tunnel retries on the next cycle).
//
// Flow when the gate passes: EnsureTunnel (idempotent by name) → TunnelToken
// + stashTunnelToken into Secret <app>/platform-local (create or repair) →
// SetIngress (all CF hosts -> localhost:<originPort>) → SyncHosts (proxied
// CNAMEs). The tunnel name is <appName>-<env>, matching the bootstrap CLI.
func maybeProvisionTunnel(ctx context.Context, reg *Registry, app AppSpec, sha, gitToken string, k *kube.Client) {
	apiToken := os.Getenv("CLOUDFLARE_API_TOKEN")
	if apiToken == "" {
		apiToken = os.Getenv("CF_API_TOKEN")
	}
	accountID := os.Getenv("CLOUDFLARE_ACCOUNT_ID")
	if accountID == "" {
		accountID = os.Getenv("CF_ACCOUNT_ID")
	}

	// Cheap gate: no network until config + creds pass.
	if !reg.CloudflareEnabled || strings.TrimSpace(apiToken) == "" || strings.TrimSpace(accountID) == "" {
		return
	}

	cluster, err := loadCluster(reg.PlatformRoot, reg.Env)
	if err != nil {
		logf("[%s] tunnel: load cluster config: %v — skipping CF provisioning", app.Name, err)
		return
	}
	if cluster == nil {
		return // no cluster.yaml in this env; nothing to match against
	}

	// Re-fetch the components to inspect their routes. This repeats what shipApp
	// already did, but it's lightweight (no build) and keeps the function
	// self-contained so shipApp's signature stays unchanged.
	comps, err := fetchComponents(ctx, app, sha, gitToken)
	if err != nil {
		logf("[%s] tunnel: fetch components: %v", app.Name, err)
		return
	}

	// Collect CF hosts and the component that owns them. Production deliberately
	// calls the same pure gate covered by unit tests.
	var allHosts []string
	var publicApps []appconfig.App
	for _, c := range comps {
		if !shouldProvisionTunnel(c.cfg, reg.CloudflareEnabled, apiToken, accountID, cluster) {
			continue
		}
		hosts := clouddns.PublicTunnelHosts(c.cfg, cluster)
		publicApps = append(publicApps, c.cfg)
		allHosts = append(allHosts, hosts...)
	}
	if len(allHosts) == 0 {
		return // no CF-eligible routes; nothing to provision
	}
	repApp, err := tunnelFrontend(publicApps)
	if err != nil {
		logf("[%s] tunnel: %v", app.Name, err)
		return
	}

	name := repApp.App + "-" + reg.Env
	cl := clouddns.New(apiToken)

	// 1. Ensure the tunnel exists (idempotent by name).
	tunnelID, created, err := cl.EnsureTunnel(accountID, name, false)
	if err != nil {
		logf("[%s] tunnel: EnsureTunnel %q: %v", app.Name, name, err)
		return
	}
	if created {
		logf("[%s] tunnel %q created: %s", app.Name, name, tunnelID)
	} else {
		logf("[%s] tunnel %q exists: %s", app.Name, name, tunnelID)
	}

	// 2. Retrieve and stash the connector token on every reconciliation. This
	// repairs a missing platform-local Secret after cluster rebuilds or manual
	// deletion without creating a second tunnel. Token contents are never logged.
	token, err := cl.TunnelToken(accountID, tunnelID)
	if err != nil {
		logf("[%s] tunnel: TunnelToken %s: %v", app.Name, tunnelID, err)
		return
	}
	if err := stashTunnelToken(ctx, k, repApp.App, token); err != nil {
		logf("[%s] tunnel: stash token FAILED (%v) — provision manually: kubectl -n platform-local create secret generic %s --from-literal=TUNNEL_TOKEN=<token>", app.Name, err, repApp.App)
	} else {
		logf("[%s] tunnel: TUNNEL_TOKEN reconciled in Secret %s/platform-local", app.Name, repApp.App)
	}

	// 3. Push ingress: each CF host -> the routed component's local port + catch-all.
	svc := fmt.Sprintf("http://localhost:%d", clouddns.TunnelOriginPort(repApp))
	rules := make([]clouddns.IngressRule, 0, len(allHosts)+1)
	for _, h := range allHosts {
		rules = append(rules, clouddns.IngressRule{Hostname: h, Service: svc})
	}
	rules = append(rules, clouddns.CatchAll())
	if err := cl.SetIngress(accountID, tunnelID, rules, false); err != nil {
		logf("[%s] tunnel: SetIngress: %v", app.Name, err)
		return
	}
	for _, h := range allHosts {
		logf("[%s] tunnel: ingress %s -> %s", app.Name, h, svc)
	}

	// 4. Upsert proxied CNAMEs (<host> -> <tunnelID>.cfargotunnel.com).
	comment := fmt.Sprintf("idp: %s/%s (managed by idp-shipper)", repApp.App, reg.Env)
	target := clouddns.TunnelTarget(tunnelID)
	results, err := cl.SyncHosts(allHosts, "", target, true, comment, false, false)
	if err != nil {
		logf("[%s] tunnel: SyncHosts: %v", app.Name, err)
		return
	}
	for _, r := range results {
		if r.Err != nil {
			logf("[%s] tunnel: dns %s: %v", app.Name, r.Host, r.Err)
		} else {
			logf("[%s] tunnel: dns %s %s -> %s", app.Name, r.Action, r.Host, target)
		}
	}
}

// stashTunnelToken creates or updates Secret <appName> in namespace platform-local
// with data key TUNNEL_TOKEN using kubectl apply (the kube.Client wrapper the
// shipper already uses). The renderer's ExternalSecret for the app sets
// remoteRef.key = <app> / property = TUNNEL_TOKEN against the Kubernetes ESO
// provider in ns platform-local, so this one write is all that's needed.
func stashTunnelToken(ctx context.Context, k *kube.Client, appName, token string) error {
	// Use data: (base64) rather than stringData: so the manifest is valid for
	// both apply (create/update) and is explicit about encoding.
	encoded := base64.StdEncoding.EncodeToString([]byte(token))
	manifest := fmt.Sprintf(`apiVersion: v1
kind: Secret
metadata:
  name: %s
  namespace: platform-local
type: Opaque
data:
  TUNNEL_TOKEN: %s
`, appName, encoded)
	_, err := k.Apply(ctx, []byte(manifest))
	return err
}
