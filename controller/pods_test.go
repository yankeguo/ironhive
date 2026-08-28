package controller

import (
	"context"
	"errors"
	"regexp"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes/fake"
)

var podNamePattern = regexp.MustCompile(`^sandbox-[0-9a-z]{26}$`)

func testPoolConfig(count int) *Config {
	return &Config{Pools: map[string]PoolConfig{
		"default": {
			Standby: StandbyConfig{Static: StaticStandbyConfig{Count: count}},
			PodTemplate: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: map[string]string{
					"team":         "platform",
					LabelManagedBy: "spoofed", // must be overridden
				}},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "main", Image: "img"}},
				},
			},
		},
	}}
}

func TestReconcileCreatesStandbyPods(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(3))
	ctx := context.Background()

	m.reconcile(ctx)

	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 3 {
		t.Fatalf("created %d pods, want 3", len(pods.Items))
	}
	for _, pod := range pods.Items {
		if !podNamePattern.MatchString(pod.Name) {
			t.Errorf("pod name %q does not match %s", pod.Name, podNamePattern)
		}
		if pod.Labels[LabelManagedBy] != ManagedByValue {
			t.Errorf("pod %s managed-by label = %q", pod.Name, pod.Labels[LabelManagedBy])
		}
		if pod.Labels[LabelPool] != "default" {
			t.Errorf("pod %s pool label = %q", pod.Name, pod.Labels[LabelPool])
		}
		if pod.Labels["team"] != "platform" {
			t.Errorf("pod %s lost template label team", pod.Name)
		}
	}

	// A second pass finds all pods active and creates nothing more.
	m.reconcile(ctx)
	pods, err = kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 3 {
		t.Fatalf("after second reconcile: %d pods, want 3", len(pods.Items))
	}
	if got := len(m.Pods()); got != 3 {
		t.Fatalf("in-memory state has %d pods, want 3", got)
	}
}

func TestReconcileSweepsTerminatedAndTopsUp(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(2))
	ctx := context.Background()

	m.reconcile(ctx)
	if got := len(m.Pods()); got != 2 {
		t.Fatalf("in-memory state has %d pods, want 2", got)
	}

	// One pod fails.
	failed := m.Pods()[0].Name
	m.mu.Lock()
	m.pods[failed].Phase = corev1.PodFailed
	m.mu.Unlock()

	m.reconcile(ctx)

	if _, err := kube.CoreV1().Pods("ironhive").Get(ctx, failed, metav1.GetOptions{}); err == nil {
		t.Fatal("failed pod was not deleted")
	}
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 2 {
		t.Fatalf("after sweep+top-up: %d pods, want 2", len(pods.Items))
	}
}

func TestListSeedsState(t *testing.T) {
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-existing",
			Namespace: "ironhive",
			Labels:    map[string]string{LabelManagedBy: ManagedByValue, LabelPool: "default"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodRunning, PodIP: "10.0.0.7"},
	}
	kube := fake.NewSimpleClientset(existing)
	m := NewPodManager(kube, "ironhive", testPoolConfig(2))

	if _, err := m.list(context.Background()); err != nil {
		t.Fatal(err)
	}
	pods := m.Pods()
	if len(pods) != 1 {
		t.Fatalf("state has %d pods, want 1", len(pods))
	}
	p := pods[0]
	if p.Name != "sandbox-existing" || p.Pool != "default" || p.IP != "10.0.0.7" || p.Phase != corev1.PodRunning {
		t.Errorf("unexpected state: %+v", p)
	}
}

func TestApplyEventUpdatesState(t *testing.T) {
	m := NewPodManager(fake.NewSimpleClientset(), "ironhive", testPoolConfig(0))

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "sandbox-a",
			Namespace: "ironhive",
			Labels:    map[string]string{LabelPool: "default"},
		},
		Status: corev1.PodStatus{Phase: corev1.PodPending},
	}
	m.applyEvent(watch.Event{Type: watch.Added, Object: pod})
	if got := len(m.Pods()); got != 1 {
		t.Fatalf("after add: %d pods, want 1", got)
	}

	ready := pod.DeepCopy()
	ready.Status.Phase = corev1.PodRunning
	ready.Status.PodIP = "10.0.0.9"
	ready.Status.Conditions = []corev1.PodCondition{{
		Type:   corev1.PodReady,
		Status: corev1.ConditionTrue,
	}}
	m.applyEvent(watch.Event{Type: watch.Modified, Object: ready})
	p := m.Pods()[0]
	if p.Phase != corev1.PodRunning || p.IP != "10.0.0.9" || !p.Ready {
		t.Errorf("after modify: %+v", p)
	}

	m.applyEvent(watch.Event{Type: watch.Deleted, Object: pod})
	if got := len(m.Pods()); got != 0 {
		t.Fatalf("after delete: %d pods, want 0", got)
	}

	// Non-pod objects (e.g. watch.Error status) are ignored.
	m.applyEvent(watch.Event{Type: watch.Error, Object: &metav1.Status{}})
	if got := len(m.Pods()); got != 0 {
		t.Fatalf("after error event: %d pods, want 0", got)
	}
}

// markReady makes every in-memory pod a valid allocation candidate.
func markReady(m *PodManager) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, p := range m.pods {
		p.Phase = corev1.PodRunning
		p.Ready = true
		if p.IP == "" {
			p.IP = "10.0.0.1"
		}
	}
}

func TestAllocateClaimsDistinctPods(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(2))
	ctx := context.Background()

	m.reconcile(ctx)
	markReady(m)

	first, err := m.Allocate(ctx, "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if !first.Allocated {
		t.Error("first pod not marked allocated")
	}

	second, err := m.Allocate(ctx, "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if second.Name == first.Name {
		t.Error("allocated the same pod twice")
	}

	// The allocation fact landed on the API object, lease included.
	pod, err := kube.CoreV1().Pods("ironhive").Get(ctx, first.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if pod.Annotations[AnnotationAllocated] == "" {
		t.Error("allocated annotation missing on the pod object")
	}
	expires, err := time.Parse(time.RFC3339, pod.Annotations[AnnotationLeaseExpires])
	if err != nil {
		t.Fatal("lease-expires annotation missing or unparseable:", err)
	}
	if until := time.Until(expires); until < 30*time.Second || until > time.Minute {
		t.Errorf("lease expires in %v, want ~1m", until)
	}
	if first.LeaseExpires.IsZero() {
		t.Error("in-memory state has no lease deadline")
	}

	// The pool is exhausted now.
	if _, err := m.Allocate(ctx, "default", time.Minute, 50*time.Millisecond); !errors.Is(err, ErrNoSandboxAvailable) {
		t.Errorf("want ErrNoSandboxAvailable, got %v", err)
	}
}

func TestAllocateUnknownPool(t *testing.T) {
	m := NewPodManager(fake.NewSimpleClientset(), "ironhive", testPoolConfig(0))
	if _, err := m.Allocate(context.Background(), "nope", time.Minute, time.Second); !errors.Is(err, ErrUnknownPool) {
		t.Errorf("want ErrUnknownPool, got %v", err)
	}
}

func TestAllocateWaitsForReady(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx := context.Background()

	m.reconcile(ctx)

	type result struct {
		st  PodState
		err error
	}
	ch := make(chan result, 1)
	go func() {
		st, err := m.Allocate(ctx, "default", time.Minute, 3*time.Second)
		ch <- result{st, err}
	}()

	// The pod becomes Ready while Allocate is blocked.
	time.Sleep(100 * time.Millisecond)
	markReady(m)

	select {
	case r := <-ch:
		if r.err != nil {
			t.Fatal(r.err)
		}
		if r.st.Name == "" {
			t.Error("allocated pod has no name")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("allocate did not return after the pod became ready")
	}
}

func TestReleaseDestroysPod(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx := context.Background()

	m.reconcile(ctx)
	markReady(m)

	st, err := m.Allocate(ctx, "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	if err := m.Release(ctx, st.Name); err != nil {
		t.Fatal(err)
	}
	if _, err := kube.CoreV1().Pods("ironhive").Get(ctx, st.Name, metav1.GetOptions{}); err == nil {
		t.Error("pod still exists after release")
	}
	if _, ok := m.Lookup(st.Name); ok {
		t.Error("pod still in memory after release")
	}
	if err := m.Release(ctx, st.Name); !errors.Is(err, ErrSandboxNotFound) {
		t.Errorf("re-release: want ErrSandboxNotFound, got %v", err)
	}
}

func TestReleaseStandbyPodFails(t *testing.T) {
	m := NewPodManager(fake.NewSimpleClientset(), "ironhive", testPoolConfig(1))
	ctx := context.Background()

	m.reconcile(ctx)
	standby := m.Pods()[0].Name
	if err := m.Release(ctx, standby); !errors.Is(err, ErrSandboxNotAllocated) {
		t.Errorf("want ErrSandboxNotAllocated, got %v", err)
	}
}

func TestReconcileReplacesAllocatedPod(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(2))
	ctx := context.Background()

	m.reconcile(ctx)
	markReady(m)
	if _, err := m.Allocate(ctx, "default", time.Minute, time.Second); err != nil {
		t.Fatal(err)
	}

	// The allocated pod no longer counts as standby, so reconcile tops up.
	m.reconcile(ctx)
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 3 {
		t.Fatalf("after top-up: %d pods, want 3", len(pods.Items))
	}
}

func TestRenewExtendsLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx := context.Background()

	m.reconcile(ctx)
	markReady(m)
	st, err := m.Allocate(ctx, "default", time.Minute, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	renewed, err := m.Renew(ctx, st.Name, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if until := time.Until(renewed.LeaseExpires); until < 59*time.Minute || until > time.Hour {
		t.Errorf("renewed lease expires in %v, want ~1h", until)
	}
	pod, err := kube.CoreV1().Pods("ironhive").Get(ctx, st.Name, metav1.GetOptions{})
	if err != nil {
		t.Fatal(err)
	}
	expires, err := time.Parse(time.RFC3339, pod.Annotations[AnnotationLeaseExpires])
	if err != nil || time.Until(expires) < 59*time.Minute {
		t.Errorf("lease-expires annotation = %q, err = %v", pod.Annotations[AnnotationLeaseExpires], err)
	}

	// Renewing a standby pod fails.
	m2 := NewPodManager(fake.NewSimpleClientset(), "ironhive", testPoolConfig(1))
	m2.reconcile(ctx)
	standby := m2.Pods()[0].Name
	if _, err := m2.Renew(ctx, standby, time.Hour); !errors.Is(err, ErrSandboxNotAllocated) {
		t.Errorf("renew standby: want ErrSandboxNotAllocated, got %v", err)
	}
	if _, err := m2.Renew(ctx, "sandbox-nope", time.Hour); !errors.Is(err, ErrSandboxNotFound) {
		t.Errorf("renew unknown: want ErrSandboxNotFound, got %v", err)
	}
}

func TestReconcileReapsExpiredLease(t *testing.T) {
	kube := fake.NewSimpleClientset()
	m := NewPodManager(kube, "ironhive", testPoolConfig(1))
	ctx := context.Background()

	m.reconcile(ctx)
	markReady(m)
	// A one-millisecond lease is expired by the next reconcile pass.
	st, err := m.Allocate(ctx, "default", time.Millisecond, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)

	m.reconcile(ctx)

	if _, err := kube.CoreV1().Pods("ironhive").Get(ctx, st.Name, metav1.GetOptions{}); err == nil {
		t.Error("expired-lease pod was not deleted")
	}
	// The pool is topped back up.
	pods, err := kube.CoreV1().Pods("ironhive").List(ctx, metav1.ListOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(pods.Items) != 1 {
		t.Fatalf("after reap+top-up: %d pods, want 1", len(pods.Items))
	}
}
