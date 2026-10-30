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
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	v1 "github.com/wutz/imagebuild/api/v1alpha1"
	"github.com/wutz/imagebuild/pkg/imagebuild"
	"github.com/wutz/imagebuild/pkg/naming"
)

const (
	// buildContainer is the container of the build pod that runs buildctl.
	buildContainer = "build"
	// waitInterval is how often a build waiting for its registry credentials is checked.
	waitInterval = 10 * time.Second
	// fieldOwner is the server-side apply manager of the objects the operator writes.
	fieldOwner = "imagebuild"
	// podSecurityEnforce is the label that sets the Pod Security level of a namespace.
	podSecurityEnforce = "pod-security.kubernetes.io/enforce"
)

// ImageBuildReconciler builds the images of ImageBuilds with a Job in the build
// namespace of their project and pushes them to the registry.
type ImageBuildReconciler struct {
	client.Client
	// APIReader reads objects outside the label-filtered cache.
	APIReader client.Reader
	// Config is the registry and builder images the operator runs with.
	Config imagebuild.Config
}

// +kubebuilder:rbac:groups=imagebuild.wutz.dev,resources=imagebuilds,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=imagebuild.wutz.dev,resources=imagebuilds/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=imagebuild.wutz.dev,resources=imagebuilds/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=rolebindings,verbs=get;list;watch;create;update;patch;delete

func (r *ImageBuildReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)
	b := &v1.ImageBuild{}
	if err := r.Get(ctx, req.NamespacedName, b); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	bns := naming.BuildNamespace(b.Namespace)

	if !b.DeletionTimestamp.IsZero() {
		if !controllerutil.ContainsFinalizer(b, naming.FinalizerImageBuild) {
			return ctrl.Result{}, nil
		}
		// The build objects live in another namespace, so the finalizer deletes them.
		if err := r.deleteBuildObjects(ctx, b.Name, bns); err != nil {
			return ctrl.Result{}, err
		}
		controllerutil.RemoveFinalizer(b, naming.FinalizerImageBuild)
		return ctrl.Result{}, r.Update(ctx, b)
	}
	if controllerutil.AddFinalizer(b, naming.FinalizerImageBuild) {
		if err := r.Update(ctx, b); err != nil {
			return ctrl.Result{}, err
		}
	}

	orig := b.DeepCopy()
	res, err := r.reconcileBuild(ctx, b, bns)
	if err != nil {
		return res, err
	}
	if err := r.Status().Patch(ctx, b, client.MergeFromWithOptions(orig, client.MergeFromWithOptimisticLock{})); err != nil {
		return ctrl.Result{}, err
	}
	if b.Status.Phase != orig.Status.Phase {
		log.Info("Image build changed phase", "imagebuild", b.Name, "phase", b.Status.Phase)
	}
	return res, nil
}

// reconcileBuild starts the Job of the build once and then maps its state to the status.
func (r *ImageBuildReconciler) reconcileBuild(ctx context.Context, b *v1.ImageBuild, bns string) (ctrl.Result, error) {
	if b.Status.Phase == v1.ImageBuildSucceeded || b.Status.Phase == v1.ImageBuildFailed {
		return ctrl.Result{}, nil
	}
	if r.Config.Registry == "" {
		failBuild(b, "the operator has no image registry configured")
		return ctrl.Result{}, nil
	}
	if err := imagebuild.Validate(b.Name, b.Spec); err != nil {
		failBuild(b, err.Error())
		return ctrl.Result{}, nil
	}
	pns := &corev1.Namespace{}
	if err := r.APIReader.Get(ctx, types.NamespacedName{Name: b.Namespace}, pns); err != nil {
		return ctrl.Result{}, err
	}
	project := pns.Labels[naming.LabelProject]
	if project == "" {
		failBuild(b, "the namespace is not a project namespace")
		return ctrl.Result{}, nil
	}
	opts := imagebuild.Options{
		Name:          b.Name,
		Namespace:     bns,
		Project:       project,
		HarborProject: b.Namespace,
		Spec:          b.Spec,
		Config:        r.Config,
	}

	job := &batchv1.Job{}
	err := r.Get(ctx, types.NamespacedName{Namespace: bns, Name: b.Name}, job)
	switch {
	case apierrors.IsNotFound(err) && b.Status.Phase == v1.ImageBuildRunning:
		// The build namespace or its Job was removed under a running build.
		failBuild(b, "the build objects were removed")
		return ctrl.Result{}, nil
	case apierrors.IsNotFound(err):
		return r.startBuild(ctx, b, opts)
	case err != nil:
		return ctrl.Result{}, err
	}
	r.observeBuild(ctx, b, job, opts)
	return ctrl.Result{}, nil
}

// startBuild prepares the build namespace, creates the Dockerfile ConfigMap and
// the Job. It waits while the registry credentials of the project are not there.
func (r *ImageBuildReconciler) startBuild(ctx context.Context, b *v1.ImageBuild, opts imagebuild.Options) (ctrl.Result, error) {
	if err := r.ensureBuildNamespace(ctx, b.Namespace, opts.Namespace, opts.Project); err != nil {
		return ctrl.Result{}, err
	}
	// The copy is made per build so a rotated robot secret reaches new builds.
	found, err := copyRegistrySecret(ctx, r.Client, r.APIReader, b.Namespace, opts.Namespace)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !found {
		return waitBuild(b, "waiting for the registry credentials")
	}
	if cm := imagebuild.DockerfileConfigMap(opts); cm != nil {
		if err := apply(ctx, r.Client, cm); err != nil {
			return ctrl.Result{}, err
		}
	}
	job, err := imagebuild.Job(opts)
	if err != nil {
		failBuild(b, err.Error())
		return ctrl.Result{}, nil
	}
	if err := r.Create(ctx, job); err != nil && !apierrors.IsAlreadyExists(err) {
		return ctrl.Result{}, err
	}
	b.Status.Phase = v1.ImageBuildPending
	b.Status.Message = ""
	return ctrl.Result{}, nil
}

// ensureBuildNamespace creates the build namespace of a project and binds the
// project's members to it with the read-only build role. The namespace enforces
// the baseline Pod Security level: the build pods use a Localhost seccomp profile,
// which baseline accepts.
func (r *ImageBuildReconciler) ensureBuildNamespace(ctx context.Context, projectNS, bns, project string) error {
	if err := apply(ctx, r.Client, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: bns, Labels: map[string]string{
			naming.LabelManagedBy: naming.ManagedBy,
			naming.LabelProject:   project,
			podSecurityEnforce:    "baseline",
		}},
	}); err != nil {
		return err
	}
	return r.syncMemberBindings(ctx, projectNS, bns, project)
}

// syncMemberBindings binds each member of the project to the build role in the
// build namespace, and deletes the bindings of members that are gone.
func (r *ImageBuildReconciler) syncMemberBindings(ctx context.Context, projectNS, bns, project string) error {
	rbs := &rbacv1.RoleBindingList{}
	if err := r.APIReader.List(ctx, rbs, client.InNamespace(projectNS)); err != nil {
		return err
	}
	want := map[string]bool{}
	for _, rb := range rbs.Items {
		if !strings.HasPrefix(rb.Name, naming.ProjectMemberBindingPrefix) || rb.Labels[naming.LabelProject] != project {
			continue
		}
		name := naming.MemberBinding(strings.TrimPrefix(rb.Name, naming.ProjectMemberBindingPrefix))
		want[name] = true
		if err := apply(ctx, r.Client, &rbacv1.RoleBinding{
			ObjectMeta: metav1.ObjectMeta{
				Name: name, Namespace: bns,
				Labels: map[string]string{naming.LabelManagedBy: naming.ManagedBy, naming.LabelProject: project, naming.LabelMemberBinding: "true"},
			},
			RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "ClusterRole", Name: naming.BuildViewRole},
			Subjects: rb.Subjects,
		}); err != nil {
			return err
		}
	}
	stale := &rbacv1.RoleBindingList{}
	if err := r.List(ctx, stale, client.InNamespace(bns), client.MatchingLabels{naming.LabelMemberBinding: "true"}); err != nil {
		return err
	}
	for i := range stale.Items {
		if rb := &stale.Items[i]; !want[rb.Name] {
			if err := client.IgnoreNotFound(r.Delete(ctx, rb)); err != nil {
				return err
			}
		}
	}
	return nil
}

// observeBuild maps the Job, and the pod it ran, to the phase of the build.
func (r *ImageBuildReconciler) observeBuild(ctx context.Context, b *v1.ImageBuild, job *batchv1.Job, opts imagebuild.Options) {
	if job.Status.StartTime != nil {
		b.Status.StartTime = job.Status.StartTime
	}
	if c := jobCondition(job, batchv1.JobComplete); c != nil || job.Status.Succeeded > 0 {
		b.Status.Phase = v1.ImageBuildSucceeded
		b.Status.Message = ""
		b.Status.Images = imagebuild.ImageRefs(r.Config.Registry, opts.HarborProject, b.Spec.Destination)
		b.Status.CompletionTime = job.Status.CompletionTime
		// The digest is informational: the image is pushed without it.
		if pod, err := r.buildPod(ctx, opts.Namespace, b.Name); err == nil && pod != nil {
			b.Status.Digest = imagebuild.ParseDigest(terminationMessage(pod))
		}
		return
	}
	if c := jobCondition(job, batchv1.JobFailed); c != nil {
		msg := c.Message
		if msg == "" {
			msg = c.Reason
		}
		if pod, err := r.buildPod(ctx, opts.Namespace, b.Name); err == nil && pod != nil {
			if t := buildTerminated(pod); t != nil && t.ExitCode != 0 {
				msg += ", the build container exited with code " + strconv.Itoa(int(t.ExitCode))
			}
		}
		failBuild(b, msg)
		b.Status.CompletionTime = job.Status.CompletionTime
		return
	}
	if job.Status.Active > 0 {
		b.Status.Phase = v1.ImageBuildRunning
	} else {
		b.Status.Phase = v1.ImageBuildPending
	}
	b.Status.Message = ""
	// A pod that cannot start, such as one on a node without the seccomp profile,
	// stays pending until the Job times out. Say why meanwhile.
	if pod, err := r.buildPod(ctx, opts.Namespace, b.Name); err == nil && pod != nil {
		if msg := startProblem(pod); msg != "" {
			b.Status.Message = msg
		}
	}
}

// startProblem describes why a container of the pod cannot start, or returns "".
// The kubelet reports these in the waiting state of the container, where a
// missing or invalid seccomp profile shows up too.
func startProblem(pod *corev1.Pod) string {
	statuses := make([]corev1.ContainerStatus, 0, len(pod.Status.InitContainerStatuses)+len(pod.Status.ContainerStatuses))
	statuses = append(statuses, pod.Status.InitContainerStatuses...)
	statuses = append(statuses, pod.Status.ContainerStatuses...)
	for _, s := range statuses {
		w := s.State.Waiting
		if w == nil {
			continue
		}
		switch w.Reason {
		case "CreateContainerConfigError", "CreateContainerError", "RunContainerError":
			msg := fmt.Sprintf("the %s container cannot start on node %s: %s", s.Name, pod.Spec.NodeName, w.Reason)
			if w.Message != "" {
				msg += ": " + w.Message
			}
			return msg
		}
	}
	return ""
}

// buildPod returns the newest pod of the build, or nil when there is none.
func (r *ImageBuildReconciler) buildPod(ctx context.Context, bns, name string) (*corev1.Pod, error) {
	pods := &corev1.PodList{}
	if err := r.List(ctx, pods, client.InNamespace(bns), client.MatchingLabels{naming.LabelImageBuild: name}); err != nil {
		return nil, err
	}
	var newest *corev1.Pod
	for i := range pods.Items {
		p := &pods.Items[i]
		if newest == nil || p.CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = p
		}
	}
	return newest, nil
}

// buildTerminated is the terminated state of the build container, or nil.
func buildTerminated(pod *corev1.Pod) *corev1.ContainerStateTerminated {
	for _, s := range pod.Status.ContainerStatuses {
		if s.Name == buildContainer && s.State.Terminated != nil {
			return s.State.Terminated
		}
	}
	return nil
}

// terminationMessage is the message the build container wrote when it exited successfully.
func terminationMessage(pod *corev1.Pod) string {
	if t := buildTerminated(pod); t != nil && t.ExitCode == 0 {
		return t.Message
	}
	return ""
}

// copyRegistrySecret copies the registry Secret of a project into its build
// namespace. It reports false when the project has none yet.
func copyRegistrySecret(ctx context.Context, c client.Client, reader client.Reader, ns, bns string) (bool, error) {
	src := &corev1.Secret{}
	err := reader.Get(ctx, types.NamespacedName{Namespace: ns, Name: naming.RegistrySecretName}, src)
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, apply(ctx, c, &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: naming.RegistrySecretName, Namespace: bns, Labels: src.Labels},
		Type:       src.Type,
		Data:       src.Data,
	})
}

// deleteBuildObjects deletes the Job, with its pods, and the Dockerfile ConfigMap.
func (r *ImageBuildReconciler) deleteBuildObjects(ctx context.Context, name, bns string) error {
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bns}}
	if err := client.IgnoreNotFound(r.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))); err != nil {
		return err
	}
	cm := &corev1.ConfigMap{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: bns}}
	return client.IgnoreNotFound(r.Delete(ctx, cm))
}

// apply server-side applies obj as the operator's field owner, taking over its fields.
func apply(ctx context.Context, c client.Client, obj client.Object) error {
	m, err := runtime.DefaultUnstructuredConverter.ToUnstructured(obj)
	if err != nil {
		return err
	}
	u := &unstructured.Unstructured{Object: m}
	gvk, err := c.GroupVersionKindFor(obj)
	if err != nil {
		return err
	}
	u.SetGroupVersionKind(gvk)
	unstructured.RemoveNestedField(u.Object, "metadata", "creationTimestamp")
	unstructured.RemoveNestedField(u.Object, "status")
	return c.Apply(ctx, client.ApplyConfigurationFromUnstructured(u), client.FieldOwner(fieldOwner), client.ForceOwnership)
}

func jobCondition(job *batchv1.Job, t batchv1.JobConditionType) *batchv1.JobCondition {
	for i := range job.Status.Conditions {
		if c := &job.Status.Conditions[i]; c.Type == t && c.Status == corev1.ConditionTrue {
			return c
		}
	}
	return nil
}

func failBuild(b *v1.ImageBuild, msg string) {
	b.Status.Phase = v1.ImageBuildFailed
	b.Status.Message = msg
}

// waitBuild keeps the build pending and checks again later.
func waitBuild(b *v1.ImageBuild, msg string) (ctrl.Result, error) {
	b.Status.Phase = v1.ImageBuildPending
	b.Status.Message = msg
	return ctrl.Result{RequeueAfter: waitInterval}, nil
}

// SetupWithManager sets up the controller with the Manager.
func (r *ImageBuildReconciler) SetupWithManager(mgr ctrl.Manager) error {
	// Jobs and pods of a build carry its name; they live in the build namespace.
	byBuild := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, o client.Object) []reconcile.Request {
		name := o.GetLabels()[naming.LabelImageBuild]
		if name == "" {
			return nil
		}
		return []reconcile.Request{{NamespacedName: types.NamespacedName{
			Namespace: naming.ProjectOfBuildNamespace(o.GetNamespace()), Name: name,
		}}}
	})
	managed := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return o.GetLabels()[naming.LabelImageBuild] != ""
	}))
	// A member binding of the project changes who may read the builds.
	byMember := handler.EnqueueRequestsFromMapFunc(func(ctx context.Context, o client.Object) []reconcile.Request {
		list := &v1.ImageBuildList{}
		if err := mgr.GetClient().List(ctx, list, client.InNamespace(o.GetNamespace())); err != nil {
			return nil
		}
		reqs := make([]reconcile.Request, 0, len(list.Items))
		for i := range list.Items {
			reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&list.Items[i])})
		}
		return reqs
	})
	members := builder.WithPredicates(predicate.NewPredicateFuncs(func(o client.Object) bool {
		return strings.HasPrefix(o.GetName(), naming.ProjectMemberBindingPrefix) && o.GetLabels()[naming.LabelProject] != ""
	}))
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1.ImageBuild{}).
		Watches(&batchv1.Job{}, byBuild, managed).
		Watches(&corev1.Pod{}, byBuild, managed).
		Watches(&rbacv1.RoleBinding{}, byMember, members).
		Named("imagebuild").
		Complete(r)
}
