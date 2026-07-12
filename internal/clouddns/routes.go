package clouddns

import (
	"strings"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// PublicTunnelHosts returns an app's public route hosts composed under the
// target env's zones, but ONLY when the env serves a Cloudflare Tunnel and
// each host falls under the env's CloudflareZone. Returns nil when the env has
// no tunnel seam or the app has no CF-eligible public routes. Pure (no network).
//
// Promoted from cmd/idpctl so the shipper (a separate main package) can reuse
// the same gate without duplicating it.
func PublicTunnelHosts(app appconfig.App, c *clusterenv.Config) []string {
	if c == nil || !c.EffectiveSeams().Tunnel {
		return nil
	}
	var hosts []string
	// Expand so a multi-component app's routes (which live on a component, e.g.
	// the nginx router, not the top level) are found. Expand() returns the app
	// itself for a single-component app, so this is correct for the simple case.
	for _, comp := range app.Expand() {
		for _, r := range comp.Routes {
			if r.Public && strings.TrimSpace(r.Host) != "" {
				host := c.ComposeHost(r.Host)
				if c.IsCloudflareHost(host) {
					hosts = append(hosts, host)
				}
			}
		}
	}
	return hosts
}

// TunnelOriginPort is the container port the tunnel ingress should target: the
// port of the component that carries the public routes (e.g. a multi-component
// app's nginx router, where the cloudflared sidecar proxies to
// localhost:<that port>). Falls back to the base app port for a
// single-component app.
//
// Promoted from cmd/idpctl for the same cross-package sharing reason as
// PublicTunnelHosts.
func TunnelOriginPort(app appconfig.App) int {
	for _, comp := range app.Expand() {
		for _, r := range comp.Routes {
			if r.Public && strings.TrimSpace(r.Host) != "" {
				return comp.Runtime.Port
			}
		}
	}
	return app.Runtime.Port
}
