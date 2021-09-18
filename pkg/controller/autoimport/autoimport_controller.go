// Copyright (c) Red Hat, Inc.
// Copyright Contributors to the Open Cluster Management project

package autoimport

import (
	"context"
	"fmt"
	"strconv"

	"github.com/open-cluster-management/managedcluster-import-controller/pkg/constants"
	"github.com/open-cluster-management/managedcluster-import-controller/pkg/helpers"
	clusterv1 "open-cluster-management.io/api/cluster/v1"

	"github.com/openshift/library-go/pkg/operator/events"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	utilerrors "k8s.io/apimachinery/pkg/util/errors"

	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const autoImportRetryName string = "autoImportRetry"

var log = logf.Log.WithName(controllerName)

// ReconcileAutoImport reconciles the managed cluster auto import secret to import the managed cluster
type ReconcileAutoImport struct {
	client   client.Client
	recorder events.Recorder
}

// Reconcile the managed cluster auto import secret to import the managed cluster
// Once the managed cluster is imported, the auto import secret will be deleted
//
// Note: The Controller will requeue the Request to be processed again if the returned error is non-nil or
// Result.Requeue is true, otherwise upon completion it will remove the work from the queue.
//func (r *ReconcileAutoImport) Reconcile(ctx context.Context, request reconcile.Request) (reconcile.Result, error) {
func (r *ReconcileAutoImport) Reconcile(request reconcile.Request) (reconcile.Result, error) {
	reqLogger := log.WithValues("Request.Namespace", request.Namespace)
	reqLogger.Info("Reconciling auto import secret")

	ctx := context.TODO()
	managedClusterName := request.Namespace
	managedCluster := &clusterv1.ManagedCluster{}
	err := r.client.Get(ctx, types.NamespacedName{Name: managedClusterName}, managedCluster)
	if errors.IsNotFound(err) {
		// the managed cluster could have been deleted, do nothing
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	autoImportSecret := &corev1.Secret{}
	err = r.client.Get(ctx, types.NamespacedName{
		Namespace: managedClusterName,
		Name:      constants.AutoImportSecretName,
	}, autoImportSecret)
	if errors.IsNotFound(err) {
		// the auto import secret could have been deleted, do nothing
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	importSecretName := fmt.Sprintf("%s-%s", managedClusterName, constants.ImportSecretNameSuffix)
	importSecret := &corev1.Secret{}
	err = r.client.Get(ctx, types.NamespacedName{Namespace: managedClusterName, Name: importSecretName}, importSecret)
	if errors.IsNotFound(err) {
		// there is no import secret, do nothing
		return reconcile.Result{}, nil
	}
	if err != nil {
		return reconcile.Result{}, err
	}

	importClient, restMapper, err := helpers.GenerateClientFromSecret(autoImportSecret)
	if err != nil {
		return reconcile.Result{}, err
	}

	importCondition := metav1.Condition{
		Type:    "ManagedClusterImportSucceeded",
		Status:  metav1.ConditionTrue,
		Message: "Import succeeded",
		Reason:  "ManagedClusterImported",
	}

	errs := []error{}
	err = helpers.ImportManagedClusterFromSecret(importClient, restMapper, r.recorder, importSecret)
	if err != nil {
		importCondition.Status = metav1.ConditionFalse
		importCondition.Message = fmt.Sprintf("Unable to import %s: %s", managedClusterName, err.Error())
		importCondition.Reason = "ManagedClusterNotImported"

		errs = append(errs, err, r.updateAutoImportRetryTimes(ctx, autoImportSecret))
	}

	if len(errs) == 0 {
		err := r.client.Delete(ctx, autoImportSecret)
		if err != nil {
			errs = append(errs, err)
		}

		r.recorder.Eventf("AutoImportSecretDeleted",
			fmt.Sprintf("The managed cluster %s is imported, delete its auto import secret", managedClusterName))
	}

	if err := helpers.UpdateManagedClusterStatus(r.client, r.recorder, managedClusterName, importCondition); err != nil {
		errs = append(errs, err)
	}

	return reconcile.Result{}, utilerrors.NewAggregate(errs)
}

func (r *ReconcileAutoImport) updateAutoImportRetryTimes(ctx context.Context, secret *corev1.Secret) error {
	autoImportRetry, err := strconv.Atoi(string(secret.Data[autoImportRetryName]))
	if err != nil {
		r.recorder.Warningf("AutoImportRetryInvalid", "The value of autoImportRetry is invalid in auto-import-secret secret")
		return err
	}

	r.recorder.Eventf("RetryToImportCluster", "Retry to import cluster %s, %d", secret.Namespace, autoImportRetry)

	autoImportRetry--
	if autoImportRetry < 0 {
		// stop retry, delete the auto-import-secret
		if err := r.client.Delete(ctx, secret); err != nil {
			return err
		}
		r.recorder.Eventf("AutoImportSecretDeleted",
			fmt.Sprintf("Exceed the retry times, delete the auto import secret %s/%s", secret.Namespace, secret.Name))
		return nil
	}

	secret.Data[autoImportRetryName] = []byte(strconv.Itoa(autoImportRetry))
	return r.client.Update(ctx, secret)
}