package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
)

// Labels the controller enforces on every pod it creates. The managed-by
// selector below is how the controller recognizes its own pods in list and
// watch; user-supplied template labels with the same keys are overridden.
const (
	LabelManagedBy = "app.kubernetes.io/managed-by"
	LabelPool      = "ironhive.dev/pool"
	// LabelTemplateHash records the deterministic hash of the pool's
	// podTemplate at creation time; reconcile recycles standby pods
	// whose hash no longer matches the configured template.
	LabelTemplateHash = "ironhive.dev/template-hash"

	ManagedByValue = "ironhive-controller"
)

// AnnotationAllocated marks a pod as handed out to a client; its value is
// the RFC3339 time of the allocation. AnnotationLeaseExpires holds the
// RFC3339 deadline of the allocation's lease — reconcile destroys the pod
// once it passes. Both live on the pod object itself, so they are shared
// across controller replicas and survive controller restarts.
const (
	AnnotationAllocated    = "ironhive.dev/allocated"
	AnnotationLeaseExpires = "ironhive.dev/lease-expires"
)

// Errors returned by Allocate and Release, wrapped with context.
var (
	ErrUnknownPool         = errors.New("unknown pool")
	ErrNoSandboxAvailable  = errors.New("no sandbox available")
	ErrSandboxNotFound     = errors.New("sandbox not found")
	ErrSandboxNotAllocated = errors.New("sandbox not allocated")
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
	// Allocated reports whether the pod has been handed out to a client.
	Allocated bool
	// LeaseExpires is the deadline of the allocation's lease; zero when
	// the pod is not allocated. Expired pods are destroyed by reconcile.
	LeaseExpires time.Time
	// TemplateHash is the hash of the pool's podTemplate when the pod was
	// created; standby pods with a stale hash are recycled by reconcile.
	TemplateHash string
	// ResourceVersion is the pod's last seen resourceVersion, used as the
	// optimistic-concurrency precondition when claiming the pod.
	ResourceVersion string
	CreatedAt       time.Time
	UpdatedAt       time.Time
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

// Run lists the managed pods, then watches for changes to keep the
// in-memory state current. It runs on every replica — the state feeds the
// allocate fast path. A broken watch is re-established from a fresh list
// with backoff, so state eventually reconverges after any disconnect. It
// returns when ctx is cancelled. Reconcile is NOT part of this loop: it
// runs only on the elected leader (see RunLeaderElection).
func (m *PodManager) Run(ctx context.Context) {
	backoff := time.Second
	for ctx.Err() == nil {
		resourceVersion, err := m.list(ctx)
		if err == nil {
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

// RunReconcile reconciles immediately and then periodically until ctx is
// cancelled. It must run on at most one replica at a time — the leader
// election in RunLeaderElection enforces that, making reconcile the
// single writer of the pool's standby set: no over-creation, and
// template-hash recycling converges without racing.
func (m *PodManager) RunReconcile(ctx context.Context) {
	m.reconcile(ctx)
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
// pods and expired leases so they do not pile up. Failures are logged and
// retried on the next pass.
func (m *PodManager) reconcile(ctx context.Context) {
	// Precompute the current template hash of every configured pool.
	hashes := make(map[string]string, len(m.cfg.Pools))
	for name, pool := range m.cfg.Pools {
		hashes[name] = podTemplateHash(pool.PodTemplate)
	}

	m.mu.RLock()
	active := make(map[string]int, len(m.cfg.Pools))
	var terminated, expired, stale []string
	now := time.Now()
	for _, p := range m.pods {
		// A freshly created pod has an empty phase until the API server
		// fills it in; count anything not terminated as active — except
		// allocated pods, which are in use and not standby capacity.
		switch {
		case p.Phase == corev1.PodSucceeded || p.Phase == corev1.PodFailed:
			terminated = append(terminated, p.Name)
		case p.Allocated:
			// A zero LeaseExpires is far in the past, so allocated pods
			// without a (parseable) lease are reaped too — no dead leases.
			if now.After(p.LeaseExpires) {
				expired = append(expired, p.Name)
			}
		case p.TemplateHash != hashes[p.Pool]:
			// The pool is gone from the config or its podTemplate
			// changed since this pod was created — recycle the standby
			// pod so the pool converges on the current template.
			// Allocated pods are left alone: a client is using them and
			// the lease expiry will reclaim them in time.
			stale = append(stale, p.Name)
		default:
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
	for _, name := range expired {
		if err := m.kube.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			log.Println("pod manager: delete expired-lease pod", name, ":", err)
		} else {
			log.Println("pod manager: deleted pod", name, "with expired lease")
		}
	}
	for _, name := range stale {
		if err := m.kube.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
			log.Println("pod manager: delete stale-template pod", name, ":", err)
		} else {
			log.Println("pod manager: deleted pod", name, "with stale template")
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
	pod.Labels[LabelTemplateHash] = podTemplateHash(pool.PodTemplate)

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
	// An unparseable lease stays zero, i.e. already expired — fail-safe
	// is reaping the pod.
	leaseExpires, _ := time.Parse(time.RFC3339, pod.Annotations[AnnotationLeaseExpires])
	return &PodState{
		Name:            pod.Name,
		Pool:            pod.Labels[LabelPool],
		Namespace:       pod.Namespace,
		IP:              pod.Status.PodIP,
		Phase:           pod.Status.Phase,
		Ready:           ready,
		Allocated:       pod.Annotations[AnnotationAllocated] != "",
		LeaseExpires:    leaseExpires,
		TemplateHash:    pod.Labels[LabelTemplateHash],
		ResourceVersion: pod.ResourceVersion,
		CreatedAt:       pod.CreationTimestamp.Time,
		UpdatedAt:       time.Now(),
	}
}

// Lookup returns the in-memory state of one managed pod.
func (m *PodManager) Lookup(name string) (PodState, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.pods[name]
	if !ok {
		return PodState{}, false
	}
	return *p, true
}

// Allocate claims one Ready standby pod of the pool for a client, leased
// for the given duration — once the lease expires without renewal,
// reconcile destroys the pod. When no candidate is available it keeps
// polling the in-memory state until wait elapses — reconcile tops the pool
// up in the meantime — and then returns ErrNoSandboxAvailable. An unknown
// pool fails immediately.
//
// The claim is a patch carrying the pod's resourceVersion as an
// optimistic-concurrency precondition, so racing controller replicas
// cannot claim the same pod: the API server accepts exactly one of them.
func (m *PodManager) Allocate(ctx context.Context, poolName string, lease, wait time.Duration) (PodState, error) {
	if _, ok := m.cfg.Pools[poolName]; !ok {
		return PodState{}, fmt.Errorf("%w: %q", ErrUnknownPool, poolName)
	}
	deadline := time.Now().Add(wait)
	for {
		for _, st := range m.candidates(poolName) {
			claimed, err := m.claim(ctx, st, lease)
			if err == nil {
				return claimed, nil
			}
			if ctx.Err() != nil {
				return PodState{}, ctx.Err()
			}
			// Lost the race or a transient error — try the next one.
		}
		if time.Now().After(deadline) {
			return PodState{}, fmt.Errorf("pool %q: %w", poolName, ErrNoSandboxAvailable)
		}
		select {
		case <-ctx.Done():
			return PodState{}, ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// candidates lists the pool's claimable pods, sorted by name.
func (m *PodManager) candidates(poolName string) []PodState {
	var out []PodState
	for _, p := range m.Pods() {
		if p.Pool == poolName && !p.Allocated && p.Phase == corev1.PodRunning && p.Ready && p.IP != "" {
			out = append(out, p)
		}
	}
	return out
}

// claim marks one pod allocated on the API server, with a lease expiring
// lease from now, and mirrors the fact into the in-memory state.
func (m *PodManager) claim(ctx context.Context, st PodState, lease time.Duration) (PodState, error) {
	now := time.Now().UTC()
	patch := fmt.Sprintf(`{"metadata":{"resourceVersion":%q,"annotations":{%q:%q,%q:%q}}}`,
		st.ResourceVersion,
		AnnotationAllocated, now.Format(time.RFC3339),
		AnnotationLeaseExpires, now.Add(lease).Format(time.RFC3339))
	pod, err := m.kube.CoreV1().Pods(m.namespace).Patch(ctx, st.Name,
		types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return PodState{}, err
	}
	claimed := podStateFrom(pod)
	m.mu.Lock()
	// The patch only touches metadata, so keep the freshest known runtime
	// fields — the watch overwrites them with the server's truth anyway.
	if prev, ok := m.pods[pod.Name]; ok {
		claimed.IP = prev.IP
		claimed.Phase = prev.Phase
		claimed.Ready = prev.Ready
	}
	m.pods[pod.Name] = claimed
	m.mu.Unlock()
	log.Println("pod manager: allocated pod", pod.Name, "of pool", st.Pool)
	return *claimed, nil
}

// Renew extends the lease of an allocated pod to lease from now and
// returns the refreshed state.
func (m *PodManager) Renew(ctx context.Context, name string, lease time.Duration) (PodState, error) {
	m.mu.RLock()
	st, ok := m.pods[name]
	m.mu.RUnlock()
	if !ok {
		return PodState{}, fmt.Errorf("%w: %q", ErrSandboxNotFound, name)
	}
	if !st.Allocated {
		return PodState{}, fmt.Errorf("%w: %q", ErrSandboxNotAllocated, name)
	}
	// No resourceVersion precondition: renewal only pushes the deadline
	// forward, so last write wins is fine.
	patch := fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`,
		AnnotationLeaseExpires, time.Now().UTC().Add(lease).Format(time.RFC3339))
	pod, err := m.kube.CoreV1().Pods(m.namespace).Patch(ctx, name,
		types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		return PodState{}, fmt.Errorf("patch pod %q: %w", name, err)
	}
	renewed := podStateFrom(pod)
	m.mu.Lock()
	if prev, ok := m.pods[pod.Name]; ok {
		renewed.IP = prev.IP
		renewed.Phase = prev.Phase
		renewed.Ready = prev.Ready
	}
	m.pods[pod.Name] = renewed
	m.mu.Unlock()
	return *renewed, nil
}

// Release destroys an allocated pod. Sandboxes are single-use: the pool is
// topped up with a fresh pod by the next reconcile pass.
func (m *PodManager) Release(ctx context.Context, name string) error {
	m.mu.RLock()
	st, ok := m.pods[name]
	m.mu.RUnlock()
	if !ok {
		return fmt.Errorf("%w: %q", ErrSandboxNotFound, name)
	}
	if !st.Allocated {
		return fmt.Errorf("%w: %q", ErrSandboxNotAllocated, name)
	}
	if err := m.kube.CoreV1().Pods(m.namespace).Delete(ctx, name, metav1.DeleteOptions{}); err != nil {
		return fmt.Errorf("delete pod %q: %w", name, err)
	}
	m.mu.Lock()
	delete(m.pods, name)
	m.mu.Unlock()
	log.Println("pod manager: released pod", name)
	return nil
}

// podTemplateHash returns a deterministic short hash of a pod template.
// encoding/json marshals struct fields in declaration order and sorts map
// keys, so equal templates always produce equal hashes.
func podTemplateHash(tmpl corev1.PodTemplateSpec) string {
	data, _ := json.Marshal(tmpl) // PodTemplateSpec has no failing marshalers
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:8])
}
