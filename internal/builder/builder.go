// Package builder kicks a one-off, scale-to-zero container image build on the
// homelab image-builder (clone -> rootless BuildKit -> push to ghcr) and waits
// for it. It is the Go replacement for the old onprem/image-builder/build.sh:
// the shipper composes it with the render pipeline so "push to master -> image
// built -> rendered -> Flux reconciles" runs end to end with no shell scripts.
//
// Like internal/kube it deliberately avoids a client-go dependency — it renders
// the Job manifest and drives it through the thin kubectl wrapper, keeping the
// binary lean and consistent with idpctl's design.
package builder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/jakenesler/idp/internal/kube"
)

// Spec describes a single image build.
type Spec struct {
	Repo       string   // GitHub "org/name" to clone, e.g. Yscale-sh/yscale-media
	Ref        string   // git ref to build (a branch or, preferably, a commit SHA)
	Image      string   // fully-qualified target image repo:tag to push
	Context    string   // build context subdir relative to repo root ("." for root)
	Submodules []string // private submodules to init before building (registry-driven)
	Namespace  string   // image-builder namespace (defaults to "image-builder")
}

// Builder runs builds through a kube client (the kubectl wrapper).
type Builder struct {
	Kube *kube.Client
	// Out, when set, receives live build logs.
	Out io.Writer
}

// New returns a Builder using the given kube client.
func New(k *kube.Client) *Builder { return &Builder{Kube: k} }

var nonDNS = regexp.MustCompile(`[^a-z0-9-]`)

func slug(s string, max int) string {
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, "/", "-")
	s = nonDNS.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > max {
		s = s[:max]
	}
	return strings.Trim(s, "-")
}

// Build renders the build Job, applies it, streams its logs, and blocks until
// the Job succeeds or fails. A failed build returns a non-nil error.
func (b *Builder) Build(ctx context.Context, s Spec) error {
	ns := s.Namespace
	if ns == "" {
		ns = "image-builder"
	}
	context := s.Context
	if context == "" {
		context = "."
	}
	leaf := path.Base(strings.SplitN(s.Image, ":", 2)[0]) // yscale-media-api
	name := fmt.Sprintf("build-%s-%s-%d", slug(leaf, 24), slug(s.Ref, 12), time.Now().Unix()%100000)

	manifest, err := renderJob(jobParams{
		JobName:       name,
		Namespace:     ns,
		Repo:          s.Repo,
		Ref:           s.Ref,
		Context:       context,
		Image:         s.Image,
		WorkerSubpath: "buildkit-worker-" + leaf,
		Submodules:    s.Submodules,
	})
	if err != nil {
		return fmt.Errorf("render build job: %w", err)
	}

	if _, err := b.Kube.Apply(ctx, manifest); err != nil {
		return fmt.Errorf("create build job %s: %w", name, err)
	}

	// Best-effort: wait for the pod, then stream logs. The build proceeds even
	// if log streaming races/fails; the authoritative result is the Job status.
	if b.Out != nil {
		_, _ = b.Kube.Run(ctx, "-n", ns, "wait", "--for=condition=ready", "pod",
			"-l", "job-name="+name, "--timeout=120s")
		_ = b.Kube.Stream(ctx, b.Out, "-n", ns, "logs", "-f", "job/"+name,
			"--all-containers", "--pod-running-timeout=5m")
	}

	return b.waitJob(ctx, ns, name)
}

// waitJob polls the Job until it reports a succeeded or failed pod.
func (b *Builder) waitJob(ctx context.Context, ns, name string) error {
	const jsonpath = `jsonpath={.status.succeeded}/{.status.failed}`
	t := time.NewTicker(3 * time.Second)
	defer t.Stop()
	deadline := time.Now().Add(30 * time.Minute)
	for {
		out, err := b.Kube.Run(ctx, "-n", ns, "get", "job", name, "-o", jsonpath)
		if err == nil {
			s := strings.TrimSpace(string(out)) // "succeeded/failed", e.g. "1/" or "/1"
			succ, fail, _ := strings.Cut(s, "/")
			if strings.TrimSpace(succ) != "" && succ != "0" {
				return nil
			}
			if strings.TrimSpace(fail) != "" && fail != "0" {
				return fmt.Errorf("build job %s failed", name)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("build job %s timed out", name)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

type jobParams struct {
	JobName, Namespace, Repo, Ref, Context, Image, WorkerSubpath string
	Submodules                                                   []string
}

func renderJob(p jobParams) ([]byte, error) {
	t, err := template.New("job").Parse(jobTemplate)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := t.Execute(&buf, p); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// jobTemplate mirrors onprem/image-builder/build-job.yaml (clone initContainer +
// rootless BuildKit), parameterized in Go. Submodules are initialized from the
// registry-supplied list rather than a hardcoded path.
const jobTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: {{.JobName}}
  namespace: {{.Namespace}}
  labels:
    app.kubernetes.io/part-of: image-builder
spec:
  backoffLimit: 1
  ttlSecondsAfterFinished: 3600
  activeDeadlineSeconds: 3600
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: image-builder
      annotations:
        container.apparmor.security.beta.kubernetes.io/buildkit: unconfined
    spec:
      restartPolicy: Never
      serviceAccountName: image-builder
      securityContext:
        fsGroup: 1000
        fsGroupChangePolicy: OnRootMismatch
      initContainers:
        - name: clone
          image: alpine/git:2.45.2
          env:
            - name: GIT_TOKEN
              valueFrom:
                secretKeyRef:
                  name: git-token
                  key: token
          command: ["/bin/sh", "-c"]
          args:
            - |
              set -eu
              echo "cloning {{.Repo}}@{{.Ref}} ..."
              git config --global url."https://x-access-token:${GIT_TOKEN}@github.com/".insteadOf "https://github.com/"
              git clone --depth 1 --branch "{{.Ref}}" \
                "https://x-access-token:${GIT_TOKEN}@github.com/{{.Repo}}.git" /workspace/src || \
                git clone "https://x-access-token:${GIT_TOKEN}@github.com/{{.Repo}}.git" /workspace/src
              git -C /workspace/src checkout "{{.Ref}}"
{{- range .Submodules}}
              git -C /workspace/src submodule update --init {{.}} || echo "  (submodule {{.}} not present on this ref; skipping)"
{{- end}}
              echo "context: /workspace/src/{{.Context}}"
              ls -la "/workspace/src/{{.Context}}" | head
          volumeMounts:
            - { name: workspace, mountPath: /workspace }
      containers:
        - name: buildkit
          image: moby/buildkit:v0.16.0-rootless
          securityContext:
            seccompProfile:
              type: Unconfined
            runAsUser: 1000
            runAsGroup: 1000
          env:
            - name: BUILDKITD_FLAGS
              value: --oci-worker-no-process-sandbox --root /home/user/.local/share/buildkit
            - name: DOCKER_CONFIG
              value: /docker
          command: ["buildctl-daemonless.sh"]
          args:
            - build
            - --frontend=dockerfile.v0
            - --local=context=/workspace/src/{{.Context}}
            - --local=dockerfile=/workspace/src/{{.Context}}
            - --opt=filename=Dockerfile
            - --output=type=image,name={{.Image}},push=true
          resources:
            requests: { cpu: "1", memory: 2Gi }
            limits:   { cpu: "6", memory: 8Gi }
          volumeMounts:
            - { name: workspace, mountPath: /workspace }
            - { name: cache, mountPath: /home/user/.local/share/buildkit, subPath: {{.WorkerSubpath}} }
            - { name: docker, mountPath: /docker, readOnly: true }
      volumes:
        - name: workspace
          emptyDir: {}
        - name: cache
          persistentVolumeClaim:
            claimName: buildkit-cache
        - name: docker
          secret:
            secretName: ghcr-push
            items:
              - { key: .dockerconfigjson, path: config.json }
`
