// Copyright (c) 2019 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package health

import (
	"context"
	"errors"
	"fmt"
	"time"

	resourcesv1alpha1 "github.com/gardener/gardener-resource-manager/api/resources/v1alpha1"
	resourcesv1alpha1helper "github.com/gardener/gardener-resource-manager/api/resources/v1alpha1/helper"
	"github.com/gardener/gardener-resource-manager/pkg/controller/utils"
	"github.com/gardener/gardener-resource-manager/pkg/filter"

	"github.com/go-logr/logr"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Reconciler performs health checks for resources managed by ManagedResources.
type Reconciler struct {
	log          logr.Logger
	client       client.Client
	targetClient client.Client
	targetScheme *runtime.Scheme
	classFilter  *filter.ClassFilter
	syncPeriod   time.Duration
}

// InjectClient injects a client into the reconciler.
func (r *Reconciler) InjectClient(c client.Client) error {
	r.client = c
	return nil
}

// InjectLogger injects a logger into the reconciler.
func (r *Reconciler) InjectLogger(l logr.Logger) error {
	r.log = l.WithName(ControllerName)
	return nil
}

// Reconcile performs health checks.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()

	log := r.log.WithValues("object", req)
	log.Info("Starting ManagedResource health checks")

	mr := &resourcesv1alpha1.ManagedResource{}
	if err := r.client.Get(ctx, req.NamespacedName, mr); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("Stopping health checks for ManagedResource, as it has been deleted")
			return reconcile.Result{}, nil
		}
		return reconcile.Result{}, fmt.Errorf("could not fetch ManagedResource: %+v", err)
	}

	// Check responsibility
	if _, responsible := r.classFilter.Active(mr); !responsible {
		log.Info("Stopping health checks as the responsibility changed")
		return ctrl.Result{}, nil // Do not requeue
	}

	// Initialize condition based on the current status.
	conditionResourcesHealthy := resourcesv1alpha1helper.GetOrInitCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesHealthy)

	if !mr.DeletionTimestamp.IsZero() {
		conditionResourcesHealthy = resourcesv1alpha1helper.UpdatedCondition(conditionResourcesHealthy, resourcesv1alpha1.ConditionFalse, resourcesv1alpha1.ConditionDeletionPending, "The resources are currently being deleted.")
		if err := tryUpdateManagedResourceCondition(ctx, r.client, mr, conditionResourcesHealthy); err != nil {
			return ctrl.Result{}, fmt.Errorf("could not update the ManagedResource status: %+v ", err)
		}

		log.Info("Stopping health checks for ManagedResource, as it has been deleted (deletionTimestamp is set)")
		return reconcile.Result{}, nil
	}

	// skip health checks until ManagedResource has been reconciled completely successfully to prevent writing
	// falsy health condition (resources may need a second try to apply, e.g. CRDs and CRs in the same MR)
	conditionResourcesApplied := resourcesv1alpha1helper.GetCondition(mr.Status.Conditions, resourcesv1alpha1.ResourcesApplied)
	if conditionResourcesApplied == nil || conditionResourcesApplied.Status == resourcesv1alpha1.ConditionProgressing || conditionResourcesApplied.Status == resourcesv1alpha1.ConditionFalse {
		log.Info("Skipping health checks for ManagedResource, as it is has not been reconciled successfully yet.")
		return ctrl.Result{RequeueAfter: r.syncPeriod}, nil
	}

	var (
		oldConditions             = []resourcesv1alpha1.ManagedResourceCondition{conditionResourcesHealthy}
		resourcesObjectReferences = mr.Status.Resources
	)

	for _, ref := range resourcesObjectReferences {
		var obj client.Object
		// sigs.k8s.io/controller-runtime/pkg/client.DelegatingReader does not use the cache for unstructured.Unstructured
		// objects, so we create a new object of the object's type to use the caching client
		runtimeObject, err := r.targetScheme.New(ref.GroupVersionKind())
		if err != nil {
			log.Info("could not create new object of kind for health checks (probably not registered in the used scheme), falling back to unstructured request",
				"GroupVersionKind", ref.GroupVersionKind().String(), "error", err.Error())

			// fallback to unstructured requests if the object's type is not registered in the scheme
			unstructuredObj := &unstructured.Unstructured{}
			unstructuredObj.SetAPIVersion(ref.APIVersion)
			unstructuredObj.SetKind(ref.Kind)
			obj = unstructuredObj
		} else {
			var ok bool
			if obj, ok = runtimeObject.(client.Object); !ok {
				log.Error(errors.New("could not execute health check because object type is unsupported"), "GroupVersionKind", ref.GroupVersionKind().String())
				// do not requeue because there anyway will be another update event to fix the problem
				return ctrl.Result{}, nil
			}
		}

		if err := r.targetClient.Get(ctx, client.ObjectKey{Namespace: ref.Namespace, Name: ref.Name}, obj); err != nil {
			if apierrors.IsNotFound(err) {
				log.Info("Could not get object", "namespace", ref.Namespace, "name", ref.Name)

				var (
					reason  = ref.Kind + "Missing"
					message = fmt.Sprintf("Required %s %q in namespace %q is missing.", ref.Kind, ref.Name, ref.Namespace)
				)

				conditionResourcesHealthy = resourcesv1alpha1helper.UpdatedCondition(conditionResourcesHealthy, resourcesv1alpha1.ConditionFalse, reason, message)
				if err := tryUpdateManagedResourceCondition(ctx, r.client, mr, conditionResourcesHealthy); err != nil {
					return ctrl.Result{}, fmt.Errorf("could not update the ManagedResource status: %+v ", err)
				}

				return ctrl.Result{RequeueAfter: r.syncPeriod}, nil // We do not want to run in the exponential backoff for the condition check.
			}

			return ctrl.Result{}, err
		}

		if err := CheckHealth(r.targetScheme, obj); err != nil {
			var (
				reason  = ref.Kind + "Unhealthy"
				message = fmt.Sprintf("Required %s %q in namespace %q is unhealthy: %v", ref.Kind, ref.Name, ref.Namespace, err.Error())
			)

			conditionResourcesHealthy = resourcesv1alpha1helper.UpdatedCondition(conditionResourcesHealthy, resourcesv1alpha1.ConditionFalse, reason, message)
			if err := tryUpdateManagedResourceCondition(ctx, r.client, mr, conditionResourcesHealthy); err != nil {
				return ctrl.Result{}, fmt.Errorf("could not update the ManagedResource status: %+v ", err)
			}

			return ctrl.Result{RequeueAfter: r.syncPeriod}, nil // We do not want to run in the exponential backoff for the condition check.
		}
	}

	conditionResourcesHealthy = resourcesv1alpha1helper.UpdatedCondition(conditionResourcesHealthy, resourcesv1alpha1.ConditionTrue, "ResourcesHealthy", "All resources are healthy.")

	if !apiequality.Semantic.DeepEqual(oldConditions, []resourcesv1alpha1.ManagedResourceCondition{conditionResourcesHealthy}) {
		if err := tryUpdateManagedResourceCondition(ctx, r.client, mr, conditionResourcesHealthy); err != nil {
			return ctrl.Result{}, fmt.Errorf("could not update the ManagedResource status: %+v ", err)
		}
	}

	log.Info("Finished ManagedResource health checks")
	return ctrl.Result{RequeueAfter: r.syncPeriod}, nil
}

func tryUpdateManagedResourceCondition(ctx context.Context, c client.Client, mr *resourcesv1alpha1.ManagedResource, condition resourcesv1alpha1.ManagedResourceCondition) error {
	return utils.TryUpdateStatus(ctx, retry.DefaultBackoff, c, mr, func() error {
		mr.Status.Conditions = resourcesv1alpha1helper.MergeConditions(mr.Status.Conditions, condition)
		return nil
	})
}
