# imagebuild

imagebuild is a Kubernetes operator that builds container images with rootless
BuildKit. An `ImageBuild` clones a Git repository, or takes an inline
Dockerfile, builds the image and pushes it to a registry.

Each build runs as a Job in the build namespace of its project, `<project>-build`.
The build namespace enforces the baseline Pod Security level. BuildKit runs
rootless with a Localhost seccomp profile instead of Unconfined, see
[Seccomp profile](#seccomp-profile). The members of the project get read-only
access to it: the Jobs, pods and logs of the builds, never the registry credentials.

## Usage

Create an `ImageBuild` in a project namespace:

```yaml
apiVersion: imagebuild.wutz.dev/v1alpha1
kind: ImageBuild
metadata:
  name: fileserver
  namespace: my-project
spec:
  source:
    git:
      url: https://github.com/wutz/fileserver.git
      revision: main
      contextPath: .
      dockerfilePath: Dockerfile
  destination:
    repository: team/fileserver
    tags:
      - v1
```

The source is either `git` or `dockerfile`. A `dockerfile` is inline and wins
over a Dockerfile in the repository. Paths are relative to the repository and
cannot leave it.

The status reports the `phase` (`Pending`, `Running`, `Succeeded` or `Failed`),
the `images` pushed, the `digest` and the times of the build. The image is pushed
to `<registry>/<project namespace>/<repository>:<tag>`.

## Install

The operator is published as a Helm chart and an image:

```sh
helm install imagebuild oci://ghcr.io/wutz/charts/imagebuild --version 0.1.0 \
  --namespace imagebuild-system --create-namespace \
  --set config.registry=harbor.example.com
```

The chart sets the registry the builds push to, the BuildKit image and the
image that clones the repositories. Without `config.registry`, every build fails
with a message that says so.

### Seccomp profile

The build pods run with the Localhost seccomp profile `operator/imagebuild.json`
(`config.seccompProfile`). The default profile of the container runtime blocks
the namespace calls that rootlesskit makes, and the baseline Pod Security level
rejects Unconfined. The profile is the runtime default plus those calls, minus the
syscalls BuildKit never needs; `charts/imagebuild/files/seccomp/imagebuild.json`
is its source.

The kubelet loads the profile from its seccomp directory, so the file must exist
on every node a build can run on. A node without it leaves the build pod stuck, and
the `ImageBuild` message says that the container cannot start. The chart can
install the profile with the [Security Profiles Operator](https://github.com/kubernetes-sigs/security-profiles-operator),
which writes it to `/var/lib/kubelet/seccomp/operator/imagebuild.json`:

```sh
--set seccompProfile.install=true
```

This needs the Security Profiles Operator and its `SeccompProfile` CRD in the
cluster. Without the operator, put the `spec` of the file on the nodes yourself and set
`config.seccompProfile` to its path below the kubelet's seccomp directory.

Install the profile before you upgrade an operator that still uses Unconfined, because
the build namespaces move to baseline as soon as the new operator creates a build.

`config.excludeNodes` keeps build pods off nodes that cannot run them, for
example a node without the profile.

### Registry mirrors

`config.registryMirrors` sends the pulls of the builds to mirrors, which helps
in clusters that cannot reach a public registry. Keys are registry domains, and
values are the mirror with an optional path:

```yaml
config:
  registryMirrors:
    docker.io: mirror.example.com/docker.io
    ghcr.io: mirror.example.com/ghcr.io
```

A `FROM busybox` then pulls `mirror.example.com/docker.io/library/busybox`. The
mirrors only apply to pulls, not to the push to `config.registry`. BuildKit falls
back to the original registry when a mirror fails. The default is no mirrors.

The chart also creates the ClusterRoles that the workspace operator aggregates into the
project roles: `imagebuild-member-edit` and `imagebuild-member-view`. It creates
`imagebuild-build-view`, the role members are bound to in a build namespace.

## Project contract

imagebuild relies on the workspace operator for the project itself:

- the project namespace carries the label `workspace.wutz.dev/project`;
- the registry credentials are the Secret `workspace-registry` in the project namespace;
- each member has a RoleBinding `project-member-<member>` in the project namespace.

The operator copies the credentials and binds the members into the build
namespace, so a build can push without the project's secrets being readable by
its members.

## Development

```sh
make test          # envtest suite and the unit tests
make lint          # golangci-lint
make helm-lint     # lint the chart
make helm-template # render the chart with its default values
```

`charts/imagebuild/templates/crd.yaml` is a copy of
`config/crd/bases/imagebuild.wutz.dev_imagebuilds.yaml`. `make manifests` keeps
the two in step. `charts/imagebuild/templates/clusterrole.yaml` mirrors the
generated `config/rbac/role.yaml` and is kept in step by hand.

## Release

Pushing a `v<major>.<minor>.<patch>` tag publishes the image
`ghcr.io/wutz/imagebuild:<version>` and the chart
`oci://ghcr.io/wutz/charts/imagebuild:<version>`. Set the chart version in
`CHART_VERSION` and `charts/imagebuild/Chart.yaml` to the tag first.

## License

Apache License 2.0.
