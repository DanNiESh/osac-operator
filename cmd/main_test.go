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

package main

import (
	"context"
	"errors"
	"testing"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/cluster"
	mcmanager "sigs.k8s.io/multicluster-runtime/pkg/manager"
	"sigs.k8s.io/multicluster-runtime/pkg/multicluster"
)

// mockCluster implements cluster.Cluster.Start for testing.
type mockCluster struct {
	cluster.Cluster
	startFunc func(ctx context.Context) error
}

func (m *mockCluster) Start(ctx context.Context) error {
	return m.startFunc(ctx)
}

// mockProvider implements both multicluster.Provider and multicluster.ProviderRunnable.
type mockProvider struct {
	multicluster.Provider
	startFunc func(ctx context.Context, mgr multicluster.Aware) error
}

func (m *mockProvider) Start(ctx context.Context, mgr multicluster.Aware) error {
	return m.startFunc(ctx, mgr)
}

// mockManager implements mcmanager.Manager.Start for testing.
type mockManager struct {
	mcmanager.Manager
	startFunc func(ctx context.Context) error
}

func (m *mockManager) Start(ctx context.Context) error {
	return m.startFunc(ctx)
}

func TestIgnoreCanceled(t *testing.T) {
	t.Run("returns nil for context.Canceled", func(t *testing.T) {
		if err := ignoreCanceled(context.Canceled); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("returns nil for wrapped context.Canceled", func(t *testing.T) {
		wrapped := errors.Join(errors.New("something"), context.Canceled)
		if err := ignoreCanceled(wrapped); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("returns nil for nil error", func(t *testing.T) {
		if err := ignoreCanceled(nil); err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("preserves real errors", func(t *testing.T) {
		realErr := errors.New("connection refused")
		if err := ignoreCanceled(realErr); err != realErr {
			t.Errorf("expected original error, got %v", err)
		}
	})
}

func TestStartComponents(t *testing.T) {
	t.Run("manager only (no remote cluster or provider)", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				cancel() // simulate clean shutdown
				return context.Canceled
			},
		}

		err := startComponents(ctx, nil, nil, mgr)
		if err != nil {
			t.Errorf("expected nil, got %v", err)
		}
	})

	t.Run("manager error propagates", func(t *testing.T) {
		mgrErr := errors.New("manager failed")
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				return mgrErr
			},
		}

		err := startComponents(context.Background(), nil, nil, mgr)
		if !errors.Is(err, mgrErr) {
			t.Errorf("expected manager error, got %v", err)
		}
	})

	t.Run("remote cluster error cancels manager", func(t *testing.T) {
		clusterErr := errors.New("remote cluster unreachable")
		cl := &mockCluster{
			startFunc: func(ctx context.Context) error {
				return clusterErr
			},
		}
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				// Block until context is cancelled by the cluster failure
				<-ctx.Done()
				return ctx.Err()
			},
		}

		err := startComponents(context.Background(), cl, nil, mgr)
		if !errors.Is(err, clusterErr) {
			t.Errorf("expected cluster error, got %v", err)
		}
	})

	t.Run("remote provider error cancels manager and cluster", func(t *testing.T) {
		providerErr := errors.New("provider failed to engage")
		cl := &mockCluster{
			startFunc: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}
		prov := &mockProvider{
			startFunc: func(ctx context.Context, mgr multicluster.Aware) error {
				return providerErr
			},
		}
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}

		err := startComponents(context.Background(), cl, prov, mgr)
		if !errors.Is(err, providerErr) {
			t.Errorf("expected provider error, got %v", err)
		}
	})

	t.Run("context cancellation shuts down all components gracefully", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())

		clusterStopped := make(chan struct{})
		providerStopped := make(chan struct{})
		mgrStopped := make(chan struct{})

		cl := &mockCluster{
			startFunc: func(ctx context.Context) error {
				<-ctx.Done()
				close(clusterStopped)
				return ctx.Err()
			},
		}
		prov := &mockProvider{
			startFunc: func(ctx context.Context, mgr multicluster.Aware) error {
				<-ctx.Done()
				close(providerStopped)
				return ctx.Err()
			},
		}
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				<-ctx.Done()
				close(mgrStopped)
				return ctx.Err()
			},
		}

		done := make(chan error, 1)
		go func() {
			done <- startComponents(ctx, cl, prov, mgr)
		}()

		// Simulate SIGTERM by cancelling the context
		cancel()

		select {
		case err := <-done:
			if err != nil {
				t.Errorf("expected nil (canceled is filtered), got %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("timeout waiting for startComponents to return")
		}

		// Verify all components were signaled to stop
		select {
		case <-clusterStopped:
		default:
			t.Error("cluster was not stopped")
		}
		select {
		case <-providerStopped:
		default:
			t.Error("provider was not stopped")
		}
		select {
		case <-mgrStopped:
		default:
			t.Error("manager was not stopped")
		}
	})

	t.Run("cluster failure stops provider and manager", func(t *testing.T) {
		clusterErr := errors.New("cluster connection lost")

		providerStopped := make(chan struct{})
		mgrStopped := make(chan struct{})

		cl := &mockCluster{
			startFunc: func(ctx context.Context) error {
				// Simulate cluster failing after brief operation
				time.Sleep(10 * time.Millisecond)
				return clusterErr
			},
		}
		prov := &mockProvider{
			startFunc: func(ctx context.Context, mgr multicluster.Aware) error {
				<-ctx.Done()
				close(providerStopped)
				return ctx.Err()
			},
		}
		mgr := &mockManager{
			startFunc: func(ctx context.Context) error {
				<-ctx.Done()
				close(mgrStopped)
				return ctx.Err()
			},
		}

		err := startComponents(context.Background(), cl, prov, mgr)
		if !errors.Is(err, clusterErr) {
			t.Errorf("expected cluster error, got %v", err)
		}

		// Verify provider and manager were cancelled
		select {
		case <-providerStopped:
		case <-time.After(5 * time.Second):
			t.Error("provider was not stopped after cluster failure")
		}
		select {
		case <-mgrStopped:
		case <-time.After(5 * time.Second):
			t.Error("manager was not stopped after cluster failure")
		}
	})
}
