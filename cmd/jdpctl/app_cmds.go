package main

import (
	"fmt"

	"github.com/jakenesler/jdp/internal/appconfig"
	"github.com/jakenesler/jdp/internal/deploy"
	"github.com/jakenesler/jdp/internal/render"
	"github.com/spf13/cobra"
)

func newValidateCmd() *cobra.Command {
	var file string
	cmd := &cobra.Command{
		Use:   "validate",
		Short: "Validate a deploy.yaml against the contract (structural, env-agnostic)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := loadApp(file)
			if err != nil {
				return err
			}
			if err := app.Validate(); err != nil {
				return fmt.Errorf("invalid deploy.yaml: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "ok: %s is valid\n", app.App)
			return nil
		},
	}
	cmd.Flags().StringVarP(&file, "file", "f", "deploy.yaml", "path to deploy.yaml")
	return cmd
}

func newPlanCmd() *cobra.Command {
	var file, env, image, deployTime, root string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Dry run: validate + policy + render to stdout (no writes)",
		Long: `plan reads and validates deploy.yaml, runs policy guardrails, and renders
the Flux HelmRelease to stdout WITHOUT writing any files or touching the
cluster. Use it as a CI preflight and for local debugging.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := loadApp(file)
			if err != nil {
				return err
			}
			cluster, err := loadCluster(root, env)
			if err != nil {
				return err
			}
			plan, err := deploy.Build(deploy.Request{
				App: app, Env: env, Image: image, DeployTime: deployTime, Cluster: cluster,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintln(out, "── plan ───────────────────────────────────────")
			fmt.Fprint(out, plan.Summary())
			fmt.Fprintln(out, "── rendered Flux HelmRelease ───────────────────")
			y, err := plan.Result.HelmReleaseYAML()
			if err != nil {
				return err
			}
			fmt.Fprintln(out, string(y))
			return nil
		},
	}
	addRenderFlags(cmd, &file, &env, &image, &deployTime, &root)
	return cmd
}

func newRenderCmd() *cobra.Command {
	var file, env, image, deployTime, root string
	var stdout bool
	cmd := &cobra.Command{
		Use:   "render",
		Short: "Upsert the app into the env umbrella (clusters/<env>/platform.yaml)",
		Long: `render validates, runs policy, and upserts the app (its app-chart values
+ its dedicated dev Postgres) into the environment's umbrella HelmRelease at
clusters/<env>/platform.yaml under --root. Flux installs the charts/cluster
umbrella, which templates an isolated HelmRelease per app. Commit the result;
this is the default deploy path (a git commit, not kubectl).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			app, err := loadApp(file)
			if err != nil {
				return err
			}
			cluster, err := loadCluster(root, env)
			if err != nil {
				return err
			}
			plan, err := deploy.Build(deploy.Request{
				App: app, Env: env, Image: image, DeployTime: deployTime, Cluster: cluster,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if stdout {
				y, err := plan.Result.HelmReleaseYAML()
				if err != nil {
					return err
				}
				fmt.Fprint(out, string(y))
				return nil
			}
			path, err := plan.Result.UpsertApp(root, cluster)
			if err != nil {
				return err
			}
			fmt.Fprint(out, plan.Summary())
			fmt.Fprintf(out, "\nupserted app into: %s", path)
			fmt.Fprintln(out)
			fmt.Fprintln(out, "next: commit the file; Flux installs charts/cluster and reconciles each app.")
			return nil
		},
	}
	addRenderFlags(cmd, &file, &env, &image, &deployTime, &root)
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print to stdout instead of writing the file")
	return cmd
}

func newRemoveCmd() *cobra.Command {
	var app, component, env, root string
	cmd := &cobra.Command{
		Use:   "remove",
		Short: "Remove a workload from the env umbrella (teardown; Flux prunes it)",
		Long: `remove deletes a workload's entry from the environment's umbrella HelmRelease at
clusters/<env>/platform.yaml under --root, so Flux re-reconciles the umbrella
WITHOUT it and prunes the workload's HelmRelease, its dedicated data stores, and
their namespaces (incl. any cloudflared tunnel sidecar). This is the teardown path
— a git commit, not kubectl delete. Idempotent: removing an absent app is a no-op.

With --component, only that component (handle <app>-<component>) is removed; without
it, EVERY component of the app is removed (tear down the whole multi-component app).`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cluster, err := loadCluster(root, env)
			if err != nil {
				return err
			}
			path, removed, err := render.RemoveApp(root, env, app, component, cluster)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			target := app
			if component != "" {
				target = app + "/" + component
			}
			if !removed {
				fmt.Fprintf(out, "%q not present in %s — nothing to remove\n", target, path)
				return nil
			}
			fmt.Fprintf(out, "removed %q from: %s\n", target, path)
			fmt.Fprintln(out, "next: commit the file; Flux prunes the workload's HelmRelease + dedicated stores + namespaces.")
			return nil
		},
	}
	cmd.Flags().StringVarP(&app, "app", "a", "", "app name to remove")
	cmd.Flags().StringVar(&component, "component", "", "remove only this component (default: all components of the app)")
	cmd.Flags().StringVarP(&env, "env", "e", appconfig.EnvDev, "target environment (dev|prod|local)")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root (holds clusters/ and charts/)")
	_ = cmd.MarkFlagRequired("app")
	_ = cmd.MarkFlagRequired("env")
	return cmd
}

func addRenderFlags(cmd *cobra.Command, file, env, image, deployTime, root *string) {
	cmd.Flags().StringVarP(file, "file", "f", "deploy.yaml", "path to deploy.yaml")
	cmd.Flags().StringVarP(env, "env", "e", appconfig.EnvDev, "target environment (dev|prod|local)")
	cmd.Flags().StringVar(image, "image", "", "fully-qualified image repo:tag (CI injects this)")
	cmd.Flags().StringVar(deployTime, "deploy-time", "", "CI deploy/build stamp for DEPLOY_TIME")
	cmd.Flags().StringVar(root, "root", ".", "platform repo root (holds environments/ and charts/)")
	_ = cmd.MarkFlagRequired("env")
	cmdMarkImageHelp(cmd)
}

// cmdMarkImageHelp leaves --image optional at the flag layer (policy enforces it
// for prod) but documents the expectation.
func cmdMarkImageHelp(cmd *cobra.Command) {
	if f := cmd.Flags().Lookup("image"); f != nil {
		f.Usage = "fully-qualified image repo:tag (required for prod; CI injects it)"
	}
}
