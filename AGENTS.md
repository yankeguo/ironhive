# AGENTS.md

Guidance for AI agents (and humans) working in this repository.

## What this is

`ironhive` runs warm pools of sandbox containers on Kubernetes:

- `ironhive-controller` (`cmd/ironhive-controller/`, `controller/`) — keeps standby pods warm per configured pool, hands them out via `POST /controller/v1/allocate`, destroys them on `POST /controller/v1/release`, and reverse-proxies `ANY /agent/**` into the allocated pod. Also serves a small web UI.
- `ironhive-agent` (`cmd/ironhive-agent/`, `agent/`) — runs as PID 1 inside every sandbox pod (reaps zombies itself, no tini) and exposes file / tar / dir / shell HTTP endpoints.
- `client` (repo root, package `ironhive` — `client.go`, `sandbox.go`) — Go client for the controller API and, through the controller's proxy, the agent API; std lib only, errors decode from the `{"message": ...}` envelope. Entry point: `ironhive.NewClient`.

## Build, test, verify

```bash
(cd controller/web && bun install --frozen-lockfile && bun run typecheck && bun run build) # required first: controller embeds web/dist
go build ./...
go vet ./...
go test ./...
go test -race ./...
gofmt -l . # must be empty; run gofmt -w on touched files
```

`controller/web/dist` is git-ignored — always run the bun build before `go build` or the embed fails. Run all of the above before declaring a change done. Do not commit or push unless the user asks.

## Conventions that matter

### HTTP API shape — the agent is the reference

The agent's API conventions (`agent/files.go`, `README.md` → *ironhive-agent → API*) were polished deliberately; **new endpoints, controller included, must follow them**:

- Non-data responses (successes and errors alike) use the JSON envelope `{"message": ...}` — never `{"error": ...}`, never plain text. Data responses return their payload directly (e.g. `{"sandbox": "..."}`, a JSON array, a file stream).
- **PUT** endpoints take parameters only in the query string — the body is the data stream.
- **POST** endpoints accept parameters in the query string, the urlencoded form body, or both (`r.ParseForm()`; body entries win on conflicts). No JSON request bodies anywhere.
- Errors carry a fitting status code; `502` when an upstream (agent, fetch URL) misbehaves.

### Kubernetes state model (controller)

- Managed pods are named `sandbox-<lowercase ULID>` and carry enforced labels `app.kubernetes.io/managed-by=ironhive-controller` (list/watch selector), `ironhive.dev/pool=<pool>`, and `ironhive.dev/template-hash` (deterministic hash of the pool's `podTemplate`; standby pods with a stale hash are recycled by reconcile, allocated ones are left to their lease). Controller-owned allocation annotations are stripped from new pod templates.
- **The pod object is the source of truth.** Allocation is the `ironhive.dev/allocated` annotation, claimed with a merge patch carrying the pod's `resourceVersion` as an optimistic-concurrency precondition — claims stay correct on every replica, no election needed on the allocate path. Leases live there too: `ironhive.dev/lease-expires` (RFC3339), set at allocate time, extended by `POST /controller/v1/renew`, reaped by reconcile.
- **Reconcile is single-writer via leader election** (`controller/leader.go`, a `coordination.k8s.io` Lease named `ironhive-controller`): only a replica with an initially synced cache can lead, and only the leader runs exact sizing, sweeps, and template-hash recycling. Deletes carry the classified pod's `resourceVersion`, so concurrent allocate/renew wins. The watch loop and allocate/renew/release run on all replicas.
- In-memory state (`PodManager.pods`) is a watch-fed cache for fast reads; it must always be able to reconverge from a fresh list. Terminating and stale-template pods are never allocation candidates.
- Sandboxes are single-use: release or lease expiry means delete the pod; reconcile tops the pool up. Do not return used pods to the standby pool.
- The dashboard and `GET /controller/v1/pools` are read-only, unauthenticated, and frameable on purpose (no `X-Frame-Options`, no CSP `frame-ancestors`) — embedding into third-party systems is a feature; access control is layered in front at deployment time.
- Everything is best-effort with log-and-retry: failed creates/deletes are retried on the next reconcile pass; missing config or missing cluster disables the pod manager but never the web UI.

### Go style

- std `net/http` only; Go 1.22+ method+path mux patterns. No web framework, no router dependency.
- One `_test.go` per source file, same package. Kubernetes code is tested with `k8s.io/client-go/kubernetes/fake`; HTTP handlers with `httptest`. The `agent/` package is the style benchmark.
- The controller takes exactly one flag/env pair — `-config` / `IHC_CONFIG`; every other setting (`http.listen`, `kubernetes.kubeconfig`, `kubernetes.namespace`, pools) lives in the config file, grouped in `http:` / `kubernetes:` / `pools:` sections. The agent takes command-line flags only — no env vars by design.
- Graceful shutdown: `signal.NotifyContext` for SIGINT/SIGTERM, second signal kills, `srv.Shutdown` with no deadline.
- Comments explain *why*, matching the density of the surrounding file.

### Git

- Commit messages: `<component>: <what changed>`, component ∈ {`agent`, `controller`, ...} — see `git log` for examples.

### Frontend (controller/web)

- Bun + Tailwind v4; every file in `web/src/entries/` becomes a hashed bundle; Go templates reference entries only via `{{cssAsset "main"}}` / `{{jsAsset "home"}}`.
- In Docker, the bun stage must mirror the repo layout (`WORKDIR /repo/controller/web`, Go files copied next to it) — `main.css`'s Tailwind `@source "../../../*.go"` hangs the build otherwise (see README → Release).

## Deploy references

- `config.yml` — annotated example pool config (`standby.static.count`, `podTemplate`, `agent.port`).
- `deploy/rbac.yaml` — namespaced Role for the controller (pods get/list/watch/create/update/patch/delete, coordination.k8s.io leases, and events in `ironhive`).
- `Dockerfile.controller` / `Dockerfile.agent` — multi-stage builds; images published as `ghcr.io/yankeguo/ironhive:{controller,agent}-*`.

## Scope discipline

Minimal diffs: no drive-by refactors, no new dependencies without checking `go.mod` first, no speculative configuration knobs. If a change alters anything documented here or in `README.md`, update the docs in the same change.
