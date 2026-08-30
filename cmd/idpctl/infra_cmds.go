package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"github.com/yscale-sh/idp/internal/kube"
	"github.com/yscale-sh/idp/internal/modules"
	"github.com/yscale-sh/idp/internal/render"
)

func newInfraCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "infra",
		Short: "Render/plan/apply platform infra modules from the env registry",
	}
	cmd.AddCommand(newInfraRenderCmd(), newInfraPlanCmd(), newInfraApplyCmd())
	return cmd
}

// loadEnvModules loads the env config and plans its enabled modules.
func loadEnvModules(root, env string) (*clusterenv.Config, []modules.PlannedModule, error) {
	path := filepath.Join(root, "environments", env, "cluster.yaml")
	c, err := clusterenv.Load(path)
	if err != nil {
		return nil, nil, err
	}
	planned, err := modules.Plan(c)
	if err != nil {
		return nil, nil, err
	}
	warnMissingLoki(c)
	return c, planned, nil
}

// warnMissingLoki nags (loudly, non-fatally) when the env promises every app a
// Loki endpoint but no loki module backs it. Non-fatal because an external /
// managed Loki is legitimate — but a fork forgetting logging entirely is not.
func warnMissingLoki(c *clusterenv.Config) {
	if c.Observability.LokiURL == "" {
		return
	}
	// An in-cluster `loki` OR a `loki-proxy` egress (forwards over the tailnet to an
	// out-of-cluster Loki, e.g. prod -> on-prem) both legitimately back LOKI_URL.
	for _, name := range []string{"loki", "loki-proxy"} {
		if m, ok := c.Modules[name]; ok && m.Enabled {
			return
		}
	}
	fmt.Fprintln(os.Stderr, "WARNING: observability.lokiURL is set but no enabled `loki` module backs it —")
	fmt.Fprintln(os.Stderr, "  every app gets LOKI_URL injected and logging.retention renders overrides for")
	fmt.Fprintln(os.Stderr, "  a Loki that this env does not provide. Enable the loki module (catalog:")
	fmt.Fprintln(os.Stderr, "  modules/registry.yaml) unless this env intentionally uses an external Loki.")
}

func newInfraPlanCmd() *cobra.Command {
	var env, root string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Show the enabled infra modules and their Flux HelmReleases (no writes)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, planned, err := loadEnvModules(root, env)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if len(planned) == 0 {
				fmt.Fprintf(out, "no enabled modules for env %s\n", env)
				return nil
			}
			for _, p := range planned {
				fmt.Fprintf(out, "module %-20s source=%-10s chart=%s version=%s ns=%s\n",
					p.Name, p.Source, p.Chart, p.Version, p.Namespace)
			}
			return nil
		},
	}
	addInfraFlags(cmd, &env, &root)
	return cmd
}

func newInfraRenderCmd() *cobra.Command {
	var env, root string
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Set the enabled infra modules in the env umbrella (clusters/<env>/platform.yaml)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, planned, err := loadEnvModules(root, env)
			if err != nil {
				return err
			}
			// Policy guardrails over module render output BEFORE any write. A
			// module that ships a LoadBalancer (inline values or templated
			// manifest) fails here, never reaching the umbrella for Flux to
			// reconcile.
			if err := modules.CheckAll(planned, root, env); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			entries := make([]render.ModuleEntry, 0, len(planned))
			for _, p := range planned {
				entries = append(entries, render.ModuleEntry{
					Name:        p.Name,
					Namespace:   p.Namespace,
					Source:      p.Source,
					Chart:       p.Chart,
					RepoURL:     c.Modules[p.Name].RepoURL,
					Version:     p.Version,
					Values:      p.HelmRelease.Spec.Values,
					DisableWait: c.Modules[p.Name].DisableWait,
				})
			}
			path, err := render.SetModules(root, env, c, entries)
			if err != nil {
				return err
			}
			fmt.Fprintf(out, "set %d enabled module(s) in: %s\n", len(entries), path)
			fmt.Fprintln(out, "next: commit the file; the umbrella chart templates each module's HelmRelease.")
			return nil
		},
	}
	addInfraFlags(cmd, &env, &root)
	return cmd
}

func newInfraApplyCmd() *cobra.Command {
	var env, root, kubeContext string
	cmd := &cobra.Command{
		Use:   "apply",
		Short: "(non-default) kubectl apply the infra HelmReleases directly",
		Long: `apply shells out to kubectl to apply the rendered infra HelmReleases.
This is NOT the default path — the default is render -> git commit -> Flux.
Use apply only for bootstrap/emergency when Flux is not yet reconciling.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, planned, err := loadEnvModules(root, env)
			if err != nil {
				return err
			}
			// Policy guardrails BEFORE mutating the cluster. A module that ships a
			// LoadBalancer fails closed here, before any kubectl apply.
			if err := modules.CheckAll(planned, root, env); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			cl := kube.New(kubeContext)
			if !cl.Available() {
				return fmt.Errorf("kubectl not found on PATH; use `infra render` + git commit instead")
			}
			for _, p := range planned {
				y, err := p.YAML(env)
				if err != nil {
					return err
				}
				res, err := cl.Apply(context.Background(), y)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "applied %s: %s", p.Name, string(res))
			}
			return nil
		},
	}
	addInfraFlags(cmd, &env, &root)
	cmd.Flags().StringVar(&kubeContext, "context", "", "kubeconfig context (default: current)")
	return cmd
}

func addInfraFlags(cmd *cobra.Command, env, root *string) {
	cmd.Flags().StringVarP(env, "env", "e", "", "target environment (dev|prod|local)")
	cmd.Flags().StringVar(root, "root", ".", "platform repo root")
	_ = cmd.MarkFlagRequired("env")
}
