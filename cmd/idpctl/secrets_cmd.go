package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
	"github.com/yscale-sh/idp/internal/render"
	"github.com/yscale-sh/idp/internal/secrets"
)

// newSecretsCmd is the missing half of the secrets story. A deploy.yaml DECLARES
// the keys an app needs (`secrets:`) and the renderer pins each one as a
// remoteRef under /apps/<app>/<env>/ — but nothing ever CREATES the parameters,
// so the first prod deploy of a new app resolves nothing and the ExternalSecret
// fails on a key no one wrote.
//
// `plan` answers "which paths does this app need, and which are missing";
// `create` writes the missing ones from a value the operator supplies. It never
// invents a value — no generated passwords, no defaults — and never prints one:
// output is paths and status only.
func newSecretsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "secrets",
		Short: "Plan and create the SSM parameters an app's deploy.yaml declares",
		Long: `secrets reconciles AWS SSM against an app's deploy.yaml shopping list.

  plan     list every SSM path this app needs and which ones are MISSING (read-only)
  create   create the missing ones from an EXPLICIT value source

Path convention (docs/ENVIRONMENTS.md):
  /apps/<app>/<env>/<KEY>   one parameter per declared secrets: key
  /shared/<group>/<key>     platform-owned groups (cloudflare, tailscale, ...)

Values come from --from-env (read $KEY out of this process's environment) or
--set KEY=VALUE. Nothing is ever generated, and no value is ever printed, logged,
or put in an error — output is paths and status. Prefer --from-env: --set leaves
the value in your shell history.

AWS access is whatever the ambient environment already has (AWS_PROFILE /
AWS_REGION / instance role); this shells the aws CLI rather than carrying an SDK,
the same way the platform shells kubectl and helm.`,
	}
	cmd.AddCommand(newSecretsPlanCmd(), newSecretsCreateCmd())
	return cmd
}

type secretsOpts struct {
	file, env, root, paramType string
	fromEnv, set               []string
	dryRun, overwrite          bool
}

func newSecretsPlanCmd() *cobra.Command {
	var o secretsOpts
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "List the SSM paths this app needs and which are MISSING (read-only)",
		RunE:  func(cmd *cobra.Command, _ []string) error { return runSecretsPlan(cmd, o) },
	}
	addSecretsFlags(cmd, &o)
	return cmd
}

func newSecretsCreateCmd() *cobra.Command {
	var o secretsOpts
	cmd := &cobra.Command{
		Use:   "create",
		Short: "Create the missing SSM parameters from an explicit value source",
		Long: `create writes the parameters this app's deploy.yaml declares but SSM does not
have yet. Every value must be supplied explicitly:

  --from-env DATABASE_URL                    read $DATABASE_URL
  --from-env /shared/tailscale/auth-key=TS_KEY   read $TS_KEY into a shared path
  --set DATABASE_URL=postgres://...          inline (ends up in shell history)

A key that is not in the plan is refused, so a typo cannot create a parameter
nothing reads. An existing parameter is never overwritten without --overwrite,
and the run refuses up front — before writing anything — if it would.`,
		RunE: func(cmd *cobra.Command, _ []string) error { return runSecretsCreate(cmd, o) },
	}
	addSecretsFlags(cmd, &o)
	// StringArray, not StringSlice: a secret value may contain commas, and
	// StringSlice would silently split it into several values.
	cmd.Flags().StringArrayVar(&o.fromEnv, "from-env", nil, "read a value from the environment: KEY (reads $KEY) or NAME=ENV_VAR (repeatable)")
	cmd.Flags().StringArrayVar(&o.set, "set", nil, "supply a value inline: KEY=VALUE (repeatable; prefer --from-env, this lands in shell history)")
	cmd.Flags().BoolVar(&o.dryRun, "dry-run", false, "report what would be written without touching SSM")
	cmd.Flags().BoolVar(&o.overwrite, "overwrite", false, "replace parameters that already exist (off: an existing parameter is refused)")
	cmd.Flags().StringVar(&o.paramType, "type", ssmSecureString, "SSM parameter type: SecureString (default, encrypted) or String")
	return cmd
}

func addSecretsFlags(cmd *cobra.Command, o *secretsOpts) {
	cmd.Flags().StringVarP(&o.file, "file", "f", "deploy.yaml", "path to deploy.yaml")
	cmd.Flags().StringVarP(&o.env, "env", "e", appconfig.EnvProd, "target environment (dev|prod)")
	cmd.Flags().StringVar(&o.root, "root", ".", "platform repo root (holds environments/) — supplies the env's secrets backend and its platform-provisioned keys")
}

const (
	ssmSecureString = "SecureString"
	ssmString       = "String"
	sharedPrefix    = "/shared/"
)

// errNotSSM means this environment does not resolve secrets from SSM, so there
// are no parameters here to plan or create.
var errNotSSM = errors.New("not an SSM environment")

// secretPath is one SSM parameter the app's runtime Secret resolves.
type secretPath struct {
	Path string
	// Key is the runtime Secret key this path feeds (DATABASE_URL, TUNNEL_TOKEN).
	Key string
	// Shared marks a /shared/<group>/* parameter. Those are platform-owned and
	// read by every app in the group, so one app's create is not the place to
	// decide their value.
	Shared bool
}

// planSecretPaths returns every SSM parameter the app resolves in env, app root
// first then shared groups.
//
// The declared `secrets:` keys come straight from the shopping list, so this
// works from an app repo with no platform checkout. When the platform repo IS
// present the renderer is asked too: it decides the app's actual remoteRefs, so
// it contributes the paths a developer never writes down — the per-app
// TUNNEL_TOKEN and the shared R2 / Tailscale keys.
func planSecretPaths(app appconfig.App, env string, c *clusterenv.Config) []secretPath {
	seen := map[string]bool{}
	var out []secretPath
	add := func(path, key string) {
		if path == "" || seen[path] {
			return
		}
		seen[path] = true
		out = append(out, secretPath{Path: path, Key: key, Shared: strings.HasPrefix(path, sharedPrefix)})
	}
	// Components share one app-level SSM root, so their key lists union.
	for _, comp := range app.Expand() {
		comp.ApplyDefaults()
		root := comp.SSMRoot(env)
		for _, key := range comp.Secrets {
			add(root+"/"+key, key)
		}
		if c == nil {
			continue
		}
		for _, ref := range render.BuildExternalSecret(comp, env, c).RemoteRefs {
			// A remoteRef is backend-shaped: against the local store (ESO's
			// Kubernetes provider) `key` is a Secret NAME, not a path. Only an
			// absolute key is an SSM parameter.
			if k := ref.RemoteRef["key"]; strings.HasPrefix(k, "/") {
				add(k, ref.SecretKey)
			}
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Shared != out[j].Shared {
			return !out[i].Shared
		}
		return out[i].Path < out[j].Path
	})
	return out
}

func pathsOf(paths []secretPath) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		out = append(out, p.Path)
	}
	return out
}

// loadSecretsPlan resolves the app and the SSM paths it needs in this env.
// Advisories go to notes (stderr) so stdout stays a clean list of paths.
func loadSecretsPlan(o secretsOpts, notes io.Writer) (appconfig.App, []secretPath, error) {
	app, err := loadApp(o.file)
	if err != nil {
		return app, nil, err
	}
	c, err := loadCluster(o.root, o.env)
	if err != nil {
		return app, nil, fmt.Errorf("load %s cluster env: %w", o.env, err)
	}
	if c != nil {
		// secrets.Plan, not c.Secrets.Backend: an app may override the env default
		// (secretStore: ssm in a local env), and then it really does read SSM.
		if backend := secrets.Plan(app, o.env, c).Backend; backend != clusterenv.BackendSSM {
			return app, nil, fmt.Errorf("%w: %s resolves secrets from the %q backend (store %s)",
				errNotSSM, o.env, backend, c.Secrets.StoreRef.Name)
		}
	} else {
		fmt.Fprintf(notes, "note: no environments/%s/cluster.yaml under %s — planning the keys deploy.yaml declares; platform-provisioned paths (TUNNEL_TOKEN, /shared/*) are not resolved from here\n", o.env, o.root)
	}
	return app, planSecretPaths(app, o.env, c), nil
}

func runSecretsPlan(cmd *cobra.Command, o secretsOpts) error {
	out, notes := cmd.OutOrStdout(), cmd.OutOrStderr()
	app, paths, err := loadSecretsPlan(o, notes)
	if errors.Is(err, errNotSSM) {
		fmt.Fprintf(out, "secrets plan: %v — nothing to plan here\n", err)
		return nil
	}
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Fprintf(out, "secrets plan: %s declares no secret keys for %s — nothing to plan\n", app.App, o.env)
		return nil
	}

	s := newSSMCLI()
	var missing map[string]bool
	if s.available() {
		if missing, err = s.missing(cmd.Context(), pathsOf(paths)); err != nil {
			return err
		}
	} else {
		fmt.Fprintf(notes, "note: aws CLI not found on PATH — listing the paths without checking which exist\n")
	}

	fmt.Fprintf(out, "secrets plan: %s / %s (root %s)\n", app.App, o.env, app.SSMRoot(o.env))
	for _, p := range paths {
		status := "unknown"
		if missing != nil {
			status = "present"
			if missing[p.Path] {
				status = "MISSING"
			}
		}
		fmt.Fprintf(out, "  %-7s  %s%s\n", status, p.Path, sharedNote(p))
	}
	if missing == nil {
		return nil
	}
	fmt.Fprintf(out, "%d of %d missing\n", len(missing), len(paths))
	if len(missing) > 0 {
		fmt.Fprintf(out, "create them with: idpctl secrets create -f %s --env %s --from-env <KEY>\n", o.file, o.env)
	}
	return nil
}

func runSecretsCreate(cmd *cobra.Command, o secretsOpts) error {
	out := cmd.OutOrStdout()
	if o.paramType != ssmSecureString && o.paramType != ssmString {
		return fmt.Errorf("--type %q must be %s or %s", o.paramType, ssmSecureString, ssmString)
	}
	app, paths, err := loadSecretsPlan(o, cmd.OutOrStderr())
	if err != nil {
		return err
	}
	if len(paths) == 0 {
		fmt.Fprintf(out, "secrets create: %s declares no secret keys for %s — nothing to create\n", app.App, o.env)
		return nil
	}
	values, err := resolveSecretValues(paths, o.fromEnv, o.set, os.LookupEnv, app.App, o.env)
	if err != nil {
		return err
	}
	if len(values) == 0 {
		return fmt.Errorf("create needs an explicit value source: --from-env KEY reads $KEY, --set KEY=VALUE takes it inline. Values are never generated — run `idpctl secrets plan -f %s --env %s` for the key list", o.file, o.env)
	}

	s := newSSMCLI()
	if !s.available() {
		return fmt.Errorf("aws CLI not found on PATH — create shells `aws ssm put-parameter` (install it, or write the paths `idpctl secrets plan` lists yourself)")
	}
	ctx := cmd.Context()
	missing, err := s.missing(ctx, pathsOf(paths))
	if err != nil {
		return err
	}

	// Refuse the whole run before writing anything when a supplied value would
	// replace a parameter that exists: a half-applied create leaves the operator
	// guessing which keys took.
	if !o.overwrite {
		var clobber []string
		for _, v := range values {
			if !missing[v.Path] {
				clobber = append(clobber, v.Path)
			}
		}
		if len(clobber) > 0 {
			return fmt.Errorf("already in SSM and --overwrite was not passed, so nothing was written: %s", strings.Join(clobber, ", "))
		}
	}

	for _, v := range values {
		verb, done := "create", "created"
		if !missing[v.Path] {
			verb, done = "overwrite", "overwrote"
		}
		if o.dryRun {
			fmt.Fprintf(out, "[dry-run] would %s  %s  %s (value from %s)\n", verb, v.Path, o.paramType, v.Source)
			continue
		}
		if err := s.put(ctx, putParameterInput{Name: v.Path, Value: v.value, Type: o.paramType, Overwrite: o.overwrite}); err != nil {
			return err
		}
		fmt.Fprintf(out, "%s  %s  %s (value from %s)\n", done, v.Path, o.paramType, v.Source)
	}

	supplied := map[string]bool{}
	for _, v := range values {
		supplied[v.Path] = true
	}
	tunnelPending := false
	for _, p := range paths {
		if !missing[p.Path] || supplied[p.Path] {
			continue
		}
		tunnelPending = tunnelPending || p.Key == "TUNNEL_TOKEN"
		fmt.Fprintf(out, "still MISSING  %s%s (no value supplied)\n", p.Path, sharedNote(p))
	}
	if tunnelPending {
		// TUNNEL_TOKEN is minted by Cloudflare, not chosen — an operator hunting
		// for a value to type will not find one.
		fmt.Fprintf(out, "hint: TUNNEL_TOKEN is minted by `idpctl tunnel up --token-out <file>`, then fed back with --from-env\n")
	}
	return nil
}

func sharedNote(p secretPath) string {
	if p.Shared {
		return "  (shared group: platform-owned, one value for every app)"
	}
	return ""
}

// secretValue is one operator-supplied value bound to a planned path.
type secretValue struct {
	Path string
	// Source describes WHERE the value came from ("$DATABASE_URL", "--set"), so
	// the report can be specific without quoting the value.
	Source string
	value  string
}

// String keeps the value out of any accidental %v of this struct.
func (v secretValue) String() string { return v.Path + " (" + v.Source + ")" }

// resolveSecretValues binds each --from-env / --set entry to a planned path.
//
// A name is either a runtime Secret key under the app root (DATABASE_URL) or a
// full path for a shared group (/shared/tailscale/auth-key) — one shared path
// backs several app keys, so a key alone would not say which parameter to write.
// A name that is not in the plan is an ERROR rather than a new parameter: a typo
// must not leave junk in SSM that nothing reads.
func resolveSecretValues(paths []secretPath, fromEnv, set []string, lookupEnv func(string) (string, bool), app, env string) ([]secretValue, error) {
	byKey := map[string]secretPath{}
	byPath := map[string]secretPath{}
	for _, p := range paths {
		byPath[p.Path] = p
		if !p.Shared {
			byKey[p.Key] = p
		}
	}
	bind := func(name string) (secretPath, error) {
		if strings.HasPrefix(name, "/") {
			if p, ok := byPath[name]; ok {
				return p, nil
			}
			return secretPath{}, fmt.Errorf("%s is not a path %s needs in %s — run `idpctl secrets plan` for the list", name, app, env)
		}
		if p, ok := byKey[name]; ok {
			return p, nil
		}
		return secretPath{}, fmt.Errorf("%s is not a key %s declares for %s — add it to deploy.yaml `secrets:` first, or pass the full /shared/... path", name, app, env)
	}

	var out []secretValue
	taken := map[string]string{}
	claim := func(p secretPath, source string) error {
		if prev, ok := taken[p.Path]; ok {
			return fmt.Errorf("%s got a value twice (%s and %s)", p.Path, prev, source)
		}
		taken[p.Path] = source
		return nil
	}

	for _, spec := range fromEnv {
		name, varName := spec, spec
		if i := strings.Index(spec, "="); i >= 0 {
			name, varName = spec[:i], spec[i+1:]
		} else if strings.HasPrefix(spec, "/") {
			return nil, fmt.Errorf("--from-env %s: a shared path needs the variable to read, as --from-env %s=ENV_VAR", spec, spec)
		}
		p, err := bind(name)
		if err != nil {
			return nil, fmt.Errorf("--from-env %s: %w", spec, err)
		}
		val, ok := lookupEnv(varName)
		if !ok || val == "" {
			return nil, fmt.Errorf("--from-env %s: $%s is not set (nothing is ever generated)", spec, varName)
		}
		source := "$" + varName
		if err := claim(p, source); err != nil {
			return nil, err
		}
		out = append(out, secretValue{Path: p.Path, Source: source, value: val})
	}

	for _, spec := range set {
		i := strings.Index(spec, "=")
		if i <= 0 {
			// The spec IS the secret when the key is missing or mistyped, so it
			// must never be echoed. This command's whole contract is that a value
			// never reaches stdout, a log, or an error.
			return nil, fmt.Errorf("--set: expected KEY=VALUE (the value is not shown)")
		}
		name, val := spec[:i], spec[i+1:]
		p, err := bind(name)
		if err != nil {
			return nil, fmt.Errorf("--set %s: %w", name, err)
		}
		if val == "" {
			return nil, fmt.Errorf("--set %s: the value is empty", name)
		}
		if err := claim(p, "--set"); err != nil {
			return nil, err
		}
		out = append(out, secretValue{Path: p.Path, Source: "--set", value: val})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// runner executes one external command. It is the injection seam: tests drive
// the exact aws invocation with no AWS in reach.
type runner func(ctx context.Context, name string, args ...string) (stdout, stderr []byte, err error)

func execRunner(ctx context.Context, name string, args ...string) ([]byte, []byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut
	err := cmd.Run()
	return out.Bytes(), errOut.Bytes(), err
}

// ssmCLI is a thin wrapper over `aws ssm`, following the platform's shell-out
// idiom (kube, helmrunner) instead of vendoring an AWS SDK. Credentials and
// region come from the ambient environment, as they do for every other AWS step.
type ssmCLI struct {
	bin string
	run runner
}

func newSSMCLI() *ssmCLI { return &ssmCLI{bin: "aws", run: execRunner} }

func (s *ssmCLI) available() bool {
	_, err := exec.LookPath(s.bin)
	return err == nil
}

// ssmBatch is GetParameters' hard limit of names per call.
const ssmBatch = 10

// missing returns which of paths SSM does not have.
//
// --query is not cosmetic: GetParameters answers WITH the parameters, and this
// makes the aws CLI print only the names it could not find — so no secret value
// is ever written to a pipe, a log, or this process's memory.
func (s *ssmCLI) missing(ctx context.Context, paths []string) (map[string]bool, error) {
	out := map[string]bool{}
	for start := 0; start < len(paths); start += ssmBatch {
		end := min(start+ssmBatch, len(paths))
		args := append([]string{"ssm", "get-parameters", "--output", "json", "--query", "InvalidParameters", "--names"}, paths[start:end]...)
		stdout, stderr, err := s.run(ctx, s.bin, args...)
		if err != nil {
			detail := strings.TrimSpace(string(stderr))
			if detail == "" {
				detail = err.Error()
			}
			return nil, fmt.Errorf("aws ssm get-parameters (check AWS_PROFILE/AWS_REGION and read access): %s", detail)
		}
		var invalid []string
		if err := json.Unmarshal(stdout, &invalid); err != nil {
			return nil, fmt.Errorf("aws ssm get-parameters returned output this cannot read: %w", err)
		}
		for _, name := range invalid {
			out[name] = true
		}
	}
	return out, nil
}

// putParameterInput is PutParameter's request shape, passed as --cli-input-json.
type putParameterInput struct {
	Name      string `json:"Name"`
	Value     string `json:"Value"`
	Type      string `json:"Type"`
	Overwrite bool   `json:"Overwrite"`
}

// put writes one parameter. Overwrite is sent with the request so AWS refuses a
// clobber server-side as well — the caller's existence check can always be one
// write out of date.
func (s *ssmCLI) put(ctx context.Context, in putParameterInput) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	file, cleanup, err := stageRequest(body)
	if err != nil {
		return err
	}
	defer cleanup()
	_, stderr, err := s.run(ctx, s.bin, "ssm", "put-parameter", "--cli-input-json", "file://"+file)
	if err != nil {
		detail := strings.TrimSpace(string(stderr))
		if detail == "" {
			detail = err.Error()
		}
		return fmt.Errorf("aws ssm put-parameter %s: %s", in.Name, redactValue(detail, in.Value))
	}
	return nil
}

// stageRequest hands the PutParameter body to the aws CLI without ever putting
// the value where another process can read it.
//
// The value cannot be an argument: argv is world-readable through /proc. It
// cannot come in on stdin either — `--cli-input-json file:///dev/stdin` reads
// empty and fails "Invalid JSON received" on aws-cli v2 (measured on 2.36).
// What is left is a file only this user can open (os.CreateTemp makes it 0600),
// deleted as soon as the call returns.
func stageRequest(body []byte) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "idpctl-ssm-*.json")
	if err != nil {
		return "", nil, fmt.Errorf("stage the put-parameter request: %w", err)
	}
	path = f.Name()
	cleanup = func() { os.Remove(path) }
	if _, err := f.Write(body); err != nil {
		f.Close()
		cleanup()
		return "", nil, fmt.Errorf("stage the put-parameter request: %w", err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return "", nil, fmt.Errorf("stage the put-parameter request: %w", err)
	}
	return path, cleanup, nil
}

// redactValue scrubs a secret out of text bound for the operator. The aws CLI
// quotes its input back on some validation errors, and nothing this command
// prints may carry a value.
func redactValue(text, value string) string {
	if value == "" {
		return text
	}
	return strings.ReplaceAll(text, value, "[redacted]")
}
