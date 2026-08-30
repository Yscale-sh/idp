package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/scaffold"
	"github.com/yscale-sh/idp/internal/tenant"
)

// init generates a starter environments/<env>/cluster.yaml for a platform
// instance, wiring in the fork's OWN identity (flux.repoURL/branch) from
// idp.yaml. It is the first step after forking: `idpctl init` then
// `idpctl infra render` + `idpctl render` produce the clusters/ desired state.
//
// It deliberately does NOT copy anyone's cluster specifics (IPs, module matrix,
// registry orgs) — the template is generic and heavily commented so the fork
// fills in only what its cluster backs. Fail-closed: needs idp.yaml for repo
// identity (or pass --repo-url); refuses to overwrite an existing cluster.yaml.
func newInitCmd() *cobra.Command {
	var env, root, repoURL, branch, backend, store string
	var force, stdout bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Generate a starter environments/<env>/cluster.yaml for this platform instance",
		Long: `init scaffolds a per-environment cluster.yaml from your platform identity
(idp.yaml) so a fork doesn't hand-author one or inherit another instance's
cluster specifics. The output is generic + heavily commented (LAN/in-cluster
only, no modules, no public routes) and is validated before it is written.

Typical first run after forking:
  idpctl init --env dev
  idpctl infra render --env dev   # render enabled modules (none yet)
  # add your apps, then: idpctl render --env dev --file <app>/deploy.yaml --image ...`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			url, br, err := resolveRepo(repoURL, branch, root)
			if err != nil {
				return err
			}
			files, err := scaffold.GenerateEnv(scaffold.EnvOptions{
				Env: env, RepoURL: url, Branch: br, Backend: backend, StoreName: store,
			})
			if err != nil {
				return err
			}
			if stdout {
				for rel, data := range files {
					fmt.Fprintf(out, "# === %s ===\n%s\n", rel, string(data))
				}
				return nil
			}
			// Honor --force by removing an existing target first (scaffold.Write
			// refuses to overwrite); otherwise let it guard.
			if force {
				for rel := range files {
					_ = os.Remove(filepath.Join(root, rel))
				}
			}
			written, err := scaffold.Write(root, files)
			if err != nil {
				return fmt.Errorf("%w (pass --force to overwrite)", err)
			}
			for _, p := range written {
				fmt.Fprintf(out, "wrote: %s\n", p)
			}
			fmt.Fprintf(out, "next: review it, then `idpctl infra render --env %s`\n", env)
			return nil
		},
	}
	cmd.Flags().StringVarP(&env, "env", "e", "dev", "environment name to scaffold (dev|stage|prod or any)")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root (holds idp.yaml; cluster.yaml is written under it)")
	cmd.Flags().StringVar(&repoURL, "repo-url", "", "flux.repoURL override (defaults from <root>/idp.yaml)")
	cmd.Flags().StringVar(&branch, "branch", "", "flux.branch override (defaults from idp.yaml, else main)")
	cmd.Flags().StringVar(&backend, "backend", "", "secrets backend: local|ssm (default: dev=local, else ssm)")
	cmd.Flags().StringVar(&store, "store", "", "secrets.storeRef.name (default by backend)")
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing cluster.yaml")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print to stdout instead of writing")
	return cmd
}

// resolveRepo returns the platform repo URL + branch: explicit flags win;
// otherwise idp.yaml supplies them. No baked-in default — repo identity always
// comes from the caller or their tenant config (so a fork can't sync from
// someone else's repo by accident).
func resolveRepo(flagURL, flagBranch, root string) (url, branch string, err error) {
	if flagURL != "" {
		b := flagBranch
		if b == "" {
			b = "main"
		}
		return flagURL, b, nil
	}
	t, err := tenant.Load(root)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return "", "", fmt.Errorf("no platform repo configured: pass --repo-url, or run from your platform repo (or point --root at it) so %s supplies one", tenant.File)
		}
		return "", "", err
	}
	branch = flagBranch
	if branch == "" {
		branch = t.Repo.Branch
	}
	return t.Repo.URL, branch, nil
}
