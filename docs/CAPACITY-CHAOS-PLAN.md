# Capacity and chaos plan

## Purpose and reliability objectives

This plan separates two different questions:

- **Availability objective:** 99% availability for each public service over a
  rolling 30-day period. The scheduled synthetic check is the low-rate signal
  for this objective; it is not a capacity test and does not inject faults.
- **Controlled-drill objective:** P95 recovery under six minutes for an
  approved, isolated failure drill. This recovery objective is separate from
  the 99%/30-day availability objective and must not be inferred from a
  disposable load run. In shorthand, this is **99%/30d availability versus
  separate P95-under-6m drill recovery**.

The next safe slice is the manual-only **Disposable capacity experiment**
workflow (`capacity-experiment.yml`). It creates one temporary k3d cluster per
run, uses only a localhost gateway, and serializes runs with the fixed
`capacity-experiment` concurrency group.

## Safe targets and hard bounds

The baseline target is a freshly created disposable k3d cluster from
`k8s/overlays/test`. The gateway is bound to `127.0.0.1` on the GitHub-hosted
runner and the load-smoke base URL is the corresponding loopback URL. The
workflow has no public target variable, secret, production context, or
approval marker. The existing load-smoke guard remains active: loopback is
allowed, while non-local targets require an explicit approval marker that this
workflow never supplies.

Workflow inputs are validated again in Bash after GitHub has supplied them;
workflow metadata is not treated as a security boundary. Invalid, fractional,
negative, non-numeric, or out-of-range values fail closed before cluster
creation. The permitted bounds are:

| Input | Default | Hard bound |
| --- | ---: | ---: |
| `iterations` | 3 | 1-50 |
| `concurrency` | 1 | 1-8 |
| `timeout_ms` | 10000 | 500-30000 |
| `max_duration_ms` | 60000 | 1000-180000 |

The experiment has no GitHub matrix, retry fan-out, or parallel workflow jobs;
the operator may run the same serialized workflow sequentially at the documented
concurrency points.
The run-ID-derived cluster name is deliberately short enough for k3d's 32-character
limit while remaining unique to the run. It builds and imports the four local images (`api`, `room`, `static`, and
`gateway`) sequentially. The cluster name is exactly derived from the run ID;
cleanup runs even when a later step fails and names only that exact cluster.
An ownership marker prevents cleanup from deleting a cluster if creation did
not succeed.

## Load-smoke semantics

The existing dependency-light `scripts/load-smoke.sh` journey is a bounded
smoke/load harness, not a production rate limiter. Each iteration checks:

1. health;
2. room creation;
3. room join and connection contract;
4. two WebSocket players receiving joined/state traffic; and
5. room cleanup after the sockets close.

It caps iterations, concurrent workers, per-operation timeout, and total
runtime. Its normal output is aggregate operation counts, failure codes, and
latency percentiles; it does not print room IDs, player names, URLs, client
addresses, tokens, or response bodies. The workflow records that aggregate
JSON and the process exit code. A failed journey remains a failed workflow even
though evidence collection and cleanup still run.

This baseline is intentionally not browser traffic. Browser/Playwright process
fan-out is **not the first baseline** and must stay one worker on isolated
targets. Browser tests can be added later for a separately justified user
experience question, but they must not turn the capacity workflow into a
public or unbounded browser load generator.

## Capacity model and bounded overload policy

The reviewable configured model is [`capacity-policy.json`](../capacity-policy.json).
It is intentionally separate from benchmark output: the policy records current
limits and the calculation for safe headroom, while a run artifact records what
actually happened in an isolated environment. The current topology has one API
replica and one SQLite writer, a global 128-session WebSocket admission limit,
and a 120-pod/120-service namespace quota. Room pods reserve 250m CPU without
a CPU limit, so the 12-core namespace CPU request quota creates a configured
dynamic room ceiling of 47 after fixed workload requests; this is lower than
the Pod/Service quota. Until a measured result is lower, the review threshold
is 80% of each boundary: 102 WebSocket sessions, 37 two-player games when
there are no spectators, and 37 active rooms. These are guardrails,
not a production capacity claim.

The first overload signal is expected to be an aggregate admission rejection,
not a crash: HTTP admission, create, join, and WebSocket rejection counters
increase; public HTTP admission returns `429 Too Many Requests`, `Retry-After:
60`, and `Cache-Control: no-store`. The benchmark does not retry 429 responses.
The lobby's room-backend dial is the only internal retrying layer and is capped
at three attempts with 100ms/200ms backoff. This prevents a rejected request from
becoming a retry storm while preserving health, room listing, existing-room
cleanup, and already-admitted WebSocket journeys as critical work.

The current capacity boundary remains the minimum of measured CPU/memory,
SQLite failure/latency, room Pod/Service quota, gateway/WebSocket saturation,
and the application admission ceilings. Never add a second `pong-api` replica
to raise it: the RWO SQLite file has a deliberate single-writer contract.
Future options are a serialized durable writer with read replicas, a
transactional state service, or room-partitioned writable shards; none is
implemented here.

## Repeatable benchmark procedure

Run the manual `capacity-experiment` workflow at `concurrency=1,2,4,8` with
three or more iterations per point and the same timeout/duration. Repeat the
matrix after changing images or resource limits. For explicit overload
validation, rerun with `overload_ws_limit` below the requested concurrent
sessions (the default 128 preserves the current topology baseline). The workflow
always creates a new loopback-only k3d cluster, applies the test overlay, and
deletes only its owned cluster.

Each aggregate result contains health/create/join/WebSocket/API-read/cleanup
counts, latency percentiles, HTTP status classes, `Retry-After` counts,
WebSocket handshake statuses, and failure codes. Interpret the first signal as
follows:

1. admission rejection with low CPU/memory: configured ceiling is the boundary;
2. quota rejection or pending room Pods: Pod/Service/CPU-request quota is the
   boundary;
3. room CPU pressure or WebSocket write timeouts with node PSI: room scheduling
   or node contention is the boundary;
4. SQLite failures or rising create/join latency: the single writer/storage is
   the boundary;
5. resource pressure with no admission rejection: CPU/memory or gateway is the
   boundary; and
6. cleanup failures or rooms remaining after the run: the result is invalid
   until reconciliation succeeds.

Record the lowest stable concurrency/ceiling pair that has no unacceptable
journey errors, then keep 20% headroom. Do not average across runs that used
different images, resource requests, node sizes, admission settings, or quota.
The workflow is evidence collection, not an automatic capacity promotion.

## Evidence and review

Every manual run uploads a short-lived artifact containing:

- the aggregate load-smoke JSON result;
- load-smoke stderr and exit code;
- the private aggregate Pong `/metrics` exposition, including admission and
  WebSocket failure counters without labels; and
- bounded, redacted `kubectl get` pod/deployment/service output plus
  `kubectl top` pod and node snapshots. Disposable room names and IP-like
  values are removed before upload; aggregate status/counts are retained.

A missing metrics server may make a `kubectl top` snapshot unavailable; that
condition is captured in the artifact rather than converted into an unbounded
wait. Reviewers should correlate the aggregate operation results with pod
readiness, resource requests/limits, and any snapshot errors. This evidence is
for a disposable baseline and cannot establish the public 30-day availability
objective or the drill recovery P95 by itself. If WebSocket failures occur while node CPU/memory remain low, inspect admission-rejection and WebSocket failure counters before calling the result resource saturation. A successful run must also verify that the room list returns to its pre-run state and that no room Pod/Service remains after bounded cleanup/reconciliation.

## Current non-goals

This slice does **not**:

- send load to a public URL or any non-local target;
- exercise a production cluster, production context, PVC, ingress, or public
  route;
- inject chaos, delete production resources, or restart production workloads;
- add secrets, public URL variables, registry credentials, or write
  permissions;
- publish a capacity claim from one short manual run; or
- create a matrix, retry fan-out, or browser/process swarm.

The scheduled synthetic workflow remains a separate, low-rate availability
check. It must not be repurposed as a capacity or chaos runner.

## Future one-fault-at-a-time drills

After an isolated baseline has been reviewed, future drills should introduce
exactly one reversible fault at a time, with an explicit hypothesis, bounded
window, pre-check, recovery measurement, and post-check. Candidate scenarios
on disposable targets include:

1. restart one non-stateful gateway pod and measure readiness and journey
   recovery;
2. restart the single API pod only after confirming the SQLite/PVC safety
   contract and measuring the resulting bounded outage;
3. make one room pod unavailable and verify room cleanup/reconciliation; or
4. apply a narrowly scoped disposable resource-pressure or network fault,
   then remove it and verify the cluster returns to baseline.

Each scenario needs its own reviewed guard and must never combine faults,
parallelize failure injection, or target native production. A later recovery
workflow can calculate P95 across repeated, separately approved drills; it
must not mix that number with the 99%/30-day availability measurement.
