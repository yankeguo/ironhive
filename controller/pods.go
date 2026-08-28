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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
	// Deleting reports whether Kubernetes has accepted deletion of the pod.
	Deleting bool
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

	initialList     chan struct{}
	initialListOnce sync.Once
}

func NewPodManager(kube kubernetes.Interface, namespace string, cfg *Config) *PodManager {
	return &PodManager{
		kube:        kube,
		namespace:   namespace,
		cfg:         cfg,
		pods:        map[string]*PodState{},
		initialList: make(chan struct{}),
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
	m.initialListOnce.Do(func() { close(m.initialList) })
	return list.ResourceVersion, nil
}

// waitForInitialList keeps a replica out of leader election until its cache
// has a trustworthy baseline. Reconciling an empty startup cache would
// duplicate every existing standby pod.
func (m *PodManager) waitForInitialList(ctx context.Context) bool {
	select {
	case <-ctx.Done():
		return false
	case <-m.initialList:
		return true
	}
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
			if ev.Type == watch.Error {
				return fmt.Errorf("watch pods: %w", apierrors.FromObject(ev.Object))
			}
			m.applyEvent(ev)
		}
	}
}

func (m *PodManager) applyEvent(ev watch.Event) {
	pod, ok := ev.Object.(*corev1.Pod)
	if !ok {
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

type sweepTarget struct {
	name            string
	resourceVersion string
	reason          string
}

func targetFor(p *PodState, reason string) sweepTarget {
	return sweepTarget{name: p.Name, resourceVersion: p.ResourceVersion, reason: reason}
}

func readyStandby(p PodState) bool {
	return p.Phase == corev1.PodRunning && p.Ready && p.IP != ""
}

// reconcile makes each pool converge on its exact standby count and sweeps
// terminated pods, expired leases and stale templates. Failures are logged
// and retried on the next pass.
func (m *PodManager) reconcile(ctx context.Context) {
	// Precompute the current template hash of every configured pool.
	hashes := make(map[string]string, len(m.cfg.Pools))
	for name, pool := range m.cfg.Pools {
		hashes[name] = podTemplateHash(pool.PodTemplate)
	}

	m.mu.RLock()
	standby := make(map[string][]PodState, len(m.cfg.Pools))
	var sweeps []sweepTarget
	now := time.Now()
	for _, p := range m.pods {
		// A freshly created pod has an empty phase until the API server
		// fills it in; count anything not terminated as active — except
		// allocated pods, which are in use and not standby capacity.
		switch {
		case p.Deleting:
			// Kubernetes has accepted deletion already. Do not allocate,
			// sweep again or count the pod toward desired standby.
		case p.Phase == corev1.PodSucceeded || p.Phase == corev1.PodFailed:
			sweeps = append(sweeps, targetFor(p, "terminated"))
		case p.Allocated:
			// A zero LeaseExpires is far in the past, so allocated pods
			// without a (parseable) lease are reaped too — no dead leases.
			if now.After(p.LeaseExpires) {
				sweeps = append(sweeps, targetFor(p, "expired-lease"))
			}
		default:
			hash, configured := hashes[p.Pool]
			if !configured || p.TemplateHash != hash {
				// The pool is gone from the config or its podTemplate
				// changed since this pod was created — recycle the standby
				// pod so the pool converges on the current template.
				// Allocated pods are left alone: a client is using them and
				// the lease expiry will reclaim them in time.
				sweeps = append(sweeps, targetFor(p, "stale-template"))
				continue
			}
			standby[p.Pool] = append(standby[p.Pool], *p)
		}
	}
	m.mu.RUnlock()

	// A static count is a target, not a floor. Prefer deleting pods that
	// cannot be allocated yet, then the newest names, preserving the
	// maximum amount of already-warm capacity.
	for name, pods := range standby {
		desired := m.cfg.Pools[name].Standby.Static.Count
		if desired < 0 {
			desired = 0
		}
		surplus := len(pods) - desired
		if surplus <= 0 {
			continue
		}
		sort.Slice(pods, func(i, j int) bool {
			iReady, jReady := readyStandby(pods[i]), readyStandby(pods[j])
			if iReady != jReady {
				return !iReady
			}
			return pods[i].Name > pods[j].Name
		})
		for _, p := range pods[:surplus] {
			p := p
			sweeps = append(sweeps, targetFor(&p, "surplus-standby"))
		}
	}
	sort.Slice(sweeps, func(i, j int) bool {
		if sweeps[i].reason != sweeps[j].reason {
			return sweeps[i].reason < sweeps[j].reason
		}
		return sweeps[i].name < sweeps[j].name
	})

	for _, target := range sweeps {
		opts := metav1.DeleteOptions{}
		if target.resourceVersion != "" {
			rv := target.resourceVersion
			opts.Preconditions = &metav1.Preconditions{ResourceVersion: &rv}
		}
		err := m.kube.CoreV1().Pods(m.namespace).Delete(ctx, target.name, opts)
		switch {
		case err == nil:
			m.forgetPod(target.name, target.resourceVersion)
			log.Println("pod manager: deleted", target.reason, "pod", target.name)
		case apierrors.IsNotFound(err):
			m.forgetPod(target.name, "")
		default:
			log.Println("pod manager: delete", target.reason, "pod", target.name, ":", err)
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
		for i := len(standby[name]); i < pool.Standby.Static.Count; i++ {
			if err := m.createPod(ctx, name, pool); err != nil {
				log.Println("pod manager: create pod for pool", name, ":", err)
				break
			}
		}
	}
}

// forgetPod removes the classified cache entry after a successful API
// deletion. A newer watch event is kept; it carries Deleting and is already
// excluded from candidates and pool sizing.
func (m *PodManager) forgetPod(name, resourceVersion string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p, ok := m.pods[name]; ok && (resourceVersion == "" || p.ResourceVersion == resourceVersion) {
		delete(m.pods, name)
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
	// Allocation annotations are controller-owned state, never template
	// defaults. Keeping either can create a phantom allocated sandbox.
	delete(pod.Annotations, AnnotationAllocated)
	delete(pod.Annotations, AnnotationLeaseExpires)

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
		Deleting:        pod.DeletionTimestamp != nil,
		Allocated:       pod.Annotations[AnnotationAllocated] != "",
		LeaseExpires:    leaseExpires,
		TemplateHash:    pod.Labels[LabelTemplateHash],
		ResourceVersion: pod.ResourceVersion,
		CreatedAt:       pod.CreationTimestamp.Time,
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
			// Lost the race (conflict) or the pod vanished underneath us —
			// try the next candidate. Anything else means the API itself is
			// failing; report it instead of burning the whole wait window
			// and masking it as ErrNoSandboxAvailable.
			if !apierrors.IsConflict(err) && !errors.Is(err, ErrSandboxNotFound) {
				return PodState{}, err
			}
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
	wantHash := podTemplateHash(m.cfg.Pools[poolName].PodTemplate)
	var out []PodState
	for _, p := range m.Pods() {
		if p.Pool == poolName && p.TemplateHash == wantHash && !p.Deleting && !p.Allocated &&
			p.Phase == corev1.PodRunning && p.Ready && p.IP != "" {
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
	claimed, err := m.patchPod(ctx, st.Name, patch)
	if err != nil {
		return PodState{}, err
	}
	log.Println("pod manager: allocated pod", claimed.Name, "of pool", st.Pool)
	return claimed, nil
}

// patchPod applies a metadata merge patch to one pod and mirrors the
// result into the in-memory state. The patch only touches metadata, so the
// freshest known runtime fields are kept — the watch overwrites them with
// the server's truth anyway. A pod deleted concurrently surfaces as
// ErrSandboxNotFound.
func (m *PodManager) patchPod(ctx context.Context, name, patch string) (PodState, error) {
	pod, err := m.kube.CoreV1().Pods(m.namespace).Patch(ctx, name,
		types.MergePatchType, []byte(patch), metav1.PatchOptions{})
	if err != nil {
		if apierrors.IsNotFound(err) {
			return PodState{}, fmt.Errorf("%w: %q", ErrSandboxNotFound, name)
		}
		return PodState{}, fmt.Errorf("patch pod %q: %w", name, err)
	}
	next := podStateFrom(pod)
	m.mu.Lock()
	if prev, ok := m.pods[pod.Name]; ok {
		next.IP = prev.IP
		next.Phase = prev.Phase
		next.Ready = prev.Ready
	}
	m.pods[pod.Name] = next
	m.mu.Unlock()
	return *next, nil
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
	return m.patchPod(ctx, name, patch)
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
		if apierrors.IsNotFound(err) {
			// Deleted concurrently — the outcome the caller wanted.
			m.mu.Lock()
			delete(m.pods, name)
			m.mu.Unlock()
			return fmt.Errorf("%w: %q", ErrSandboxNotFound, name)
		}
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
