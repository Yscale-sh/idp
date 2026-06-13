package main

import (
	"fmt"

	"github.com/jakenesler/idp/internal/appconfig"
	"github.com/jakenesler/idp/internal/deploy"
	"github.com/jakenesler/idp/internal/render"
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
			comps := app.Expand()
			for _, a := range comps {
				a.ApplyDefaults()
				if err := a.Validate(); err != nil {
					if a.Component != "" {
						return fmt.Errorf("invalid deploy.yaml (component %q): %w", a.Component, err)
					}
					return fmt.Errorf("invalid deploy.yaml: %w", err)
				}
			}
			if len(comps) > 1 {
				fmt.Fprintf(cmd.OutOrStdout(), "ok: %s is valid (%d components)\n", app.App, len(comps))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "ok: %s is valid\n", app.App)
			}
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
			out := cmd.OutOrStdout()
			comps := app.Expand()
			for i, a := range comps {
				plan, err := deploy.Build(deploy.Request{
					App: a, Env: env, Image: resolveImage(image, a, len(comps) > 1), DeployTime: deployTime, Cluster: cluster,
				})
				if err != nil {
					return componentErr(a, err)
				}
				if i > 0 {
					fmt.Fprintln(out)
				}
				fmt.Fprintln(out, "── plan ───────────────────────────────────────")
				fmt.Fprint(out, plan.Summary())
				fmt.Fprintln(out, "── rendered Flux HelmRelease ───────────────────")
				y, err := plan.Result.HelmReleaseYAML()
				if err != nil {
					return err
				}
				fmt.Fprintln(out, string(y))
			}
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
			out := cmd.OutOrStdout()
			comps := app.Expand()
			var lastPath string
			for i, a := range comps {
				plan, err := deploy.Build(deploy.Request{
					App: a, Env: env, Image: resolveImage(image, a, len(comps) > 1), DeployTime: deployTime, Cluster: cluster,
				})
				if err != nil {
					return componentErr(a, err)
				}
				if stdout {
					y, err := plan.Result.HelmReleaseYAML()
					if err != nil {
						return err
					}
					if i > 0 {
						fmt.Fprintln(out, "---")
					}
					fmt.Fprint(out, string(y))
					continue
				}
				path, err := plan.Result.UpsertApp(root, cluster)
				if err != nil {
					return err
				}
				lastPath = path
				fmt.Fprint(out, plan.Summary())
				if a.Component != "" {
					fmt.Fprintf(out, "\nupserted %s/%s into: %s\n", a.App, a.Component, path)
				} else {
					fmt.Fprintf(out, "\nupserted %s into: %s\n", a.App, path)
				}
			}
			if !stdout {
				if len(comps) > 1 {
					fmt.Fprintf(out, "\n%d components upserted into %s\n", len(comps), lastPath)
				}
				fmt.Fprintln(out, "next: commit the file; Flux installs charts/cluster and reconciles each app.")
			}
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

// resolveImage picks the concrete image for one expanded component. A single-
// component app uses --image verbatim (the full repo:tag CI built). A multi-
// component app's components have DIFFERENT image repos (e.g. api vs ui), so CI
// passes the shared TAG (the commit) as --image and each component renders at
// <its runtime.image>:<tag>. Empty --image (local dry runs) is left as-is.
func resolveImage(image string, a appconfig.App, multi bool) string {
	if !multi || image == "" || a.Runtime.Image == "" {
		return image
	}
	return a.Runtime.Image + ":" + image
}

// componentErr prefixes an error with the component name (for multi-component
// shopping lists) so a failure points at the offending part, not just the app.
func componentErr(a appconfig.App, err error) error {
	if a.Component != "" {
		return fmt.Errorf("component %q: %w", a.Component, err)
	}
	return err
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
