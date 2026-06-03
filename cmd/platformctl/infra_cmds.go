package main

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jakenesler/platformctl/internal/clusterenv"
	"github.com/jakenesler/platformctl/internal/kube"
	"github.com/jakenesler/platformctl/internal/modules"
	"github.com/spf13/cobra"
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
	return c, planned, nil
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
		Short: "Write a Flux HelmRelease (+ HelmRepository) per enabled module to environments/<env>/infra/",
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, planned, err := loadEnvModules(root, env)
			if err != nil {
				return err
			}
			// Policy guardrails over module render output BEFORE any write. A
			// module that ships a LoadBalancer (inline values or templated
			// manifest) fails here, never reaching environments/<env>/infra/ for
			// Flux to reconcile.
			if err := modules.CheckAll(planned, root, env); err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			for _, p := range planned {
				path, err := p.Write(root, env)
				if err != nil {
					return err
				}
				fmt.Fprintf(out, "wrote: %s\n", path)
			}
			if len(planned) == 0 {
				fmt.Fprintf(out, "no enabled modules for env %s\n", env)
			}
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
