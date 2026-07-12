package main

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clouddns"
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
	file, env, root, name, accountID, zoneID, tokenOut string
	deleteTunnel, printToken, dryRun, skipDNS          bool
	verifyAccess                                       bool
}

func newTunnelUpCmd() *cobra.Command {
	var o tunnelOpts
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create/adopt the tunnel, mint its token, set ingress, upsert DNS",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runTunnelUp(cmd, o) },
	}
	addTunnelFlags(cmd, &o)
	cmd.Flags().BoolVar(&o.skipDNS, "skip-dns", false, "create the tunnel + token + ingress but DON'T upsert the public DNS CNAME — for staging an app live on its tunnel before the production cutover (flip DNS later)")
	cmd.Flags().StringVar(&o.tokenOut, "token-out", "", "write the minted TUNNEL_TOKEN to this file (mode 0600) for the pipeline to stash in SSM")
	cmd.Flags().BoolVar(&o.printToken, "print-token", false, "also print TUNNEL_TOKEN=<token> to stdout (mask it in CI)")
	cmd.Flags().BoolVar(&o.verifyAccess, "verify-access", false, "verify each public host is protected by Cloudflare Access after DNS upsert (default: on when env cloudflareZone is set, off otherwise; explicit flag wins; skipped under --dry-run or --skip-dns)")
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
	cmd.Flags().StringVar(&o.root, "root", ".", "platform repo root (holds environments/) — for composing route hosts under the env's zone")
	cmd.Flags().StringVar(&o.name, "name", "", "tunnel name (default: <app>-<env>)")
	cmd.Flags().StringVar(&o.accountID, "account-id", "", "Cloudflare account id (default: env CLOUDFLARE_ACCOUNT_ID / CF_ACCOUNT_ID)")
	cmd.Flags().StringVar(&o.zoneID, "zone-id", "", "Cloudflare zone id (default: env CLOUDFLARE_ZONE_ID / CF_ZONE_ID, else looked up)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "report intended changes without writing to Cloudflare")
}

// cfClientFor resolves the API token + account id (flag or env) and returns a
// client plus the resolved account/zone ids.
func cfClientFor(o tunnelOpts) (cl *clouddns.Client, accountID, zoneID string, err error) {
	return resolveCFCreds(o.accountID, o.zoneID)
}

// resolveCFCreds turns the Cloudflare API token (env), account id (arg or env),
// and optional zone id (arg or env) into a client. Shared by `tunnel`/`dns` and
// the promote DNS-on-deploy step so every path reads credentials the same way.
func resolveCFCreds(accountID, zoneID string) (cl *clouddns.Client, account, zone string, err error) {
	apiToken := strings.TrimSpace(firstNonEmpty(os.Getenv("CLOUDFLARE_API_TOKEN"), os.Getenv("CF_API_TOKEN")))
	if apiToken == "" {
		return nil, "", "", fmt.Errorf("CLOUDFLARE_API_TOKEN (or CF_API_TOKEN) is not set")
	}
	account = strings.TrimSpace(firstNonEmpty(accountID, os.Getenv("CLOUDFLARE_ACCOUNT_ID"), os.Getenv("CF_ACCOUNT_ID")))
	if account == "" {
		return nil, "", "", fmt.Errorf("Cloudflare account id is not set (pass --account-id or CLOUDFLARE_ACCOUNT_ID / CF_ACCOUNT_ID)")
	}
	zone = strings.TrimSpace(firstNonEmpty(zoneID, os.Getenv("CLOUDFLARE_ZONE_ID"), os.Getenv("CF_ZONE_ID")))
	return clouddns.New(apiToken), account, zone, nil
}

func runTunnelUp(cmd *cobra.Command, o tunnelOpts) error {
	app, err := loadApp(o.file)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	// Compose the route hosts under the env's zone (bare "api" -> "api.<zone>") —
	// the SAME helper the promote DNS-on-deploy uses, so the bootstrap ingress +
	// DNS match what every deploy re-asserts (a bare label here 404s at cloudflared).
	c, err := loadCluster(o.root, o.env)
	if err != nil {
		return fmt.Errorf("load %s cluster env: %w", o.env, err)
	}
	hosts := publicTunnelHosts(app, c)
	if len(hosts) == 0 {
		fmt.Fprintf(out, "tunnel: no Cloudflare-eligible routes for env %s — nothing to do\n", o.env)
		return nil
	}
	// --verify-access effective value: on by default when this env has a
	// CloudflareZone (all CF-zone hosts sit behind the wildcard Access app),
	// off otherwise; an explicit --verify-access flag wins in either direction.
	cfZone := ""
	if c != nil {
		cfZone = c.CloudflareZone
	}
	effectiveVerify := cfZone != ""
	if cmd.Flags().Changed("verify-access") {
		effectiveVerify = o.verifyAccess
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

	// 3. Push the ingress (host -> the routed component's local port) + catch-all.
	// For a multi-component app the cloudflared sidecar lives in the component that
	// owns the public routes (the nginx router), so target ITS port, not the base.
	svc := fmt.Sprintf("http://localhost:%d", tunnelOriginPort(app))
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
	// --skip-dns stages the tunnel live (connected + routable by ingress) WITHOUT
	// repointing public DNS — so an app can be verified on its tunnel before the
	// production cutover. Flip DNS later with a plain `tunnel up` (or promote).
	if o.skipDNS {
		fmt.Fprintf(out, "%sdns: SKIPPED (--skip-dns) for %s — public CNAME(s) not touched; tunnel target is %s\n", prefix, strings.Join(hosts, ", "), target)
		return nil
	}
	comment := fmt.Sprintf("idp: %s/%s (managed by idpctl tunnel)", app.App, o.env)
	results, err := cl.SyncHosts(hosts, zoneID, target, true /* proxied */, comment, false, o.dryRun)
	if err != nil {
		return err
	}
	if err := reportDNS(out, prefix, "dns", target, false, results); err != nil {
		return err
	}
	// After DNS is live, confirm each host is behind Cloudflare Access.
	// Skipped in dry-run (no real DNS was written) and --skip-dns (returned earlier).
	if effectiveVerify && !o.dryRun {
		return verifyAccessGates(out, hosts, 90*time.Second)
	}
	return nil
}

func runTunnelDown(cmd *cobra.Command, o tunnelOpts) error {
	app, err := loadApp(o.file)
	if err != nil {
		return err
	}
	out := cmd.OutOrStdout()
	c, err := loadCluster(o.root, o.env)
	if err != nil {
		return fmt.Errorf("load %s cluster env: %w", o.env, err)
	}
	hosts := publicTunnelHosts(app, c)
	if len(hosts) == 0 && !o.deleteTunnel {
		fmt.Fprintf(out, "tunnel: no Cloudflare-eligible routes for env %s — nothing to do\n", o.env)
		return nil
	}
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

// isAccessProtected reports whether a (status, location) pair from an
// unauthenticated GET indicates Cloudflare Access protection: any redirect
// status (301/302/303/307/308) AND ".cloudflareaccess.com" in the Location
// header. Pure function — no network; unit-testable without side effects.
func isAccessProtected(status int, location string) bool {
	switch status {
	case 301, 302, 303, 307, 308:
	default:
		return false
	}
	return strings.Contains(location, ".cloudflareaccess.com")
}

// verifyAccessGates probes each host with a no-follow GET and confirms it
// returns a Cloudflare Access redirect (isAccessProtected). Retries with
// exponential backoff (2s → 10s cap) until each host passes or budget expires.
// Returns an error naming the first unprotected host.
func verifyAccessGates(out io.Writer, hosts []string, budget time.Duration) error {
	client := &http.Client{
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Timeout: 8 * time.Second,
	}
	for _, host := range hosts {
		url := "https://" + host + "/"
		deadline := time.Now().Add(budget)
		sleep := 2 * time.Second
		for {
			resp, err := client.Get(url)
			if err == nil {
				loc := resp.Header.Get("Location")
				resp.Body.Close()
				if isAccessProtected(resp.StatusCode, loc) {
					fmt.Fprintf(out, "verify-access: %s ok (HTTP %d -> Cloudflare Access)\n", host, resp.StatusCode)
					break
				}
				if time.Now().After(deadline) {
					return fmt.Errorf("verify-access: %s is not protected by Cloudflare Access (HTTP %d, Location: %q)", host, resp.StatusCode, loc)
				}
				fmt.Fprintf(out, "verify-access: %s — HTTP %d, not protected yet, retrying...\n", host, resp.StatusCode)
			} else {
				if time.Now().After(deadline) {
					return fmt.Errorf("verify-access: %s unreachable after timeout: %w", host, err)
				}
				fmt.Fprintf(out, "verify-access: %s — error (%v), retrying...\n", host, err)
			}
			time.Sleep(sleep)
			if sleep < 10*time.Second {
				sleep *= 2
			}
		}
	}
	return nil
}
