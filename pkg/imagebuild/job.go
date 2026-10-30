// Package imagebuild renders the Job that builds an ImageBuild with BuildKit. It
// does no I/O, so the rendering can be tested on its own.
package imagebuild

import (
	"fmt"
	"maps"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"

	v1 "github.com/wutz/imagebuild/api/v1alpha1"
	"github.com/wutz/imagebuild/pkg/naming"
)

const (
	// DockerfileKey is the key of the inline Dockerfile in its ConfigMap.
	DockerfileKey = "Dockerfile"
	// Timeout bounds one build, clone and push included.
	Timeout = time.Hour

	workspaceSrc    = "/workspace/src"
	dockerfileMount = "/workspace/dockerfile"
	dockerConfigDir = "/var/run/imagebuild/docker"
	// builderUID runs the rootless BuildKit of the image. Its rootlesskit maps this
	// uid to root in a user namespace of its own; the pod stays in the host user
	// namespace, because hostUsers false leaves no subuid range for that mapping.
	builderUID   = 1000
	metadataFile = "/tmp/metadata.json"
	// buildkitdConfigFile is where the build script writes the BuildKit config.
	buildkitdConfigFile = "/tmp/buildkitd.toml"

	volSrc        = "src"
	volDockerfile = "dockerfile"
	volRegistry   = "registry"
)

// cloneScript fetches the revision into the working directory. The URL and the
// revision come from the environment, so nothing is interpolated into the script.
const cloneScript = `set -eu
git init -q /workspace/src
cd /workspace/src
git fetch -q --depth 1 "$GIT_URL" "$GIT_REVISION"
git checkout -q FETCH_HEAD`

// buildScript writes the BuildKit config when there is one, runs buildctl with
// the arguments that follow it, then writes the digest of the pushed image to the termination message.
const buildScript = `set -u
if [ -n "${BUILDKITD_CONFIG:-}" ]; then
  printf '%s\n' "$BUILDKITD_CONFIG" > ` + buildkitdConfigFile + `
fi
buildctl-daemonless.sh "$@" --metadata-file=` + metadataFile + `
status=$?
if [ "$status" -eq 0 ]; then
  sed -n 's/.*"containerimage.digest": *"\([^"]*\)".*/\1/p' ` + metadataFile + ` > /dev/termination-log
fi
exit "$status"`

var (
	digestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	// registryHostPattern is a registry domain with an optional port.
	registryHostPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?$`)
	// mirrorPattern is a mirror: a domain with an optional port and an optional path.
	mirrorPattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]*[a-z0-9])?(:[0-9]{1,5})?(/[A-Za-z0-9._~-]+)*$`)
)

// DefaultSeccompProfile is the Localhost seccomp profile of the build pods,
// relative to the seccomp directory of the kubelet. The Security Profiles
// Operator installs the SeccompProfile named imagebuild there.
const DefaultSeccompProfile = "operator/imagebuild.json"

// Config is the platform configuration a build needs.
type Config struct {
	// BuilderImage is the BuildKit rootless image, with its tag.
	BuilderImage string
	// GitImage clones the build context. It needs git and sh.
	GitImage string
	// Registry is the domain of the registry the image is pushed to.
	Registry string
	// RegistryInsecure pushes over plain HTTP.
	RegistryInsecure bool
	// SeccompProfile is the Localhost seccomp profile of the build pods, relative
	// to the seccomp directory of the kubelet. It must exist on every node a build
	// can run on. Empty means DefaultSeccompProfile.
	SeccompProfile string
	// RegistryMirrors maps a registry domain to the mirror BuildKit pulls its
	// images from, for example "docker.io" to "mirror.example.com/docker.io".
	// Pushes are not affected.
	RegistryMirrors map[string]string
	// ExcludeNodes are the names of the nodes the build pods must not run on.
	ExcludeNodes []string
}

// Validate reports whether the configuration can be rendered into a build.
func (c Config) Validate() error {
	for host, mirror := range c.RegistryMirrors {
		if !registryHostPattern.MatchString(host) {
			return fmt.Errorf("registry mirror %q: %q is not a registry domain", host, host)
		}
		if !mirrorPattern.MatchString(mirror) {
			return fmt.Errorf("registry mirror %q: %q is not a domain with an optional path", host, mirror)
		}
	}
	return nil
}

func (c Config) seccompProfile() string {
	if c.SeccompProfile == "" {
		return DefaultSeccompProfile
	}
	return c.SeccompProfile
}

// buildkitdConfig is the buildkitd.toml that sends the pulls of the mirrored
// registries to their mirrors, or "" when there are none. The domains and
// mirrors are checked by Validate, so they need no escaping.
func (c Config) buildkitdConfig() string {
	hosts := slices.Sorted(maps.Keys(c.RegistryMirrors))
	var b strings.Builder
	for _, h := range hosts {
		fmt.Fprintf(&b, "[registry.%q]\n  mirrors = [%q]\n", h, c.RegistryMirrors[h])
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// Options describes one ImageBuild.
type Options struct {
	// Name is the ImageBuild name. The Job and the Dockerfile ConfigMap share it.
	Name string
	// Namespace is the build namespace of the project.
	Namespace string
	// Project is the Project name, the value of naming.LabelProject.
	Project string
	// HarborProject is the registry project, the project namespace.
	HarborProject string
	Spec          v1.ImageBuildSpec
	Config        Config
}

// Validate reports whether the build can run. The CRD checks the shape of the
// spec; this checks what the CRD cannot, such as paths that leave the repository.
func Validate(name string, spec v1.ImageBuildSpec) error {
	if errs := validation.IsDNS1123Label(name); len(errs) > 0 {
		return fmt.Errorf("name %q is not a DNS label: %s", name, strings.Join(errs, "; "))
	}
	if _, _, _, err := layout(spec.Source); err != nil {
		return err
	}
	return nil
}

// CleanPath returns p relative to the repository root, or an error when p is
// absolute or leaves the root. An empty p is the root.
func CleanPath(p string) (string, error) {
	if p == "" {
		return ".", nil
	}
	if path.IsAbs(p) {
		return "", fmt.Errorf("path %q must be relative to the repository", p)
	}
	c := path.Clean(p)
	if c == ".." || strings.HasPrefix(c, "../") {
		return "", fmt.Errorf("path %q leaves the repository", p)
	}
	return c, nil
}

// layout returns the build context, the directory of the Dockerfile and the
// Dockerfile name inside the builder. An inline Dockerfile wins over the one in
// the repository, and without a repository the context is empty.
func layout(src v1.ImageBuildSource) (contextDir, dockerfileDir, dockerfileName string, err error) {
	if src.Git == nil {
		if src.Dockerfile == "" {
			return "", "", "", fmt.Errorf("a git repository or a Dockerfile is required")
		}
		return workspaceSrc, dockerfileMount, DockerfileKey, nil
	}
	ctx, err := CleanPath(src.Git.ContextPath)
	if err != nil {
		return "", "", "", fmt.Errorf("contextPath: %w", err)
	}
	contextDir = path.Join(workspaceSrc, ctx)
	if src.Dockerfile != "" {
		return contextDir, dockerfileMount, DockerfileKey, nil
	}
	df, err := CleanPath(src.Git.DockerfilePath)
	if err != nil {
		return "", "", "", fmt.Errorf("dockerfilePath: %w", err)
	}
	if df == "." {
		return "", "", "", fmt.Errorf("dockerfilePath %q is not a file", src.Git.DockerfilePath)
	}
	return contextDir, path.Join(workspaceSrc, path.Dir(df)), path.Base(df), nil
}

// ImageRefs returns the references the tags are pushed to in the registry project.
func ImageRefs(registry, harborProject string, dest v1.ImageBuildDestination) []string {
	refs := make([]string, 0, len(dest.Tags))
	for _, t := range dest.Tags {
		refs = append(refs, registry+"/"+harborProject+"/"+dest.Repository+":"+string(t))
	}
	return refs
}

// outputValue is the --output value that pushes every reference. Multiple
// names are quoted as one CSV field.
func outputValue(refs []string, insecure bool) string {
	v := `type=image,"name=` + strings.Join(refs, ",") + `",push=true`
	if insecure {
		v += ",registry.insecure=true"
	}
	return v
}

// ParseDigest returns the digest a builder wrote to its termination message, or
// "" when the message is not a digest.
func ParseDigest(msg string) string {
	d := strings.TrimSpace(msg)
	if digestPattern.MatchString(d) {
		return d
	}
	return ""
}

// Labels returns the labels of the objects of an ImageBuild in its build namespace.
func Labels(o Options) map[string]string {
	return map[string]string{
		naming.LabelManagedBy:  naming.ManagedBy,
		naming.LabelProject:    o.Project,
		naming.LabelImageBuild: o.Name,
	}
}

// DockerfileConfigMap holds the inline Dockerfile of an ImageBuild. It is named
// like the Job. The ConfigMap is nil when the Dockerfile is in the repository.
func DockerfileConfigMap(o Options) *corev1.ConfigMap {
	if o.Spec.Source.Dockerfile == "" {
		return nil
	}
	return &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, Labels: Labels(o)},
		Data:       map[string]string{DockerfileKey: o.Spec.Source.Dockerfile},
	}
}

// Job renders the Job that clones the repository, builds the image and pushes
// it to the registry. The Job needs a Localhost seccomp profile and the setuid
// newuidmap, so it belongs in the build namespace and never in a project namespace.
func Job(o Options) (*batchv1.Job, error) {
	if err := Validate(o.Name, o.Spec); err != nil {
		return nil, err
	}
	contextDir, dockerfileDir, dockerfileName, err := layout(o.Spec.Source)
	if err != nil {
		return nil, err
	}
	labels := Labels(o)
	refs := ImageRefs(o.Config.Registry, o.HarborProject, o.Spec.Destination)

	mounts := []corev1.VolumeMount{
		{Name: volSrc, MountPath: workspaceSrc},
		{Name: volRegistry, MountPath: dockerConfigDir, ReadOnly: true},
	}
	volumes := []corev1.Volume{
		{Name: volSrc, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: volRegistry, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
			SecretName: naming.RegistrySecretName,
			Items:      []corev1.KeyToPath{{Key: corev1.DockerConfigJsonKey, Path: "config.json"}},
		}}},
	}
	if o.Spec.Source.Dockerfile != "" {
		volumes = append(volumes, corev1.Volume{Name: volDockerfile, VolumeSource: corev1.VolumeSource{
			ConfigMap: &corev1.ConfigMapVolumeSource{
				LocalObjectReference: corev1.LocalObjectReference{Name: o.Name},
				Items:                []corev1.KeyToPath{{Key: DockerfileKey, Path: DockerfileKey}},
			},
		}})
		mounts = append(mounts, corev1.VolumeMount{Name: volDockerfile, MountPath: dockerfileMount, ReadOnly: true})
	}

	var inits []corev1.Container
	if g := o.Spec.Source.Git; g != nil {
		revision := g.Revision
		if revision == "" {
			revision = "HEAD"
		}
		inits = append(inits, corev1.Container{
			Name:    "clone",
			Image:   o.Config.GitImage,
			Command: []string{"sh", "-c", cloneScript},
			Env: []corev1.EnvVar{
				{Name: "GIT_URL", Value: g.URL},
				{Name: "GIT_REVISION", Value: revision},
				{Name: "GIT_TERMINAL_PROMPT", Value: "0"},
				// The emptyDir may not be owned by the user the clone runs as.
				{Name: "GIT_CONFIG_COUNT", Value: "1"},
				{Name: "GIT_CONFIG_KEY_0", Value: "safe.directory"},
				{Name: "GIT_CONFIG_VALUE_0", Value: "*"},
			},
			VolumeMounts: mounts[:1],
		})
	}

	flags := "--oci-worker-no-process-sandbox"
	env := []corev1.EnvVar{
		// ROOTLESSKIT is not set, so buildctl-daemonless.sh starts buildkitd under rootlesskit.
		{Name: "DOCKER_CONFIG", Value: dockerConfigDir},
	}
	if cfg := o.Config.buildkitdConfig(); cfg != "" {
		flags = "--config=" + buildkitdConfigFile + " " + flags
		env = append(env, corev1.EnvVar{Name: "BUILDKITD_CONFIG", Value: cfg})
	}
	// NoProcessSandbox is accepted only in rootless mode. Rootless mode runs
	// without cgroups, so it does not need the writable /sys that the pod lacks.
	env = append(env, corev1.EnvVar{Name: "BUILDKITD_FLAGS", Value: flags})

	builder := corev1.Container{
		Name:    "build",
		Image:   o.Config.BuilderImage,
		Command: []string{"sh", "-c", buildScript, "buildctl"},
		Args: []string{
			"build",
			"--frontend=dockerfile.v0",
			"--local=context=" + contextDir,
			"--local=dockerfile=" + dockerfileDir,
			"--opt=filename=" + dockerfileName,
			"--output=" + outputValue(refs, o.Config.RegistryInsecure),
		},
		Env:          env,
		VolumeMounts: mounts,
		// rootlesskit runs newuidmap, a setuid binary, so escalation stays allowed.
		// SETUID and SETGID let it write the uid map. The default seccomp profile
		// blocks the namespace calls rootlesskit makes, so the pod runs with the
		// Localhost profile that allows them. The baseline Pod Security level accepts it.
		SecurityContext: &corev1.SecurityContext{
			RunAsUser:                new(int64(builderUID)),
			RunAsGroup:               new(int64(builderUID)),
			AllowPrivilegeEscalation: new(true),
			SeccompProfile: &corev1.SeccompProfile{
				Type:             corev1.SeccompProfileTypeLocalhost,
				LocalhostProfile: new(o.Config.seccompProfile()),
			},
			Capabilities: &corev1.Capabilities{
				Drop: []corev1.Capability{"ALL"},
				Add:  []corev1.Capability{"SETUID", "SETGID"},
			},
		},
	}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{Name: o.Name, Namespace: o.Namespace, Labels: labels},
		Spec: batchv1.JobSpec{
			BackoffLimit:          new(int32(0)),
			ActiveDeadlineSeconds: new(int64(Timeout / time.Second)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					Affinity:                     excludeNodes(o.Config.ExcludeNodes),
					AutomountServiceAccountToken: new(false),
					InitContainers:               inits,
					Containers:                   []corev1.Container{builder},
					Volumes:                      volumes,
				},
			},
		},
	}, nil
}

// excludeNodes keeps a pod off the nodes with the given names, or returns nil
// when there are none.
func excludeNodes(nodes []string) *corev1.Affinity {
	if len(nodes) == 0 {
		return nil
	}
	return &corev1.Affinity{NodeAffinity: &corev1.NodeAffinity{
		RequiredDuringSchedulingIgnoredDuringExecution: &corev1.NodeSelector{
			NodeSelectorTerms: []corev1.NodeSelectorTerm{{
				MatchExpressions: []corev1.NodeSelectorRequirement{{
					Key: corev1.LabelHostname, Operator: corev1.NodeSelectorOpNotIn, Values: nodes,
				}},
			}},
		},
	}}
}
