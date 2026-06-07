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

// jobDeadline must be >= the Job's activeDeadlineSeconds (3600s) so the Job's own
// deadline is authoritative — otherwise a legitimately slow build (cold Rust
// cache) is reported as a timeout and the next poll starts a SECOND build on the
// same per-image BuildKit root.
const jobDeadline = 65 * time.Minute

// Spec describes a single image build.
type Spec struct {
	Repo       string   // GitHub "org/name" to clone, e.g. Yscale-sh/yscale-media
	Ref        string   // git ref to build (a branch or, preferably, a commit SHA)
	Image      string   // fully-qualified target image repo:tag to push
	Context    string   // build context subdir relative to repo root ("." for root)
	Dockerfile string   // Dockerfile name within the context (default "Dockerfile")
	Submodules []string // private submodules to init before building (shopping-list-driven)
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
	buildCtx := s.Context
	if buildCtx == "" {
		buildCtx = "."
	}
	dockerfile := s.Dockerfile
	if dockerfile == "" {
		dockerfile = "Dockerfile"
	}
	leaf := slug(path.Base(strings.SplitN(s.Image, ":", 2)[0]), 30) // yscale-media-api
	name := fmt.Sprintf("build-%s-%s-%d", leaf, slug(s.Ref, 12), time.Now().Unix()%100000)

	// Two builds of the SAME image must never run concurrently — they share one
	// per-image BuildKit worker root (RWO) and would corrupt it. Delete any prior
	// build Job for this image leaf before starting (covers a slow predecessor a
	// restart/re-ship would otherwise race).
	_, _ = b.Kube.Run(ctx, "-n", ns, "delete", "job", "-l", "build.idp/leaf="+leaf, "--ignore-not-found")

	manifest, err := renderJob(jobParams{
		JobName:       name,
		Namespace:     ns,
		Leaf:          leaf,
		Repo:          s.Repo,
		Ref:           s.Ref,
		Context:       buildCtx,
		Dockerfile:    dockerfile,
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

	// Stream logs best-effort. `logs -f job/<name>` waits for the pod itself, so
	// no separate `wait --for=condition=ready` (which never fires for a fast pod
	// that already Completed, wasting up to 120s on every cache-warm build).
	if b.Out != nil {
		_ = b.Kube.Stream(ctx, b.Out, "-n", ns, "logs", "-f", "job/"+name,
			"--all-containers", "--pod-running-timeout=10m")
	}

	return b.waitJob(ctx, ns, name)
}

// waitJob polls the Job until its terminal condition. It waits on the Complete /
// Failed *conditions* (set by the Job controller only after backoffLimit is
// exhausted) rather than the raw succeeded/failed pod counters, so a transient
// first-attempt failure that backoffLimit:1 is meant to absorb is not reported
// as a hard failure.
func (b *Builder) waitJob(ctx context.Context, ns, name string) error {
	const jsonpath = `jsonpath={range .status.conditions[*]}{.type}={.status};{end}`
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	deadline := time.Now().Add(jobDeadline)
	for {
		out, err := b.Kube.Run(ctx, "-n", ns, "get", "job", name, "-o", jsonpath)
		if err == nil {
			conds := string(out) // e.g. "Complete=True;" or "Failed=True;"
			if strings.Contains(conds, "Complete=True") {
				return nil
			}
			if strings.Contains(conds, "Failed=True") {
				return fmt.Errorf("build job %s failed", name)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("build job %s timed out after %s", name, jobDeadline)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
	}
}

type jobParams struct {
	JobName, Namespace, Leaf, Repo, Ref, Context, Dockerfile, Image, WorkerSubpath string
	Submodules                                                                     []string
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
// rootless BuildKit), parameterized in Go. The clone fetches the exact ref by SHA
// (git fetch --depth 1 <sha>; `clone --branch <sha>` is invalid for a commit and
// would force a full-history clone every build). Submodules come from the
// registry-supplied list.
const jobTemplate = `apiVersion: batch/v1
kind: Job
metadata:
  name: {{.JobName}}
  namespace: {{.Namespace}}
  labels:
    app.kubernetes.io/part-of: image-builder
    build.idp/leaf: {{.Leaf}}
spec:
  backoffLimit: 1
  ttlSecondsAfterFinished: 3600
  activeDeadlineSeconds: 3600
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: image-builder
        build.idp/leaf: {{.Leaf}}
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
              echo "fetching {{.Repo}}@{{.Ref}} ..."
              # Auth every github.com fetch (incl. PRIVATE submodules) with the token
              # via insteadOf, so no credential is baked into a remote URL.
              git config --global url."https://x-access-token:${GIT_TOKEN}@github.com/".insteadOf "https://github.com/"
              git init -q /workspace/src
              git -C /workspace/src remote add origin "https://github.com/{{.Repo}}.git"
              git -C /workspace/src fetch --depth 1 origin "{{.Ref}}"
              git -C /workspace/src checkout -q FETCH_HEAD
{{- range .Submodules}}
              # NOT --depth 1: the gitlink pins a specific commit that may not be the
              # submodule's branch tip, which a shallow fetch wouldn't contain.
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
            - --opt=filename={{.Dockerfile}}
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
