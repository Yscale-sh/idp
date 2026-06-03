// Command platformctl is the platform CLI: it validates a developer's
// deploy.yaml, plans/renders desired state for an environment, and emits Argo CD
// Applications for apps and infra modules. Argo CD is the only writer to the
// cluster; "deploying" is a git commit of the rendered desired state.
package main

import (
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}
