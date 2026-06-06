package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/clouddns"
	"github.com/spf13/cobra"
)

// newTunnelCmd is the AUTO-REGISTRATION command: from just the app's deploy.yaml
// (a container + public hostnames) it makes the app live behind a Cloudflare Tunnel
// with no manual Cloudflare steps — create the tunnel, mint its token, push the
// ingress, and upsert the DNS CNAME. It mirrors carshowdatabase's prod_api.yml
// "Create Cloudflare Tunnel" step, but as a single reusable command.
//
// It talks to the Cloudflare API only (never the cluster). The minted token is the
// one the cloudflared sidecar runs with; --token-out writes it so the pipeline can
// stash it in SSM for the app's Secret. Whole thing is OPTIONAL — an app ships fine
// without it; this just removes the one manual DNS/tunnel step.
func newTunnelCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "tunnel",
		Short: "Auto-register an app's Cloudflare Tunnel + DNS (optional)",
		Long: `tunnel auto-registers the Cloudflare Tunnel for an app's public routes:

  up    create the named tunnel (idempotent), mint its connector token, push the
        ingress (host -> the app's local port), and upsert a proxied CNAME per
        public host -> <tunnelID>.cfargotunnel.com.
  down  delete the CNAMEs and (with --delete-tunnel) the tunnel itself.

Exposure is always the tunnel; this is the one-time wiring so a browser finds it,
done from the API instead of by hand. Credentials come from the environment so CI
can source them from SSM:

  CLOUDFLARE_API_TOKEN  (or CF_API_TOKEN)    zone DNS:Edit + account Tunnel:Edit
  CLOUDFLARE_ACCOUNT_ID (or CF_ACCOUNT_ID)   the Cloudflare account (tunnel API)
  CLOUDFLARE_ZONE_ID    (or CF_ZONE_ID)      optional; else the zone is looked up`,
	}
	cmd.AddCommand(newTunnelUpCmd(), newTunnelDownCmd())
	return cmd
}

type tunnelOpts struct {
	file, env, name, accountID, zoneID, tokenOut string
	deleteTunnel, printToken, dryRun             bool
}

func newTunnelUpCmd() *cobra.Command {
	var o tunnelOpts
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create/adopt the tunnel, mint its token, set ingress, upsert DNS",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runTunnelUp(cmd, o) },
	}
	addTunnelFlags(cmd, &o)
	cmd.Flags().StringVar(&o.tokenOut, "token-out", "", "write the minted TUNNEL_TOKEN to this file (mode 0600) for the pipeline to stash in SSM")
	cmd.Flags().BoolVar(&o.printToken, "print-token", false, "also print TUNNEL_TOKEN=<token> to stdout (mask it in CI)")
	return cmd
}

func newTunnelDownCmd() *cobra.Command {
	var o tunnelOpts
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete the public-route CNAMEs (and the tunnel with --delete-tunnel)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runTunnelDown(cmd, o) },
	}
	addTunnelFlags(cmd, &o)
	cmd.Flags().BoolVar(&o.deleteTunnel, "delete-tunnel", false, "also delete the tunnel itself (tear down the workload first so it has no live connections)")
	return cmd
}

func addTunnelFlags(cmd *cobra.Command, o *tunnelOpts) {
	cmd.Flags().StringVarP(&o.file, "file", "f", "deploy.yaml", "path to deploy.yaml")
	cmd.Flags().StringVarP(&o.env, "env", "e", appconfig.EnvProd, "target environment (dev|prod)")
	cmd.Flags().StringVar(&o.name, "name", "", "tunnel name (default: <app>-<env>)")
	cmd.Flags().StringVar(&o.accountID, "account-id", "", "Cloudflare account id (default: env CLOUDFLARE_ACCOUNT_ID / CF_ACCOUNT_ID)")
	cmd.Flags().StringVar(&o.zoneID, "zone-id", "", "Cloudflare zone id (default: env CLOUDFLARE_ZONE_ID / CF_ZONE_ID, else looked up)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "report intended changes without writing to Cloudflare")
}

// cfClientFor resolves the API token + account id (flag or env) and returns a
// client plus the resolved account/zone ids.
func cfClientFor(o tunnelOpts) (cl *clouddns.Client, accountID, zoneID string, err error) {
	apiToken := strings.TrimSpace(firstNonEmpty(os.Getenv("CLOUDFLARE_API_TOKEN"), os.Getenv("CF_API_TOKEN")))
	if apiToken == "" {
		return nil, "", "", fmt.Errorf("CLOUDFLARE_API_TOKEN (or CF_API_TOKEN) is not set")
	}
	accountID = strings.TrimSpace(firstNonEmpty(o.accountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CF_ACCOUNT_ID")))
	if accountID == "" {
		return nil, "", "", fmt.Errorf("Cloudflare account id is not set (pass --account-id or CLOUDFLARE_ACCOUNT_ID / CF_ACCOUNT_ID)")
	}
	zoneID = strings.TrimSpace(firstNonEmpty(o.zoneID, os.Getenv("CLOUDFLARE_ZONE_ID"), os.Getenv("CF_ZONE_ID")))
	return clouddns.New(apiToken), accountID, zoneID, nil
}

func runTunnelUp(cmd *cobra.Command, o tunnelOpts) error {
	app, err := loadApp(o.file)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	hosts := publicHosts(app)
	if len(hosts) == 0 {
		fmt.Fprintf(out, "tunnel: %s declares no public routes (routes[].public: true) — nothing to do\n", app.App)
		return nil
	}
	cl, accountID, zoneID, err := cfClientFor(o)
	if err != nil {
		return err
	}
	name := o.name
	if name == "" {
		name = app.App + "-" + o.env
	}
	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}

	// 1. Ensure the tunnel exists (idempotent by name).
	tunnelID, created, err := cl.EnsureTunnel(accountID, name, o.dryRun)
	if err != nil {
		return fmt.Errorf("ensure tunnel %q: %w", name, err)
	}
	switch {
	case o.dryRun && tunnelID == "":
		fmt.Fprintf(out, "%stunnel %q would be CREATED\n", prefix, name)
		fmt.Fprintf(out, "%s(dry-run stops here — token/ingress/DNS need a real tunnel id)\n", prefix)
		return nil
	case created:
		fmt.Fprintf(out, "tunnel %q created: %s\n", name, tunnelID)
	default:
		fmt.Fprintf(out, "tunnel %q exists: %s\n", name, tunnelID)
	}
	target := clouddns.TunnelTarget(tunnelID)

	// 2. Mint the connector token (what the cloudflared sidecar runs with).
	token, err := cl.TunnelToken(accountID, tunnelID)
	if err != nil {
		return fmt.Errorf("mint tunnel token: %w", err)
	}
	if o.tokenOut != "" {
		if err := os.WriteFile(o.tokenOut, []byte(token), 0o600); err != nil {
			return fmt.Errorf("write --token-out: %w", err)
		}
		fmt.Fprintf(out, "tunnel token written to %s (stash it at /apps/%s/%s/TUNNEL_TOKEN)\n", o.tokenOut, app.App, o.env)
	}
	if o.printToken {
		fmt.Fprintf(out, "TUNNEL_TOKEN=%s\n", token)
	}

	// 3. Push the ingress (host -> the app's local port) + the required catch-all.
	svc := fmt.Sprintf("http://localhost:%d", app.Runtime.Port)
	rules := make([]clouddns.IngressRule, 0, len(hosts)+1)
	for _, h := range hosts {
		rules = append(rules, clouddns.IngressRule{Hostname: h, Service: svc})
	}
	rules = append(rules, clouddns.CatchAll())
	if err := cl.SetIngress(accountID, tunnelID, rules, o.dryRun); err != nil {
		return fmt.Errorf("set tunnel ingress: %w", err)
	}
	for _, h := range hosts {
		fmt.Fprintf(out, "%singress: %s -> %s\n", prefix, h, svc)
	}

	// 4. Upsert the proxied CNAME per host -> <tunnelID>.cfargotunnel.com.
	comment := fmt.Sprintf("idp: %s/%s (managed by idpctl tunnel)", app.App, o.env)
	results, err := cl.SyncHosts(hosts, zoneID, target, true /* proxied */, comment, false, o.dryRun)
	if err != nil {
		return err
	}
	return reportDNS(out, prefix, "dns", target, false, results)
}

func runTunnelDown(cmd *cobra.Command, o tunnelOpts) error {
	app, err := loadApp(o.file)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	hosts := publicHosts(app)
	cl, accountID, zoneID, err := cfClientFor(o)
	if err != nil {
		return err
	}
	prefix := ""
	if o.dryRun {
		prefix = "[dry-run] "
	}

	// 1. Delete the CNAMEs.
	if len(hosts) > 0 {
		results, err := cl.SyncHosts(hosts, zoneID, "", true, "", true /* del */, o.dryRun)
		if err != nil {
			return err
		}
		if err := reportDNS(out, prefix, "dns prune", "", true, results); err != nil {
			return err
		}
	}

	// 2. Optionally delete the tunnel itself.
	if o.deleteTunnel {
		name := o.name
		if name == "" {
			name = app.App + "-" + o.env
		}
		id, err := cl.FindTunnel(accountID, name)
		if err != nil {
			return fmt.Errorf("find tunnel %q: %w", name, err)
		}
		if id == "" {
			fmt.Fprintf(out, "tunnel %q not found — nothing to delete\n", name)
			return nil
		}
		if err := cl.DeleteTunnel(accountID, id, o.dryRun); err != nil {
			return fmt.Errorf("delete tunnel %q (tear down the workload first so it has no live connections): %w", name, err)
		}
		fmt.Fprintf(out, "%stunnel %q (%s) deleted\n", prefix, name, id)
	}
	return nil
}

// reportDNS prints the per-host SyncHosts results and returns an aggregate error.
func reportDNS(out interface{ Write([]byte) (int, error) }, prefix, verb, target string, del bool, results []clouddns.Result) error {
	failed := 0
	for _, r := range results {
		if r.Err != nil {
			failed++
			fmt.Fprintf(out, "%s%s: %-38s ERROR: %v\n", prefix, verb, r.Host, r.Err)
			continue
		}
		dst := target
		if del {
			dst = "-"
		}
		fmt.Fprintf(out, "%s%s: %-38s %-9s zone=%s -> %s\n", prefix, verb, r.Host, r.Action, r.Zone, dst)
	}
	if failed > 0 {
		return fmt.Errorf("%s: %d of %d host(s) failed", verb, failed, len(results))
	}
	return nil
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
