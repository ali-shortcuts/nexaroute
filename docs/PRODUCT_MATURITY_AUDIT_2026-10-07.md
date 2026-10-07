# ممیزی جامع بلوغ NexaRoute و roadmap رقابتی

**تاریخ:** 2026-10-07  
**دامنه:** معماری، امنیت، چنداجاره‌ای، اقتصاد مصرف، HA، routing، cache، observability، policy، protocol، MCP/A2A، GitOps و کیفیت مهندسی.

## نتیجهٔ اصلی

NexaRoute در حال حاضر یک gateway تک‌نودی قوی و قابل‌ممیزی است، نه هنوز یک platform چنداجاره‌ای و distributed در سطح enterprise. مزیت واقعی آن در routing توضیح‌پذیر، health و failover در سطح deployment، حفظ fidelity ابزارها و stream، یک باینری سبک، privacy محلی و تست‌های regression دقیق است.

برای جلو زدن از gatewayهای مطرح، افزودن provider یا strategy بیشتر کافی نیست. ترتیب درست باید چنین باشد:

1. **درستی و امنیت control plane**
2. **اقتصاد قابل‌اعتماد در مسیر request**
3. **درستی چند replica و state مشترک**
4. **telemetry و audit در سطح GenAI**
5. **policy و guardrail قابل‌نسخه‌بندی**
6. **MCP production-grade**
7. **cache و routing اقتصادی**
8. **protocol breadth و A2A**
9. **GitOps و assurance قابل‌تکرار**

## وضعیت واقعی فعلی

### نقاط قوت

- routing deterministic و explainable با health، capability، priority، weight، latency، failure evidence، affinity و fallback؛
- failover پیش از commit شدن stream و عدم جعل continuation پس از شروع stream؛
- provider/model deployment health جدا از provider-wide incident؛
- credential pool و secret-preserving edits؛
- OpenAI Chat، Anthropic Messages، OpenAI Responses و Gemini adapter؛
- tool-call و streaming fidelity با تست‌های گسترده؛
- global admission limit، provider concurrency و cancellation؛
- admin loopback-first، secretهای write-only و logهای بدون body؛
- usage tracking، virtual key، tenant/project/team پایه، RPM/TPM و config rollback؛
- traceparent propagation، guardrail پایه و graceful drain؛
- پوشش تست کلی اندازه‌گیری‌شده: **75.4٪**؛ `go test ./...` و `go vet ./...` موفق.

### شکاف‌های قطعی که خود پروژه نیز ثبت کرده است

- state و health هنوز single-process/in-memory هستند؛
- secretها در config به‌صورت plaintext-at-rest ذخیره می‌شوند؛
- RBAC/SSO/SCIM/CSRF و built-in TLS کامل وجود ندارد؛
- billing و budget reservation durable و distributed وجود ندارد؛
- semantic cache وجود ندارد؛
- quality-aware routing هنوز routing input نیست؛
- Bedrock، Vertex، Azure-specific semantics، embeddings و rerank native نیستند؛
- token counting در بعضی مسیرها تخمینی است؛
- evaluation عمدتاً offline و scorecardها routing-neutral هستند؛
- provider conformance matrix واحد هنوز وجود ندارد؛
- MCP/A2A gateway کامل وجود ندارد؛
- Helm/CRD/IaC و GitOps promotion کامل وجود ندارد.

منبع وضعیت داخلی: `docs/KNOWN_GAPS.md` و `ARCHITECTURE.md`.

## مقایسهٔ رقابتی

### LiteLLM

LiteLLM روی سازمان، virtual key، team/project، بودجه، billing callback، PostgreSQL، Redis، Prometheus، OTel، cache، MCP و A2A تمرکز کرده است. مستندات آن صراحتاً نشان می‌دهد که multi-pod بدون Redis برای برخی قابلیت‌های shared حالت کامل محسوب نمی‌شود. این برای NexaRoute یک درس مهم است: نباید distributed را فقط با چند replica و بدون تعریف consistency اعلام کرد.

منابع: [Enterprise](https://docs.litellm.ai/docs/enterprise)، [Redis requirements](https://docs.litellm.ai/docs/proxy/redis_requirements)، [Budgets](https://docs.litellm.ai/docs/proxy/users)، [MCP](https://docs.litellm.ai/docs/mcp)، [A2A](https://docs.litellm.ai/docs/a2a).

### Envoy AI Gateway / Agent Router

Agent Router در token quota، Redis-backed counters، fail-open/fail-closed، leader election، Kubernetes scaling، OTel GenAI metrics، TTFT/inter-token latency و MCPRoute قوی است. A2A آن هنوز preview/alpha توصیف شده است؛ بنابراین MCP باید زودتر از A2A ساخته شود.

منابع: [Quota](https://theagentrouter.ai/docs/capabilities/traffic/quota-policy)، [Scaling](https://theagentrouter.ai/docs/capabilities/scaling)، [Observability](https://theagentrouter.ai/docs/capabilities/observability/metrics/)، [MCP](https://theagentrouter.ai/docs/capabilities/mcp/)، [A2A](https://theagentrouter.ai/docs/capabilities/a2a/).

### Kong AI Gateway

Kong در consumer group، tiered budget، cost-aware limit، Redis synchronization، active-active data plane، audit log، PII/prompt guard، semantic cache، MCP و A2A سطح محصولی دارد. محدودیت مهم آن این است که بخشی از AI Gateway 2.0 به control plane مدیریت‌شدهٔ Konnect وابسته است؛ NexaRoute می‌تواند با self-hosted control plane واقعاً مستقل تمایز ایجاد کند.

منابع: [Architecture](https://developer.konghq.com/ai-gateway/architecture/)، [Cost](https://developer.konghq.com/ai-gateway/model-cost-management/)، [Rate limit](https://developer.konghq.com/ai-gateway/policies/ai-rate-limiting-advanced/)، [Semantic cache](https://developer.konghq.com/ai-gateway/policies/ai-semantic-cache/)، [MCP](https://developer.konghq.com/ai-gateway/mcp/)، [A2A](https://developer.konghq.com/ai-gateway/a2a/).

### Bifrost

Bifrost در virtual key، hierarchical budget، CEL policy، MCP، semantic cache، OTel/Prometheus، mocker و Helm قوی است. clustering و guardrailهای پیشرفته در مستندات آن عمدتاً Enterprise هستند. برای NexaRoute، policy-as-code، mock provider و failure simulation درس‌های مهمی هستند.

منابع: [Governance](https://docs.getbifrost.ai/features/governance/virtual-keys)، [Budgets](https://docs.getbifrost.ai/features/governance/budget-and-limits)، [Clustering](https://docs.getbifrost.ai/enterprise/clustering)، [Guardrails](https://docs.getbifrost.ai/enterprise/guardrails)، [MCP](https://docs.getbifrost.ai/mcp/overview).

### Portkey

Portkey در workspace isolation، policyهای input/output، retry/fallback، canary، batch evaluation، MCP identity forwarding و A2A invocation قوی است. فرصت تمایز NexaRoute: policy simulator، replay و promotion کاملاً self-hosted و قابل‌ممیزی.

منابع: [Access control](https://docs.portkey.ai/docs/product/enterprise-offering/access-control-management)، [Guardrails](https://docs.portkey.ai/docs/product/guardrails)، [Observability](https://docs.portkey.ai/docs/product/observability)، [MCP](https://docs.portkey.ai/docs/product/mcp-gateway)، [A2A](https://docs.portkey.ai/docs/product/agent-gateway/quickstart).

## roadmap پیشنهادی

### P0 — باید پیش از ادعای enterprise کامل شود

#### 1. Durable control plane

PostgreSQL را منبع حقیقت برای config، tenant، key، policy، budget، audit و price book کنید. Redis را برای rate limit، lease، lock، short-lived OAuth/MCP state، cache invalidation و shared counters استفاده کنید.

باید semantics مستند شود:

- atomic reservation و settlement؛
- optimistic concurrency؛
- last-known-good config؛
- migration version؛
- fail-open یا fail-closed برای هر قابلیت؛
- رفتار هنگام قطع Redis/Postgres؛
- leader ownership برای probe و jobها.

#### 2. Identity و tenant isolation واقعی

مدل فعلی را به سازمان کامل تبدیل کنید:

```text
Organization → Workspace/Project/Team → User/Service Account → Virtual Key
```

اضافه شود:

- OIDC/JWT و بعد SSO/SCIM؛
- RBAC با scope واقعی؛
- key rotation/revoke و IP/network policy؛
- tenant-aware cache، metrics، logs و audit؛
- credential broker برای upstream؛
- CSRF token و session policy برای UI؛
- audit log append-only با hash chain یا امضای رکورد.

#### 3. FinOps enforceable

Usage ledger فعلی باید به budget engine تبدیل شود:

- input، cached-input، output و reasoning token؛
- price book نسخه‌دار با provider/model/tier/batch/cache؛
- request، token، concurrency، daily و monthly budget؛
- reservation پیش از upstream؛
- settlement بر اساس usage واقعی؛
- thresholdهای 50/80/95/100٪؛
- hard block و dry-run؛
- export برای chargeback/invoice؛
- گزارش بر اساس tenant، user، key، project، model، provider و route.

این بخش باید در حالت multi-replica با Redis/Postgres تست شود، نه فقط با map در memory.

#### 4. AI-native observability

trace propagation فعلی باید به OTel exporter واقعی تبدیل شود. هر request باید spanهای زیر را داشته باشد:

```text
ingress → auth → policy → estimate → route → attempt → retry/failover → stream → usage/billing
```

فیلدهای privacy-safe:

- tenant/key fingerprint/model/provider/route؛
- selected و candidateها؛
- queue wait، header latency، TTFT و inter-token؛
- retry/failover reason؛
- token، cost، cache، policy و guardrail outcome؛
- payload capture فقط opt-in و redacted.

Prometheus، OTLP، structured JSON و alertهای SLO باید artifact رسمی داشته باشند.

### P1 — تمایز اصلی در کیفیت و قابلیت

#### 5. Policy-as-code production-grade

Guardrail فعلی detector پایه است. آن را به policy engine تبدیل کنید:

- policyهای ordered و composable؛
- inheritance از org تا key/model/route؛
- PII/secrets/prompt injection/jailbreak؛
- input و output safety؛
- JSON/schema validation؛
- allow/block/redact/modify/retry/fallback/audit؛
- MCP argument/result policy؛
- timeout مستقل و fail-open/fail-closed؛
- dry-run و simulator پیش از deploy؛
- version، diff، approval و promotion؛
- decision explanation برای هر request.

#### 6. Exact، prefix و semantic cache

سه لایه را جدا کنید:

1. exact response cache؛
2. provider prefix-cache metadata؛
3. semantic response cache با Redis/Valkey و pgvector یا backend قابل‌تعویض.

الزامات:

- tenant/model/tool/schema isolation؛
- TTL و similarity threshold؛
- cache key اجباری یا policy پیش‌فرض شفاف؛
- حذف با config/policy/version؛
- عدم cache برای PII، ابزار خطرناک یا request زمان‌حساس؛
- ثبت cache hit در billing و trace؛
- semantics روشن برای streaming.

#### 7. Routing اقتصادی و قابل‌اثبات

Routing فعلی را حفظ کنید و فقط با evidence توسعه دهید:

- quota headroom واقعی؛
- cost و latency و TTFT؛
- quality score از evaluation؛
- data residency و region؛
- cache locality؛
- canary؛
- shadow evaluation؛
- adaptive routing فقط opt-in؛
- rollback خودکار بر اساس SLO.

هر تصمیم باید reason code و candidate evidence داشته باشد. semantic router نباید بدون benchmark وارد hot path شود.

#### 8. MCP Gateway کامل

MCP را پیش از A2A بسازید:

- registry و onboarding؛
- STDIO، SSE و Streamable HTTP؛
- tool discovery/filtering؛
- OAuth/OIDC/API-key credential brokering؛
- tenant/user/tool scope؛
- approval برای ابزار حساس؛
- SSRF و egress policy؛
- timeout/retry/rate limit؛
- redact نتیجه؛
- trace و audit متصل به request اصلی.

### P2 — توسعهٔ سطح محصول و moat

#### 9. Provider conformance suite

به‌جای فهرست provider، برای هر adapter یک matrix versioned بسازید:

- text، stream، tools، parallel tools، vision؛
- structured output و JSON mode؛
- reasoning؛
- count tokens؛
- cancellation؛
- retry-after و error schema؛
- long context؛
- batch، embeddings، rerank و moderation.

هر تست باید mock contract و optional live contract داشته باشد و capability matrix را تولید کند.

#### 10. Protocol surface

ترتیب پیشنهادی:

1. embeddings و rerank؛
2. native Bedrock/Vertex/Azure؛
3. Gemini native؛
4. image/audio/STT/TTS؛
5. realtime/WebSocket؛
6. batch؛
7. OpenAI Responses کامل‌تر.

برای هر protocol، fidelity و unsupported semantics باید صریح باشد؛ breadth بدون conformance ارزش تبلیغاتی دارد نه production.

#### 11. A2A production-grade

بعد از MCP:

- agent-card discovery و validation؛
- registry و health؛
- method-level auth؛
- invocation key و budget؛
- JSON-RPC و REST binding؛
- task lifecycle telemetry؛
- agent-card URL rewrite؛
- retry و cancellation؛
- conformance tests.

#### 12. GitOps و deployment maturity

ارائه شود:

- OpenAPI versioned؛
- JSON Schema config؛
- validate/diff/plan؛
- Helm chart و Kubernetes manifests؛
- migration job جدا از serving؛
- PDB، HPA، ServiceMonitor و NetworkPolicy؛
- Terraform؛
- Argo CD/Flux مثال رسمی؛
- signed images، SBOM، provenance و cosign؛
- upgrade/rollback tested.

## بزرگ‌ترین مزیت رقابتی پیشنهادی

NexaRoute نباید صرفاً نسخهٔ Go از LiteLLM یا Kong شود. جایگاه متمایز پیشنهادی:

> **Privacy-first, explainable, self-hosted AI Gateway with verifiable routing and deterministic policy promotion.**

برای تحقق این جایگاه، پنج قابلیت باید به‌صورت یکپارچه ساخته شوند:

1. decision trace قابل توضیح؛
2. policy simulator قبل از deploy؛
3. replay امن بدون prompt leakage؛
4. provider conformance و failure drill؛
5. cost/budget decision همراه با دلیل و evidence.

بیشتر رقبا featureهای فراوان دارند، اما این زنجیرهٔ assurance را به‌صورت شفاف و self-hosted کمتر ارائه می‌کنند.

## معیار «واقعاً بالغ شده»

قبل از انتشار نسخهٔ enterprise، این gateها باید سبز باشند:

- دو یا سه replica با قطع و وصل Redis/Postgres؛
- تست budget reservation هم‌زمان و عدم عبور از سقف؛
- failover در قطع provider، Redis، DB و control plane؛
- config rollback و migration upgrade/downgrade؛
- tenant isolation test برای cache، metrics، logs و audit؛
- OTel trace و Prometheus cardinality audit؛
- fuzz روی parser، policy، tool arguments و protocol translation؛
- provider conformance matrix با mock server؛
- MCP SSRF/OAuth/tool-permission tests؛
- load، soak، race و chaos test؛
- signed release، SBOM و reproducible build؛
- browser accessibility و security scan؛
- benchmark همسان در برابر gatewayهای رقیب، بدون عدد ساختگی.

## اولویت اجرایی نهایی

```text
Phase A: Postgres/Redis contracts + migrations + durable identity
Phase B: distributed budget/rate-limit + audit + last-known-good config
Phase C: OTel/Prometheus + tenant-safe observability
Phase D: policy-as-code + simulator + output/input enforcement
Phase E: semantic cache + cost/quality-aware routing
Phase F: provider conformance + embeddings/rerank/native providers
Phase G: MCP gateway
Phase H: Helm/Terraform/GitOps + signed supply chain
Phase I: A2A + advanced protocol surface
Phase J: chaos/benchmark/release certification
```

**تصمیم پیشنهادی:** فعلاً providerهای بیشتر و adaptive routing را متوقف کنید. ابتدا Phaseهای A تا D را به‌صورت یک vertical slice کامل کنید؛ چون بدون آن‌ها هر قابلیت جدید فقط سطح feature را زیاد می‌کند و بلوغ واقعی را نه.

## مراجع رقابتی منتخب

- [LiteLLM enterprise](https://docs.litellm.ai/docs/enterprise)
- [LiteLLM Redis requirements](https://docs.litellm.ai/docs/proxy/redis_requirements)
- [Agent Router quota policy](https://theagentrouter.ai/docs/capabilities/traffic/quota-policy)
- [Agent Router observability](https://theagentrouter.ai/docs/capabilities/observability/metrics/)
- [Kong AI Gateway architecture](https://developer.konghq.com/ai-gateway/architecture/)
- [Kong AI semantic cache](https://developer.konghq.com/ai-gateway/policies/ai-semantic-cache/)
- [Bifrost clustering](https://docs.getbifrost.ai/enterprise/clustering)
- [Bifrost guardrails](https://docs.getbifrost.ai/enterprise/guardrails)
- [Portkey guardrails](https://docs.portkey.ai/docs/product/guardrails)
- [Portkey MCP Gateway](https://docs.portkey.ai/docs/product/mcp-gateway)
