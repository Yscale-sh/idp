// Package kube holds optional, minimal Kubernetes helpers. platformctl's default
// path never touches a live cluster (Argo CD reconciles desired state), so this
// package is intentionally small: it parses kubeconfig context names and applies
// manifests by shelling out to kubectl for the non-default `infra apply` /
// emergency path. It pulls in no client-go dependency, keeping the binary lean.
package kube

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
)

// Client is a thin kubectl wrapper. Context selects the kubeconfig context;
// empty uses the current context.
type Client struct {
	Bin     string
	Context string
}

// New returns a Client using kubectl on PATH and the given context (may be "").
func New(kubeContext string) *Client {
	return &Client{Bin: "kubectl", Context: kubeContext}
}

func (c *Client) bin() string {
	if c.Bin != "" {
		return c.Bin
	}
	return "kubectl"
}

// Available reports whether kubectl is on PATH.
func (c *Client) Available() bool {
	_, err := exec.LookPath(c.bin())
	return err == nil
}

func (c *Client) baseArgs() []string {
	if c.Context != "" {
		return []string{"--context", c.Context}
	}
	return nil
}

// Apply runs `kubectl apply -f -` with the given manifest on stdin. This is the
// NON-default emergency/infra path; the normal flow commits desired state for
// Argo CD instead of applying directly.
func (c *Client) Apply(ctx context.Context, manifest []byte) ([]byte, error) {
	args := append(c.baseArgs(), "apply", "-f", "-")
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdin = bytes.NewReader(manifest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("kubectl apply: %w: %s", err, stderr.String())
	}
	return stdout.Bytes(), nil
}

// Diff runs `kubectl diff -f -` for a dry preview. A non-zero exit with diff
// output is normal for kubectl diff (it signals "differences exist"), so callers
// treat output, not just error, as meaningful.
func (c *Client) Diff(ctx context.Context, manifest []byte) ([]byte, error) {
	args := append(c.baseArgs(), "diff", "-f", "-")
	cmd := exec.CommandContext(ctx, c.bin(), args...)
	cmd.Stdin = bytes.NewReader(manifest)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.Bytes(), err
}
