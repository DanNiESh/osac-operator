/*
Copyright 2025.

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
	"net"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/osac-project/osac-operator/api/v1alpha1"
	privatev1 "github.com/osac-project/osac-operator/internal/api/osac/private/v1"
)

var _ = Describe("SecurityGroupFeedbackController", func() {
	const (
		sgName      = "test-sg"
		sgNamespace = "test-namespace"
		sgID        = "sg-123"
		testVnetRef = "vnet-abc"
	)

	var (
		ctx        context.Context
		k8sClient  client.Client
		mockServer *mockSecurityGroupsServer
		reconciler *SecurityGroupFeedbackReconciler
		grpcServer *grpc.Server
		listener   *bufconn.Listener
	)

	BeforeEach(func() {
		ctx = context.Background()

		// Create fake Kubernetes client
		scheme := runtime.NewScheme()
		Expect(v1alpha1.AddToScheme(scheme)).To(Succeed())
		k8sClient = fake.NewClientBuilder().WithScheme(scheme).Build()

		// Create mock gRPC server
		mockServer = &mockSecurityGroupsServer{
			securityGroups: make(map[string]*privatev1.SecurityGroup),
			updates:        make([]*privatev1.SecurityGroup, 0),
		}
		listener = bufconn.Listen(1024 * 1024)
		grpcServer = grpc.NewServer()
		privatev1.RegisterSecurityGroupsServer(grpcServer, mockServer)

		go func() {
			_ = grpcServer.Serve(listener)
		}()

		// Create gRPC client connection
		conn, err := grpc.NewClient("passthrough:///bufnet",
			grpc.WithContextDialer(func(ctx context.Context, s string) (net.Conn, error) {
				return listener.Dial()
			}),
			grpc.WithTransportCredentials(insecure.NewCredentials()),
		)
		Expect(err).NotTo(HaveOccurred())

		// Create reconciler
		reconciler = NewSecurityGroupFeedbackReconciler(k8sClient, conn, sgNamespace)
	})

	AfterEach(func() {
		if grpcServer != nil {
			grpcServer.Stop()
		}
		if listener != nil {
			_ = listener.Close()
		}
	})

	Context("when reconciling a SecurityGroup CR", func() {
		It("should sync Phase=Ready to database state=READY", func() {
			// Create security group in mock database
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING,
				},
			}
			mockServer.addSecurityGroup(sg)

			// Create SecurityGroup CR with Phase=Ready
			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			// Reconcile
			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert gRPC Update called with state=READY
			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY))
		})

		It("should sync Phase=Progressing to database state=PENDING", func() {
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY,
				},
			}
			mockServer.addSecurityGroup(sg)

			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseProgressing,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING))
		})

		It("should sync Phase=Failed to database state=FAILED", func() {
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING,
				},
			}
			mockServer.addSecurityGroup(sg)

			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseFailed,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_FAILED))
		})

		It("should sync Phase=Deleting to database state=PENDING", func() {
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY,
				},
			}
			mockServer.addSecurityGroup(sg)

			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			Expect(mockServer.updates).To(HaveLen(1))
			Expect(mockServer.updates[0].GetStatus().GetState()).To(Equal(privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING))
		})

		It("should skip CRs without securitygroup-uuid label", func() {
			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels:    map[string]string{},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert no Update RPC was called
			Expect(mockServer.updates).To(BeEmpty())
		})

		It("should skip CRs being deleted", func() {
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_PENDING,
				},
			}
			mockServer.addSecurityGroup(sg)

			now := metav1.Now()
			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:              sgName,
					Namespace:         sgNamespace,
					DeletionTimestamp: &now,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseDeleting,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert no Update RPC was called
			Expect(mockServer.updates).To(BeEmpty())
		})

		It("should not update if status unchanged", func() {
			sg := &privatev1.SecurityGroup{
				Id: sgID,
				Metadata: &privatev1.Metadata{
					Name: sgName,
				},
				Spec: &privatev1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: &privatev1.SecurityGroupStatus{
					State: privatev1.SecurityGroupState_SECURITY_GROUP_STATE_READY,
				},
			}
			mockServer.addSecurityGroup(sg)

			cr := &v1alpha1.SecurityGroup{
				ObjectMeta: metav1.ObjectMeta{
					Name:      sgName,
					Namespace: sgNamespace,
					Labels: map[string]string{
						osacSecurityGroupIDLabel: sgID,
					},
				},
				Spec: v1alpha1.SecurityGroupSpec{
					VirtualNetwork: testVnetRef,
				},
				Status: v1alpha1.SecurityGroupStatus{
					Phase: v1alpha1.SecurityGroupPhaseReady,
				},
			}
			Expect(k8sClient.Create(ctx, cr)).To(Succeed())

			_, err := reconciler.Reconcile(ctx, reconcile.Request{
				NamespacedName: types.NamespacedName{
					Name:      sgName,
					Namespace: sgNamespace,
				},
			})
			Expect(err).NotTo(HaveOccurred())

			// Assert no Update RPC was called (state already READY)
			Expect(mockServer.updates).To(BeEmpty())
		})
	})
})

// mockSecurityGroupsServer implements privatev1.SecurityGroupsServer for testing
type mockSecurityGroupsServer struct {
	privatev1.UnimplementedSecurityGroupsServer
	mu             sync.Mutex
	securityGroups map[string]*privatev1.SecurityGroup
	updates        []*privatev1.SecurityGroup
}

func (m *mockSecurityGroupsServer) addSecurityGroup(sg *privatev1.SecurityGroup) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.securityGroups[sg.GetId()] = sg
}

func (m *mockSecurityGroupsServer) Get(ctx context.Context, req *privatev1.SecurityGroupsGetRequest) (*privatev1.SecurityGroupsGetResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sg, ok := m.securityGroups[req.GetId()]
	if !ok {
		return nil, fmt.Errorf("security group not found: %s", req.GetId())
	}

	return &privatev1.SecurityGroupsGetResponse{
		Object: sg,
	}, nil
}

func (m *mockSecurityGroupsServer) Update(ctx context.Context, req *privatev1.SecurityGroupsUpdateRequest) (*privatev1.SecurityGroupsUpdateResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	sg := req.GetObject()
	m.securityGroups[sg.GetId()] = sg
	m.updates = append(m.updates, sg)

	return &privatev1.SecurityGroupsUpdateResponse{
		Object: sg,
	}, nil
}
