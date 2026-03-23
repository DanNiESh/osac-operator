/*
Copyright (c) 2025 Red Hat Inc.

Licensed under the Apache License, Version 2.0 (the "License"); you may not use this file except in compliance with the
License. You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software distributed under the License is distributed on an
"AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied. See the License for the specific
language governing permissions and limitations under the License.
*/

package controller

import (
	"context"

	"google.golang.org/grpc"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	clnt "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

// SecurityGroupFeedbackReconciler sends updates to the fulfillment service.
type SecurityGroupFeedbackReconciler struct {
	hubClient            clnt.Client
	securityGroupsClient privatev1.SecurityGroupsClient
	networkingNamespace  string
}

// securityGroupFeedbackReconcilerTask contains data that is used for the reconciliation of a specific security group, so there is less
// need to pass around as function parameters that and other related objects.
type securityGroupFeedbackReconcilerTask struct {
	r             *SecurityGroupFeedbackReconciler
	object        *v1alpha1.SecurityGroup
	securityGroup *privatev1.SecurityGroup
}

// NewSecurityGroupFeedbackReconciler creates a reconciler that sends to the fulfillment service updates about security groups.
func NewSecurityGroupFeedbackReconciler(hubClient clnt.Client, grpcConn *grpc.ClientConn, networkingNamespace string) *SecurityGroupFeedbackReconciler {
	return &SecurityGroupFeedbackReconciler{
		hubClient:            hubClient,
		securityGroupsClient: privatev1.NewSecurityGroupsClient(grpcConn),
		networkingNamespace:  networkingNamespace,
	}
}

// SetupWithManager adds the reconciler to the controller manager.
func (r *SecurityGroupFeedbackReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		Named("securitygroup-feedback").
		For(&v1alpha1.SecurityGroup{}, builder.WithPredicates(NetworkingNamespacePredicate(r.networkingNamespace))).
		Complete(r)
}

// Reconcile is the implementation of the reconciler interface.
func (r *SecurityGroupFeedbackReconciler) Reconcile(ctx context.Context, request ctrl.Request) (result ctrl.Result, err error) {
	log := ctrllog.FromContext(ctx)

	// Fetch the object to reconcile, and do nothing if it no longer exists:
	object := &v1alpha1.SecurityGroup{}
	err = r.hubClient.Get(ctx, request.NamespacedName, object)
	if err != nil {
		err = clnt.IgnoreNotFound(err)
		return //nolint:nakedret
	}

	// Get the identifier of the security group from the labels. If this isn't present it means that the object wasn't
	// created by the fulfillment service, so we ignore it.
	securityGroupID, ok := object.Labels[osacSecurityGroupIDLabel]
	if !ok {
		log.Info(
			"There is no label containing the security group identifier, will ignore it",
			"label", osacSecurityGroupIDLabel,
		)
		return
	}

	// Check if the SecurityGroup is being deleted before fetching from fulfillment service
	if !object.ObjectMeta.DeletionTimestamp.IsZero() {
		log.Info(
			"SecurityGroup is being deleted, skipping feedback reconciliation",
		)
		return
	}

	// Fetch the security group:
	securityGroup, err := r.fetchSecurityGroup(ctx, securityGroupID)
	if err != nil {
		return
	}

	// Create a task to do the rest of the job, but using copies of the objects, so that we can later compare the
	// before and after values and save only the objects that have changed.
	t := &securityGroupFeedbackReconcilerTask{
		r:             r,
		object:        object,
		securityGroup: clone(securityGroup),
	}

	t.handleUpdate(ctx)

	// Save the objects that have changed:
	err = r.saveSecurityGroup(ctx, securityGroup, t.securityGroup)
	if err != nil {
		return
	}
	return
}

func (r *SecurityGroupFeedbackReconciler) fetchSecurityGroup(ctx context.Context, id string) (securityGroup *privatev1.SecurityGroup, err error) {
	response, err := r.securityGroupsClient.Get(ctx, privatev1.SecurityGroupsGetRequest_builder{
		Id: id,
	}.Build())
	if err != nil {
		return
	}
	securityGroup = response.GetObject()
	if !securityGroup.HasSpec() {
		securityGroup.SetSpec(&privatev1.SecurityGroupSpec{})
	}
	if !securityGroup.HasStatus() {
		securityGroup.SetStatus(&privatev1.SecurityGroupStatus{})
	}
	return
}

func (r *SecurityGroupFeedbackReconciler) saveSecurityGroup(ctx context.Context, before, after *privatev1.SecurityGroup) error {
	log := ctrllog.FromContext(ctx)

	if !equal(after, before) {
		log.Info(
			"Updating security group",
			"before", before,
			"after", after,
		)
		_, err := r.securityGroupsClient.Update(ctx, privatev1.SecurityGroupsUpdateRequest_builder{
			Object: after,
		}.Build())
		if err != nil {
			return err
		}
	}
	return nil
}

func (t *securityGroupFeedbackReconcilerTask) handleUpdate(ctx context.Context) {
	t.syncPhase(ctx)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhase(ctx context.Context) {
	switch t.object.Status.Phase {
	case v1alpha1.SecurityGroupPhaseProgressing:
		t.syncPhaseProgressing()
	case v1alpha1.SecurityGroupPhaseFailed:
		t.syncPhaseFailed()
	case v1alpha1.SecurityGroupPhaseReady:
		t.syncPhaseReady()
	case v1alpha1.SecurityGroupPhaseDeleting:
		t.syncPhaseDeleting()
	default:
		log := ctrllog.FromContext(ctx)
		log.Info(
			"Unknown phase, will ignore it",
			"phase", t.object.Status.Phase,
		)
	}
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseProgressing() {
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseFailed() {
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_FAILED)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseReady() {
	securityGroupStatus := t.securityGroup.GetStatus()
	securityGroupStatus.SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY)
}

func (t *securityGroupFeedbackReconcilerTask) syncPhaseDeleting() {
	// Deleting state maps to PENDING as deletion is in progress
	t.securityGroup.GetStatus().SetState(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING)
}
