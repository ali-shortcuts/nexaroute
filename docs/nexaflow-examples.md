# NexaFlow Examples (Phase 8)

All examples are definition JSON. Save one through
`POST /admin/api/nexaflow/definitions`, publish it, then execute
it with `POST /admin/api/nexaflow/definitions/{id}/runs`. Every
example uses only control nodes except where noted, so each runs
without providers, credentials, or billable calls — except the
LLM example, which needs a resolving selector and a client-auth
token (see the user guide).

## 1. Linear: shape the input and mark it done

Manual trigger → transform. The transform reads the workflow
input's `value` object and adds `done: true`.

```json
{
  "schema_version": "v1",
  "id": "wf-linear",
  "name": "Linear",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "t", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {
       "value": {"$ref": "input.value"},
       "operations": [{"op": "set_const", "field": "done", "value": true}]
     }}
  ],
  "edges": [{"id": "e1", "from": "start", "to": "t"}],
  "triggers": [{"name": "start", "kind": "manual"}]
}
```

Execute with `{"input": {"value": {"a": 1}}}`. The transform
output is `{"value": {"a": 1, "done": true}}`.

## 2. Branch on a score, with an approval gate

Manual trigger → condition → branch → (yes: approval → done) /
(no: done). Scores ≥ 5 take the approval path; lower scores skip
it. The approval node has no author-supplied input: the engine
injects the decision on resume.

```json
{
  "schema_version": "v1",
  "id": "wf-branch-approval",
  "name": "Branch with approval",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "cond", "node_id": "nexaroute.control.condition", "version": "v1",
     "config": {"field": "score", "operator": "gte", "expected": "5"},
     "input": {"value": {"$ref": "input.payload"}}},
    {"id": "br", "node_id": "nexaroute.control.branch", "version": "v1",
     "config": {"true_branch": "yes", "false_branch": "no"},
     "input": {"condition_result": {"$ref": "cond.result"}}},
    {"id": "appr", "node_id": "nexaroute.control.approval", "version": "v1"},
    {"id": "yes", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"branch": "yes"}, "operations": []}},
    {"id": "no", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"branch": "no"}, "operations": []}}
  ],
  "edges": [
    {"id": "e1", "from": "start", "to": "cond"},
    {"id": "e2", "from": "cond", "to": "br"},
    {"id": "e3", "from": "br", "to": "appr", "branch": "yes"},
    {"id": "e4", "from": "appr", "to": "yes"},
    {"id": "e5", "from": "br", "to": "no", "branch": "no"}
  ],
  "triggers": [{"name": "start", "kind": "manual"}]
}
```

Execute with `{"input": {"payload": {"score": 7}}}`: the run
pauses at `appr` (`awaiting_approval`) until a different
operator approves. With `{"score": 2}` the approval path is
skipped and the run finishes on the `no` node.

## 3. Bounded loop over planned iterations

Manual trigger → loop_plan → worker (`for_each`). The worker
unwraps each iteration object (`{index, item}`) — a raw array
source would expose the element itself as `loop.item` instead.

```json
{
  "schema_version": "v1",
  "id": "wf-loop",
  "name": "Bounded loop",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "plan", "node_id": "nexaroute.control.loop_plan", "version": "v1",
     "config": {"max_iterations": 10},
     "input": {"items": {"$ref": "input.items"}}},
    {"id": "worker", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "for_each": {"source": "plan.iterations"},
     "input": {
       "value": {"item": {"$ref": "loop.item.item"}, "index": {"$ref": "loop.item.index"}},
       "operations": [{"op": "set_const", "field": "seen", "value": true}]
     }}
  ],
  "edges": [
    {"id": "e1", "from": "start", "to": "plan"},
    {"id": "e2", "from": "plan", "to": "worker"}
  ],
  "triggers": [{"name": "start", "kind": "manual"}]
}
```

Execute with `{"input": {"items": [10, 20, 30]}}`. The worker
output is an array of three shaped values. With more items than
`max_iterations`, the plan reports `truncated: true` and the
worker runs only for the planned prefix.

## 4. Error handler with fallback

A failing node, a handler, a fallback, and a downstream node.
The handler and fallback need incoming edges (the association)
but never run on the success path: they are pre-skipped at run
start and execute only through the handler.

```json
{
  "schema_version": "v1",
  "id": "wf-fallback",
  "name": "Fallback",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "fail", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {}, "operations": [{"op": "bogus"}]}},
    {"id": "handler", "node_id": "nexaroute.control.error_handler", "version": "v1",
     "config": {"policy": "fallback"}},
    {"id": "fallback", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"recovered": true}, "operations": []}},
    {"id": "down", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"$ref": "fail.value"},
               "operations": [{"op": "set_const", "field": "done", "value": true}]}}
  ],
  "edges": [
    {"id": "e1", "from": "start", "to": "fail"},
    {"id": "e2", "from": "fail", "to": "handler"},
    {"id": "e3", "from": "fail", "to": "fallback"},
    {"id": "e4", "from": "fail", "to": "down"}
  ],
  "triggers": [{"name": "start", "kind": "manual"}],
  "on_error": {"handler_node_id": "handler", "fallback_node_id": "fallback"}
}
```

The run succeeds: the failed steps stay in history, the
fallback output stands in for the failed node's output, and
`down` sees `{"recovered": true, "done": true}`. Change
`policy` to `retry` (at most 2 handler invocations, then fail)
or `fail` (the run fails with the original error).

## 5. Scheduled transform (hourly)

A schedule trigger fires every hour; the first tick fires
immediately. The schedule node needs no instance config: its
declaration carries the interval.

```json
{
  "schema_version": "v1",
  "id": "wf-hourly",
  "name": "Hourly",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "sched", "node_id": "nexaroute.trigger.schedule", "version": "v1"},
    {"id": "done", "node_id": "nexaroute.control.json_transform", "version": "v1",
     "input": {"value": {"finished": true}, "operations": []}}
  ],
  "edges": [{"id": "e1", "from": "sched", "to": "done"}],
  "triggers": [{"name": "sched", "kind": "schedule", "config": {"interval": "1h"}}]
}
```

Publish it, then tick (`POST /admin/api/nexaflow/scheduler/tick`
or background ticking). The slot claim guarantees one run per
hour even if two ticks race.

## 6. LLM chat (needs a live gateway)

Policy approval is required: without
`"settings": {"policy_approved": true}` the LLM node fails with
`policy_required`. The selector must resolve (public model,
endpoint id, deployment id, or deployment alias), and loopback
chat needs a client-auth token (see the user guide).

```json
{
  "schema_version": "v1",
  "id": "wf-llm",
  "name": "LLM",
  "tenant_scope": "tenant-a",
  "nodes": [
    {"id": "start", "node_id": "nexaroute.trigger.manual", "version": "v1"},
    {"id": "llm", "node_id": "nexaroute.llm.chat", "version": "v1",
     "config": {"route_or_alias": "team/coding"},
     "input": {"prompt": "Say hello in one word."}}
  ],
  "edges": [{"id": "e1", "from": "start", "to": "llm"}],
  "triggers": [{"name": "start", "kind": "manual"}],
  "settings": {"policy_approved": true}
}
```

The output carries `model`, `text` (truncated at 64 KiB),
token counts, and `resolved_route`. This node is billable:
every execution is a real provider call through the gateway.
