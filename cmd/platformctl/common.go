package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/jakenesler/platformctl/internal/appconfig"
	"github.com/jakenesler/platformctl/internal/clusterenv"
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

// loadApp loads + defaults a deploy.yaml from file.
func loadApp(file string) (appconfig.App, error) {
	if file == "" {
		return appconfig.App{}, fmt.Errorf("--file is required")
	}
	return appconfig.LoadDefaulted(file)
}
