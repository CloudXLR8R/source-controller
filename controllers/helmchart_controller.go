/*
Copyright 2020 The Flux authors

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

package controllers

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	helper "github.com/fluxcd/pkg/runtime/controller"
	"github.com/fluxcd/pkg/runtime/events"
	"github.com/fluxcd/pkg/runtime/patch"
	"github.com/fluxcd/pkg/runtime/predicates"
	"github.com/go-logr/logr"
	"helm.sh/helm/v3/pkg/getter"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"sigs.k8s.io/controller-runtime/pkg/source"

	sourcev1 "github.com/fluxcd/source-controller/api/v1beta1"
	"github.com/fluxcd/source-controller/internal/helm"
)

// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=source.toolkit.fluxcd.io,resources=helmcharts/finalizers,verbs=get;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// HelmChartReconciler reconciles a HelmChart object
type HelmChartReconciler struct {
	client.Client
	helper.Events
	helper.Metrics

	Getters getter.Providers
	Storage *Storage
}

type HelmChartReconcilerOptions struct {
	MaxConcurrentReconciles int
}

func (r *HelmChartReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return r.SetupWithManagerAndOptions(mgr, HelmChartReconcilerOptions{})
}

func (r *HelmChartReconciler) SetupWithManagerAndOptions(mgr ctrl.Manager, opts HelmChartReconcilerOptions) error {
	if err := mgr.GetCache().IndexField(context.TODO(), &sourcev1.HelmRepository{}, sourcev1.HelmRepositoryURLIndexKey,
		r.indexHelmRepositoryByURL); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}
	if err := mgr.GetCache().IndexField(context.TODO(), &sourcev1.HelmChart{}, sourcev1.SourceIndexKey,
		r.indexHelmChartBySource); err != nil {
		return fmt.Errorf("failed setting index fields: %w", err)
	}

	return ctrl.NewControllerManagedBy(mgr).
		For(&sourcev1.HelmChart{}, builder.WithPredicates(
			predicate.Or(predicate.GenerationChangedPredicate{}, predicates.ReconcileRequestedPredicate{}),
		)).
		Watches(
			&source.Kind{Type: &sourcev1.HelmRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForHelmRepositoryChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.GitRepository{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForGitRepositoryChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		Watches(
			&source.Kind{Type: &sourcev1.Bucket{}},
			handler.EnqueueRequestsFromMapFunc(r.requestsForBucketChange),
			builder.WithPredicates(SourceRevisionChangePredicate{}),
		).
		WithOptions(controller.Options{MaxConcurrentReconciles: opts.MaxConcurrentReconciles}).
		Complete(r)
}

func (r *HelmChartReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	start := time.Now()
	log := logr.FromContext(ctx)

	// Fetch the HelmChart
	obj := &sourcev1.HelmChart{}
	if err := r.Get(ctx, req.NamespacedName, obj); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	// Record suspended status metric
	r.RecordSuspend(ctx, obj, obj.Spec.Suspend)

	// Return early if the object is suspended
	if obj.Spec.Suspend {
		log.Info("Reconciliation is suspended for this object")
		return ctrl.Result{}, nil
	}

	// Initialize the patch helper
	patchHelper, err := patch.NewHelper(obj, r.Client)
	if err != nil {
		return ctrl.Result{}, err
	}

	// Always attempt to patch the object and status after each
	// reconciliation
	defer func() {
		// Record the value of the reconciliation request, if any
		if v, ok := meta.ReconcileAnnotationValue(obj.GetAnnotations()); ok {
			obj.Status.SetLastHandledReconcileRequest(v)
		}

		// Summarize Ready condition
		conditions.SetSummary(obj,
			meta.ReadyCondition,
			conditions.WithConditions(
				sourcev1.BuildFailedCondition,
				sourcev1.FetchFailedCondition,
				sourcev1.ArtifactOutdatedCondition,
				sourcev1.ArtifactUnavailableCondition,
			),
			conditions.WithNegativePolarityConditions(
				sourcev1.BuildFailedCondition,
				sourcev1.FetchFailedCondition,
				sourcev1.ArtifactOutdatedCondition,
				sourcev1.ArtifactUnavailableCondition,
			),
		)

		// Patch the object, ignoring conflicts on the conditions owned by
		// this controller
		patchOpts := []patch.Option{
			patch.WithOwnedConditions{
				Conditions: []string{
					sourcev1.BuildFailedCondition,
					sourcev1.FetchFailedCondition,
					sourcev1.ArtifactOutdatedCondition,
					sourcev1.ArtifactUnavailableCondition,
					meta.ReadyCondition,
					meta.ReconcilingCondition,
					meta.StalledCondition,
				},
			},
		}

		// Determine if the resource is still being reconciled, or if
		// it has stalled, and record this observation
		if retErr == nil && (result.IsZero() || !result.Requeue) {
			// We are no longer reconciling
			conditions.Delete(obj, meta.ReconcilingCondition)

			// We have now observed this generation
			patchOpts = append(patchOpts, patch.WithStatusObservedGeneration{})

			readyCondition := conditions.Get(obj, meta.ReadyCondition)
			switch readyCondition.Status {
			case metav1.ConditionFalse:
				// As we are no longer reconciling and the end-state
				// is not ready, the reconciliation has stalled
				conditions.MarkStalled(obj, readyCondition.Reason, readyCondition.Message)
			case metav1.ConditionTrue:
				// As we are no longer reconciling and the end-state
				// is ready, the reconciliation is no longer stalled
				conditions.Delete(obj, meta.StalledCondition)
			}
		}

		// Finally, patch the resource
		if err := patchHelper.Patch(ctx, obj, patchOpts...); err != nil {
			retErr = kerrors.NewAggregate([]error{retErr, err})
		}

		// Always record readiness and duration metrics
		r.Metrics.RecordReadiness(ctx, obj)
		r.Metrics.RecordDuration(ctx, obj, start)
	}()

	// Add finalizer first if not exist to avoid the race condition
	// between init and delete
	if !controllerutil.ContainsFinalizer(obj, sourcev1.SourceFinalizer) {
		controllerutil.AddFinalizer(obj, sourcev1.SourceFinalizer)
		return ctrl.Result{Requeue: true}, nil
	}

	// Examine if the object is under deletion
	if !obj.ObjectMeta.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, obj)
	}

	// Reconcile actual object
	return r.reconcile(ctx, obj)
}

func (r *HelmChartReconciler) reconcile(ctx context.Context, obj *sourcev1.HelmChart) (ctrl.Result, error) {
	// Mark the resource as under reconciliation
	conditions.MarkReconciling(obj, "Reconciling", "")

	// Reconcile the storage data
	if result, err := r.reconcileStorage(ctx, obj); err != nil || result.IsZero() {
		return result, err
	}

	// Create a working directory for all fs related operations we may have to do
	workDir, err := os.MkdirTemp("", fmt.Sprintf("%s-%s-", obj.GetNamespace(), obj.GetName()))
	if err != nil {
		conditions.MarkTrue(obj, sourcev1.FetchFailedCondition, sourcev1.StorageOperationFailedReason,
			"Failed to create temporary working directory: %s", err.Error())
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to create temporary working directory: %s", err.Error())
		return ctrl.Result{}, err
	}
	defer os.RemoveAll(workDir)

	// Initialize new chart builder
	build := newChartBuilder(r.Client, r.Storage, r.Getters, obj.GetNamespace(), workDir)

	// Reconcile the source
	var sourcePath string
	defer os.RemoveAll(sourcePath)
	if result, err := r.reconcileSource(ctx, obj, build); err != nil || result.IsZero() {
		return result, err
	}

	// Reconcile the chart using the source data
	var artifact sourcev1.Artifact
	var chartPath string
	if result, err := r.reconcileChart(ctx, obj, build, &artifact, &chartPath); err != nil || result.IsZero() {
		return result, err
	}

	// Reconcile artifact to storage
	return r.reconcileArtifact(ctx, obj, artifact, chartPath)
}

// reconcileStorage ensures the current state of the storage matches the desired and previously observed state.
//
// All artifacts for the resource except for the current one are garbage collected from the storage.
// If the artifact in the Status object of the resource disappeared from storage, it is removed from the object.
// If the object does not have an artifact in its Status object, a v1beta1.ArtifactUnavailableCondition is set.
// If the hostname of any of the URLs on the object do not match the current storage server hostname, they are updated.
//
// The caller should assume a failure if an error is returned, or the Result is zero.
func (r *HelmChartReconciler) reconcileStorage(ctx context.Context, obj *sourcev1.HelmChart) (ctrl.Result, error) {
	// Garbage collect previous advertised artifact(s) from storage
	_ = r.garbageCollect(ctx, obj)

	// Determine if the advertised artifact is still in storage
	if artifact := obj.GetArtifact(); artifact != nil && !r.Storage.ArtifactExist(*artifact) {
		obj.Status.Artifact = nil
		obj.Status.URL = ""
	}

	// Record that we do not have an artifact
	if obj.GetArtifact() == nil {
		conditions.MarkTrue(obj, sourcev1.ArtifactUnavailableCondition, "NoArtifact", "No artifact for resource in storage")
		return ctrl.Result{Requeue: true}, nil
	}
	conditions.Delete(obj, sourcev1.ArtifactUnavailableCondition)

	// Always update URLs to ensure hostname is up-to-date
	// TODO(hidde): we may want to send out an event only if we notice the URL has changed
	r.Storage.SetArtifactURL(obj.GetArtifact())
	obj.Status.URL = r.Storage.SetHostname(obj.Status.URL)

	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

func (r *HelmChartReconciler) reconcileChart(ctx context.Context, obj *sourcev1.HelmChart, build *chartBuilder, artifact *sourcev1.Artifact, result *string) (ctrl.Result, error) {
	// Collect chart metadata
	chartMeta, err := build.LoadMetadata()
	if err != nil {
		return ctrl.Result{}, err
	}

	// If the current revision of the artifact equals to that of the chart, and we do not have a change in spec (which
	// can result in a change of values files); the artifact still matches the desired state
	if obj.GetArtifact().HasRevision(chartMeta.Version) && obj.Generation == obj.Status.ObservedGeneration {
		logr.FromContext(ctx).Info("Artifact up-to-date: skipping chart reconciliation")
		return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
	}

	// Merge referred values files as instructed
	if valuesRefs := obj.GetValuesFiles(); len(valuesRefs) > 0 {
		if err := build.MergeValuesFiles(valuesRefs); err != nil {
			return ctrl.Result{}, err
		}
		// TODO: event
	}

	// If the chart source is a directory (and thus not packaged), we should ensure all dependencies are present
	if isChartDir, err := build.ChartSourceIsDir(); err != nil || isChartDir {
		if err != nil {
			return ctrl.Result{}, err
		}
		downloaded, err := build.FetchMissingDependencies(ctx)
		if err != nil {
			return ctrl.Result{}, err
		}
		if downloaded > 0 {
			// TODO: event
		}
	}

	// Build the chart, if we did not make any modifications earlier this is a no-op
	chartPath, err := build.Build()
	if err != nil {
		return ctrl.Result{}, err
	}

	// Create potential new artifact
	*artifact = r.Storage.NewArtifactFor(obj.Kind, obj, chartMeta.Version, fmt.Sprintf("%s-%s.tgz", chartMeta.Name, chartMeta.Version))
	*result = chartPath
	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

func (r *HelmChartReconciler) reconcileArtifact(ctx context.Context, obj *sourcev1.HelmChart, artifact sourcev1.Artifact, path string) (ctrl.Result, error) {
	// Always restore the Ready condition in case it got removed due to a transient error
	defer func() {
		if obj.GetArtifact() != nil {
			conditions.Delete(obj, sourcev1.ArtifactUnavailableCondition)
		}
		if obj.GetArtifact().HasRevision(artifact.Revision) {
			conditions.Delete(obj, sourcev1.ArtifactOutdatedCondition)
			conditions.MarkTrue(obj, meta.ReadyCondition, meta.SucceededReason,
				"Stored artifact for revision '%s'", artifact.Revision)
		}
	}()

	// Ensure artifact directory exists and acquire lock
	if err := r.Storage.MkdirAll(artifact); err != nil {
		err = fmt.Errorf("failed to create artifact directory: %w", err)
		return ctrl.Result{}, err
	}
	unlock, err := r.Storage.Lock(artifact)
	if err != nil {
		err = fmt.Errorf("failed to acquire lock for artifact: %w", err)
		return ctrl.Result{}, err
	}
	defer unlock()

	// Copy the packaged chart to the artifact path
	if err := r.Storage.CopyFromPath(&artifact, path); err != nil {
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to write Helm repository index to storage: %s", err.Error())
		return ctrl.Result{}, err
	}
	r.EventWithMetaf(ctx, obj, map[string]string{
		"revision": artifact.Revision,
		"checksum": artifact.Checksum,
	}, events.EventSeverityInfo, "NewArtifact", "Stored artifact for revision '%s'", artifact.Revision)

	// Record it on the object
	obj.Status.Artifact = artifact.DeepCopy()

	// Update symlink on a "best effort" basis
	url, err := r.Storage.Symlink(artifact, "latest.tar.gz")
	if err != nil {
		r.Eventf(ctx, obj, events.EventSeverityError, sourcev1.StorageOperationFailedReason,
			"Failed to update status URL symlink: %s", err)
	}
	if url != "" {
		obj.Status.URL = url
	}
	return ctrl.Result{RequeueAfter: obj.GetRequeueAfter()}, nil
}

// reconcileDelete handles the delete of an object. It first garbage collects all artifacts for the object from the
// artifact storage, if successful, the finalizer is removed from the object.
func (r *HelmChartReconciler) reconcileDelete(ctx context.Context, obj *sourcev1.HelmChart) (ctrl.Result, error) {
	// Garbage collect the resource's artifacts
	if err := r.garbageCollect(ctx, obj); err != nil {
		// Return the error so we retry the failed garbage collection
		return ctrl.Result{}, err
	}

	// Remove our finalizer from the list
	controllerutil.RemoveFinalizer(obj, sourcev1.SourceFinalizer)

	// Stop reconciliation as the object is being deleted
	return ctrl.Result{}, nil
}

// garbageCollect performs a garbage collection for the given v1beta1.HelmChart. It removes all but the current
// artifact except for when the deletion timestamp is set, which will result in the removal of all artifacts for the
// resource.
func (r *HelmChartReconciler) garbageCollect(ctx context.Context, obj *sourcev1.HelmChart) error {
	if !obj.DeletionTimestamp.IsZero() {
		if err := r.Storage.RemoveAll(r.Storage.NewArtifactFor(obj.Kind, obj.GetObjectMeta(), "", "*")); err != nil {
			r.Eventf(ctx, obj, events.EventSeverityError, "GarbageCollectionFailed",
				"Garbage collection for deleted resource failed: %s", err)
			return err
		}
		obj.Status.Artifact = nil
		// TODO(hidde): we should only push this event if we actually garbage collected something
		r.Eventf(ctx, obj, events.EventSeverityInfo, "GarbageCollectionSucceeded",
			"Garbage collected artifacts for deleted resource")
		return nil
	}
	if obj.GetArtifact() != nil {
		if err := r.Storage.RemoveAllButCurrent(*obj.GetArtifact()); err != nil {
			r.Eventf(ctx, obj, events.EventSeverityError, "GarbageCollectionFailed", "Garbage collection of old artifacts failed: %s", err)
			return err
		}
		// TODO(hidde): we should only push this event if we actually garbage collected something
		r.Eventf(ctx, obj, events.EventSeverityInfo, "GarbageCollectionSucceeded", "Garbage collected old artifacts")
	}
	return nil
}

func (r *HelmChartReconciler) indexHelmRepositoryByURL(o client.Object) []string {
	repo, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmRepository, got %T", o))
	}
	u := helm.NormalizeChartRepositoryURL(repo.Spec.URL)
	if u != "" {
		return []string{u}
	}
	return nil
}

func (r *HelmChartReconciler) indexHelmChartBySource(o client.Object) []string {
	hc, ok := o.(*sourcev1.HelmChart)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmChart, got %T", o))
	}
	return []string{fmt.Sprintf("%s/%s", hc.Spec.SourceRef.Kind, hc.Spec.SourceRef.Name)}
}

func (r *HelmChartReconciler) requestsForHelmRepositoryChange(o client.Object) []reconcile.Request {
	repo, ok := o.(*sourcev1.HelmRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a HelmRepository, got %T", o))
	}
	// If we do not have an artifact, we have no requests to make
	if repo.GetArtifact() == nil {
		return nil
	}

	ctx := context.Background()
	var list sourcev1.HelmChartList
	if err := r.List(ctx, &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.HelmRepositoryKind, repo.Name),
	}); err != nil {
		return nil
	}

	// TODO(hidde): unlike other places (e.g. the helm-controller),
	//  we have no reference here to determine if the request is coming
	//  from the _old_ or _new_ update event, and resources are thus
	//  enqueued twice.
	var reqs []reconcile.Request
	for _, i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
	}
	return reqs
}

func (r *HelmChartReconciler) requestsForGitRepositoryChange(o client.Object) []reconcile.Request {
	repo, ok := o.(*sourcev1.GitRepository)
	if !ok {
		panic(fmt.Sprintf("Expected a GitRepository, got %T", o))
	}

	// If we do not have an artifact, we have no requests to make
	if repo.GetArtifact() == nil {
		return nil
	}

	var list sourcev1.HelmChartList
	if err := r.List(context.TODO(), &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.GitRepositoryKind, repo.Name),
	}); err != nil {
		return nil
	}

	// TODO(hidde): unlike other places (e.g. the helm-controller),
	//  we have no reference here to determine if the request is coming
	//  from the _old_ or _new_ update event, and resources are thus
	//  enqueued twice.
	var reqs []reconcile.Request
	for _, i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
	}
	return reqs
}

func (r *HelmChartReconciler) requestsForBucketChange(o client.Object) []reconcile.Request {
	bucket, ok := o.(*sourcev1.Bucket)
	if !ok {
		panic(fmt.Sprintf("Expected a Bucket, got %T", o))
	}

	// If we do not have an artifact, we have no requests to make
	if bucket.GetArtifact() == nil {
		return nil
	}

	var list sourcev1.HelmChartList
	if err := r.List(context.TODO(), &list, client.MatchingFields{
		sourcev1.SourceIndexKey: fmt.Sprintf("%s/%s", sourcev1.BucketKind, bucket.Name),
	}); err != nil {
		return nil
	}

	// TODO(hidde): unlike other places (e.g. the helm-controller),
	//  we have no reference here to determine if the request is coming
	//  from the _old_ or _new_ update event, and resources are thus
	//  enqueued twice.
	var reqs []reconcile.Request
	for _, i := range list.Items {
		reqs = append(reqs, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&i)})
	}
	return reqs
}

// validHelmChartName returns an error if the given string is not a
// valid Helm chart name; a valid name must be lower case letters
// and numbers, words may be separated with dashes (-).
// Ref: https://helm.sh/docs/chart_best_practices/conventions/#chart-names
func validHelmChartName(s string) error {
	chartFmt := regexp.MustCompile("^([-a-z0-9]*)$")
	if !chartFmt.MatchString(s) {
		return fmt.Errorf("invalid chart name '%s', a valid name must be lower case letters and numbers and MAY be separated with dashes (-)", s)
	}
	return nil
}
