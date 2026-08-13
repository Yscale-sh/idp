package main

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/builder"
	"github.com/yscale-sh/idp/internal/kube"
)

// build triggers the in-cluster image-builder to build + push a container image
// (clone -> rootless BuildKit -> push to ghcr), the SAME path the idp-shipper
// uses for app images. It drives a one-off Job via kubectl, so NO local Docker
// is needed and the build runs on an amd64 cluster node with the builder's ghcr
// credentials. The platform thus builds its OWN image (idpctl) through its own
// pipeline instead of a developer's laptop:
//
//	idpctl build --repo Yscale-sh/idp --ref main --image ghcr.io/yscale-sh/idpctl:v2
func newBuildCmd() *cobra.Command {
	var repo, ref, image, buildContext, dockerfile, namespace, kctx string
	var submodules []string
	cmd := &cobra.Command{
		Use:   "build",
		Short: "Build + push an image on the in-cluster image-builder (clone -> BuildKit -> ghcr)",
		Long: `build triggers the cluster image-builder to clone a repo at a ref, build its
Dockerfile with rootless BuildKit, and push the result to ghcr — the same Job the
idp-shipper runs for app images. It drives the Job through kubectl, so it needs
cluster access but NO local Docker, and the build runs in-cluster on amd64 with
the builder's ghcr push credentials. Blocks until the build succeeds or fails.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			out := cmd.OutOrStdout()
			if repo == "" || ref == "" || image == "" {
				return fmt.Errorf("--repo, --ref and --image are required")
			}
			k := kube.New(kctx)
			if !k.Available() {
				return fmt.Errorf("kubectl not found on PATH — build drives an in-cluster Job")
			}
			b := builder.New(k)
			ctx, cancel := context.WithTimeout(context.Background(), 70*time.Minute)
			defer cancel()
			fmt.Fprintf(out, "building %s from %s@%s on the image-builder (ns %s)...\n", image, repo, ref, namespace)
			if err := b.Build(ctx, builder.Spec{
				Repo:       repo,
				Ref:        ref,
				Image:      image,
				Context:    buildContext,
				Dockerfile: dockerfile,
				Submodules: submodules,
				Namespace:  namespace,
			}); err != nil {
				return err
			}
			fmt.Fprintf(out, "✓ built + pushed %s\n", image)
			return nil
		},
	}
	cmd.Flags().StringVar(&repo, "repo", "", "GitHub org/name to clone (e.g. Yscale-sh/idp)")
	cmd.Flags().StringVar(&ref, "ref", "", "git ref to build — a branch or, preferably, a commit SHA")
	cmd.Flags().StringVar(&image, "image", "", "fully-qualified target image repo:tag to push")
	cmd.Flags().StringVar(&buildContext, "build-context", ".", "build context subdir relative to repo root")
	cmd.Flags().StringVar(&dockerfile, "dockerfile", "Dockerfile", "Dockerfile name within the context")
	cmd.Flags().StringSliceVar(&submodules, "submodule", nil, "private submodule path to init before building (repeatable)")
	cmd.Flags().StringVar(&namespace, "namespace", "image-builder", "image-builder namespace")
	cmd.Flags().StringVar(&kctx, "context", "", "kubeconfig context (default: current)")
	return cmd
}
