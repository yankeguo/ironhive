package controller

import (
	"context"
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oklog/ulid/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Labels the controller enforces on every pod it creates. The managed-by
// selector below is how the controller recognizes its own pods in list and
// watch; user-supplied template labels with the same keys are overridden.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPool      = "ironhive.dev/pool"

	ManagedByValue = "ironhive-controller"
)

// podSelector selects exactly the pods this controller manages.
var podSelector = labels.SelectorFromSet(labels.Set{
	LabelManagedBy: ManagedByValue,
}).String()

// PodState is the in-memory state of one managed pod.
type PodState struct {
	Name      string
	Pool      string
	Namespace string
	IP        string
	Phase     corev1.PodPhase
	Ready     bool // the pod's Ready condition
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PodManager keeps each pool's standby pods created and tracks all managed
// pods in memory via list+watch. Run it once; it is safe to use Pods from
// other goroutines concurrently.
type PodManager struct {
	kube      kubernetes.Interface
	namespace string
	cfg       *Config

	mu   sync.RWMutex
	pods map[string]*PodState // key: pod name
}

func NewPodManager(kube kubernetes.Interface, namespace string, cfg *Config) *PodManager {
	return &PodManager{
		kube:      kube,
		namespace: namespace,
		cfg:       cfg,
		pods:      map[string]*PodState{},
	}
}

// Pods returns a snapshot of all known managed pods, sorted by name.
func (m *PodManager) Pods() []PodState {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]PodState, 0, len(m.pods))
	for _, p := range m.pods {
		out = append(out, *p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// Run lists the managed pods, reconciles the standby counts, then watches
// for changes. A broken watch is re-established from a fresh list with
// backoff, so state eventually reconverges after any disconnect. It returns
// when ctx is cancelled.
func (m *PodManager) Run(ctx context.Context) {
	go m.reconcileLoop(ctx)
	backoff := time.Second
	for ctx.Err() == nil {
		resourceVersion, err := m.list(ctx)
		if err == nil {
			m.reconcile(ctx)
			backoff = time.Second
			err = m.watch(ctx, resourceVersion)
		}
		if ctx.Err() != nil {
			return
		}
		log.Println("pod manager: restarting watch:", err)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

// reconcileLoop re-runs reconcile periodically as a safety net against
// silently missed watch events.
func (m *PodManager) reconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			m.reconcile(ctx)
		}
	}
}

// list replaces the in-memory state with a fresh list of managed pods and
// returns the list's resourceVersion for the following watch.
func (m *PodManager) list(ctx context.Context) (string, error) {
	list, err := m.kube.CoreV1().Pods(m.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: podSelector,
	})
	if err != nil {
		return "", fmt.Errorf("list pods: %w", err)
	}
	fresh := make(map[string]*PodState, len(list.Items))
	for i := range list.Items {
		pod := &list.Items[i]
		fresh[pod.Name] = podStateFrom(pod)
	}
	m.mu.Lock()
	m.pods = fresh
	m.mu.Unlock()
	return list.ResourceVersion, nil
}

// watch applies pod events to the in-memory state until the watch breaks
// or ctx is cancelled.
func (m *PodManager) watch(ctx context.Context, resourceVersion string) error {
	w, err := m.kube.CoreV1().Pods(m.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector:   podSelector,
		ResourceVersion: resourceVersion,
	})
	if err != nil {
		return fmt.Errorf("watch pods: %w", err)
	}
	defer w.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-w.ResultChan():
			if !ok {
				return fmt.Errorf("watch channel closed")
			}
			m.applyEvent(ev)
		}
	}
}

func (m *PodManager) applyEvent(ev watch.Event) {
	pod, ok := ev.Object.(*corev1.Pod)
	if !ok {
		// Includes watch.Error events (e.g. 410 Gone) — logged by the
		// caller restarting from a fresh list.
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	switch ev.Type {
	case watch.Added, watch.Modified:
		m.pods[pod.Name] = podStateFrom(pod)
	case watch.Deleted:
		delete(m.pods, pod.Name)
	}
}

// reconcile tops up each pool to its standby count and sweeps terminated
// pods so they do not pile up. Failures are logged and retried on the next
// pass.
func (m *PodManager) reconcile(ctx context.Context) {
	m.mu.RLock()
	active := make(map[string]int, len(m.cfg.Pools))
	var terminated []string
	for _, p := range m.pods {
		// A freshly created pod has an empty phase until the API server
		// fills it in; count anything not terminated as active.
		if p.Phase == corev1.PodSucceeded || p.Phase == corev1.PodFailed {
			terminated = append(terminated, p.Name)
		} else {
			active[p.Pool]++
		}
	}
	m.mu.RUnlock()

	for _, name := range terminated {
		if err := m.kube.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			log.Println("pod manager: delete terminated pod", name, ":", err)
		} else {
			log.Println("pod manager: deleted terminated pod", name)
		}
	}

	// Sort pool names so top-up order is deterministic.
	names := make([]string, 0, len(m.cfg.Pools))
	for name := range m.cfg.Pools {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		pool := m.cfg.Pools[name]
		for i := active[name]; i < pool.Standby.Static.Count; i++ {
			if err := m.createPod(ctx, name, pool); err != nil {
				log.Println("pod manager: create pod for pool", name, ":", err)
				break
			}
		}
	}
}

// createPod creates one standby pod for the pool and records it in memory
// immediately, so the next reconcile pass does not double-create before
// the watch event arrives.
func (m *PodManager) createPod(ctx context.Context, poolName string, pool PoolConfig) error {
	tmpl := pool.PodTemplate.DeepCopy()
	pod := &corev1.Pod{
		ObjectMeta: tmpl.ObjectMeta,
		Spec:       tmpl.Spec,
	}
	pod.Name = "sandbox-" + strings.ToLower(ulid.Make().String())
	pod.GenerateName = ""
	pod.Namespace = m.namespace
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	pod.Labels[LabelManagedBy] = ManagedByValue
	pod.Labels[LabelPool] = poolName

	created, err := m.kube.CoreV1().Pods(m.namespace).Create(ctx, pod, metav1.CreateOptions{})
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.pods[created.Name] = podStateFrom(created)
	m.mu.Unlock()
	log.Println("pod manager: created pod", created.Name, "for pool", poolName)
	return nil
}

func podStateFrom(pod *corev1.Pod) *PodState {
	ready := false
	for _, c := range pod.Status.Conditions {
		if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
			ready = true
		}
	}
	return &PodState{
		Name:      pod.Name,
		Pool:      pod.Labels[LabelPool],
		Namespace: pod.Namespace,
		IP:        pod.Status.PodIP,
		Phase:     pod.Status.Phase,
		Ready:     ready,
		CreatedAt: pod.CreationTimestamp.Time,
		UpdatedAt: time.Now(),
	}
}
