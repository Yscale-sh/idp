package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/render"
)

func TestPromotePreservesSourceRollback(t *testing.T) {
	for _, contractRollback := range []string{"", "rollback: {strategy: auto, dataCompatible: true}\n"} {
		t.Run(contractRollback, func(t *testing.T) {
			root := t.TempDir()
			for _, env := range []string{"dev", "prod"} {
				raw, err := os.ReadFile(filepath.Join("..", "..", "environments", env, "cluster.yaml"))
				if err != nil {
					t.Fatal(err)
				}
				dir := filepath.Join(root, "environments", env)
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(dir, "cluster.yaml"), raw, 0o600); err != nil {
					t.Fatal(err)
				}
			}
			source, err := loadCluster(root, "dev")
			if err != nil {
				t.Fatal(err)
			}
			pr, err := render.ReadPlatform(root, "dev", source)
			if err != nil {
				t.Fatal(err)
			}
			pr.Spec.Values.Apps = []render.AppEntry{{
				Name: "audit", ReleaseName: "audit", Namespace: "audit-dev-api", Component: "api",
				Rollback: &render.RollbackEntry{Strategy: "manual", DataCompatible: false, Note: "drops legacy_id"},
				Values:   map[string]any{"image": map[string]any{"repository": "ghcr.io/example/audit", "tag": "v2.0.0"}},
			}}
			if _, err := render.WritePlatform(root, "dev", pr); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(root, "deploy.yaml")
			contract := "app: audit\nruntime: {image: ghcr.io/example/audit, port: 8080}\nmetrics: {enabled: false}\n" + contractRollback
			if err := os.WriteFile(file, []byte(contract), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := newPromoteCmd()
			var out bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&out)
			cmd.SetArgs([]string{"audit", "prod", "--from", "dev", "--root", root, "--file", file, "--skip-dns"})
			if err := cmd.Execute(); err != nil {
				t.Fatal(err)
			}
			target, err := loadCluster(root, "prod")
			if err != nil {
				t.Fatal(err)
			}
			promoted, err := render.ReadPlatform(root, "prod", target)
			if err != nil {
				t.Fatal(err)
			}
			if len(promoted.Spec.Values.Apps) != 1 {
				t.Fatalf("unexpected apps: %+v", promoted.Spec.Values.Apps)
			}
			rb := promoted.Spec.Values.Apps[0].Rollback
			if rb == nil || rb.Strategy != "manual" || rb.DataCompatible || rb.Note != "drops legacy_id" {
				t.Fatalf("promotion lost the source release rollback boundary: %+v", rb)
			}
			if !strings.Contains(out.String(), "dataCompatible=false") {
				t.Fatal("promotion omitted the source release warning")
			}
		})
	}
}
