package main

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"github.com/yscale-sh/idp/internal/kube"
)

// doctor verifies that the SEAMS an environment declares are actually present
// and healthy on the live cluster — the runtime half of the loose-coupling
// contract (cluster.yaml says what the env provides; doctor checks the cluster
// backs it). Read-only: every probe is a `kubectl get`. Exits non-zero if any
// hard check fails, so it doubles as a pre-promote / pre-bootstrap gate in CI.
func newDoctorCmd() *cobra.Command {
	var env, root, kctx string
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Probe the live cluster for the seams this env declares (Flux, ESO, ingress/tunnel, observability, KEDA)",
		Long: `doctor reads environments/<env>/cluster.yaml, resolves the seams the env
provides, and probes the live cluster (read-only kubectl) that each is present
and healthy: Flux (source + the platform-<env> umbrella), the ESO secret store,
the observability endpoints, the autoscale backend (KEDA) + its ownership, and
the ingress path (Cloudflare Tunnel / MetalLB). Non-zero exit on any failure.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			c, err := loadCluster(root, env)
			if err != nil {
				return err
			}
			if c == nil {
				return fmt.Errorf("no environments/%s/cluster.yaml under --root", env)
			}
			k := kube.New(kctx)
			if !k.Available() {
				return fmt.Errorf("kubectl not found on PATH — doctor needs cluster access")
			}
			d := &doctor{k: k, env: env, c: c, seams: c.EffectiveSeams(), out: out}
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()

			fmt.Fprintf(out, "idpctl doctor — env %q (context %q)\n\n", env, kubeCtxLabel(kctx))
			d.checkCluster(ctx)
			d.checkFlux(ctx)
			d.checkSecrets(ctx)
			d.checkObservability(ctx)
			d.checkAutoscale(ctx)
			d.checkIngress(ctx)

			fmt.Fprintf(out, "\n%d ok, %d warn, %d FAIL\n", d.ok, d.warn, d.fail)
			if d.fail > 0 {
				return fmt.Errorf("%d seam check(s) failed", d.fail)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&env, "env", "e", "dev", "environment to check (reads environments/<env>/cluster.yaml)")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root")
	cmd.Flags().StringVar(&kctx, "context", "", "kubeconfig context (default: current)")
	return cmd
}

type doctor struct {
	k              *kube.Client
	env            string
	c              *clusterenv.Config
	seams          clusterenv.ResolvedSeams
	out            interface{ Write([]byte) (int, error) }
	ok, warn, fail int
}

func (d *doctor) pass(label, detail string) {
	d.ok++
	fmt.Fprintf(d.out, "  ✓ %-22s %s\n", label, detail)
}
func (d *doctor) warnf(label, detail string) {
	d.warn++
	fmt.Fprintf(d.out, "  ⚠ %-22s %s\n", label, detail)
}
func (d *doctor) failf(label, detail string) {
	d.fail++
	fmt.Fprintf(d.out, "  ✗ %-22s %s\n", label, detail)
}

// get runs `kubectl get <args> -o jsonpath=<jp>` and returns trimmed stdout.
func (d *doctor) get(ctx context.Context, jp string, args ...string) (string, error) {
	full := append([]string{"get"}, args...)
	full = append(full, "-o", "jsonpath="+jp, "--request-timeout=8s")
	b, err := d.k.Run(ctx, full...)
	return strings.TrimSpace(string(b)), err
}

func (d *doctor) checkCluster(ctx context.Context) {
	if v, err := d.get(ctx, "{.serverVersion.gitVersion}", "--raw", "/version"); err == nil && v != "" {
		d.pass("cluster reachable", v)
	} else if v, err := d.k.Run(ctx, "version", "--output=json", "--request-timeout=8s"); err == nil && len(v) > 0 {
		d.pass("cluster reachable", "apiserver responding")
	} else {
		d.failf("cluster reachable", "apiserver not responding — check --context / kubeconfig")
	}
}

func (d *doctor) checkFlux(ctx context.Context) {
	if _, err := d.get(ctx, "{.items[0].metadata.name}", "fluxinstance", "-A"); err != nil {
		d.failf("flux: operator", "no FluxInstance found — is the Flux Operator installed?")
	} else {
		d.pass("flux: operator", "FluxInstance present")
	}
	ns := d.c.Flux.Namespace
	if ns == "" {
		ns = "flux-system"
	}
	src := d.c.Flux.SourceName
	if src == "" {
		src = "flux-system"
	}
	if st, err := d.get(ctx, "{.status.conditions[?(@.type=='Ready')].status}", "gitrepository", src, "-n", ns); err == nil {
		if st == "True" {
			d.pass("flux: source", fmt.Sprintf("GitRepository %s/%s Ready", ns, src))
		} else {
			d.failf("flux: source", fmt.Sprintf("GitRepository %s/%s not Ready (%q)", ns, src, st))
		}
	} else {
		d.failf("flux: source", fmt.Sprintf("GitRepository %s/%s not found", ns, src))
	}
	umbrella := "platform"
	if d.env != "dev" {
		umbrella = "platform-" + d.env
	}
	if st, err := d.get(ctx, "{.status.conditions[?(@.type=='Ready')].status}", "helmrelease", umbrella, "-n", ns); err == nil {
		if st == "True" {
			d.pass("flux: umbrella", umbrella+" reconciled")
		} else {
			d.warnf("flux: umbrella", fmt.Sprintf("%s present but not Ready (%q)", umbrella, st))
		}
	} else {
		d.warnf("flux: umbrella", umbrella+" not found (nothing promoted to this env yet?)")
	}
}

func (d *doctor) checkSecrets(ctx context.Context) {
	kind := strings.ToLower(d.c.Secrets.StoreRef.Kind) // ClusterSecretStore | SecretStore
	name := d.c.Secrets.StoreRef.Name
	if kind == "" || name == "" {
		d.failf("secrets: store", "secrets.storeRef not declared in cluster.yaml")
		return
	}
	args := []string{kind, name}
	if kind == "secretstore" { // namespaced
		args = append(args, "-A")
	}
	if st, err := d.get(ctx, "{.status.conditions[?(@.type=='Ready')].status}", args...); err == nil {
		if st == "" || strings.Contains(st, "True") {
			d.pass("secrets: store", fmt.Sprintf("%s/%s present", kind, name))
		} else {
			d.failf("secrets: store", fmt.Sprintf("%s/%s not Ready (%q)", kind, name, st))
		}
	} else {
		d.failf("secrets: store", fmt.Sprintf("%s/%s not found — is external-secrets installed + the store created?", kind, name))
	}
}

func (d *doctor) checkObservability(ctx context.Context) {
	for label, raw := range map[string]string{"loki": d.c.Observability.LokiURL, "otlp": d.c.Observability.OTLPEndpoint} {
		if raw == "" {
			if label == "loki" { // logs are the universal seam — required
				d.failf("observability: loki", "lokiURL not declared")
			}
			// otlp is optional: absent endpoint = this env provides no collector, fine.
			continue
		}
		ns, svc := svcFromURL(raw)
		if svc == "" {
			d.warnf("observability: "+label, raw+" (external — not cluster-probed)")
			continue
		}
		if _, err := d.get(ctx, "{.metadata.name}", "svc", svc, "-n", ns); err == nil {
			d.pass("observability: "+label, fmt.Sprintf("svc %s/%s resolves", ns, svc))
		} else {
			d.failf("observability: "+label, fmt.Sprintf("svc %s/%s not found", ns, svc))
		}
	}
}

func (d *doctor) checkAutoscale(ctx context.Context) {
	if !d.seams.Autoscale {
		return
	}
	if _, err := d.get(ctx, "{.items[0].metadata.name}", "deploy", "-n", "keda", "-l", "app=keda-operator"); err != nil {
		d.failf("autoscale: keda", "seam declared but no keda-operator in ns keda")
		return
	}
	d.pass("autoscale: keda", "keda-operator present")
	// KEDA ownership: a ScaledObject whose target Deployment ALSO pins a literal
	// spec.replicas is the SSA-fight that power-cycled hosts (2026-06-12). KEDA
	// strips the field on adoption, so a lingering value means a manifest still
	// declares it. Best-effort: flag any ScaledObject reporting a fallback/error.
	if active, err := d.get(ctx, "{range .items[*]}{.metadata.namespace}/{.metadata.name}={.status.conditions[?(@.type=='Fallback')].status} {end}", "scaledobject", "-A"); err == nil && active != "" {
		bad := []string{}
		for _, kv := range strings.Fields(active) {
			if strings.HasSuffix(kv, "=True") {
				bad = append(bad, strings.TrimSuffix(kv, "=True"))
			}
		}
		if len(bad) > 0 {
			d.warnf("autoscale: ownership", "ScaledObjects in fallback: "+strings.Join(bad, ", "))
		} else {
			d.pass("autoscale: ownership", "all ScaledObjects nominal")
		}
	}
}

func (d *doctor) checkIngress(ctx context.Context) {
	if d.seams.PublicRoutes {
		if _, err := d.get(ctx, "{.items[0].metadata.name}", "deploy", "-A", "-l", "app=cloudflared"); err == nil {
			d.pass("ingress: tunnel", "cloudflared present")
		} else if _, err := d.get(ctx, "{.items[0].metadata.name}", "deploy", "-A", "-l", "app.kubernetes.io/name=ingress-nginx"); err == nil {
			d.pass("ingress: controller", "ingress-nginx present")
		} else {
			d.warnf("ingress: public", "publicRoutes seam declared but no cloudflared / ingress-nginx found")
		}
	}
	if d.seams.LANExpose {
		// MetalLB labels its controller app=metallb,component=controller (the
		// vanilla install) or app.kubernetes.io/component=controller (the chart).
		found := false
		for _, sel := range []string{"app=metallb,component=controller", "app.kubernetes.io/component=controller"} {
			if _, err := d.get(ctx, "{.items[0].metadata.name}", "deploy", "-n", "metallb-system", "-l", sel); err == nil {
				found = true
				break
			}
		}
		if found {
			d.pass("ingress: lan", "MetalLB controller present")
		} else {
			d.warnf("ingress: lan", "lanExpose seam declared but no MetalLB controller in metallb-system")
		}
	}
}

// svcFromURL pulls (namespace, service) from a cluster-DNS URL like
// http://loki.monitoring.svc.cluster.local:3100 -> ("monitoring","loki").
// Returns ("","") for non-cluster (external) hosts.
func svcFromURL(raw string) (ns, svc string) {
	u, err := url.Parse(raw)
	if err != nil {
		return "", ""
	}
	host := u.Hostname()
	if !strings.Contains(host, ".svc") {
		return "", ""
	}
	parts := strings.Split(host, ".")
	if len(parts) >= 2 {
		return parts[1], parts[0]
	}
	return "", ""
}

func kubeCtxLabel(c string) string {
	if c == "" {
		return "current"
	}
	return c
}
