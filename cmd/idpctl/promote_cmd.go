package main

import (
	"fmt"
	"strings"

	"github.com/jakenesler/idp/internal/clusterenv"
	"github.com/jakenesler/idp/internal/deploy"
	"github.com/jakenesler/idp/internal/render"
	"github.com/spf13/cobra"
)

// promote moves a workload between environments DIGEST-FORWARD: the image
// reference is read out of the SOURCE env's umbrella (so only something that
// already runs there can move), then re-rendered with the TARGET env's
// policy/values. The artifact never rebuilds; only its surroundings change.
//
// Flexible by design (fork-model): env names are plain strings the operator
// owns — nothing here knows "prod" or "stage". The target env's cluster.yaml
// can declare `promotion: {from: <env>}` to gate its sources; its
// `flux.branch` says which git branch must carry the resulting commit. The
// command renders the file and PRINTS the commit/branch step rather than
// running git itself, so any CI shape can own the actual push.
func newPromoteCmd() *cobra.Command {
	var from, file, root, sourceRoot, deployTime string
	var force bool
	cmd := &cobra.Command{
		Use:   "promote <app-or-workload> <target-env>",
		Short: "Promote a workload between envs, pinned to the image already running in the source env",
		Long: `promote reads the workload's image reference from the SOURCE env's umbrella
(clusters/<from>/platform.yaml) — provenance: only an image that already runs
there can be promoted — and renders the workload into the TARGET env's
umbrella with the target's policy, secrets backend, and namespaces. Same
digest, new environment.

The first argument is the workload handle as it appears in the source
umbrella: "<app>" for single-component apps, "<app>-<component>" otherwise.
deploy.yaml (--file) supplies the contract to render — ideally the same
revision that built the image.

The target's cluster.yaml may declare a gate (promotion.from) and the branch
its Flux tracks (flux.branch); commit the rendered file to THAT branch.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			workload, toEnv := args[0], args[1]
			out := cmd.OutOrStdout()

			target, err := loadCluster(root, toEnv)
			if err != nil {
				return err
			}
			if target == nil {
				return fmt.Errorf("no environments/%s/cluster.yaml under --root", toEnv)
			}

			if from == toEnv {
				return fmt.Errorf("source and target env are both %q", toEnv)
			}
			// Gate: the target may declare its only legal source.
			if g := target.Promotion; g != nil && g.From != "" && g.From != from {
				if !force {
					return fmt.Errorf("env %q only accepts promotions from %q (got --from %q); --force overrides", toEnv, g.From, from)
				}
				fmt.Fprintf(out, "WARNING: bypassing promotion gate (%s only accepts %s) via --force\n", toEnv, g.From)
			}

			// Cross-branch read: dev/stage umbrellas live on `main`, the prod
			// umbrella on the `prod` branch. --source-root lets promote READ the
			// source env from a `main` checkout while WRITING the target into the
			// (prod-branch) --root. Defaults to --root for same-tree promotes.
			srcRoot := sourceRoot
			if srcRoot == "" {
				srcRoot = root
			}
			source, err := loadCluster(srcRoot, from)
			if err != nil {
				return err
			}
			if source == nil {
				return fmt.Errorf("no environments/%s/cluster.yaml under --source-root (%s)", from, srcRoot)
			}

			// Provenance: the image reference comes from the source umbrella.
			image, err := imageFromUmbrella(srcRoot, from, source, workload)
			if err != nil {
				return err
			}

			app, err := loadApp(file)
			if err != nil {
				return err
			}

			plan, err := deploy.Build(deploy.Request{
				App: app, Env: toEnv, Image: image, DeployTime: deployTime, Cluster: target,
			})
			if err != nil {
				return err
			}
			path, err := plan.Result.UpsertApp(root, target)
			if err != nil {
				return err
			}

			fmt.Fprintf(out, "promoted %s  %s -> %s\n", workload, from, toEnv)
			fmt.Fprintf(out, "image (pinned from %s): %s\n", from, image)
			fmt.Fprintf(out, "rendered: %s\n", path)
			branch := target.Flux.Branch
			if branch == "" {
				branch = "main"
			}
			fmt.Fprintf(out, "next: commit %s on branch %q (the branch %s's Flux tracks); rollback = git revert that commit.\n", path, branch, toEnv)
			return nil
		},
	}
	cmd.Flags().StringVar(&from, "from", "dev", "source environment (the umbrella the image is read from)")
	cmd.Flags().StringVarP(&file, "file", "f", "deploy.yaml", "path to the workload's deploy.yaml (use the revision that built the image)")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root (TARGET — where the render is written)")
	cmd.Flags().StringVar(&sourceRoot, "source-root", "", "repo root to READ the source env from (default: --root); set to a main checkout when promoting to a branch-isolated env like prod")
	cmd.Flags().StringVar(&deployTime, "deploy-time", "", "CI deploy/build stamp for DEPLOY_TIME")
	cmd.Flags().BoolVar(&force, "force", false, "bypass the target env's promotion.from gate")
	return cmd
}

// imageFromUmbrella extracts a workload's image reference (repo:tag) from an
// env's rendered umbrella. Mutable/empty tags are refused outright — a
// promotion's whole point is pinning a proven artifact.
func imageFromUmbrella(root, env string, c *clusterenv.Config, workload string) (string, error) {
	pr, err := render.ReadPlatform(root, env, c)
	if err != nil {
		return "", err
	}
	var handles []string
	for _, a := range pr.Spec.Values.Apps {
		handles = append(handles, a.ReleaseName)
		if a.ReleaseName != workload {
			continue
		}
		vals, ok := a.Values.(map[string]any)
		if !ok {
			return "", fmt.Errorf("workload %q in %s has unexpected values shape", workload, env)
		}
		img, _ := vals["image"].(map[string]any)
		repo, _ := img["repository"].(string)
		tag, _ := img["tag"].(string)
		if repo == "" || tag == "" {
			return "", fmt.Errorf("workload %q in %s has no image.repository/image.tag to pin", workload, env)
		}
		switch strings.ToLower(tag) {
		case "latest", "main", "master", "dev":
			return "", fmt.Errorf("workload %q in %s runs mutable tag %q — promotion requires a pinned artifact", workload, env, tag)
		}
		return repo + ":" + tag, nil
	}
	return "", fmt.Errorf("workload %q not found in %s umbrella (have: %s)", workload, env, strings.Join(handles, ", "))
}
