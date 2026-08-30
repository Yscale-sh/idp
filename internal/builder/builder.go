// Package builder kicks a one-off, scale-to-zero container image build on the
// cluster image-builder (clone -> rootless BuildKit -> push to ghcr) and waits
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

	"github.com/yscale-sh/idp/internal/kube"
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
	Submodules []string // private submodules to init before building (app-manifest-driven)
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
var githubRepoPattern = regexp.MustCompile(`^[A-Za-z0-9_.-]+/[A-Za-z0-9_.-]+$`)
var gitRefPattern = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
var imageRefPattern = regexp.MustCompile(`^[A-Za-z0-9._:/@+-]+$`)
var buildPathPattern = regexp.MustCompile(`^[A-Za-z0-9._/@+-]+$`)
var dnsLabelPattern = regexp.MustCompile(`^[a-z0-9]([-a-z0-9]*[a-z0-9])?$`)

// ValidateSpec rejects values before a Job receives git and registry
// credentials. These values are embedded in both YAML and a /bin/sh script, so
// validation is a security boundary rather than only a usability check.
func ValidateSpec(s Spec) error {
	if !githubRepoPattern.MatchString(s.Repo) {
		return fmt.Errorf("invalid GitHub repository %q: want owner/name", s.Repo)
	}
	if s.Ref == "" || !gitRefPattern.MatchString(s.Ref) || strings.Contains(s.Ref, "..") || strings.Contains(s.Ref, "@{") {
		return fmt.Errorf("invalid git ref %q", s.Ref)
	}
	if s.Image == "" || !imageRefPattern.MatchString(s.Image) {
		return fmt.Errorf("invalid target image %q", s.Image)
	}
	if s.Namespace != "" && !dnsLabelPattern.MatchString(s.Namespace) {
		return fmt.Errorf("invalid builder namespace %q", s.Namespace)
	}
	for field, value := range map[string]string{
		"context":    s.Context,
		"dockerfile": s.Dockerfile,
	} {
		if err := validateBuildPath(field, value); err != nil {
			return err
		}
	}
	for i, submodule := range s.Submodules {
		if err := validateBuildPath(fmt.Sprintf("submodule[%d]", i), submodule); err != nil {
			return err
		}
	}
	return nil
}

func validateBuildPath(field, value string) error {
	if value == "" {
		return nil
	}
	clean := path.Clean(value)
	if !buildPathPattern.MatchString(value) {
		return fmt.Errorf("invalid %s %q: contains unsupported characters", field, value)
	}
	if path.IsAbs(value) || clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("invalid %s %q: must stay within the repository", field, value)
	}
	if clean != value {
		return fmt.Errorf("invalid %s %q: must be a clean relative path", field, value)
	}
	return nil
}

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
	if err := ValidateSpec(s); err != nil {
		return err
	}
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

	// Do NOT stream `kubectl logs -f job/<name>`: it does not reliably terminate
	// when the Job's pod completes (it hangs following the finished pod), which
	// wedged the whole shipper. The Job status is the authoritative result; the
	// full build log stays available via `kubectl logs job/<name>` (TTL 1h). On
	// failure waitJob captures a log tail for the error.
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
				tail, _ := b.Kube.Run(ctx, "-n", ns, "logs", "job/"+name, "--tail=40")
				return fmt.Errorf("build job %s failed:\n%s", name, string(tail))
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
	if err := ValidateSpec(Spec{
		Repo: p.Repo, Ref: p.Ref, Image: p.Image, Context: p.Context,
		Dockerfile: p.Dockerfile, Submodules: p.Submodules, Namespace: p.Namespace,
	}); err != nil {
		return nil, err
	}
	if !dnsLabelPattern.MatchString(p.JobName) || !dnsLabelPattern.MatchString(p.Leaf) || !buildPathPattern.MatchString(p.WorkerSubpath) {
		return nil, fmt.Errorf("invalid generated build identity")
	}
	funcs := template.FuncMap{
		"repoURL": func(repo string) string { return "https://github.com/" + repo + ".git" },
		"shellQuote": func(value string) string {
			return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
		},
		"workspacePath": func(value string) string {
			if value == "." {
				return "/workspace/src"
			}
			return "/workspace/src/" + value
		},
	}
	t, err := template.New("job").Funcs(funcs).Parse(jobTemplate)
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
              printf 'fetching %s@%s ...\n' {{shellQuote .Repo}} {{shellQuote .Ref}}
              # Auth every github.com fetch (incl. PRIVATE submodules) with the token
              # via insteadOf, so no credential is baked into a remote URL.
              git config --global url."https://x-access-token:${GIT_TOKEN}@github.com/".insteadOf "https://github.com/"
              git init -q /workspace/src
              git -C /workspace/src remote add origin {{shellQuote (repoURL .Repo)}}
              git -C /workspace/src fetch --depth 1 -- origin {{shellQuote .Ref}}
              git -C /workspace/src checkout -q FETCH_HEAD
{{- range .Submodules}}
              # NOT --depth 1: the gitlink pins a specific commit that may not be the
              # submodule's branch tip, which a shallow fetch wouldn't contain.
              git -C /workspace/src submodule update --init -- {{shellQuote .}} || printf '  (submodule %s not present on this ref; skipping)\n' {{shellQuote .}}
{{- end}}
              printf 'context: %s\n' {{shellQuote (workspacePath .Context)}}
              ls -la {{shellQuote (workspacePath .Context)}} | head
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
              # gc-keepstorage caps EACH app's worker cache (subPath) at ~3GB — without it the
              # shared buildkit-cache PVC grew unbounded (34GB) and filled node3 into a
              # DiskPressure eviction storm (2026-07-04). ~8 apps × 3GB bounds the PVC ≤ ~24GB.
              value: --oci-worker-no-process-sandbox --root /home/user/.local/share/buildkit --oci-worker-gc-keepstorage=3000
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
