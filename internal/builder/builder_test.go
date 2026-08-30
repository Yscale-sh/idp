package builder

import (
	"strings"
	"testing"
)

func safeJobParams() jobParams {
	return jobParams{
		JobName: "build-example-abc123", Namespace: "image-builder", Leaf: "example",
		Repo: "example/repo", Ref: "abc123", Context: ".", Dockerfile: "Dockerfile",
		Image: "ghcr.io/example/repo:abc123", WorkerSubpath: "buildkit-worker-example",
	}
}

func TestRenderJobQuotesGitInputs(t *testing.T) {
	raw, err := renderJob(safeJobParams())
	if err != nil {
		t.Fatal(err)
	}
	manifest := string(raw)
	for _, want := range []string{
		"remote add origin 'https://github.com/example/repo.git'",
		"fetch --depth 1 -- origin 'abc123'",
		"ls -la '/workspace/src'",
	} {
		if !strings.Contains(manifest, want) {
			t.Fatalf("rendered Job missing %q", want)
		}
	}
}

func TestRenderJobRejectsUnsafeBuildInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*jobParams)
	}{
		{name: "context shell syntax", mutate: func(p *jobParams) { p.Context = "ui; touch /tmp/pwned" }},
		{name: "dockerfile traversal", mutate: func(p *jobParams) { p.Dockerfile = "../Dockerfile" }},
		{name: "submodule option", mutate: func(p *jobParams) { p.Submodules = []string{"--config=x"} }},
		{name: "repository injection", mutate: func(p *jobParams) { p.Repo = `example/repo\";id` }},
		{name: "ref injection", mutate: func(p *jobParams) { p.Ref = "main$(id)" }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := safeJobParams()
			tc.mutate(&p)
			if _, err := renderJob(p); err == nil {
				t.Fatal("unsafe build input must be rejected")
			}
		})
	}
}
