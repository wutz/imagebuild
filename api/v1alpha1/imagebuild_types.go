/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// ImageBuildSpec defines the desired state of ImageBuild. The spec is immutable:
// a different build is a new ImageBuild.
// +kubebuilder:validation:XValidation:rule="self == oldSelf",message="spec is immutable"
type ImageBuildSpec struct {
	// source is the build context and the Dockerfile.
	Source ImageBuildSource `json:"source"`
	// destination is the repository and the tags the image is pushed to in the
	// project registry.
	Destination ImageBuildDestination `json:"destination"`
}

// ImageBuildSource is where the build context and the Dockerfile come from.
// +kubebuilder:validation:XValidation:rule="has(self.git) || has(self.dockerfile)",message="a git repository or a Dockerfile is required"
type ImageBuildSource struct {
	// git is the repository the build context is cloned from. Without it the
	// context is empty and the Dockerfile must not copy files from it.
	// +optional
	Git *GitSource `json:"git,omitempty"`
	// dockerfile is the Dockerfile content. When set it is used instead of the
	// Dockerfile of the repository.
	// +optional
	// +kubebuilder:validation:MaxLength=65536
	Dockerfile string `json:"dockerfile,omitempty"`
}

// GitSource is a repository cloned over HTTPS for the build.
type GitSource struct {
	// url is the HTTPS address of the repository.
	// +kubebuilder:validation:Pattern=`^https://[^\s]+$`
	// +kubebuilder:validation:MaxLength=512
	URL string `json:"url"`
	// revision is a branch, a tag or a commit.
	// +kubebuilder:default=HEAD
	// +kubebuilder:validation:MaxLength=255
	// +optional
	Revision string `json:"revision,omitempty"`
	// contextPath is the directory of the repository used as the build context,
	// relative to its root.
	// +kubebuilder:default="."
	// +kubebuilder:validation:MaxLength=255
	// +optional
	ContextPath string `json:"contextPath,omitempty"`
	// dockerfilePath is the Dockerfile in the repository, relative to its root.
	// +kubebuilder:default=Dockerfile
	// +kubebuilder:validation:MaxLength=255
	// +optional
	DockerfilePath string `json:"dockerfilePath,omitempty"`
}

// ImageBuildDestination is the image repository and its tags in the project registry.
type ImageBuildDestination struct {
	// repository is the image repository, without the registry or the project.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]+([._-][a-z0-9]+)*(/[a-z0-9]+([._-][a-z0-9]+)*)*$`
	// +kubebuilder:validation:MaxLength=200
	Repository string `json:"repository"`
	// tags are pushed for the image.
	// +kubebuilder:validation:MinItems=1
	// +kubebuilder:validation:MaxItems=8
	// +listType=set
	Tags []ImageBuildTag `json:"tags"`
}

// ImageBuildTag is a tag of the pushed image.
// +kubebuilder:validation:Pattern=`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`
type ImageBuildTag string

// ImageBuildPhase summarises the build.
// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed
type ImageBuildPhase string

const (
	ImageBuildPending   ImageBuildPhase = "Pending"
	ImageBuildRunning   ImageBuildPhase = "Running"
	ImageBuildSucceeded ImageBuildPhase = "Succeeded"
	// ImageBuildFailed means the build or the push failed, or the spec cannot be built.
	ImageBuildFailed ImageBuildPhase = "Failed"
)

// ImageBuildStatus defines the observed state of ImageBuild.
type ImageBuildStatus struct {
	// +optional
	Phase ImageBuildPhase `json:"phase,omitempty"`
	// message says why the build failed, or what it waits for.
	// +optional
	Message string `json:"message,omitempty"`
	// images are the pushed references, one per tag.
	// +optional
	Images []string `json:"images,omitempty"`
	// digest is the digest of the pushed image, when the builder reports it.
	// +optional
	Digest string `json:"digest,omitempty"`
	// startTime is when the build pod started.
	// +optional
	StartTime *metav1.Time `json:"startTime,omitempty"`
	// completionTime is when the build finished.
	// +optional
	CompletionTime *metav1.Time `json:"completionTime,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=imb,categories=imagebuild
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Repository",type=string,JSONPath=`.spec.destination.repository`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ImageBuild builds an image from a Dockerfile with BuildKit in the build
// namespace of its project and pushes it to the project registry.
type ImageBuild struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ImageBuild
	// +optional
	Spec ImageBuildSpec `json:"spec"`

	// status defines the observed state of ImageBuild
	// +optional
	Status ImageBuildStatus `json:"status,omitzero"`
}

// +kubebuilder:object:root=true

// ImageBuildList contains a list of ImageBuild
type ImageBuildList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ImageBuild `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ImageBuild{}, &ImageBuildList{})
		return nil
	})
}
