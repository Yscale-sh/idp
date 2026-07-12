package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// loadCluster loads environments/<env>/cluster.yaml under root, returning nil
// (with no error) when the file is absent so commands degrade gracefully for
// quick local validation. A present-but-invalid file IS an error.
func loadCluster(root, env string) (*clusterenv.Config, error) {
	path := filepath.Join(root, "environments", env, "cluster.yaml")
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}
	return clusterenv.Load(path)
}

// loadApp reads the shopping list WITHOUT applying defaults — callers Expand() it
// into per-component Apps and default/validate/render each (a base carrying
// `components:` is never rendered itself, so defaulting it is wrong). A plain
// single-component file Expands to just itself, so the loop is uniform.
func loadApp(file string) (appconfig.App, error) {
	if file == "" {
		return appconfig.App{}, fmt.Errorf("--file is required")
	}
	return appconfig.Load(file)
}
