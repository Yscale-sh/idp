package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "platformctl",
		Short: "Internal Developer Platform CLI for self-hosted k3s",
		Long: `platformctl renders a developer's deploy.yaml into desired state
(Argo CD Applications + Helm values) for an environment.

The default path is GitOps: render -> git commit -> Argo CD reconciles.
platformctl never runs helm/kubectl against the cluster on the default path.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newValidateCmd(),
		newPlanCmd(),
		newRenderCmd(),
		newInfraCmd(),
		newNewCmd(),
	)
	return root
}
