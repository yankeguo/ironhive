package controller

import (
	"context"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// shrinkLeaderTimings makes the election fast enough for tests.
func shrinkLeaderTimings(t *testing.T) {
	t.Helper()
	lease, renew, retry := leaderLeaseDuration, leaderRenewDeadline, leaderRetryPeriod
	leaderLeaseDuration = time.Second
	leaderRenewDeadline = 500 * time.Millisecond
	leaderRetryPeriod = 50 * time.Millisecond
	t.Cleanup(func() {
		leaderLeaseDuration, leaderRenewDeadline, leaderRetryPeriod = lease, renew, retry
	})
}

func TestLeaderElectionReconciles(t *testing.T) {
	shrinkLeaderTimings(t)
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(2))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.Run(ctx)
	go m.RunLeaderElection(ctx)

	deadline := time.Now().Add(5 * time.Second)
	for {
		pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
		if err != nil {
			t.Fatal(err)
		}
		if len(pods.Items) == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("leader did not reconcile: %d pods, want 2", len(pods.Items))
		}
		time.Sleep(20 * time.Millisecond)
	}

	// The leadership is recorded on the Lease object.
	lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Error("lease has no holder identity")
	}
}

func TestLeaderElectionYieldsToHolder(t *testing.T) {
	shrinkLeaderTimings(t)
	// A lease freshly held by another replica for the next minute.
	now := metav1.MicroTime{Time: time.Now()}
	duration := int32(60)
	holder := "other-replica"
	existing := &coordinationv1.Lease{
		ObjectMeta: metav1.ObjectMeta{Name: leaderLeaseName, Namespace: "ironhive"},
		Spec: coordinationv1.LeaseSpec{
			HolderIdentity:       &holder,
			RenewTime:            &now,
			LeaseDurationSeconds: &duration,
		},
	}
	kube := fake.NewSimpleClientset(existing)
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.Run(ctx)
	go m.RunLeaderElection(ctx)

	// This replica is not the leader, so no reconcile may happen.
	time.Sleep(500 * time.Millisecond)
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 0 {
		t.Fatalf("non-leader reconciled: %d pods, want 0", len(pods.Items))
	}
}

func TestLeaderWaitsForInitialList(t *testing.T) {
	shrinkLeaderTimings(t)
	cfg := testPoolConfig(1)
	existing := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name:            "sandbox-existing",
		Namespace:       "ironhive",
		ResourceVersion: "1",
		Labels: map[string]string{
			LabelManagedBy:    ManagedByValue,
			LabelPool:         "default",
			LabelTemplateHash: podTemplateHash(cfg.Pools["default"].PodTemplate),
		},
	}}
	kube := fake.NewSimpleClientset(existing)
	m := NewPodManager(kube, "ironhive", cfg)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.RunLeaderElection(ctx)
	time.Sleep(200 * time.Millisecond)
	for _, action := range kube.Actions() {
		if action.GetVerb() == "create" && action.GetResource().Resource == "pods" {
			t.Fatal("leader created a pod before the initial list completed")
		}
	}
	if _, err := m.list(ctx); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not enter leader election after initial list")
		}
		time.Sleep(20 * time.Millisecond)
	}
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("restart produced %d pods, want the existing one only", len(pods.Items))
	}
}
