// Package naming holds the label keys, finalizer and object names that the
// imagebuild operator and its clients share.
package naming

import "strings"

const (
	// LabelManagedBy is set on every object the operator creates.
	LabelManagedBy = "app.kubernetes.io/managed-by"
	// ManagedBy is the LabelManagedBy value of every object the operator creates.
	ManagedBy = "imagebuild"

	// LabelProject is set on project namespaces by project-operator. The build
	// objects carry it too (value = Project name).
	LabelProject = "project.wutz.dev/project"
	// LabelMember is set on member bindings by project-operator.
	LabelMember = "project.wutz.dev/member"
	// LabelAggregateToMember aggregates a ClusterRole into the project-edit or
	// project-view role of project-operator (value = edit or view).
	LabelAggregateToMember = "project.wutz.dev/aggregate-to-member"

	// LabelImageBuild is set on the Job, the pods and the Dockerfile ConfigMap of
	// an ImageBuild (value = ImageBuild name).
	LabelImageBuild = "imagebuild.wutz.dev/image-build"
	// LabelMemberBinding marks the RoleBindings the operator copies into a build
	// namespace for the members of its project.
	LabelMemberBinding = "imagebuild.wutz.dev/member-binding"
	// FinalizerImageBuild keeps an ImageBuild until its build objects in the
	// build namespace are deleted, since owner references cannot cross namespaces.
	FinalizerImageBuild = "imagebuild.wutz.dev/finalizer"

	// RegistrySecretName is the registry robot Secret project-operator keeps in
	// each project namespace. Builds copy it into the build namespace.
	RegistrySecretName = "project-registry"

	// ProjectMemberBindingPrefix starts the name of the RoleBinding project-operator
	// creates for each member in the project namespace.
	ProjectMemberBindingPrefix = "project-member-"
	// BuildViewRole is the ClusterRole that members get in a build namespace: read
	// access to the builds' Jobs, pods and logs, without the registry credentials.
	BuildViewRole = "imagebuild-build-view"
	// BuildNamespaceSuffix names the build namespace of a project namespace.
	BuildNamespaceSuffix = "-build"
)

// BuildNamespace is the namespace where the image builds of a project run. The
// BuildKit pods need the Unconfined seccomp profile, which the restricted and
// baseline Pod Security levels reject, so the namespace is privileged.
func BuildNamespace(namespace string) string { return namespace + BuildNamespaceSuffix }

// ProjectOfBuildNamespace is the project namespace of a build namespace.
func ProjectOfBuildNamespace(buildNamespace string) string {
	return strings.TrimSuffix(buildNamespace, BuildNamespaceSuffix)
}

// MemberBinding is the build-namespace RoleBinding of a project member.
func MemberBinding(member string) string { return "imagebuild-member-" + member }
