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

package controller

import (
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1 "github.com/wutz/imagebuild/api/v1alpha1"
	"github.com/wutz/imagebuild/pkg/imagebuild"
	"github.com/wutz/imagebuild/pkg/naming"
)

const (
	ibProject = "imgbuild"
	ibNS      = "forge-imgbuild"
	ibBuildNS = "forge-imgbuild-build"
	// ibImage is the image of the inline builds, pushed to the test registry.
	ibImage = "cr.example.com:7443/forge-imgbuild/team/app:v1"

	timeout  = 10 * time.Second
	interval = 100 * time.Millisecond
)

func inlineBuild(name string) *v1.ImageBuild {
	return imageBuild(name, v1.ImageBuildSource{Dockerfile: "FROM alpine\nRUN echo hi\n"})
}

func imageBuild(name string, src v1.ImageBuildSource) *v1.ImageBuild {
	return &v1.ImageBuild{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ibNS},
		Spec: v1.ImageBuildSpec{
			Source:      src,
			Destination: v1.ImageBuildDestination{Repository: "team/app", Tags: []v1.ImageBuildTag{"v1"}},
		},
	}
}

func getImageBuild(name string) *v1.ImageBuild {
	b := &v1.ImageBuild{}
	ExpectWithOffset(1, get(b, ibNS, name)()).To(Succeed())
	return b
}

// completeJob marks a build Job complete as the Job controller does.
func completeJob(name string) {
	Eventually(func() error {
		job := &batchv1.Job{}
		if err := get(job, ibBuildNS, name)(); err != nil {
			return err
		}
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.CompletionTime = &now
		job.Status.Succeeded = 1
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobSuccessCriteriaMet, Status: corev1.ConditionTrue, LastTransitionTime: now},
			{Type: batchv1.JobComplete, Status: corev1.ConditionTrue, LastTransitionTime: now},
		}
		return k8sClient.Status().Update(ctx, job)
	}, timeout, interval).Should(Succeed())
}

// failJob marks a build Job failed as the Job controller does.
func failJob(name, reason, message string) {
	Eventually(func() error {
		job := &batchv1.Job{}
		if err := get(job, ibBuildNS, name)(); err != nil {
			return err
		}
		now := metav1.Now()
		job.Status.StartTime = &now
		job.Status.Conditions = []batchv1.JobCondition{
			{Type: batchv1.JobFailureTarget, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now},
			{Type: batchv1.JobFailed, Status: corev1.ConditionTrue, Reason: reason, Message: message, LastTransitionTime: now},
		}
		return k8sClient.Status().Update(ctx, job)
	}, timeout, interval).Should(Succeed())
}

var _ = Describe("ImageBuild controller", Ordered, func() {
	BeforeAll(func() {
		// project-operator labels the project namespace, keeps the registry robot
		// secret in it and binds each member with a project-member RoleBinding.
		Expect(k8sClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{
			Name:   ibNS,
			Labels: map[string]string{naming.LabelProject: ibProject},
		}})).To(Succeed())
		Expect(k8sClient.Create(ctx, &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: naming.RegistrySecretName, Namespace: ibNS},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
		})).To(Succeed())
		for _, member := range []string{"alice", "bob"} {
			Expect(k8sClient.Create(ctx, &rbacv1.RoleBinding{
				ObjectMeta: metav1.ObjectMeta{
					Name: naming.ProjectMemberBindingPrefix + member, Namespace: ibNS,
					Labels: map[string]string{naming.LabelProject: ibProject},
				},
				RoleRef: rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: "project-edit"},
				Subjects: []rbacv1.Subject{{
					Kind: rbacv1.ServiceAccountKind, Name: ibProject + "." + member, Namespace: "project-members",
				}},
			})).To(Succeed())
		}
	})

	It("starts a Job for an inline Dockerfile in a privileged build namespace", func() {
		Expect(k8sClient.Create(ctx, inlineBuild("app"))).To(Succeed())

		// The build namespace is created by the first build of the project.
		bns := &corev1.Namespace{}
		Eventually(get(bns, "", ibBuildNS), timeout, interval).Should(Succeed())
		Expect(bns.Labels[naming.LabelProject]).To(Equal(ibProject))
		Expect(bns.Labels["pod-security.kubernetes.io/enforce"]).To(Equal("privileged"))

		// Members get the read-only build role, not the edit role of the project.
		for _, member := range []string{"alice", "bob"} {
			rb := &rbacv1.RoleBinding{}
			Eventually(get(rb, ibBuildNS, naming.MemberBinding(member)), timeout, interval).Should(Succeed())
			Expect(rb.RoleRef.Name).To(Equal(naming.BuildViewRole),
				"member %s must not get an edit role in the build namespace", member)
			Expect(rb.Subjects).To(HaveLen(1))
			Expect(rb.Subjects[0].Name).To(Equal(ibProject + "." + member))
		}

		// The registry credentials are copied in for the builds to push with.
		Eventually(get(&corev1.Secret{}, ibBuildNS, naming.RegistrySecretName), timeout, interval).Should(Succeed())

		job := &batchv1.Job{}
		Eventually(get(job, ibBuildNS, "app"), timeout, interval).Should(Succeed())
		Expect(job.Labels[naming.LabelImageBuild]).To(Equal("app"))
		Expect(job.Labels[naming.LabelProject]).To(Equal(ibProject))
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElement(HavePrefix(
			`--output=type=image,"name=` + ibImage + `",push=true`)))

		cm := &corev1.ConfigMap{}
		Expect(get(cm, ibBuildNS, "app")()).To(Succeed())
		Expect(cm.Data[imagebuild.DockerfileKey]).To(Equal("FROM alpine\nRUN echo hi\n"))

		Eventually(func() v1.ImageBuildPhase { return getImageBuild("app").Status.Phase }, timeout, interval).
			Should(Equal(v1.ImageBuildPending))
	})

	It("clones a repository into the build Job", func() {
		b := imageBuild("repo", v1.ImageBuildSource{Git: &v1.GitSource{
			URL: "https://example.com/app.git", Revision: "main", ContextPath: "svc", DockerfilePath: "svc/Dockerfile",
		}})
		Expect(k8sClient.Create(ctx, b)).To(Succeed())

		job := &batchv1.Job{}
		Eventually(get(job, ibBuildNS, "repo"), timeout, interval).Should(Succeed())
		Expect(job.Spec.Template.Spec.InitContainers).To(HaveLen(1))
		Expect(job.Spec.Template.Spec.InitContainers[0].Name).To(Equal("clone"))
		Expect(job.Spec.Template.Spec.Containers[0].Args).To(ContainElements(
			"--local=context=/workspace/src/svc",
			"--local=dockerfile=/workspace/src/svc",
			"--opt=filename=Dockerfile",
		))
	})

	It("marks a build Succeeded with its image references when the Job completes", func() {
		Eventually(get(&batchv1.Job{}, ibBuildNS, "app"), timeout, interval).Should(Succeed())
		completeJob("app")

		Eventually(func() v1.ImageBuildPhase { return getImageBuild("app").Status.Phase }, timeout, interval).
			Should(Equal(v1.ImageBuildSucceeded))
		b := getImageBuild("app")
		Expect(b.Status.Images).To(Equal([]string{ibImage}))
		Expect(b.Status.CompletionTime).NotTo(BeNil())
		Expect(b.Status.Message).To(BeEmpty())
	})

	It("marks a build Failed with the reason of its Job", func() {
		Expect(k8sClient.Create(ctx, inlineBuild("broken"))).To(Succeed())
		Eventually(get(&batchv1.Job{}, ibBuildNS, "broken"), timeout, interval).Should(Succeed())
		failJob("broken", "BackoffLimitExceeded", "Job has reached the specified backoff limit")

		Eventually(func() v1.ImageBuildPhase { return getImageBuild("broken").Status.Phase }, timeout, interval).
			Should(Equal(v1.ImageBuildFailed))
		Expect(getImageBuild("broken").Status.Message).To(ContainSubstring("backoff limit"))
	})

	It("fails a build whose context leaves the repository without creating a Job", func() {
		b := imageBuild("escape", v1.ImageBuildSource{Git: &v1.GitSource{
			URL: "https://example.com/app.git", ContextPath: "../x",
		}})
		Expect(k8sClient.Create(ctx, b)).To(Succeed())

		Eventually(func() v1.ImageBuildPhase { return getImageBuild("escape").Status.Phase }, timeout, interval).
			Should(Equal(v1.ImageBuildFailed))
		Expect(getImageBuild("escape").Status.Message).To(ContainSubstring("leaves the repository"))
		Expect(apierrors.IsNotFound(get(&batchv1.Job{}, ibBuildNS, "escape")())).To(BeTrue())
	})

	It("fails a build whose name is not a DNS label", func() {
		Expect(k8sClient.Create(ctx, inlineBuild("a.b"))).To(Succeed())

		Eventually(func() v1.ImageBuildPhase { return getImageBuild("a.b").Status.Phase }, timeout, interval).
			Should(Equal(v1.ImageBuildFailed))
		Expect(getImageBuild("a.b").Status.Message).To(ContainSubstring("is not a DNS label"))
		Expect(apierrors.IsNotFound(get(&batchv1.Job{}, ibBuildNS, "a.b")())).To(BeTrue())
	})

	It("deletes the Job and the Dockerfile ConfigMap with the ImageBuild", func() {
		Expect(k8sClient.Delete(ctx, getImageBuild("app"))).To(Succeed())

		Eventually(func() bool { return apierrors.IsNotFound(get(&batchv1.Job{}, ibBuildNS, "app")()) },
			timeout, interval).Should(BeTrue())
		Eventually(func() bool { return apierrors.IsNotFound(get(&corev1.ConfigMap{}, ibBuildNS, "app")()) },
			timeout, interval).Should(BeTrue())
		Eventually(func() bool { return apierrors.IsNotFound(get(&v1.ImageBuild{}, ibNS, "app")()) },
			timeout, interval).Should(BeTrue())
	})
})
