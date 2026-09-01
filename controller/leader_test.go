package controller

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
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

func TestLeaderElectionRejoinsAfterLoss(t *testing.T) {
	shrinkLeaderTimings(t)
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// The reactor must be registered before the clientset is in use; the
	// atomic flag arms it later, once the first term is established.
	var fail atomic.Bool
	kube.Fake.PrependReactor("update", "leases", func(ktesting.Action) (bool, runtime.Object, error) {
		if fail.Load() {
			return true, nil, apierrors.NewInternalError(errors.New("injected renewal failure"))
		}
		return false, nil, nil
	})

	go m.Run(ctx)
	go m.RunLeaderElection(ctx)

	// Wait for the first leadership term.
	deadline := time.Now().Add(5 * time.Second)
	var first *coordinationv1.Lease
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" &&
			lease.Spec.RenewTime != nil {
			first = lease
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not acquire leadership")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Make every lease update fail; renewal failing past RenewDeadline
	// ends the election run entirely.
	fail.Store(true)
	time.Sleep(time.Second)
	fail.Store(false)

	// The election loop must rejoin: renewals eventually advance RenewTime
	// well past the first term's last one.
	deadline = time.Now().Add(5 * time.Second)
	threshold := first.Spec.RenewTime.Time.Add(900 * time.Millisecond)
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.RenewTime != nil && lease.Spec.RenewTime.Time.After(threshold) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not rejoin the election after losing leadership")
		}
		time.Sleep(20 * time.Millisecond)
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

func TestLeaderReentersElectionAfterLosingLease(t *testing.T) {
	shrinkLeaderTimings(t)
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go m.Run(ctx)
	go m.RunLeaderElection(ctx)

	// Wait for the first leadership and remember the identity — it stays
	// the same across election rounds of one RunLeaderElection call.
	var identity string
	deadline := time.Now().Add(5 * time.Second)
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity != "" {
			identity = *lease.Spec.HolderIdentity
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not become leader")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Preempt the Lease: another holder with a fresh renew time makes this
	// replica's renews fail until RenewDeadline ends its leadership — what
	// a brief API server outage looks like to the elector. The renew may
	// race the update, so retry on conflict.
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err != nil {
			t.Fatal(err)
		}
		preemptor := "preemptor"
		now := metav1.MicroTime{Time: time.Now()}
		lease.Spec.HolderIdentity = &preemptor
		lease.Spec.RenewTime = &now
		if _, err := kube.CoordinationV1().Leases("ironhive").Update(ctx, lease, metav1.UpdateOptions{}); err == nil {
			break
		} else if !apierrors.IsConflict(err) {
			t.Fatal(err)
		}
	}

	// The preemptor never renews, so its lease expires; this replica must
	// re-enter the election and win it back under the same identity —
	// the first elector can never lead again once it has stopped.
	deadline = time.Now().Add(10 * time.Second)
	for {
		lease, err := kube.CoordinationV1().Leases("ironhive").Get(ctx, leaderLeaseName, metav1.GetOptions{})
		if err == nil && lease.Spec.HolderIdentity != nil && *lease.Spec.HolderIdentity == identity {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("replica did not regain leadership after losing the lease")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// And it reconciles again: the pool sits at its configured size.
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("after regaining leadership: %d pods, want 1", len(pods.Items))
	}
}
