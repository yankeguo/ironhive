package controller

import (
	"context"
	"regexp"
	"testing"

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
