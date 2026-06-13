package main

import (
	"github.com/spf13/cobra"
)

// version is idpctl's version, stamped at build time:
//
//	go build -ldflags "-X main.version=v1.2.3" ./cmd/idpctl
//
// Defaults to "dev" for local/source builds. Exposed via `idpctl --version`.
var version = "dev"

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:     "idpctl",
		Short:   "Internal Developer Platform CLI for self-hosted k3s",
		Version: version,
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
		newCatalogCmd(),
		newRemoveCmd(),
		newDNSCmd(),
		newTunnelCmd(),
		newInfraCmd(),
		newNewCmd(),
	)
	return root
}
