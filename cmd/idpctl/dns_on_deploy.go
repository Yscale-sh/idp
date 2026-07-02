package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clouddns"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// publicTunnelHosts returns an app's public route hosts COMPOSED under the target
// env's zones (a bare label "carshowdb" becomes "carshowdb.carshowdatabase.com"),
// but ONLY when the env actually serves a real Cloudflare Tunnel. It returns nil —
// meaning "no DNS step" — for a non-tunnel env (dev, or a prod stand-in with
// seams.tunnel:false) or an app with no public routes. Pure (no network): this is
// the gate the promote DNS-on-deploy step keys on, so it stays unit-testable.
func publicTunnelHosts(app appconfig.App, c *clusterenv.Config) []string {
	if c == nil || !c.EffectiveSeams().Tunnel {
		return nil
	}
	var hosts []string
	// Expand so a MULTI-COMPONENT app's routes (which live on a component, e.g. the
	// nginx `router`, not the top level) are found. Expand() returns the app itself
	// for a single-component app, so this stays correct for the simple case.
	for _, comp := range app.Expand() {
		for _, r := range comp.Routes {
			if r.Public && strings.TrimSpace(r.Host) != "" {
				hosts = append(hosts, c.ComposeHost(r.Host))
			}
		}
	}
	return hosts
}

// tunnelOriginPort is the container port the tunnel ingress should target: the port
// of the component that carries the public routes (e.g. a multi-component app's
// nginx `router`, where the cloudflared sidecar is injected and proxies to
// localhost:<that port>). Falls back to the base app port for a single-component app.
func tunnelOriginPort(app appconfig.App) int {
	for _, comp := range app.Expand() {
		for _, r := range comp.Routes {
			if r.Public && strings.TrimSpace(r.Host) != "" {
				return comp.Runtime.Port
			}
		}
	}
	return app.Runtime.Port
}

// ensureTunnelDNS makes the app reachable through its Cloudflare Tunnel: ensure the
// named tunnel exists (idempotent), point its ingress at the app's port for each
// host, and upsert the proxied CNAME (host -> <tunnelID>.cfargotunnel.com). Safe to
// run on every deploy — re-asserting ingress + DNS is how a promote "points
// everything correctly". hosts must already be env-composed (see publicTunnelHosts).
//
// It needs only the Cloudflare API token + account id (no AWS/SSM): the per-app
// TUNNEL_TOKEN the cloudflared sidecar runs with is provisioned ONCE out-of-band
// (the `idpctl tunnel up --token-out` bootstrap), not on every deploy.
func ensureTunnelDNS(out io.Writer, app appconfig.App, env string, hosts []string, accountID, zoneID string, dryRun bool) error {
	cl, account, zone, err := resolveCFCreds(accountID, zoneID)
	if err != nil {
		return err
	}
	name := app.App + "-" + env
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}

	// 1. Ensure the tunnel exists (idempotent by name).
	tunnelID, created, err := cl.EnsureTunnel(account, name, dryRun)
	if err != nil {
		return fmt.Errorf("ensure tunnel %q: %w", name, err)
	}
	if dryRun && tunnelID == "" {
		fmt.Fprintf(out, "%stunnel %q would be CREATED; ingress + DNS need a real tunnel id (dry-run stops here)\n", prefix, name)
		return nil
	}
	if created {
		fmt.Fprintf(out, "tunnel %q created: %s\n", name, tunnelID)
	} else {
		fmt.Fprintf(out, "tunnel %q exists: %s\n", name, tunnelID)
	}
	target := clouddns.TunnelTarget(tunnelID)

	// 2. Push the ingress (each host -> the routed component's local port) + catch-all.
	svc := fmt.Sprintf("http://localhost:%d", tunnelOriginPort(app))
	rules := make([]clouddns.IngressRule, 0, len(hosts)+1)
	for _, h := range hosts {
		rules = append(rules, clouddns.IngressRule{Hostname: h, Service: svc})
	}
	rules = append(rules, clouddns.CatchAll())
	if err := cl.SetIngress(account, tunnelID, rules, dryRun); err != nil {
		return fmt.Errorf("set tunnel ingress: %w", err)
	}
	for _, h := range hosts {
		fmt.Fprintf(out, "%singress: %s -> %s\n", prefix, h, svc)
	}

	// 3. Upsert the proxied CNAME per host -> <tunnelID>.cfargotunnel.com.
	comment := fmt.Sprintf("idp: %s/%s (managed by idpctl promote)", app.App, env)
	results, err := cl.SyncHosts(hosts, zone, target, true /* proxied */, comment, false, dryRun)
	if err != nil {
		return err
	}
	return reportDNS(out, prefix, "dns", target, false, results)
}
