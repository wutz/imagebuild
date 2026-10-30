package imagebuild

import (
	"encoding/json"
	"os"
	"reflect"
	"slices"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"

	v1 "github.com/wutz/imagebuild/api/v1alpha1"
	"github.com/wutz/imagebuild/pkg/naming"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func gitSource(contextPath, dockerfilePath string) *v1.GitSource {
	return &v1.GitSource{URL: "https://example.com/app.git", Revision: "main", ContextPath: contextPath, DockerfilePath: dockerfilePath}
}

func testOptions(spec v1.ImageBuildSpec) Options {
	return Options{
		Name:          "app",
		Namespace:     "proj-build",
		Project:       "proj",
		HarborProject: "proj",
		Spec:          spec,
		Config: Config{
			BuilderImage:     "moby/buildkit:v0.34.1-rootless",
			GitImage:         "alpine/git:v2.54.0",
			Registry:         "harbor.example.com",
			RegistryInsecure: true,
		},
	}
}

func dest(tags ...string) v1.ImageBuildDestination {
	d := v1.ImageBuildDestination{Repository: "team/app"}
	for _, t := range tags {
		d.Tags = append(d.Tags, v1.ImageBuildTag(t))
	}
	return d
}

func TestCleanPath(t *testing.T) {
	tests := []struct {
		in, want string
		wantErr  bool
	}{
		{in: "", want: "."},
		{in: ".", want: "."},
		{in: "app/src", want: "app/src"},
		{in: "./app/", want: "app"},
		{in: "app/../lib", want: "lib"},
		{in: "/etc/passwd", wantErr: true},
		{in: "../secret", wantErr: true},
		{in: "app/../../secret", wantErr: true},
	}
	for _, tt := range tests {
		got, err := CleanPath(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("CleanPath(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if got != tt.want {
			t.Errorf("CleanPath(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestLayout(t *testing.T) {
	tests := []struct {
		name                         string
		src                          v1.ImageBuildSource
		wantContext, wantDir, wantDF string
		wantErr                      bool
	}{
		{
			name:        "repository root",
			src:         v1.ImageBuildSource{Git: gitSource(".", "Dockerfile")},
			wantContext: "/workspace/src",
			wantDir:     "/workspace/src",
			wantDF:      "Dockerfile",
		},
		{
			name:        "nested context and Dockerfile",
			src:         v1.ImageBuildSource{Git: gitSource("svc/api", "svc/api/build/Dockerfile.prod")},
			wantContext: "/workspace/src/svc/api",
			wantDir:     "/workspace/src/svc/api/build",
			wantDF:      "Dockerfile.prod",
		},
		{
			name:        "inline Dockerfile wins over the repository",
			src:         v1.ImageBuildSource{Git: gitSource("svc", "Dockerfile"), Dockerfile: "FROM alpine"},
			wantContext: "/workspace/src/svc",
			wantDir:     "/workspace/dockerfile",
			wantDF:      DockerfileKey,
		},
		{
			name:        "inline Dockerfile without a repository",
			src:         v1.ImageBuildSource{Dockerfile: "FROM alpine"},
			wantContext: "/workspace/src",
			wantDir:     "/workspace/dockerfile",
			wantDF:      DockerfileKey,
		},
		{name: "neither repository nor Dockerfile", src: v1.ImageBuildSource{}, wantErr: true},
		{name: "context leaves the repository", src: v1.ImageBuildSource{Git: gitSource("../x", "Dockerfile")}, wantErr: true},
		{name: "Dockerfile path leaves the repository", src: v1.ImageBuildSource{Git: gitSource(".", "/etc/Dockerfile")}, wantErr: true},
		{name: "Dockerfile path is the root", src: v1.ImageBuildSource{Git: gitSource(".", ".")}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx, dir, df, err := layout(tt.src)
			if (err != nil) != tt.wantErr {
				t.Fatalf("layout() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if ctx != tt.wantContext || dir != tt.wantDir || df != tt.wantDF {
				t.Errorf("layout() = (%q, %q, %q), want (%q, %q, %q)", ctx, dir, df, tt.wantContext, tt.wantDir, tt.wantDF)
			}
		})
	}
}

func TestImageRefs(t *testing.T) {
	got := ImageRefs("harbor.example.com", "proj", dest("v1", "latest"))
	want := []string{"harbor.example.com/proj/team/app:v1", "harbor.example.com/proj/team/app:latest"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ImageRefs() = %v, want %v", got, want)
	}
}

func TestOutputValue(t *testing.T) {
	refs := []string{"r/p/a:v1", "r/p/a:latest"}
	tests := []struct {
		insecure bool
		want     string
	}{
		{insecure: false, want: `type=image,"name=r/p/a:v1,r/p/a:latest",push=true`},
		{insecure: true, want: `type=image,"name=r/p/a:v1,r/p/a:latest",push=true,registry.insecure=true`},
	}
	for _, tt := range tests {
		if got := outputValue(refs, tt.insecure); got != tt.want {
			t.Errorf("outputValue(insecure=%v) = %q, want %q", tt.insecure, got, tt.want)
		}
	}
}

func TestParseDigest(t *testing.T) {
	tests := []struct {
		name, msg, want string
	}{
		{name: "digest with newline", msg: testDigest + "\n", want: testDigest},
		{name: "empty", msg: "", want: ""},
		{name: "short digest", msg: "sha256:abc", want: ""},
		{name: "uppercase hex", msg: strings.ToUpper(testDigest), want: ""},
		{name: "builder log instead of digest", msg: "error: push denied", want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ParseDigest(tt.msg); got != tt.want {
				t.Errorf("ParseDigest() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestValidateName(t *testing.T) {
	spec := v1.ImageBuildSpec{Source: v1.ImageBuildSource{Dockerfile: "FROM alpine"}}
	if err := Validate("Bad_Name", spec); err == nil {
		t.Error("Validate() accepted a name that is not a DNS label")
	}
	if err := Validate("app-1", spec); err != nil {
		t.Errorf("Validate() = %v, want nil", err)
	}
}

func TestJobFromRepository(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{
		Source:      v1.ImageBuildSource{Git: gitSource("svc", "svc/Dockerfile")},
		Destination: dest("v1"),
	})
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	if job.Namespace != "proj-build" || job.Name != "app" {
		t.Errorf("Job() = %s/%s, want proj-build/app", job.Namespace, job.Name)
	}
	if got := job.Spec.Template.Labels[naming.LabelImageBuild]; got != "app" {
		t.Errorf("pod label %s = %q, want app", naming.LabelImageBuild, got)
	}
	if n := len(job.Spec.Template.Spec.InitContainers); n != 1 {
		t.Fatalf("init containers = %d, want 1 clone container", n)
	}
	clone := job.Spec.Template.Spec.InitContainers[0]
	if clone.Image != "alpine/git:v2.54.0" {
		t.Errorf("clone image = %q", clone.Image)
	}
	env := envMap(clone.Env)
	if env["GIT_URL"] != "https://example.com/app.git" || env["GIT_REVISION"] != "main" {
		t.Errorf("clone env = %v", env)
	}
	if env["GIT_CONFIG_VALUE_0"] != "*" || env["GIT_CONFIG_KEY_0"] != "safe.directory" {
		t.Errorf("clone env misses safe.directory: %v", env)
	}

	b := job.Spec.Template.Spec.Containers[0]
	if b.Image != "moby/buildkit:v0.34.1-rootless" {
		t.Errorf("builder image = %q", b.Image)
	}
	wantArgs := []string{
		"build",
		"--frontend=dockerfile.v0",
		"--local=context=/workspace/src/svc",
		"--local=dockerfile=/workspace/src/svc",
		"--opt=filename=Dockerfile",
		"--output=type=image,\"name=harbor.example.com/proj/team/app:v1\",push=true,registry.insecure=true",
	}
	if !reflect.DeepEqual(b.Args, wantArgs) {
		t.Errorf("builder args = %q\nwant %q", b.Args, wantArgs)
	}
	if got := envMap(b.Env)["BUILDKITD_FLAGS"]; got != "--oci-worker-no-process-sandbox" {
		t.Errorf("BUILDKITD_FLAGS = %q", got)
	}
	// Without ROOTLESSKIT, buildctl-daemonless.sh runs buildkitd under rootlesskit.
	if v, ok := envMap(b.Env)["ROOTLESSKIT"]; ok {
		t.Errorf("ROOTLESSKIT = %q, want unset", v)
	}
	assertBuilderSecurity(t, b)
	if hu := job.Spec.Template.Spec.HostUsers; hu != nil {
		t.Errorf("HostUsers = %v, want unset so the builder's rootlesskit has its subuid range", *hu)
	}
	assertNoVolume(t, job, volDockerfile)
	if cm := DockerfileConfigMap(o); cm != nil {
		t.Errorf("DockerfileConfigMap() = %v for a repository build, want nil", cm)
	}
}

func TestJobFromInlineDockerfile(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{
		Source:      v1.ImageBuildSource{Dockerfile: "FROM alpine\nRUN echo hi\n"},
		Destination: dest("latest"),
	})
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	if n := len(job.Spec.Template.Spec.InitContainers); n != 0 {
		t.Errorf("init containers = %d, want none without a repository", n)
	}
	b := job.Spec.Template.Spec.Containers[0]
	for _, want := range []string{"--local=context=/workspace/src", "--local=dockerfile=/workspace/dockerfile", "--opt=filename=Dockerfile"} {
		if !slices.Contains(b.Args, want) {
			t.Errorf("builder args %q miss %q", b.Args, want)
		}
	}
	vol := findVolume(job, volDockerfile)
	if vol == nil || vol.ConfigMap == nil || vol.ConfigMap.Name != "app" {
		t.Fatalf("dockerfile volume = %+v, want the ConfigMap of the build", vol)
	}
	cm := DockerfileConfigMap(o)
	if cm == nil || cm.Name != "app" || cm.Namespace != "proj-build" || cm.Data[DockerfileKey] != "FROM alpine\nRUN echo hi\n" {
		t.Errorf("DockerfileConfigMap() = %+v", cm)
	}
}

func TestJobRejectsInvalidPaths(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{
		Source:      v1.ImageBuildSource{Git: gitSource("../escape", "Dockerfile")},
		Destination: dest("v1"),
	})
	if _, err := Job(o); err == nil {
		t.Error("Job() accepted a context path that leaves the repository")
	}
}

func TestJobRegistryInsecureOff(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{
		Source:      v1.ImageBuildSource{Dockerfile: "FROM alpine"},
		Destination: dest("v1"),
	})
	o.Config.RegistryInsecure = false
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	out := ""
	for _, a := range job.Spec.Template.Spec.Containers[0].Args {
		if strings.HasPrefix(a, "--output=") {
			out = a
		}
	}
	if strings.Contains(out, "registry.insecure") {
		t.Errorf("output %q sets registry.insecure for a TLS registry", out)
	}
}

func assertBuilderSecurity(t *testing.T, c corev1.Container) {
	t.Helper()
	sc := c.SecurityContext
	if sc == nil || sc.SeccompProfile == nil || sc.SeccompProfile.Type != corev1.SeccompProfileTypeLocalhost ||
		sc.SeccompProfile.LocalhostProfile == nil || *sc.SeccompProfile.LocalhostProfile != DefaultSeccompProfile {
		t.Errorf("builder seccomp = %+v, want Localhost %s", sc, DefaultSeccompProfile)
	}
	if sc == nil || sc.AppArmorProfile != nil {
		t.Errorf("builder apparmor = %+v, want unset", sc)
	}
	if sc == nil || sc.ProcMount != nil {
		t.Errorf("builder procMount = %+v, want unset", sc)
	}
	if sc == nil || sc.RunAsUser == nil || *sc.RunAsUser != builderUID {
		t.Errorf("builder runAsUser = %+v, want %d", sc, builderUID)
	}
	if sc == nil || sc.AllowPrivilegeEscalation == nil || !*sc.AllowPrivilegeEscalation {
		t.Errorf("builder allowPrivilegeEscalation = %+v, want true for newuidmap", sc)
	}
	if sc == nil || sc.Capabilities == nil || !slices.Equal(sc.Capabilities.Drop, []corev1.Capability{"ALL"}) ||
		!slices.Equal(sc.Capabilities.Add, []corev1.Capability{"SETUID", "SETGID"}) {
		t.Errorf("builder capabilities = %+v, want drop ALL and add SETUID, SETGID", sc)
	}
}

func assertNoVolume(t *testing.T, job *batchv1.Job, name string) {
	t.Helper()
	if findVolume(job, name) != nil {
		t.Errorf("volume %q present, want absent", name)
	}
}

func findVolume(job *batchv1.Job, name string) *corev1.Volume {
	for i := range job.Spec.Template.Spec.Volumes {
		if v := &job.Spec.Template.Spec.Volumes[i]; v.Name == name {
			return v
		}
	}
	return nil
}

func envMap(env []corev1.EnvVar) map[string]string {
	m := map[string]string{}
	for _, e := range env {
		m[e.Name] = e.Value
	}
	return m
}

func TestJobSeccompProfileFromConfig(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{Source: v1.ImageBuildSource{Dockerfile: "FROM alpine"}, Destination: dest("v1")})
	o.Config.SeccompProfile = "custom/build.json"
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	sp := job.Spec.Template.Spec.Containers[0].SecurityContext.SeccompProfile
	if sp.Type != corev1.SeccompProfileTypeLocalhost || sp.LocalhostProfile == nil || *sp.LocalhostProfile != "custom/build.json" {
		t.Errorf("seccomp = %+v, want Localhost custom/build.json", sp)
	}
}

func TestJobWithoutMirrors(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{Source: v1.ImageBuildSource{Dockerfile: "FROM alpine"}, Destination: dest("v1")})
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	env := envMap(job.Spec.Template.Spec.Containers[0].Env)
	if _, ok := env["BUILDKITD_CONFIG"]; ok {
		t.Errorf("BUILDKITD_CONFIG = %q, want unset without mirrors", env["BUILDKITD_CONFIG"])
	}
	if env["BUILDKITD_FLAGS"] != "--oci-worker-no-process-sandbox" {
		t.Errorf("BUILDKITD_FLAGS = %q", env["BUILDKITD_FLAGS"])
	}
}

func TestJobWithMirrors(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{Source: v1.ImageBuildSource{Dockerfile: "FROM alpine"}, Destination: dest("v1")})
	o.Config.RegistryMirrors = map[string]string{
		"quay.io":   "mir.example.com/quay.io",
		"docker.io": "mir.example.com/docker.io",
	}
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	b := job.Spec.Template.Spec.Containers[0]
	env := envMap(b.Env)
	want := "[registry.\"docker.io\"]\n  mirrors = [\"mir.example.com/docker.io\"]\n" +
		"[registry.\"quay.io\"]\n  mirrors = [\"mir.example.com/quay.io\"]"
	if env["BUILDKITD_CONFIG"] != want {
		t.Errorf("BUILDKITD_CONFIG = %q\nwant %q", env["BUILDKITD_CONFIG"], want)
	}
	if env["BUILDKITD_FLAGS"] != "--config=/tmp/buildkitd.toml --oci-worker-no-process-sandbox" {
		t.Errorf("BUILDKITD_FLAGS = %q", env["BUILDKITD_FLAGS"])
	}
	if !strings.Contains(b.Command[2], `"$BUILDKITD_CONFIG" > /tmp/buildkitd.toml`) {
		t.Errorf("build script does not write the config:\n%s", b.Command[2])
	}
	// The push target stays untouched.
	if out := b.Args[len(b.Args)-1]; !strings.Contains(out, `"name=harbor.example.com/proj/team/app:v1"`) {
		t.Errorf("output = %q, want the registry as push target", out)
	}
}

func TestConfigValidate(t *testing.T) {
	tests := []struct {
		name    string
		mirrors map[string]string
		wantErr bool
	}{
		{name: "none"},
		{name: "path", mirrors: map[string]string{"docker.io": "mir.example.com/docker.io"}},
		{name: "port", mirrors: map[string]string{"registry.local:5000": "mir.example.com:8443"}},
		{name: "scheme", mirrors: map[string]string{"docker.io": "https://mir.example.com"}, wantErr: true},
		{name: "quote", mirrors: map[string]string{"docker.io": `mir.example.com/"x`}, wantErr: true},
		{name: "newline", mirrors: map[string]string{"docker.io": "mir.example.com\n[x]"}, wantErr: true},
		{name: "bad host", mirrors: map[string]string{`docker.io"]`: "mir.example.com"}, wantErr: true},
		{name: "empty mirror", mirrors: map[string]string{"docker.io": ""}, wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{RegistryMirrors: tt.mirrors}.Validate()
			if (err != nil) != tt.wantErr {
				t.Errorf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}

func TestJobExcludeNodes(t *testing.T) {
	o := testOptions(v1.ImageBuildSpec{Source: v1.ImageBuildSource{Dockerfile: "FROM alpine"}, Destination: dest("v1")})
	job, err := Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	if a := job.Spec.Template.Spec.Affinity; a != nil {
		t.Errorf("affinity = %+v, want none without excluded nodes", a)
	}
	o.Config.ExcludeNodes = []string{"gn-1", "gn-2"}
	job, err = Job(o)
	if err != nil {
		t.Fatalf("Job() error = %v", err)
	}
	a := job.Spec.Template.Spec.Affinity
	if a == nil || a.NodeAffinity == nil || a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Fatalf("affinity = %+v, want required node affinity", a)
	}
	terms := a.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 || len(terms[0].MatchExpressions) != 1 {
		t.Fatalf("terms = %+v", terms)
	}
	e := terms[0].MatchExpressions[0]
	if e.Key != corev1.LabelHostname || e.Operator != corev1.NodeSelectorOpNotIn || !slices.Equal(e.Values, []string{"gn-1", "gn-2"}) {
		t.Errorf("expression = %+v, want hostname NotIn gn-1, gn-2", e)
	}
}

// The chart ships the profile the build pods run with. Keep it from drifting to
// a profile that allows everything or that blocks what BuildKit needs.
func TestShippedSeccompProfile(t *testing.T) {
	data, err := os.ReadFile("../../charts/imagebuild/files/seccomp/imagebuild.json")
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		DefaultAction string   `json:"defaultAction"`
		Architectures []string `json:"architectures"`
		Syscalls      []struct {
			Action string   `json:"action"`
			Names  []string `json:"names"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(data, &p); err != nil {
		t.Fatal(err)
	}
	if p.DefaultAction != "SCMP_ACT_ERRNO" {
		t.Errorf("defaultAction = %q, want SCMP_ACT_ERRNO", p.DefaultAction)
	}
	// runc rejects an architecture name it does not know.
	for _, a := range p.Architectures {
		if !slices.Contains([]string{"SCMP_ARCH_X86_64", "SCMP_ARCH_X86", "SCMP_ARCH_X32"}, a) {
			t.Errorf("architecture %q is not one runc knows on x86", a)
		}
	}
	allowed := map[string]bool{}
	for _, r := range p.Syscalls {
		if r.Action == "SCMP_ACT_ALLOW" {
			for _, n := range r.Names {
				allowed[n] = true
			}
		}
	}
	// rootlesskit, runc and apt need these.
	for _, n := range []string{"unshare", "setns", "mount", "pivot_root", "sethostname", "clone", "clone3", "vfork", "setresuid", "getgroups"} {
		if !allowed[n] {
			t.Errorf("syscall %q is not allowed, BuildKit needs it", n)
		}
	}
	for _, n := range []string{"ptrace", "kexec_load", "bpf", "reboot", "init_module", "add_key", "landlock_create_ruleset"} {
		if allowed[n] {
			t.Errorf("syscall %q is allowed unconditionally", n)
		}
	}
}
