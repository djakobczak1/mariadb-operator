package controller

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/go-logr/logr"
	mariadbv1alpha1 "github.com/mariadb-operator/mariadb-operator/api/v1alpha1"
	labels "github.com/mariadb-operator/mariadb-operator/pkg/builder/labels"
	condition "github.com/mariadb-operator/mariadb-operator/pkg/condition"
	"github.com/mariadb-operator/mariadb-operator/pkg/metadata"
	"github.com/mariadb-operator/mariadb-operator/pkg/pod"
	"github.com/mariadb-operator/mariadb-operator/pkg/predicate"
	"github.com/mariadb-operator/mariadb-operator/pkg/refresolver"
	sqlClient "github.com/mariadb-operator/mariadb-operator/pkg/sql"
	sqlClientSet "github.com/mariadb-operator/mariadb-operator/pkg/sqlset"
	"github.com/mariadb-operator/mariadb-operator/pkg/statefulset"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	klabels "k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type StatefulSetGaleraReconciler struct {
	client.Client
	Recorder    record.EventRecorder
	RefResolver *refresolver.RefResolver
}

//+kubebuilder:rbac:groups=apps,resources=statefulsets,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *StatefulSetGaleraReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, req.NamespacedName, &sts); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	mariadb, err := r.RefResolver.MariaDBFromAnnotation(ctx, sts.ObjectMeta)
	if err != nil {
		if errors.Is(err, refresolver.ErrMariaDBAnnotationNotFound) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !shouldPerformClusterRecovery(mariadb) {
		return ctrl.Result{}, nil
	}
	logger := log.FromContext(ctx).WithName("galera").WithName("health")
	logger.Info("Checking Galera cluster health")

	galera := ptr.Deref(mariadb.Spec.Galera, mariadbv1alpha1.Galera{})
	recovery := ptr.Deref(galera.Recovery, mariadbv1alpha1.GaleraRecovery{})

	clusterHealthyTimeout := ptr.Deref(recovery.ClusterHealthyTimeout, metav1.Duration{Duration: 30 * time.Second}).Duration
	healthyCtx, cancelHealthy := context.WithTimeout(ctx, clusterHealthyTimeout)
	defer cancelHealthy()

	healthy, err := r.pollUntilHealthyWithTimeout(healthyCtx, sts.ObjectMeta, logger)
	if err != nil {
		logger.V(1).Info("Error polling MariaDB health", "err", err)
		return ctrl.Result{Requeue: true}, nil
	}
	if healthy {
		return ctrl.Result{}, nil
	}
	logger.Info("Galera cluster is not healthy")
	r.Recorder.Event(mariadb, corev1.EventTypeWarning, mariadbv1alpha1.ReasonGaleraClusterNotHealthy, "Galera cluster is not healthy")

	if err := r.patchStatus(ctx, mariadb, func(status *mariadbv1alpha1.MariaDBStatus) {
		status.GaleraRecovery = nil
		condition.SetGaleraNotReady(status, mariadb)
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("error patching MariaDB: %v", err)
	}
	return ctrl.Result{}, nil
}

func (r *StatefulSetGaleraReconciler) pollUntilHealthyWithTimeout(ctx context.Context, stsObjMeta metav1.ObjectMeta,
	logger logr.Logger) (bool, error) {
	err := wait.PollUntilContextCancel(ctx, 1*time.Second, true, func(ctx context.Context) (bool, error) {
		return r.isHealthy(ctx, stsObjMeta, logger)
	})
	if err != nil {
		if wait.Interrupted(err) {
			return false, nil
		}
		return false, fmt.Errorf("error polling health: %v", err)
	}
	return true, nil
}

func (r *StatefulSetGaleraReconciler) isHealthy(ctx context.Context, stsObjMeta metav1.ObjectMeta, logger logr.Logger) (bool, error) {
	mdb, err := r.RefResolver.MariaDBFromAnnotation(ctx, stsObjMeta)
	if err != nil {
		return false, fmt.Errorf("error getting MariaDB: %v", err)
	}
	if !shouldPerformClusterRecovery(mdb) {
		return true, nil
	}

	key := types.NamespacedName{
		Name:      stsObjMeta.Name,
		Namespace: stsObjMeta.Namespace,
	}
	var sts appsv1.StatefulSet
	if err := r.Get(ctx, key, &sts); err != nil {
		return false, fmt.Errorf("error getting StatefulSet: %v", err)
	}
	logger.Info("StatefulSet ready replicas", "replicas", sts.Status.ReadyReplicas)
	if sts.Status.ReadyReplicas == 0 {
		return false, nil
	}

	clientCtx, cancelClient := context.WithTimeout(ctx, 5*time.Second)
	defer cancelClient()

	clientSet := sqlClientSet.NewClientSet(mdb, r.RefResolver)
	defer clientSet.Close()
	client, err := r.readyClient(clientCtx, mdb, clientSet)
	if err != nil {
		return false, fmt.Errorf("error getting ready client: %v", err)
	}

	size, err := client.GaleraClusterSize(clientCtx)
	if err != nil {
		return false, fmt.Errorf("error getting Galera cluster size: %v", err)
	}

	galera := ptr.Deref(mdb.Spec.Galera, mariadbv1alpha1.Galera{})
	recovery := ptr.Deref(galera.Recovery, mariadbv1alpha1.GaleraRecovery{})
	clusterHasMinSize, err := recovery.HasMinClusterSize(size, mdb)
	if err != nil {
		return false, fmt.Errorf("error checking min cluster size: %v", err)
	}
	logger.Info("Galera cluster size", "size", size, "has-min-size", clusterHasMinSize)

	return clusterHasMinSize, nil
}

func (r *StatefulSetGaleraReconciler) readyClient(ctx context.Context, mariadb *mariadbv1alpha1.MariaDB,
	clientSet *sqlClientSet.ClientSet) (*sqlClient.Client, error) {
	list := corev1.PodList{}
	listOpts := &client.ListOptions{
		LabelSelector: klabels.SelectorFromSet(
			labels.NewLabelsBuilder().
				WithMariaDB(mariadb).
				Build(),
		),
		Namespace: mariadb.GetNamespace(),
	}
	if err := r.List(ctx, &list, listOpts); err != nil {
		return nil, fmt.Errorf("error listing Pods: %v", err)
	}

	for _, p := range list.Items {
		if !pod.PodReady(&p) {
			continue
		}
		index, err := statefulset.PodIndex(p.Name)
		if err != nil {
			return nil, fmt.Errorf("error getting Pod index: %v", err)
		}
		if client, err := clientSet.ClientForIndex(ctx, *index); err == nil {
			return client, nil
		}
	}
	return nil, errors.New("no Ready Pods were found")
}

func (r *StatefulSetGaleraReconciler) patchStatus(ctx context.Context, mariadb *mariadbv1alpha1.MariaDB,
	patcher func(*mariadbv1alpha1.MariaDBStatus)) error {
	patch := client.MergeFrom(mariadb.DeepCopy())
	patcher(&mariadb.Status)
	return r.Status().Patch(ctx, mariadb, patch)
}

func shouldPerformClusterRecovery(mdb *mariadbv1alpha1.MariaDB) bool {
	galera := ptr.Deref(mdb.Spec.Galera, mariadbv1alpha1.Galera{})
	recovery := ptr.Deref(galera.Recovery, mariadbv1alpha1.GaleraRecovery{})
	if !galera.Enabled || !recovery.Enabled {
		return false
	}
	if mdb.IsRestoringBackup() || mdb.IsResizingStorage() || !mdb.HasGaleraConfiguredCondition() || mdb.HasGaleraNotReadyCondition() {
		return false
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *StatefulSetGaleraReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.StatefulSet{}).
		WithEventFilter(
			predicate.PredicateWithAnnotations(
				[]string{
					metadata.MariadbAnnotation,
					metadata.GaleraAnnotation,
				},
			),
		).
		Complete(r)
}
