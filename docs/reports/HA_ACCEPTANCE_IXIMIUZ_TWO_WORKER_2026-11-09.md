# Issue #652 — iximiuz two-worker acceptance evidence

**Date:** 2026-11-09 07:49 UTC  
**Revision:** `83a3330467bee986665d1a20c11ac4583fe621e2`  
**Image:** `registry.iximiuz.com/nexaroute:ha652-83a33304`  
**Image digest:** `sha256:48c9cf18489e71bbbb60768980a615ecf687f22c245bf84f1c21fb52948accbf`

## Environment

The disposable iximiuz Kubernetes Cluster Playground provided a kubeadm cluster with one control-plane node and two workers:

- `cplane-01` — control-plane
- `node-01` — worker
- `node-02` — worker

All three nodes were `Ready` on Kubernetes `v1.37.1` with containerd. The NexaRoute deployment was constrained to `node-01` and `node-02` using required node affinity and required hostname anti-affinity.

PostgreSQL ran on `node-01` and Redis ran on `node-02`. Both were disposable acceptance workloads using `emptyDir` volumes.

## Baseline result

The deployment reached two ready replicas on distinct workers:

```text
nexaroute-nexaroute-fd76d6d97-4tvvc   2/2   Running   node-02
nexaroute-nexaroute-fd76d6d97-97xlz   2/2   Running   node-01
```

Deployment status at capture time:

```text
desired=2 ready=2 available=2 updated=2
```

Health checks returned:

```json
{"deployments":1,"ok":true,"usable":1}
{"ok":true}
```

The first response is `/readyz`; the second is `/healthz`.

## Replica recovery test

One gateway Pod on `node-02` was deleted while the service was being checked through the Kubernetes Service.

During recovery, the remaining gateway continued to serve successfully:

```text
ready=1 available=1
/readyz: {"deployments":1,"ok":true,"usable":1}
```

Kubernetes recreated the deleted replica on `node-02`. The final state returned to:

```text
ready=2 available=2
```

The final Pod placement again covered both workers, and the final `/readyz` and `/healthz` checks returned successful responses.

## Result

**PASS — disposable Kubernetes two-worker replica placement and single-replica recovery.**

This evidence supports the following claims only:

1. Two NexaRoute replicas can be scheduled on distinct worker nodes.
2. The image can be pulled from the iximiuz private registry by both workers.
3. The Service remains usable while one gateway replica is being recreated.
4. Kubernetes restores the desired replica count on the affected worker.

## Explicit limitations

This is not production HA certification. The environment is ephemeral. PostgreSQL and Redis used `emptyDir`, so their data did not have durable storage. RPO/RTO were not measured. Backup/restore was not executed. Redis failover was not tested. The build reports an `M4-2` warning for the PostgreSQL security-store adapter, so admin authentication should not be treated as production-ready evidence. No claim is made about independent physical failure domains beyond the two worker VMs supplied by the playground.
