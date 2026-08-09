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

The experiment has no GitHub matrix, retry fan-out, or parallel workflow jobs.
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
- load-smoke stderr and exit code; and
- bounded `kubectl get` pod/deployment/service output plus `kubectl top` pod
  and node snapshots.

A missing metrics server may make a `kubectl top` snapshot unavailable; that
condition is captured in the artifact rather than converted into an unbounded
wait. Reviewers should correlate the aggregate operation results with pod
readiness, resource requests/limits, and any snapshot errors. This evidence is
for a disposable baseline and cannot establish the public 30-day availability
objective or the drill recovery P95 by itself.

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
