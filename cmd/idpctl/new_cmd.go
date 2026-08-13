package main

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/scaffold"
	"github.com/yscale-sh/idp/internal/tenant"
)

func newNewCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "new",
		Short: "Scaffold platform resources",
	}
	cmd.AddCommand(newNewAppCmd())
	return cmd
}

func newNewAppCmd() *cobra.Command {
	var name, host, product, dir, registry, root string
	var port int
	var withDB, stdout bool
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Generate a starter deploy.yaml for a new app",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			reg, err := resolveRegistry(registry, root)
			if err != nil {
				return err
			}
			files, err := scaffold.Generate(scaffold.Options{
				Name: name, Registry: reg, Host: host, Port: port, Product: product, WithDB: withDB,
			})
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			if stdout {
				for rel, data := range files {
					fmt.Fprintf(out, "# === %s ===\n%s\n", rel, string(data))
				}
				return nil
			}
			written, err := scaffold.Write(dir, files)
			if err != nil {
				return err
			}
			for _, p := range written {
				fmt.Fprintf(out, "wrote: %s\n", p)
			}
			fmt.Fprintf(out, "next: idpctl validate --file %s/deploy.yaml\n", dir)
			fmt.Fprintf(out, "      then register the app in the idp-shipper registry to deploy on push\n")
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name (DNS-1123)")
	cmd.Flags().StringVar(&registry, "registry", "", "image registry prefix (e.g. ghcr.io/your-org); defaults from <root>/idp.yaml")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root holding idp.yaml (used when --registry is unset)")
	cmd.Flags().StringVar(&host, "host", "", "primary route host (optional)")
	cmd.Flags().IntVar(&port, "port", 8080, "container port")
	cmd.Flags().StringVar(&product, "product", "", "product group (optional)")
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to write the starter files into")
	cmd.Flags().BoolVar(&withDB, "with-db", false, "include a primary postgres db")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print files to stdout instead of writing")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}

// resolveRegistry returns the image registry prefix: the explicit flag wins;
// otherwise the platform repo's idp.yaml supplies it. There is NO baked-in
// default — identity always comes from the caller or their tenant config.
func resolveRegistry(flag, root string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	t, err := tenant.Load(root)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return "", fmt.Errorf("no registry configured: pass --registry, or run from your platform repo (or point --root at it) so %s supplies one", tenant.File)
		}
		return "", err
	}
	return t.Registry, nil
}
