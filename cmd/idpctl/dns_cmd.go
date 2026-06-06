package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clouddns"
	"github.com/spf13/cobra"
)

// newDNSCmd is the OPTIONAL DNS step. Exposure is always the Cloudflare Tunnel
// (the cloudflared sidecar); a public hostname still needs a DNS record pointing
// at that tunnel. Setting it by hand (Cloudflare dashboard) is the default; this
// command automates it for operators who opt in (ship.yml manage-dns: true).
func newDNSCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "dns",
		Short: "Reconcile public DNS for an app's Cloudflare Tunnel routes (optional)",
		Long: `dns reconciles the public DNS for an app's public routes (routes[].public: true)
so a browser resolves them THROUGH the app's Cloudflare Tunnel. Each public host
becomes a proxied CNAME -> <tunnelID>.cfargotunnel.com.

Exposure is always the tunnel — this never creates a LoadBalancer or Ingress, it
only writes the CNAME. The domain can be set by hand instead; this just automates
it. The tunnel id is derived from TUNNEL_TOKEN (or pass --tunnel-id).

Credentials are read from the environment so CI can source them from SSM:
  CLOUDFLARE_API_TOKEN   (required) a zone DNS:Edit API token
  TUNNEL_TOKEN           the cloudflared tunnel token (tunnel id derived from it)
  CLOUDFLARE_TUNNEL_ID   (optional) overrides the derived tunnel id`,
	}
	cmd.AddCommand(newDNSSyncCmd(), newDNSPruneCmd())
	return cmd
}

func newDNSSyncCmd() *cobra.Command {
	var file, env, tunnelID, zoneID string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Upsert a proxied CNAME per public route to the app's tunnel",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDNS(cmd, file, env, tunnelID, zoneID, dryRun, false)
		},
	}
	addDNSFlags(cmd, &file, &env, &tunnelID, &zoneID, &dryRun)
	return cmd
}

func newDNSPruneCmd() *cobra.Command {
	var file, env, tunnelID, zoneID string
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete the public-route CNAMEs (DNS teardown; tunnel stays platform-managed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDNS(cmd, file, env, tunnelID, zoneID, dryRun, true)
		},
	}
	addDNSFlags(cmd, &file, &env, &tunnelID, &zoneID, &dryRun)
	return cmd
}

func addDNSFlags(cmd *cobra.Command, file, env, tunnelID, zoneID *string, dryRun *bool) {
	cmd.Flags().StringVarP(file, "file", "f", "deploy.yaml", "path to deploy.yaml")
	cmd.Flags().StringVarP(env, "env", "e", appconfig.EnvProd, "target environment (dev|prod)")
	cmd.Flags().StringVar(tunnelID, "tunnel-id", "", "Cloudflare Tunnel UUID (default: derived from TUNNEL_TOKEN)")
	cmd.Flags().StringVar(zoneID, "zone-id", "", "Cloudflare zone id to write into (default: env CLOUDFLARE_ZONE_ID, else looked up by listing zones)")
	cmd.Flags().BoolVar(dryRun, "dry-run", false, "report intended changes without writing to Cloudflare")
}

func runDNS(cmd *cobra.Command, file, env, tunnelID, zoneID string, dryRun, del bool) error {
	app, err := loadApp(file)
	if err != nil {
		return err
	}
	hosts := publicHosts(app)
	out := cmd.OutOrStdout()
	if len(hosts) == 0 {
		fmt.Fprintf(out, "dns: %s declares no public routes (routes[].public: true) — nothing to do\n", app.App)
		return nil
	}

	apiToken := strings.TrimSpace(os.Getenv("CLOUDFLARE_API_TOKEN"))
	if apiToken == "" {
		return fmt.Errorf("CLOUDFLARE_API_TOKEN is not set (the ship pipeline sources it from SSM or a secret)")
	}

	if tunnelID == "" {
		tunnelID = strings.TrimSpace(os.Getenv("CLOUDFLARE_TUNNEL_ID"))
	}
	if tunnelID == "" {
		tok := strings.TrimSpace(os.Getenv("TUNNEL_TOKEN"))
		if tok == "" {
			return fmt.Errorf("no tunnel id: pass --tunnel-id, or set CLOUDFLARE_TUNNEL_ID or TUNNEL_TOKEN")
		}
		if tunnelID, err = clouddns.TunnelIDFromToken(tok); err != nil {
			return fmt.Errorf("derive tunnel id from TUNNEL_TOKEN: %w", err)
		}
	}
	target := clouddns.TunnelTarget(tunnelID)

	// An explicit zone id (flag or env) lets a single-zone-scoped DNS:Edit token
	// work without listing zones; empty means look the zone up by listing.
	if zoneID == "" {
		zoneID = strings.TrimSpace(os.Getenv("CLOUDFLARE_ZONE_ID"))
	}

	verb := "sync"
	if del {
		verb = "prune"
	}
	prefix := ""
	if dryRun {
		prefix = "[dry-run] "
	}
	comment := fmt.Sprintf("idp: %s/%s (managed by idpctl dns)", app.App, env)

	results, err := clouddns.New(apiToken).SyncHosts(hosts, zoneID, target, true /* proxied */, comment, del, dryRun)
	if err != nil {
		return err
	}

	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(out, "%sdns %s: %-38s ERROR: %v\n", prefix, verb, r.Host, r.Err)
			continue
		}
		dst := target
		if del {
			dst = "-"
		}
		fmt.Fprintf(out, "%sdns %s: %-38s %-9s zone=%s -> %s\n", prefix, verb, r.Host, r.Action, r.Zone, dst)
	}
	if failed > 0 {
		return fmt.Errorf("dns %s: %d of %d host(s) failed", verb, failed, len(results))
	}
	return nil
}

// publicHosts returns the hostnames the app exposes publicly (routes[].public).
func publicHosts(app appconfig.App) []string {
	var hosts []string
	for _, r := range app.Routes {
		if r.Public && strings.TrimSpace(r.Host) != "" {
			hosts = append(hosts, r.Host)
		}
	}
	return hosts
}
