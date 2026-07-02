package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yscale-sh/idp/internal/catalog"
	"github.com/spf13/cobra"
)

// newCatalogCmd builds `idpctl catalog` — a READ-ONLY viewer over the committed
// desired state (clusters/<env>/platform.yaml). It never touches a cluster and
// never writes platform state; it only projects what `render`/`infra render`
// already wrote into a glanceable text page, a self-contained HTML viewer, or
// JSON. The "something to see" on top of a CLI+GitOps platform.
func newCatalogCmd() *cobra.Command {
	var env, root, format, out, outDir string
	var all bool
	cmd := &cobra.Command{
		Use:   "catalog",
		Short: "Read-only view of apps + modules — one env (text|html|json) or a whole-platform site",
		Long: `catalog renders a read-only view of clusters/<env>/platform.yaml — the
committed desired state Flux reconciles. It is a projection, never a writer:
catalog does not talk to a cluster.

  idpctl catalog --env dev                                    # quick terminal view
  idpctl catalog --env dev --format html --out catalog.html   # self-contained page
  idpctl catalog --env dev --format json                      # machine-readable model
  idpctl catalog --all --out-dir public                       # every env + index.html (a site)`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if all {
				envs, err := catalog.BuildSite(root, outDir)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "wrote catalog site for %d env(s) (%v) to %s/ (open %s)\n",
					len(envs), envs, outDir, filepath.Join(outDir, "index.html"))
				return nil
			}

			if env == "" {
				return fmt.Errorf("either --env or --all is required")
			}
			c, err := catalog.Load(root, env)
			if err != nil {
				return err
			}

			var body []byte
			switch format {
			case "text", "":
				if out == "" {
					c.WriteText(cmd.OutOrStdout())
					return nil
				}
				var sb stringWriter
				c.WriteText(&sb)
				body = []byte(sb.String())
			case "html":
				body, err = catalog.RenderHTML(c)
			case "json":
				body, err = catalog.JSON(c)
			default:
				return fmt.Errorf("unknown --format %q (want text|html|json)", format)
			}
			if err != nil {
				return err
			}

			if out == "" {
				_, err = cmd.OutOrStdout().Write(body)
				return err
			}
			if err := os.WriteFile(out, body, 0o644); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "wrote %s view of env %s to %s\n", format, env, out)
			return nil
		},
	}
	cmd.Flags().StringVarP(&env, "env", "e", "", "target environment (dev|prod|local)")
	cmd.Flags().StringVar(&root, "root", ".", "platform repo root")
	cmd.Flags().StringVar(&format, "format", "text", "output format: text|html|json")
	cmd.Flags().StringVarP(&out, "out", "o", "", "write to a file instead of stdout")
	cmd.Flags().BoolVar(&all, "all", false, "render every environment as a site (with index.html)")
	cmd.Flags().StringVar(&outDir, "out-dir", "public", "output directory for --all site")
	cmd.MarkFlagsMutuallyExclusive("env", "all")
	cmd.MarkFlagsMutuallyExclusive("out", "all")
	return cmd
}

// stringWriter is a tiny io.Writer that accumulates into a string (for capturing
// the text view when writing to a file).
type stringWriter struct{ b []byte }

func (s *stringWriter) Write(p []byte) (int, error) { s.b = append(s.b, p...); return len(p), nil }
func (s *stringWriter) String() string              { return string(s.b) }
