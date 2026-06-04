package main

import (
	"fmt"

	"github.com/jakenesler/jdp/internal/scaffold"
	"github.com/spf13/cobra"
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
	var name, host, product, dir string
	var port int
	var withDB, stdout bool
	cmd := &cobra.Command{
		Use:   "app",
		Short: "Generate a starter deploy.yaml + ship.yml for a new app",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			files, err := scaffold.Generate(scaffold.Options{
				Name: name, Host: host, Port: port, Product: product, WithDB: withDB,
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
			fmt.Fprintf(out, "next: jdpctl validate --file %s/deploy.yaml\n", dir)
			return nil
		},
	}
	cmd.Flags().StringVar(&name, "name", "", "app name (DNS-1123)")
	cmd.Flags().StringVar(&host, "host", "", "primary route host (optional)")
	cmd.Flags().IntVar(&port, "port", 8080, "container port")
	cmd.Flags().StringVar(&product, "product", "", "product group (optional)")
	cmd.Flags().StringVar(&dir, "dir", ".", "directory to write the starter files into")
	cmd.Flags().BoolVar(&withDB, "with-db", false, "include a primary postgres db")
	cmd.Flags().BoolVar(&stdout, "stdout", false, "print files to stdout instead of writing")
	_ = cmd.MarkFlagRequired("name")
	return cmd
}
