# Routing Contract

## Simple routes

`POST /admin/api/simple-routes` and `PUT/DELETE /admin/api/simple-routes/{id}` accept `{id,name,public_model,mode,deployments,enabled?}`. `mode` is `automatic` or `ordered`; at least one deployment is required. The backend compiles automatic routes to Candidate Pool → Route Profile → Virtual Endpoint. Ordered routes additionally compile one pool per deployment and a Fallback Chain. The frontend never selects the winner at request time.

## Snapshot routing data

`GET /admin/api/snapshot` returns deployments, health, health_counts, node_telemetry, provider_health, provider_pressure, scope_health, request_total, version, client_auth, virtual_endpoints, route_profiles, candidate_pools, fallback_chains, config and bounded events. It is the authoritative read model for Overview, Routing, Activity and Advanced surfaces.

## Gateway

The actual data plane remains `/v1/messages`, `/v1/messages/count_tokens`, `/v1/chat/completions`, `/v1/responses` and `GET /v1/models`. Client auth is enforced by backend policy when enabled. Upstream credentials are applied only by provider adapters.

## Routing invariants

Backend eligibility continues to own alias matching, capability filtering, health/circuit state, session affinity, priority/weight, latency/failure evidence, bounded attempts, failover and stream commit semantics.
