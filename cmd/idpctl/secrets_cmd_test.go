package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yscale-sh/idp/internal/appconfig"
	"github.com/yscale-sh/idp/internal/clusterenv"
)

// fakeAWS records every invocation so a test can assert the exact aws command
// line — and that a secret value never appears in it. Nothing here touches AWS.
type fakeAWS struct {
	calls  []fakeCall
	stdout []byte
	stderr []byte
	err    error
}

type fakeCall struct {
	args []string
	// body is the --cli-input-json file read AT CALL TIME: put deletes it as soon
	// as aws returns, so a later read would find nothing.
	body     []byte
	bodyPath string
	bodyMode os.FileMode
}

func (f *fakeAWS) run(_ context.Context, name string, args ...string) ([]byte, []byte, error) {
	call := fakeCall{args: append([]string{name}, args...)}
	for _, a := range args {
		if !strings.HasPrefix(a, "file://") {
			continue
		}
		call.bodyPath = strings.TrimPrefix(a, "file://")
		call.body, _ = os.ReadFile(call.bodyPath)
		if fi, err := os.Stat(call.bodyPath); err == nil {
			call.bodyMode = fi.Mode().Perm()
		}
	}
	f.calls = append(f.calls, call)
	return f.stdout, f.stderr, f.err
}

func secretsApp() appconfig.App {
	return appconfig.App{
		App:     "carshowdb",
		Runtime: appconfig.Runtime{Image: "ghcr.io/example-org/example-api", Port: 8080},
		Secrets: []string{"DATABASE_URL", "JWT_SECRET"},
	}
}

func ssmEnv() *clusterenv.Config {
	return &clusterenv.Config{
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendSSM,
			StoreRef: clusterenv.StoreRef{Name: "platform-ssm", Kind: clusterenv.KindClusterSecretStore},
		},
	}
}

// The declared keys alone must plan correctly with NO platform checkout: an app
// repo holds a deploy.yaml and nothing else, and that is the whole point of the
// command.
func TestPlanSecretPathsFromDeclaredKeysOnly(t *testing.T) {
	got := planSecretPaths(secretsApp(), "prod", nil)
	want := []string{
		"/apps/carshowdb/prod/DATABASE_URL",
		"/apps/carshowdb/prod/JWT_SECRET",
	}
	if diff := pathsOf(got); !equalStrings(diff, want) {
		t.Fatalf("paths = %v, want %v", diff, want)
	}
	for _, p := range got {
		if p.Shared {
			t.Errorf("%s must not be marked shared", p.Path)
		}
	}
}

// Components share ONE app-level SSM root (App.SSMRoot is app-level, not
// per-workload), so their key lists union into a single parameter list.
func TestPlanSecretPathsUnionsComponents(t *testing.T) {
	app := secretsApp()
	app.Secrets = []string{"DATABASE_URL"}
	app.Components = []appconfig.Component{
		{Component: "api", Secrets: []string{"JWT_SECRET"}},
		{Component: "scanner", Secrets: []string{"GEMINI_API_KEY", "JWT_SECRET"}},
	}
	want := []string{
		"/apps/carshowdb/prod/DATABASE_URL",
		"/apps/carshowdb/prod/GEMINI_API_KEY",
		"/apps/carshowdb/prod/JWT_SECRET",
	}
	if got := pathsOf(planSecretPaths(app, "prod", nil)); !equalStrings(got, want) {
		t.Fatalf("paths = %v, want %v", got, want)
	}
}

// With the platform repo in hand the renderer contributes the paths a developer
// never writes down — here the shared R2 credentials a storage bucket pulls.
// Those are platform-owned, and the plan has to say so.
func TestPlanSecretPathsIncludesSharedGroupsFromRenderer(t *testing.T) {
	app := secretsApp()
	app.Storage = []appconfig.Storage{{Name: "uploads", Bucket: "carshowdb-uploads"}}

	got := planSecretPaths(app, "prod", ssmEnv())
	shared := map[string]bool{}
	for _, p := range got {
		if p.Shared {
			shared[p.Path] = true
		}
	}
	for _, want := range []string{"/shared/cloudflare/r2-access-key-id", "/shared/cloudflare/r2-secret-access-key"} {
		if !shared[want] {
			t.Errorf("plan is missing shared path %s; got %v", want, pathsOf(got))
		}
	}
	// App-root paths sort ahead of shared ones so the operator's own keys read first.
	if got[0].Path != "/apps/carshowdb/prod/DATABASE_URL" {
		t.Errorf("app-root paths must sort first, got %s", got[0].Path)
	}
}

// A local-backend env's remoteRefs are Secret NAMES, not paths (ESO's Kubernetes
// provider). Treating one as an SSM path would plan a parameter that no
// environment reads.
func TestPlanSecretPathsSkipsLocalBackendRefs(t *testing.T) {
	local := &clusterenv.Config{
		Secrets: clusterenv.SecretsConfig{
			Backend:  clusterenv.BackendLocal,
			StoreRef: clusterenv.StoreRef{Name: "platform-local", Kind: clusterenv.KindClusterSecretStore},
		},
	}
	for _, p := range planSecretPaths(secretsApp(), "dev", local) {
		if !strings.HasPrefix(p.Path, "/") {
			t.Errorf("planned a non-path parameter %q", p.Path)
		}
	}
}

func TestSSMMissingDiffsAgainstInvalidParameters(t *testing.T) {
	f := &fakeAWS{stdout: []byte(`["/apps/carshowdb/prod/JWT_SECRET"]`)}
	s := &ssmCLI{bin: "aws", run: f.run}

	missing, err := s.missing(context.Background(), []string{
		"/apps/carshowdb/prod/DATABASE_URL",
		"/apps/carshowdb/prod/JWT_SECRET",
	})
	if err != nil {
		t.Fatalf("missing: %v", err)
	}
	if len(missing) != 1 || !missing["/apps/carshowdb/prod/JWT_SECRET"] {
		t.Fatalf("missing = %v, want just JWT_SECRET", missing)
	}
	// The query is what keeps parameter VALUES from ever reaching stdout.
	line := strings.Join(f.calls[0].args, " ")
	if !strings.Contains(line, "--query InvalidParameters") {
		t.Errorf("get-parameters must query InvalidParameters only, got %q", line)
	}
	if strings.Contains(line, "--with-decryption") {
		t.Errorf("get-parameters must never decrypt: %q", line)
	}
}

// GetParameters takes at most 10 names, so a bigger plan has to be chunked or
// AWS rejects the call.
func TestSSMMissingBatchesAtTen(t *testing.T) {
	f := &fakeAWS{stdout: []byte(`[]`)}
	s := &ssmCLI{bin: "aws", run: f.run}
	var paths []string
	for i := range 23 {
		paths = append(paths, "/apps/carshowdb/prod/KEY"+string(rune('A'+i)))
	}
	if _, err := s.missing(context.Background(), paths); err != nil {
		t.Fatalf("missing: %v", err)
	}
	if len(f.calls) != 3 {
		t.Fatalf("23 paths must batch into 3 calls, got %d", len(f.calls))
	}
	for i, c := range f.calls {
		names := 0
		counting := false
		for _, a := range c.args {
			if counting {
				names++
			}
			counting = counting || a == "--names"
		}
		if names > ssmBatch {
			t.Errorf("call %d asked for %d names, over the %d limit", i, names, ssmBatch)
		}
	}
}

// The value never goes in argv: process arguments are readable by any local
// user through /proc. It is staged in an owner-only file that is deleted the
// moment aws returns.
func TestSSMPutKeepsValueOutOfArgv(t *testing.T) {
	f := &fakeAWS{}
	s := &ssmCLI{bin: "aws", run: f.run}
	const value = "postgres://user:hunter2@db/carshowdb"

	err := s.put(context.Background(), putParameterInput{
		Name: "/apps/carshowdb/prod/DATABASE_URL", Value: value, Type: ssmSecureString,
	})
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	if len(f.calls) != 1 {
		t.Fatalf("want 1 aws call, got %d", len(f.calls))
	}
	for _, a := range f.calls[0].args {
		if strings.Contains(a, "hunter2") {
			t.Fatalf("the value landed in argv: %q", a)
		}
	}
	call := f.calls[0]
	var sent putParameterInput
	if err := json.Unmarshal(call.body, &sent); err != nil {
		t.Fatalf("--cli-input-json did not point at a put-parameter request: %v", err)
	}
	// Never %+v the request in a failure message — that prints the value.
	if sent.Value != value || sent.Type != ssmSecureString {
		t.Errorf("request for %s did not carry the value as %s", sent.Name, ssmSecureString)
	}
	// Overwrite travels with the request so AWS refuses a clobber server-side too.
	if sent.Overwrite {
		t.Errorf("Overwrite must default to false")
	}
	if call.bodyMode != 0o600 {
		t.Errorf("the staged request is mode %o, want 600 — only this user may read it", call.bodyMode)
	}
	if _, err := os.Stat(call.bodyPath); !os.IsNotExist(err) {
		t.Errorf("the staged request outlived the call: %v", err)
	}
}

// The aws CLI quotes its input back on some validation errors. Nothing this
// command surfaces may carry a secret value.
func TestSSMPutRedactsValueFromError(t *testing.T) {
	f := &fakeAWS{
		stderr: []byte("Parameter validation failed: Invalid length for parameter Value, value: hunter2"),
		err:    errFake,
	}
	s := &ssmCLI{bin: "aws", run: f.run}
	err := s.put(context.Background(), putParameterInput{
		Name: "/apps/carshowdb/prod/DATABASE_URL", Value: "hunter2", Type: ssmSecureString,
	})
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("the error leaked the value: %v", err)
	}
	if !strings.Contains(err.Error(), "[redacted]") || !strings.Contains(err.Error(), "/apps/carshowdb/prod/DATABASE_URL") {
		t.Fatalf("error should name the path and mark the redaction: %v", err)
	}
}

func TestResolveSecretValues(t *testing.T) {
	paths := planSecretPaths(withStorage(secretsApp()), "prod", ssmEnv())
	env := map[string]string{
		"DATABASE_URL": "postgres://db",
		"R2_KEY_ID":    "abc123",
		"EMPTY":        "",
	}
	lookup := func(k string) (string, bool) { v, ok := env[k]; return v, ok }

	t.Run("key binds to the app root path", func(t *testing.T) {
		got, err := resolveSecretValues(paths, []string{"DATABASE_URL"}, nil, lookup, "carshowdb", "prod")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(got) != 1 || got[0].Path != "/apps/carshowdb/prod/DATABASE_URL" || got[0].value != "postgres://db" {
			t.Fatalf("got %v", got)
		}
		if got[0].Source != "$DATABASE_URL" {
			t.Errorf("source = %q", got[0].Source)
		}
	})

	t.Run("shared path binds through NAME=ENV_VAR", func(t *testing.T) {
		got, err := resolveSecretValues(paths, []string{"/shared/cloudflare/r2-access-key-id=R2_KEY_ID"}, nil, lookup, "carshowdb", "prod")
		if err != nil {
			t.Fatalf("resolve: %v", err)
		}
		if len(got) != 1 || got[0].Path != "/shared/cloudflare/r2-access-key-id" || got[0].value != "abc123" {
			t.Fatalf("got %v", got)
		}
	})

	t.Run("a key outside the plan is refused", func(t *testing.T) {
		_, err := resolveSecretValues(paths, nil, []string{"DATABSE_URL=oops"}, lookup, "carshowdb", "prod")
		if err == nil || !strings.Contains(err.Error(), "DATABSE_URL") {
			t.Fatalf("a typo must be refused, got %v", err)
		}
	})

	t.Run("an unset variable is an error, never a generated value", func(t *testing.T) {
		_, err := resolveSecretValues(paths, []string{"JWT_SECRET"}, nil, lookup, "carshowdb", "prod")
		if err == nil || !strings.Contains(err.Error(), "$JWT_SECRET is not set") {
			t.Fatalf("got %v", err)
		}
		if _, err := resolveSecretValues(paths, []string{"JWT_SECRET=EMPTY"}, nil, lookup, "carshowdb", "prod"); err == nil {
			t.Fatal("an empty variable must be refused")
		}
	})

	t.Run("one path cannot take two values", func(t *testing.T) {
		_, err := resolveSecretValues(paths, []string{"DATABASE_URL"}, []string{"DATABASE_URL=other"}, lookup, "carshowdb", "prod")
		if err == nil || !strings.Contains(err.Error(), "twice") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("--set needs KEY=VALUE", func(t *testing.T) {
		if _, err := resolveSecretValues(paths, nil, []string{"DATABASE_URL"}, lookup, "carshowdb", "prod"); err == nil {
			t.Fatal("want an error")
		}
	})

	t.Run("a shared path without a variable says how to pass one", func(t *testing.T) {
		_, err := resolveSecretValues(paths, []string{"/shared/cloudflare/r2-access-key-id"}, nil, lookup, "carshowdb", "prod")
		if err == nil || !strings.Contains(err.Error(), "=ENV_VAR") {
			t.Fatalf("got %v", err)
		}
	})
}

// create must refuse rather than invent: with no --from-env/--set it stops
// before it ever reaches AWS, so this runs anywhere.
func TestSecretsCreateRefusesWithoutAValueSource(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(file, []byte(`app: carshowdb
runtime:
  image: ghcr.io/example-org/example-api
  port: 8080
secrets:
  - DATABASE_URL
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newSecretsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"create", "-f", file, "--env", "prod", "--root", dir})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("create with no value source must fail")
	}
	if !strings.Contains(err.Error(), "--from-env") || !strings.Contains(err.Error(), "never generated") {
		t.Fatalf("the refusal should name the value sources: %v", err)
	}
}

// An env whose backend is not SSM has no parameters to plan; plan says so
// instead of listing paths nothing would read.
func TestSecretsPlanStopsOnANonSSMEnv(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "deploy.yaml")
	if err := os.WriteFile(file, []byte(`app: carshowdb
runtime:
  image: ghcr.io/example-org/example-api
  port: 8080
secrets:
  - DATABASE_URL
`), 0o644); err != nil {
		t.Fatal(err)
	}
	envDir := filepath.Join(dir, "environments", "dev")
	if err := os.MkdirAll(envDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(envDir, "cluster.yaml"), []byte(`env: dev
secrets:
  backend: local
  storeRef:
    name: platform-local
    kind: ClusterSecretStore
observability:
  lokiURL: http://loki.loki.svc.cluster.local:3100
flux:
  namespace: flux-system
  sourceName: flux-system
  kustomizationName: flux-system
  repoURL: https://github.com/yscale-sh/idp.git
  branch: main
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := newSecretsCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"plan", "-f", file, "--env", "dev", "--root", dir})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("plan on a local env should report, not fail: %v", err)
	}
	if !strings.Contains(out.String(), "not an SSM environment") {
		t.Fatalf("plan output = %q", out.String())
	}
	if strings.Contains(out.String(), "/apps/carshowdb/dev/DATABASE_URL") {
		t.Fatalf("plan must not list SSM paths for a local-backend env: %q", out.String())
	}
}

func withStorage(app appconfig.App) appconfig.App {
	app.Storage = []appconfig.Storage{{Name: "uploads", Bucket: "carshowdb-uploads"}}
	return app
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

var errFake = errors.New("exit status 255")

// A malformed --set is the one place the whole argument could be echoed, and
// the whole argument is the secret. The command's stated contract is that no
// value ever reaches an error.
func TestMalformedSetNeverEchoesTheValue(t *testing.T) {
	const secret = "postgres://u:hunter2@h/db"
	_, err := resolveSecretValues(nil, nil, []string{"=" + secret},
		func(string) (string, bool) { return "", false }, "app", "prod")
	if err == nil {
		t.Fatal("a --set with no key was accepted")
	}
	if strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), secret) {
		t.Fatalf("the secret reached the error: %q", err)
	}
}
