/*
Copyright 2021.

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
	"reflect"
	"strings"
	"time"

	"github.com/go-logr/logr"
	"github.com/pkg/errors"

	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/pointer"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/source"

	remediationv1alpha1 "github.com/medik8s/node-healthcheck-operator/api/v1alpha1"
	"github.com/medik8s/node-healthcheck-operator/controllers/cluster"
	"github.com/medik8s/node-healthcheck-operator/controllers/mhc"
	"github.com/medik8s/node-healthcheck-operator/controllers/utils"
	"github.com/medik8s/node-healthcheck-operator/metrics"
)

const (
	oldRemediationCRAnnotationKey = "nodehealthcheck.medik8s.io/old-remediation-cr-flag"
	templateSuffix                = "Template"
	remediationCRAlertTimeout     = time.Hour * 48
	eventReasonRemediationCreated = "RemediationCreated"
	eventReasonRemediationSkipped = "RemediationSkipped"
	eventReasonRemediationRemoved = "RemediationRemoved"
	eventReasonDisabled           = "Disabled"
	eventReasonEnabled            = "Enabled"
	eventTypeNormal               = "Normal"
	eventTypeWarning              = "Warning"
)

// NodeHealthCheckReconciler reconciles a NodeHealthCheck object
type NodeHealthCheckReconciler struct {
	client.Client
	Log                         logr.Logger
	Scheme                      *runtime.Scheme
	Recorder                    record.EventRecorder
	ClusterUpgradeStatusChecker cluster.UpgradeChecker
	MHCChecker                  mhc.Checker
}

// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=remediation.medik8s.io,resources=nodehealthchecks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=remediation.medik8s.io,resources=nodehealthchecks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=remediation.medik8s.io,resources=nodehealthchecks/finalizers,verbs=update
// +kubebuilder:rbac:groups=config.openshift.io,resources=clusterversions,verbs=get;list;watch
// +kubebuilder:rbac:groups=machine.openshift.io,resources=machinehealthchecks,verbs=get;list;watch

// Reconcile is part of the main kubernetes reconciliation loop which aims to
// move the current state of the cluster closer to the desired state.
func (r *NodeHealthCheckReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := r.Log.WithValues("NodeHealthCheck", req.NamespacedName)

	// fetch nhc
	nhc := &remediationv1alpha1.NodeHealthCheck{}
	err := r.Get(ctx, req.NamespacedName, nhc)
	result := ctrl.Result{}
	if err != nil {
		log.Error(err, "failed fetching Node Health Check", "object", nhc)
		if apierrors.IsNotFound(err) {
			return result, nil
		}
		return result, err
	}

	// check if we need to disable NHC because of existimg MHCs
	if disable := r.MHCChecker.NeedDisableNHC(); disable {
		// update status if needed
		if !meta.IsStatusConditionTrue(nhc.Status.Conditions, remediationv1alpha1.ConditionTypeDisabled) {
			log.Info("disabling NHC in order to avoid conflict with custom MHCs configured in the cluster")
			meta.SetStatusCondition(&nhc.Status.Conditions, metav1.Condition{
				Type:    remediationv1alpha1.ConditionTypeDisabled,
				Status:  metav1.ConditionTrue,
				Reason:  remediationv1alpha1.ConditionReasonDisabledMHC,
				Message: "Custom MachineHealthCheck(s) detected, disabling NHC to avoid conflicts",
			})
			r.Recorder.Eventf(nhc, eventTypeWarning, eventReasonDisabled, "Custom MachineHealthCheck(s) detected, disabling NHC to avoid conflicts")
			err = r.Client.Status().Update(context.Background(), nhc)
			if err != nil {
				log.Error(err, "failed to update NHC status conditions")
				return result, err
			}
		}
		// stop reconciling
		return result, nil
	}
	if meta.IsStatusConditionTrue(nhc.Status.Conditions, remediationv1alpha1.ConditionTypeDisabled) {
		log.Info("re-enabling NHC, no conflicting MHC configured in the cluster")
		meta.RemoveStatusCondition(&nhc.Status.Conditions, remediationv1alpha1.ConditionTypeDisabled)
		r.Recorder.Eventf(nhc, eventTypeNormal, eventReasonEnabled, "Custom MachineHealthCheck(s) removed, re-enabling NHC")
		err = r.Client.Status().Update(context.Background(), nhc)
		if err != nil {
			log.Error(err, "failed to update NHC status conditions")
			return result, err
		}
	}

	// select nodes using the nhc.selector
	nodes, err := r.fetchNodes(ctx, nhc.Spec.Selector)
	if err != nil {
		return result, err
	}

	// check nodes health
	unhealthyNodes, err := r.checkNodesHealth(nodes, nhc)
	if err != nil {
		return result, err
	}

	minHealthy, err := intstr.GetScaledValueFromIntOrPercent(nhc.Spec.MinHealthy, len(nodes), true)
	if err != nil {
		log.Error(err, "failed to calculate min healthy allowed nodes",
			"minHealthy", nhc.Spec.MinHealthy, "observedNodes", nhc.Status.ObservedNodes)
		return result, err
	}

	if r.shouldTryRemediation(nhc, nodes, unhealthyNodes, minHealthy, &result) {
		for i := range unhealthyNodes {
			nextReconcile, err := r.remediate(ctx, &unhealthyNodes[i], nhc)
			if err != nil {
				return ctrl.Result{}, err
			}
			if nextReconcile != nil {
				updateResultNextReconcile(&result, *nextReconcile)
			}
		}
	}

	inFlightRemediations, err := r.getInflightRemediations(nhc)
	if err != nil {
		return ctrl.Result{}, errors.Wrapf(err, "failed fetching remediation objects of the NHC")
	}

	err = r.patchStatus(nhc, len(nodes), len(unhealthyNodes), inFlightRemediations)
	if err != nil {
		log.Error(err, "failed to patch NHC status")
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *NodeHealthCheckReconciler) shouldTryRemediation(
	nhc *remediationv1alpha1.NodeHealthCheck, nodes []v1.Node, unhealthyNodes []v1.Node, minHealthy int, result *ctrl.Result) bool {

	if len(unhealthyNodes) == 0 {
		return false
	}

	healthyNodes := len(nodes) - len(unhealthyNodes)
	if healthyNodes >= minHealthy {
		if len(nhc.Spec.PauseRequests) > 0 {
			// some actors want to pause remediation.
			msg := "Skipping remediation because there are pause requests"
			r.Log.Info(msg)
			r.Recorder.Event(nhc, eventTypeNormal, eventReasonRemediationSkipped, msg)
			return false
		}
		if r.isClusterUpgrading() {
			updateResultNextReconcile(result, 1*time.Minute)
			r.Recorder.Event(nhc, eventTypeNormal, eventReasonRemediationSkipped, "Skipped remediation because the cluster is upgrading")
			return false
		}
		return true
	}
	msg := fmt.Sprintf("Skipped remediation because the number of healthy nodes selected by the selector is %d and should equal or exceed %d", healthyNodes, minHealthy)
	r.Log.Info(msg,
		"healthyNodes", healthyNodes, "minHealthy", minHealthy)
	r.Recorder.Event(nhc, eventTypeWarning, eventReasonRemediationSkipped, msg)
	return false
}

func (r *NodeHealthCheckReconciler) isClusterUpgrading() bool {
	clusterUpgrading, err := r.ClusterUpgradeStatusChecker.Check()
	if err != nil {
		// log the error but don't return - if we can't reliably tell if
		// the cluster is upgrading then just continue with remediation.
		// TODO finer error handling may help to decide otherwise here.
		r.Log.Error(err, "failed to check if the cluster is upgrading. Proceed with remediation as if it is not upgrading")
	}
	if clusterUpgrading {
		r.Log.Info("Skipping remediation because the cluster is currently upgrading.")
		return true
	}
	return false
}

func (r *NodeHealthCheckReconciler) fetchNodes(ctx context.Context, labelSelector metav1.LabelSelector) ([]v1.Node, error) {
	var nodes v1.NodeList
	selector, err := metav1.LabelSelectorAsSelector(&labelSelector)
	if err != nil {
		err = errors.Wrapf(err, "failed converting a selector from NHC selector")
		return []v1.Node{}, err
	}
	err = r.List(
		ctx,
		&nodes,
		&client.ListOptions{LabelSelector: selector},
	)
	return nodes.Items, err
}

func (r *NodeHealthCheckReconciler) checkNodesHealth(nodes []v1.Node, nhc *remediationv1alpha1.NodeHealthCheck) ([]v1.Node, error) {
	var unhealthy []v1.Node
	for i := range nodes {
		n := &nodes[i]
		if isHealthy(nhc.Spec.UnhealthyConditions, n.Status.Conditions) {
			err := r.markHealthy(n, nhc)
			if err != nil {
				return nil, err
			}
		} else {
			// ignore nodes handled by MHC
			if r.MHCChecker.NeedIgnoreNode(n) {
				continue
			}
			unhealthy = append(unhealthy, *n)
		}
	}
	return unhealthy, nil
}

func (r *NodeHealthCheckReconciler) markHealthy(n *v1.Node, nhc *remediationv1alpha1.NodeHealthCheck) error {
	cr, err := r.generateRemediationCR(n, nhc)
	if err != nil {
		return err
	}

	// check if CR is deleted already
	err = r.Client.Get(context.Background(), client.ObjectKeyFromObject(cr), cr)
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	} else if apierrors.IsNotFound(err) || cr.GetDeletionTimestamp() != nil {
		return nil
	}

	r.Log.V(5).Info("node seems healthy", "Node name", n.Name)

	err = r.Client.Delete(context.Background(), cr, &client.DeleteOptions{})
	// if the node is already healthy then there is no remediation object for it
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}

	if err == nil {
		// deleted an actual object
		r.Log.Info("deleted node external remediation object", "Node name", n.Name)
		r.Recorder.Eventf(nhc, eventTypeNormal, eventReasonRemediationRemoved, "Deleted remediation object for node %s", n.Name)
	}
	return nil
}

func isHealthy(conditionTests []remediationv1alpha1.UnhealthyCondition, nodeConditions []v1.NodeCondition) bool {
	now := time.Now()
	nodeConditionByType := make(map[v1.NodeConditionType]v1.NodeCondition)
	for _, nc := range nodeConditions {
		nodeConditionByType[nc.Type] = nc
	}

	for _, c := range conditionTests {
		n, exists := nodeConditionByType[c.Type]
		if !exists {
			continue
		}
		if n.Status == c.Status && now.After(n.LastTransitionTime.Add(c.Duration.Duration)) {
			return false
		}
	}
	return true
}

// SetupWithManager sets up the controller with the Manager.
func (r *NodeHealthCheckReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&remediationv1alpha1.NodeHealthCheck{}).
		Watches(&source.Kind{Type: &v1.Node{}}, handler.EnqueueRequestsFromMapFunc(utils.NHCByNodeMapperFunc(mgr.GetClient(), mgr.GetLogger()))).
		Complete(r)
}

func (r *NodeHealthCheckReconciler) remediate(ctx context.Context, n *v1.Node, nhc *remediationv1alpha1.NodeHealthCheck) (*time.Duration, error) {
	cr, err := r.generateRemediationCR(n, nhc)
	if err != nil {
		return nil, err
	}

	// check if CR already exists
	if err = r.Client.Get(ctx, client.ObjectKeyFromObject(cr), cr); err != nil {
		if !apierrors.IsNotFound(err) {
			r.Log.Error(err, "failed to check for existing external remediation object")
			return nil, err
		}

		// create CR
		r.Log.Info("node seems unhealthy. Creating an external remediation object",
			"nodeName", n.Name, "CR name", cr.GetName(), "CR gvk", cr.GroupVersionKind(), "ns", cr.GetNamespace())
		if err = r.Client.Create(ctx, cr); err != nil {
			r.Log.Error(err, "failed to create an external remediation object")
			return nil, err
		}
		r.Recorder.Event(nhc, eventTypeNormal, eventReasonRemediationCreated, fmt.Sprintf("Created remediation object for node %s", n.Name))
		return nil, nil
	}

	// CR exists
	isAlert, nextReconcile := r.alertOldRemediationCR(cr)
	if isAlert {
		metrics.ObserveNodeHealthCheckOldRemediationCR(n.Name, n.Namespace)
	}
	return nextReconcile, nil
}

func (r *NodeHealthCheckReconciler) generateRemediationCR(n *v1.Node, nhc *remediationv1alpha1.NodeHealthCheck) (*unstructured.Unstructured, error) {
	t, err := r.fetchTemplate(nhc)
	if err != nil {
		return nil, err
	}

	templateSpec, found, err := unstructured.NestedMap(t.Object, "spec", "template")
	if !found || err != nil {
		return nil, errors.Errorf("Failed to retrieve Spec.Template on %v %q %v", t.GroupVersionKind(), t.GetName(), err)
	}

	u := unstructured.Unstructured{Object: templateSpec}
	u.SetName(n.Name)
	u.SetNamespace(t.GetNamespace())
	u.SetGroupVersionKind(schema.GroupVersionKind{
		Group:   t.GroupVersionKind().Group,
		Version: t.GroupVersionKind().Version,
		Kind:    strings.TrimSuffix(t.GetKind(), templateSuffix),
	})
	u.SetOwnerReferences([]metav1.OwnerReference{
		{
			APIVersion:         nhc.APIVersion,
			Kind:               nhc.Kind,
			Name:               nhc.Name,
			UID:                nhc.UID,
			Controller:         pointer.BoolPtr(false),
			BlockOwnerDeletion: nil,
		},
	})
	u.SetLabels(map[string]string{
		"app.kubernetes.io/part-of": "node-healthcheck-controller",
	})
	u.SetResourceVersion("")
	u.SetFinalizers(nil)
	u.SetUID("")
	u.SetSelfLink("")
	u.SetCreationTimestamp(metav1.Now())
	return &u, nil
}

func (r *NodeHealthCheckReconciler) fetchTemplate(nhc *remediationv1alpha1.NodeHealthCheck) (*unstructured.Unstructured, error) {
	t := nhc.Spec.RemediationTemplate.DeepCopy()
	obj := new(unstructured.Unstructured)
	obj.SetAPIVersion(t.APIVersion)
	obj.SetGroupVersionKind(t.GroupVersionKind())
	obj.SetName(t.Name)
	key := client.ObjectKey{Name: obj.GetName(), Namespace: t.Namespace}
	if err := r.Client.Get(context.Background(), key, obj); err != nil {
		return nil, errors.Wrapf(err, "failed to retrieve %s external remdiation template %q/%q", obj.GetKind(), key.Namespace, key.Name)
	}
	return obj, nil
}

func (r *NodeHealthCheckReconciler) patchStatus(nhc *remediationv1alpha1.NodeHealthCheck, observedNodes int, unhealthyNodes int, remediations map[string]metav1.Time) error {

	healthyNodes := observedNodes - unhealthyNodes

	// skip when no changes
	if nhc.Status.ObservedNodes == observedNodes &&
		nhc.Status.HealthyNodes == healthyNodes &&
		((len(nhc.Status.InFlightRemediations) == 0 && len(remediations) == 0) || reflect.DeepEqual(nhc.Status.InFlightRemediations, remediations)) {
		return nil
	}

	base := nhc.DeepCopy()
	mergeFrom := client.MergeFrom(base)

	nhc.Status.ObservedNodes = observedNodes
	nhc.Status.HealthyNodes = healthyNodes
	nhc.Status.InFlightRemediations = remediations

	// all values to be patched expected to be updated on the current nhc.status
	r.Log.Info("Patching NHC object", "patch", nhc.Status)
	return r.Client.Status().Patch(context.Background(), nhc, mergeFrom, &client.PatchOptions{})
}

func (r *NodeHealthCheckReconciler) getInflightRemediations(nhc *remediationv1alpha1.NodeHealthCheck) (map[string]metav1.Time, error) {
	cr, err := r.generateRemediationCR(&v1.Node{}, nhc)
	if err != nil {
		return nil, err
	}
	crList := &unstructured.UnstructuredList{Object: cr.Object}
	err = r.Client.List(context.Background(), crList)

	if err != nil && !apierrors.IsNotFound(err) {
		return nil,
			errors.Wrapf(err, "failed to fetch all remediation objects from kind %s and apiVersion %s",
				cr.GroupVersionKind(),
				cr.GetAPIVersion())
	}

	remediations := make(map[string]metav1.Time)
	for _, remediationCR := range crList.Items {
		for _, ownerRefs := range remediationCR.GetOwnerReferences() {
			if ownerRefs.Name == nhc.Name &&
				ownerRefs.Kind == nhc.Kind &&
				ownerRefs.APIVersion == nhc.APIVersion {
				remediations[remediationCR.GetName()] = remediationCR.GetCreationTimestamp()
				continue
			}
		}
	}
	return remediations, nil
}

func (r *NodeHealthCheckReconciler) alertOldRemediationCR(remediationCR *unstructured.Unstructured) (bool, *time.Duration) {
	isSendAlert := false
	var nextReconcile *time.Duration = nil
	//verify remediationCR is old
	now := time.Now()
	if now.After(remediationCR.GetCreationTimestamp().Add(remediationCRAlertTimeout)) {
		var remediationCrAnnotations map[string]string
		if remediationCrAnnotations = remediationCR.GetAnnotations(); remediationCrAnnotations == nil {
			remediationCrAnnotations = map[string]string{}
		}
		//verify this is the first alert for this remediationCR
		if _, isAlertedSent := remediationCrAnnotations[oldRemediationCRAnnotationKey]; !isAlertedSent {
			remediationCrAnnotations[oldRemediationCRAnnotationKey] = "flagon"
			remediationCR.SetAnnotations(remediationCrAnnotations)
			if err := r.Client.Update(context.TODO(), remediationCR); err == nil {
				isSendAlert = true
			} else {
				r.Log.Error(err, "Setting `old remediationCR` annotation on remediation CR %s: failed to update: %v", remediationCR.GetName(), err)
			}

		}
	} else {
		calcNextReconcile := remediationCRAlertTimeout - now.Sub(remediationCR.GetCreationTimestamp().Time) + time.Minute
		nextReconcile = &calcNextReconcile
	}
	return isSendAlert, nextReconcile

}

func updateResultNextReconcile(result *ctrl.Result, updatedRequeueAfter time.Duration) {
	if result.RequeueAfter == 0 || updatedRequeueAfter < result.RequeueAfter {
		result.RequeueAfter = updatedRequeueAfter
	}
}
