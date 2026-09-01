# AGENTS.md

Guidance for AI agents (and humans) working in this repository.

## What this is

`ironhive` runs warm pools of sandbox containers on Kubernetes:

- `ironhive-controller` (`cmd/ironhive-controller/`, `controller/`) — keeps standby pods warm per configured pool, hands them out via `POST /controller/v1/allocate`, destroys them on `POST /controller/v1/release`, and reverse-proxies `ANY /agent/**` into the allocated pod.
- `ironhive-agent` (`cmd/ironhive-agent/`, `agent/`) — runs as PID 1 inside every sandbox pod (reaps zombies itself, no tini) and exposes file / tar / dir / shell HTTP endpoints. bash is a hard dependency (shell wrapper syntax and caller commands assume it): the agent probes it at boot and refuses to start without a working one on its own `PATH`.
- `client` (repo root, package `ironhive` — `client.go`, `sandbox.go`) — Go client for the controller API and, through the controller's proxy, the agent API; std lib only, errors decode from the `{"message": ...}` envelope. Entry point: `ironhive.NewClient`.

## Build, test, verify

```bash
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l . # must be empty; run gofmt -w on touched files
```

Run all of the above before declaring a change done. Do not commit or push unless the user asks.

## Conventions that matter

### HTTP API shape — the agent is the reference

The agent's API conventions (`agent/files.go`, `README.md` → *ironhive-agent → API*) were polished deliberately; **new endpoints, controller included, must follow them**:

- Non-data responses (successes and errors alike) use the JSON envelope `{"message": ...}` — never `{"error": ...}`, never plain text. Data responses return their payload directly (e.g. `{"sandbox": "..."}`, a JSON array, a file stream).
- **PUT** endpoints take parameters only in the query string — the body is the data stream.
- **POST** endpoints accept parameters in the query string, the urlencoded form body, or both (`r.ParseForm()`; body entries win on conflicts). No JSON request bodies anywhere.
- Errors carry a fitting status code; `502` when an upstream (agent, fetch URL) misbehaves.

### Kubernetes state model (controller)

- Managed pods are named `sandbox-<lowercase ULID>` and carry enforced labels `app.kubernetes.io/managed-by=ironhive-controller` (list/watch selector), `ironhive.dev/pool=<pool>`, and `ironhive.dev/template-hash` (deterministic hash of the pool's `podTemplate`; standby pods with a stale hash are recycled by reconcile, allocated ones are left to their lease). Controller-owned allocation annotations are stripped from new pod templates.
- **The pod object is the source of truth.** Allocation is the `ironhive.dev/allocated` annotation, claimed with a merge patch carrying the pod's `resourceVersion` as an optimistic-concurrency precondition — claims stay correct on every replica, no election needed on the allocate path. Leases live there too: `ironhive.dev/lease-expires` (RFC3339), set at allocate time, extended by `POST /controller/v1/renew`, reaped by reconcile. Renew rejects terminating pods and already-expired leases as not found — renewing an expired pod could otherwise win the resourceVersion race against reconcile's sweep.
- **Reconcile is single-writer via leader election** (`controller/leader.go`, a `coordination.k8s.io` Lease named `ironhive-controller`): only a replica with an initially synced cache can lead, and every leader pass starts from an authoritative List before exact sizing, sweeps, and template-hash recycling. A replica that loses the lease re-enters the election with backoff instead of going silent. Standby pods still not Ready past a fixed timeout (10 min, e.g. ImagePullBackOff) are swept so top-up replaces them — they would otherwise deadlock the pool without counting as a shortage. Deletes carry the classified pod's `resourceVersion`, so concurrent allocate/renew wins. The watch loop and allocate/renew/release run on all replicas.
- In-memory state (`PodManager.pods`) is a watch-fed cache for fast reads; it must always be able to reconverge from a fresh list. Terminating and stale-template pods are never allocation candidates.
- Sandboxes are single-use: release or lease expiry means delete the pod; reconcile tops the pool up. Do not return used pods to the standby pool.
- `GET /controller/v1/pools` is read-only, unauthenticated, and CORS-open on purpose — embedding into third-party systems is a feature; access control is layered in front at deployment time.
- Everything is best-effort with log-and-retry: failed creates/deletes are retried on the next reconcile pass; a missing cluster disables the pod manager but never the API. Zero configured pools still runs the pod manager — reconcile sweeps whatever managed pods an earlier configuration left behind.

### Go style

- std `net/http` only; Go 1.22+ method+path mux patterns. No web framework, no router dependency.
- One `_test.go` per source file, same package. Kubernetes code is tested with `k8s.io/client-go/kubernetes/fake`; HTTP handlers with `httptest`. The `agent/` package is the style benchmark.
- Both binaries take exactly one flag — `-config` — pointing at a YAML config file; every other setting lives in the file (controller: `http:` / `kubernetes:` / `pools:` sections; agent: `http.listen`, `allowed_envs`). Absent file = defaults, present-but-invalid = startup failure. No environment variables: flag plus default is enough, and the agent's process environment is visible to sandboxed commands (it is their PID 1).
- Graceful shutdown: `signal.NotifyContext` for SIGINT/SIGTERM, second signal kills, `srv.Shutdown` with no deadline.
- No HTTP duration or concurrency limits anywhere: the `http.Server`s run with zero timeouts (SSE streams and tar/file transfers last the request's whole lifetime), handlers spawn no semaphores, the Go client sets no `Timeout`, and the Kubernetes client's QPS/burst is raised (200/400) so client-go's rate limiter cannot throttle allocate/renew/release under churn. Upstream hits its own limits long before we would — we must never be the bottleneck.
- Comments explain *why*, matching the density of the surrounding file.

### Git

- Commit messages: `<component>: <what changed>`, component ∈ {`agent`, `controller`, ...} — see `git log` for examples.

## Deploy references

- `config.example.yml` — annotated example pool config (`standby.static.count`, `podTemplate`; agent port derived from `podTemplate` container ports — `http-ironhive` wins, else the first, else 19173).
- `agent.example.yml` — annotated example agent config (image-provided `/opt/ironhive/etc/agent.yml`: `http.listen`; `allowed_envs` wildcard patterns fully replace the built-in shell env allowlist, absent file/field falls back to defaults).
- `manifest.yml` — full demo deployment in namespace `ironhive`: namespaced Role for the controller (pods get/list/watch/create/update/patch/delete, coordination.k8s.io leases, and events), ConfigMap with the controller config, 3-replica Deployment, Service.
- `Dockerfile.controller` / `Dockerfile.agent` — multi-stage builds; images published as `ghcr.io/yankeguo/ironhive:{controller,agent}-*` and `quay.io/yankeguo/ironhive:{controller,agent}-*`.

## Scope discipline

Minimal diffs: no drive-by refactors, no new dependencies without checking `go.mod` first, no speculative configuration knobs. If a change alters anything documented here or in `README.md`, update the docs in the same change.
