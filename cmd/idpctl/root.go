package main

import (
	"github.com/spf13/cobra"
)

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "idpctl",
		Short: "Internal Developer Platform CLI for self-hosted k3s",
		Long: `idpctl renders a developer's deploy.yaml into desired state
(Flux HelmReleases + Helm values) for an environment.

The default path is GitOps: render -> git commit -> Flux reconciles.
idpctl never runs helm/kubectl against the cluster on the default path.`,
		SilenceUsage:  true,
		SilenceErrors: true,
	}

	root.AddCommand(
		newValidateCmd(),
		newPlanCmd(),
		newRenderCmd(),
		newPromoteCmd(),
		newBuildCmd(),
		newDoctorCmd(),
		newRemoveCmd(),
		newDNSCmd(),
		newTunnelCmd(),
		newInfraCmd(),
		newNewCmd(),
	)
	return root
}
