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

The supported runners are manual-only **Disposable capacity experiment**
(`capacity-experiment.yml`) and **Disposable chaos experiment**
(`chaos-experiment.yml`). Each creates one temporary k3d cluster per run,
uses only a localhost gateway, and serializes both workflow files with the
same fixed `capacity-chaos-experiment` concurrency group. They are not part of
scheduled public synthetic monitoring.

## Safe targets and hard bounds

The baseline target is a freshly created disposable k3d cluster from
`k8s/overlays/test`. The gateway is bound to `127.0.0.1` on the GitHub-hosted
runner and the load-smoke base URL is the corresponding loopback URL. The
workflow has no public target variable, secret, or production context. Every
non-local target is rejected unless `PONG_EXPERIMENT_MODE` is `capacity` or
`chaos`, `PONG_EXPERIMENT_APPROVED=1`, and
`PONG_EXPERIMENT_TARGET=isolated`. The workflow additionally requires the
manual boolean `approve_experiment=true`; `pong.belacca.com` and documented
native public edge addresses are denied even when markers are present.

Workflow inputs are validated again in Bash after GitHub has supplied them;
workflow metadata is not treated as a security boundary. Invalid, fractional, negative, non-numeric, or out-of-range values fail closed
before cluster creation. Malformed values are rejected, not clamped. The
permitted bounds are:

| Input | Default | Hard bound |
| --- | ---: | ---: |
| `iterations` | 3 | 1-50 |
| `concurrency` | 1 | 1-8 |
| `timeout_ms` | 10000 | 500-30000 |
| `max_duration_ms` | 60000 | 1000-180000 |
| `abort_threshold` | 3 | 1-20 |

The experiment has no GitHub matrix, retry fan-out, or parallel workflow jobs.
Load-smoke has a hard three-minute maximum, an abort threshold, and bounded
cleanup. Cluster/namespace cleanup has a 120-second deadline and must produce
a verification marker or the workflow fails.
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
objective or the drill recovery P95 by itself. If WebSocket failures occur
while node CPU/memory remain low, inspect admission-rejection and WebSocket
failure counters before calling the result resource saturation.

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

## One-fault-at-a-time chaos drills

`chaos-experiment.yml` accepts exactly one scenario and performs three
sequential comparable repetitions after a passing concurrent-room baseline:

1. `api-restart` — restart the disposable API deployment;
2. `gateway-restart` — restart the disposable gateway deployment;
3. `room-termination` — create a disposable room and terminate its room pod;
4. `node-drain` — drain and uncordon one disposable k3d agent; or
5. `resource-pressure` — create and remove one bounded pressure pod.

It emits aggregate recovery durations and P95, failures, cleanup markers, and
resource snapshot availability. A recovery P95 under 360000 ms is marked as
the controlled-drill objective; failed or incomplete runs record
`objective_passed: false`. Faults are never combined or injected in parallel.

This branch cannot provide live-cluster validation, production credentials, or
three executed recovery-drill artifacts. Operator follow-up is required: run
an approved isolated workflow invocation for at least three comparable runs,
retain the aggregate JSON and cleanup markers, and review P95 against six
minutes. Never point these workflows at native production or a public ingress.
