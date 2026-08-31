package controller

import (
	"context"
	"fmt"
	"log"
	"os"
	"time"

	"github.com/oklog/ulid/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/scheme"
	corev1client "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
)

// leaderLeaseName is the coordination.k8s.io Lease the replicas elect
// their reconcile leader through.
const leaderLeaseName = "ironhive-controller"

// Leader election timings; variables so tests can shrink them.
var (
	leaderLeaseDuration = 15 * time.Second
	leaderRenewDeadline = 10 * time.Second
	leaderRetryPeriod   = 2 * time.Second
)

// RunLeaderElection elects a single leader among the controller replicas
// via a coordination.k8s.io Lease in the managed namespace, and runs the
// reconcile loop only while this replica holds the lease — reconcile is
// the pool's single writer, so standby top-up and template recycling
// never race between replicas. The watch loop (Run) and the allocate /
// renew / release fast paths stay multi-replica; claims are already
// serialized by the API server through resourceVersion preconditions.
//
// It returns when ctx is cancelled, releasing the lease for a fast
// failover. Losing leadership only ends one election term — the loop
// below rejoins so this replica can lead again.
func (m *PodManager) RunLeaderElection(ctx context.Context) {
	// Joining the election before the first list can make a restarting
	// controller reconcile an empty cache and duplicate the whole pool.
	if !m.waitForInitialList(ctx) {
		return
	}

	hostname, err := os.Hostname()
	if err != nil {
		hostname = "unknown"
	}
	// The ULID suffix keeps identities distinct even off-cluster, where
	// several replicas may share one hostname.
	id := fmt.Sprintf("%s_%s", hostname, ulid.Make())

	// Leader changes are recorded as Events on the Lease object —
	// kubectl describe lease ironhive-controller shows the history.
	broadcaster := record.NewBroadcaster()
	defer broadcaster.Shutdown()
	broadcaster.StartRecordingToSink(&corev1client.EventSinkImpl{
		Interface: m.kube.CoreV1().Events(m.namespace),
	})
	recorder := broadcaster.NewRecorder(scheme.Scheme, corev1.EventSource{
		Component: "ironhive-controller",
		Host:      hostname,
	})

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      leaderLeaseName,
			Namespace: m.namespace,
		},
		Client: m.kube.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		},
	}

	// RunOrDie returns for good once renewal has failed past
	// RenewDeadline — leadership loss ends the whole election, not just
	// the term. Rejoin so this replica reconciles again later; that is
	// safe because every reconcile pass starts from an authoritative
	// List and the initial-list gate is still satisfied by the running
	// watch.
	for ctx.Err() == nil {
		leaderelection.RunOrDie(ctx, leaderelection.LeaderElectionConfig{
			Lock:          lock,
			LeaseDuration: leaderLeaseDuration,
			RenewDeadline: leaderRenewDeadline,
			RetryPeriod:   leaderRetryPeriod,
			// Give the lease up on graceful shutdown so the next leader
			// starts reconciling immediately instead of after expiry.
			ReleaseOnCancel: true,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(ctx context.Context) {
					log.Println("pod manager: leading as", id)
					m.RunReconcile(ctx)
				},
				OnStoppedLeading: func() {
					log.Println("pod manager:", id, "lost leadership")
				},
				OnNewLeader: func(identity string) {
					if identity != id {
						log.Println("pod manager: leader is", identity)
					}
				},
			},
		})
	}
}
