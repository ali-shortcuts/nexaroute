**آیا باگ‌های NexaRoute برطرف شده و آخرین نسخه کدام است؟** برای مواردی که در دامنهٔ این ارزیابی ثبت شده‌اند، **مورد ناموفقِ باز وجود ندارد**: اجرای `go test -count=1 ./...` روی ۲۷ پکیج NexaRoute و smoke محلی (UI، health، مدل‌ها، redaction رازها و persistence) موفق بوده است. این نتیجه به معنی «نبود هر باگ ممکن» یا تأیید رفتار همهٔ providerهای بیرونی نیست؛ تست‌ها بدون فراخوانی اعتبارنامه‌دار به LLMهای واقعی اجرا شده‌اند. آخرین release مبنای این گزارش **NexaRoute v0.10.1** در commit `dcaeba7993bfb08d9b51e698624da1033899d5a0` است: [صفحهٔ انتشار v0.10.1][1]. این release باینری‌های Linux برای amd64/arm64، `install.sh` و `SHA256SUMS` دارد و در بازبینی، checksumها و build/release smoke نیز تأیید شدند.[1][2]

> **جمع‌بندی اجرایی:** NexaRoute برای gateway محلی/تک‌نودی، قابل‌نصب و قابل‌مشاهده با مرزهای routing روشن، کنترل‌پلین سبک و حفظ‌پذیری tool-call انتخاب قوی‌تری است. LiteLLM، Bifrost و OmniRoute در وسعت provider/endpoint و عملیات سازمانی پیش‌ترند؛ 9Router برای UX چندحسابی و صرفه‌جویی توکن جذاب است. انتخاب باید بر مبنای نیاز عملیاتی باشد، نه «بهترین» مطلق.

## روش، دامنه و سطح اطمینان

- **Baseline ثابت NexaRoute:** tag `v0.10.1` / commit `dcaeba7993bfb08d9b51e698624da1033899d5a0`.[1][2]
- مقایسهٔ کد/مستندات با revisionهای مشخص انجام شده است، نه با branch شناور: 9Router `v0.5.91` / `f01fb90`؛ OmniRoute branch `release/v3.8.51` / `a58000c` (در حالی‌که آخرین artifact رسمی مشاهده‌شده v3.8.50 است)؛ Bifrost `dev` / `ed40d13` با releaseهای ماژولی Core v1.10.4 و HTTP v2.2.3؛ LiteLLM main `22b36cb` و آخرین release غیر-pre-release مشاهده‌شده v1.102.1.[3][4][5][6]
- **تأیید عملی NexaRoute:** full Go test، build محلی، integrity/release و smoke محلی موفق بودند. این smoke فقط loopback/mock غیرفعال داشت؛ therefore تأیید availability/latency سرویس‌های بیرونی نیست.
- **رقبا:** هر نتیجهٔ تست عملی در جدول و بخش «شواهد اجرا» صریحاً تفکیک شده است. نبود اجرای کامل، اثبات نقص نیست؛ صرفاً محدودیت این گزارش است. ادعاهای نبود قابلیت فقط به source/docs/workflowهای بررسی‌شده محدودند.
- **بدون benchmark ساختگی:** هیچ عدد throughput، latency، cost یا نرخ موفقیتی که در یک سناریوی همسان اندازه‌گیری نشده باشد، ارائه نشده است.

## جدول مقایسهٔ فشرده

| پروژه | جایگاه/معماری | پروتکل و سطح قابلیت | routing و تاب‌آوری | امنیت و کنترل‌پلین | نصب و شواهد آزمون در sandbox | برداشت عملی |
|---|---|---|---|---|---|---|
| **NexaRoute v0.10.1** | یک باینری Go تک‌فرایندی با UI embedded، config XDG، snapshot/hot-reload و جداسازی data/control plane؛ router از translator جدا می‌ماند.[2] | ingressهای OpenAI Chat، Anthropic Messages، OpenAI Responses، count tokens و adapterهای Gemini؛ دامنهٔ native/vendor extensionها عمداً محدود و مستند است.[7][8] | health/capability per-deployment، `ready_mesh`/P2C، affinity، latency/failure scoring، fallback متنوع از provider، circuit/recovery، hedging اختیاری؛ failover stream فقط پیش از commit byte.[2] | admin محلی‌پیش‌فرض، secret/header write-only، config/log با 0600 و hardening remote-decision؛ اما secretها در rest plaintext هستند و URL دلخواه provider/proxy برای admin strict SSRF policy ندارد.[9] | باینری Linux amd64/arm64 + installer/checksum. `go test ./...`، build و smoke محلی **PASS**؛ checksum artifacts **PASS**.[1] | **بهترین برای:** Claude Code/local gateway، Linux headless سبک، routing قابل‌توضیح و privacy کنترل‌پلین. |
| **LiteLLM** | Python SDK + gateway/proxy بزرگ با FastAPI-style stack، dashboard/DB/auth و Redis اختیاری؛ monolith یا deployment جداگانه/HA.[6][10] | سطح بسیار وسیع OpenAI/Anthropic/Responses/Gemini native و endpoints مانند embeddings، image/audio/files/batch؛ پشتیبانی دقیق پارامتر به provider وابسته است.[11][12][13] | weighted، RPM/TPM-aware، least-busy، latency/cost، priority، retries/cooldown، Redis و mirroring؛ اکوسیستم multi-instance وسیع‌تر.[14] | RBAC/virtual key/JWT/SSO، secret managerها و URL validator با DNS/redirect hardening؛ برخی قابلیت‌های secrets Enterprise هستند.[15][16] | Python/CLI/Docker/Helm/Terraform و image امضاشده cosign؛ focused pytest محلی به علت provisioning maturin پس از حدود ۱۰ دقیقه **کامل نشد**؛ `compileall` **PASS**.[6] | **بهترین برای:** سازمان چندtenant، provider و endpoint بسیار متنوع، استقرار Kubernetes/Redis/Postgres. |
| **Bifrost** | monorepo ماژولار Go: core/framework/HTTP/UI/plugin؛ SDK و gateway، plugin/MCP/telemetry.[5][17] | 20+ provider با unified API، Chat/Responses، Anthropic/Google GenAI و عملیات provider-dependent فراتر از chat.[17][18] | OSS: Virtual Key، allow/deny، weight، fallback، key pool و retry؛ adaptive LB/clustering در docs Enterprise برچسب خورده‌اند.[19] | `env.*`/`vault.*` SecretVar، redaction و SSRF dialer ضد DNS-rebinding با block private/metadata؛ TLS/access boundary همچنان الزامی است.[20][21] | NPX/Docker/SDK/Helm/Terraform؛ focused Go tests به سبب EOF dependency و سپس vLLM test معلق **کامل نشد**؛ HTTP smoke/full suite اجرا نشد.[5] | **بهترین برای:** پلتفرم قابل‌گسترش، plugin/OTel/governance و deploymentهای چندجزئی. |
| **OmniRoute** | TypeScript/Next.js محلی، open-sse/translation/executor، SQLite، dashboard و cloud-sync اختیاری؛ feature-rich اما ناهمگن‌تر.[4][22] | OpenAI Chat/Responses، Anthropic Messages، native Gemini ingress، embedding و image-style endpoints.[22][23] | combo/fallback، ۱۹ strategy، auto scoring، breaker، cooldown account/connection، retry و queue؛ قابلیت و سطح تنظیم بیشتر.[24] | AES-256-GCM+scrypt **وقتی** `STORAGE_ENCRYPTION_KEY` تنظیم شود، JWT/OAuth PKCE/scopes و outbound guard؛ بدون آن encryption-at-rest تضمین‌شده نیست.[25] | npm/pnpm/source/Docker/Electron؛ Node >=22.22.2. سه test انتخابی: **18 pass / 0 fail**؛ full suite، Docker/Electron E2E و provider live اجرا نشد.[4] | **بهترین برای:** desktop/چندابزار، dashboard عمیق و endpoint/integration گسترده. |
| **9Router** | Node.js 20+، Next.js/React/Tailwind، SQLite + SSE gateway/dashboard؛ open-sse provider executors و RTK پیش از translation.[3] | Chat، Anthropic conversion، Responses و Gemini/Vertex translation مستند؛ SSE موجود، اما full streaming suite اجرا نشد.[26] | model-combo، fallback subscription→cheap→free، quota/error fallback و multi-account RR/priority؛ evidence هم‌سطح NexaRoute برای health/capability/hedging/circuit recovery مشاهده نشد.[3] | SSRF guard با DNS/redirect checks و regression tests؛ ولی quick-start با password اولیه `123456`، API-key enforcement خاموش و Docker روی `0.0.0.0` نیازمند hardening اپراتور است.[3][27] | npm/source/Docker. SSRF smoke **PASS**؛ focused Vitest: **23 pass، 2 expected failure، 1 assertion failure**؛ full suite اجرا نشد.[3] | **بهترین برای:** provider/coding-client breadth، چندحسابی و RTK/token-saving؛ با hardening و اعتبارسنجی fidelity پیش از production. |

## تفاوت‌های مهم و ادعاهای تأییدشده

### ۱) معماری: سادگی قابل‌عملیات در برابر breadth

NexaRoute یک data-plane Go تک‌فرایندی با UI embedded و config محلی است؛ contract طراحی‌اش این است که router نیازمندی‌های پروتکل را تغییر ندهد و translation انتخاب provider را تعیین نکند.[2] این ساختار مسیر عیب‌یابی و مرز مسئولیت را کوتاه و روشن نگه می‌دارد.

در طرف مقابل، **LiteLLM** برای HA و عملیات چندسرویسی ساخته شده و **Bifrost** برای modularity/plugin؛ هر دو در scale و integration گسترده‌ترند، اما به اجزای عملیاتی و version/configuration بیشتری نیاز دارند.[5][6][10] **OmniRoute** و **9Router** نیز dashboard و translation/provider surface وسیع‌تری دارند، اما به runtime Node و dependencyهای آن وابسته‌اند.[3][4]

**نتیجه:** «تک‌باینری» مزیت عملکردی سنجش‌نشده نیست؛ مزیت آن **کاهش سطح نصب و عملیات** است. برای معماری multi-tenant یا cloud-native، LiteLLM/Bifrost معمولاً fit طبیعی‌تری هستند.

### ۲) سازگاری پروتکل و fidelity ابزارها

NexaRoute خانواده‌های OpenAI Chat، Anthropic Messages، OpenAI Responses و Gemini را در canonical pipeline پوشش می‌دهد، اما صریحاً اعلام می‌کند که همهٔ semanticsهای native Bedrock/Vertex/Azure و embeddings/rerank را هدف نگرفته است.[7][8] مزیت آن در **contract قابل ممیزی برای tool fidelity** است: tool-name reversible، argument خامِ malformed به صورت wrapper حفظ می‌شود، parallel/streaming tool handling مشخص است و validation type fail-closed است.[28]

LiteLLM، OmniRoute و Bifrost از نظر endpoint/provider surface وسیع‌ترند. OmniRoute در سه تست انتخابی، reassembly چهار fragment، parallel tool calls و جلوگیری از double emit را با **18/18 pass** نشان داد؛ با این حال، matrix واحدی هم‌سطح audit NexaRoute در source/docs بازبینی‌شده پیدا نشد.[4] Bifrost و LiteLLM نیز testهای Responses/stream/tools دارند، اما fidelity نهایی به adapter و model/provider انتخابی وابسته است.[5][6]

برای 9Router باید محتاط‌تر بود: دو expected failure ثبت‌شده در Responses conversion مربوط به function-name خالی و استفاده از file identifier تصویر به‌عنوان raw URL است؛ focused run نیز همین موارد را دید. assertion ناموفق دیگر test/source drift در ماسک‌سازی aggregation key بود، نه اثبات افشای پاسخ به کاربر.[3]

**نتیجه:** اگر ابزارهای agent و مهاجرت Anthropic↔OpenAI با stream حساس‌ترین مسیر شماست، NexaRoute در این revision شواهد قراردادی شفاف‌تری دارد. اگر native endpointهای بیشتری می‌خواهید، LiteLLM/OmniRoute/Bifrost برترند، ولی باید مسیر model/provider خود را integration-test کنید.

### ۳) routing، health و streaming

NexaRoute در نسخهٔ پایه health/capability فیلترشده در سطح deployment، ready-mesh/P2C، scoring latency/failure، provider-diverse fallback، circuit/recovery probe، bounded admission و hedge اختیاری را صورت‌بندی می‌کند. مهم‌تر از تعداد strategyها، قاعدهٔ deterministic آن است: پس از commit شدن byteهای stream failover نمی‌کند.[2]

OmniRoute از نظر breadth برنده است: ۱۹ strategy، auto-scoring چندعاملی، combo، queue و cooldownهای account/connection.[24] LiteLLM نیز strategyهای متعدد و Redis-backed routing دارد.[14] Bifrost در OSS روی Virtual Key/fallback/weight/key pool متمرکز است؛ adaptive LB و clustering در مستنداتش Enterprise هستند.[19] 9Router راه‌حل ساده‌تر combo و quota fallback ارائه می‌دهد، اما در scope بررسی‌شده شواهد هم‌ارز برای policy health/hedging/recovery NexaRoute دیده نشد.[3]

**نتیجه:** NexaRoute برای رفتار محلی قابل‌پیش‌بینی و health-aware مناسب‌تر است؛ LiteLLM و OmniRoute برای policy surface و تنظیمات گسترده‌تر؛ Bifrost برای governance/key-routing قابل‌گسترش.

### ۴) امنیت: مزیت‌ها و محدودیت‌های واقعی

NexaRoute admin را محلی‌پیش‌فرض، secrets/headers را write-only و metadata logging را محدود می‌کند؛ این‌ها برای ابزار local-first ارزشمندند. اما خود پروژه plaintext-at-rest بودن config و فقدان vault encrypted را اعلام می‌کند؛ همچنین URLهای provider/proxy انتخاب‌شده توسط admin strict SSRF policy ندارند.[9] بنابراین نباید بدون TLS/network boundary مناسب روی اینترنت expose شود.

- **LiteLLM** controls سازمانی وسیع‌تر، secret-manager integration و URL validator سخت‌گیر برای user-controlled fetch دارد.[15][16]
- **Bifrost** dialer سطح شبکه با resolve-on-every-dial و کنترل DNS rebinding/metadata دارد.[20]
- **OmniRoute** encryption SQLite را با کلید پیکربندی‌شده و outbound guard ارائه می‌دهد؛ اما encryption پیش‌فرض نیست.[25]
- **9Router** SSRF guard قابل مشاهده و smoke-tested دارد، اما defaults quick-start آن باید پیش از اینترنتی‌کردن تغییر کند.[3][27]

**نتیجه:** برندهٔ مطلق امنیت وجود ندارد. LiteLLM برای enterprise identity/secrets، Bifrost برای outbound network guard، NexaRoute برای local admin/privacy UX، و OmniRoute برای encryption اختیاری قوی‌اند. هر چهار پروژه به hardening deployment نیاز دارند.

### ۵) نصب، artifact و اعتماد زنجیرهٔ انتشار

NexaRoute برای Linux server/workstation ساده‌ترین مسیر را دارد: artifact نسخه‌دار amd64/arm64، checksum و installer با replacement اتمیک؛ در این ارزیابی checksum و install/release smoke واقعاً pass شد.[1] LiteLLM Docker/Helm/Terraform و imageهای cosign-signed ارائه می‌کند، اما استقرار production آن معمولاً به اجزای بیشتر نیاز دارد.[6] Bifrost با NPX/Docker شروع بسیار سریعی دارد، ولی versioning core/transport/plugin نیازمند pin سازگاری است.[5] OmniRoute برای desktop چندپلتفرمی artifactهای بیشتری دارد، و 9Router بیشتر بر npm/Docker با tag شناور `latest` تکیه دارد.[3][4]

## شواهد اجرا و محدودیت‌ها

| پروژه | آنچه واقعاً اجرا شد | تفسیر صحیح |
|---|---|---|
| **NexaRoute** | `go test -count=1 ./...` روی ۲۷ package، build باینری، checksum release و `smoke-local.sh`: **PASS**. | پوشش قوی local regression/runtime؛ **نه** تأیید provider live یا benchmark. |
| **OmniRoute** | Node v24.11.1، `npm ci` و سه test انتخابی رسمی: **18 pass، 0 fail** در 4.181 ثانیه. | evidence مثبت برای URL guard/DNS rebinding و streaming tools؛ **نه** full suite/E2E/live provider. |
| **9Router** | SSRF smoke: **PASS**. Vitest focused با pnpm: **23 pass، 2 expected failure، 1 failure**. | دو known gap Responses باید در تصمیم production لحاظ شوند؛ full 298-file suite اجرا نشده است. |
| **Bifrost** | focused Go test: retry از برخی مراحل گذشت، ولی dependency EOF و سپس vLLM test معلق مانع completion شد. | هیچ **PASS کامل** یا HTTP smoke برای Bifrost ادعا نمی‌شود. |
| **LiteLLM** | `python -m compileall` **PASS**؛ focused pytest به provisioning editable/maturin رسید و برای جلوگیری از انتظار طولانی متوقف شد. | هیچ pytest/provider-live pass برای LiteLLM ادعا نمی‌شود. |

## verdict کاربردی و پیشنهاد roadmap

### چه زمانی NexaRoute انتخاب بهتر است؟

NexaRoute را انتخاب کنید اگر اولویت شما این‌هاست:

1. **Gateway محلی یا Linux headless با footprint کم:** نصب باینری، UI embedded و config محلی، بدون اجبار Node/Python/DB خارجی.[1][2]
2. **Routing قابل‌فهم و قابل‌مشاهده:** health در سطح deployment، fallback متنوع از provider و مرز روشن stream-failover برای رفتار قابل پیش‌بینی.[2]
3. **Agent/tool-call حساس:** نیاز به evidence مشخص برای argument type، name mapping، malformed payload و parallel streaming tools.[28]
4. **کنترل‌پلین local-first و privacy عملی:** default loopback، write-only secret editing، log metadata و permission محدود فایل config.[9]
5. **اعتماد artifact برای Linux:** release نسخه‌دار، SHA256 و installer اتمیک که در این ارزیابی صحتش آزمایش شد.[1]

### چه زمانی رقیب مناسب‌تر است؟

- **LiteLLM:** multi-tenant/enterprise، provider و endpointهای بسیار گسترده، HA، Redis/Postgres/Kubernetes و ecosystem observability/security integrations.[10][11][15]
- **Bifrost:** معماری extensible با plugin/MCP/OTel و governance/Virtual Key در یک platform ماژولار.[17][19]
- **OmniRoute:** API surface بسیار وسیع به‌ویژه native Gemini، dashboard عمیق، combo strategyهای پرشمار و تجربهٔ desktop چندپلتفرمی.[22][24]
- **9Router:** اتصال سریع به providerها و coding clients، چندحسابی/OAuth، comboهای عملی و RTK/token-saving؛ مشروط به hardening defaults و پذیرش/رفع gaps Responses.[3][26]

### قابلیت‌هایی از رقبا که برای roadmap NexaRoute ارزش بالایی دارند

1. **گسترش endpointهای واقعی، نه صرفاً compatibility:** embeddings، rerank، files/batch و native Gemini/Vertex/Azure semantics (الگو از LiteLLM/OmniRoute)؛ همراه با matrix رسمی capability per-provider.[8][11][23]
2. **export و observability استاندارد:** OpenTelemetry/OTLP، Prometheus، trace/search و dashboard analytics قابل اتصال (Bifrost/LiteLLM).[17][29]
3. **control-plane سازمانی، به‌صورت optional:** virtual keys، RBAC/scopes، audit log، tenancy و policy/budget (LiteLLM/Bifrost).[15][19]
4. **secret storage امن‌تر:** keyring/vault/encrypted-at-rest به‌صورت opt-in، بدون از دست دادن write-only UX؛ این مهم‌ترین gap امنیتی صریح NexaRoute است.[9][15][20]
5. **تنوع artifact بدون پیچیده‌کردن Linux path:** container signing/SBOM و optionally desktop packages؛ LiteLLM و OmniRoute الگوهای مناسبی برای delivery breadth دارند.[4][6]
6. **حفظ مزیت فعلی:** پیش از افزودن strategyهای فراوان OmniRoute/LiteLLM، contract فعلیِ deployment-health، provider-diverse fallback و «عدم failover پس از شروع stream» حفظ و برای قابلیت‌های جدید نیز test/fuzz/matrix منتشر شود.[2][24]

## References

[1] [NexaRoute v0.10.1 release](https://github.com/ali-shortcuts/nexaroute/releases/tag/v0.10.1)؛ [README در commit مرجع](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/README.md).

[2] [NexaRoute ARCHITECTURE.md](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/ARCHITECTURE.md)؛ [Control Plane v2](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/docs/CONTROL_PLANE_V2.md).

[3] [9Router repository در revision بررسی‌شده](https://github.com/decolua/9router/tree/f01fb909e37189008080632ddaf404f096345cde)؛ [README](https://raw.githubusercontent.com/decolua/9router/f01fb909e37189008080632ddaf404f096345cde/README.md)؛ [SSRF guard](https://raw.githubusercontent.com/decolua/9router/f01fb909e37189008080632ddaf404f096345cde/src/shared/utils/ssrfGuard.js)؛ [security tests](https://raw.githubusercontent.com/decolua/9router/f01fb909e37189008080632ddaf404f096345cde/tests/unit/security-audit.test.js).

[4] [OmniRoute repository](https://github.com/diegosouzapw/OmniRoute)؛ [release/v3.8.51 architecture](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/docs/architecture/ARCHITECTURE.md)؛ [releases](https://github.com/diegosouzapw/OmniRoute/releases)؛ [CI](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/.github/workflows/ci.yml).

[5] [Bifrost repository](https://github.com/maximhq/bifrost)؛ [Bifrost overview](https://docs.getbifrost.ai/overview)؛ [releases](https://github.com/maximhq/bifrost/releases).

[6] [LiteLLM repository](https://github.com/BerriAI/litellm)؛ [LiteLLM releases](https://github.com/BerriAI/litellm/releases)؛ [deployment docs](https://docs.litellm.ai/docs/proxy/deploy)؛ [signed Docker images](https://docs.litellm.ai/docs/proxy/docker_image_security).

[7] [NexaRoute compatibility](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/docs/COMPATIBILITY.md).

[8] [NexaRoute known gaps](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/docs/KNOWN_GAPS.md).

[9] [NexaRoute SECURITY.md](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/SECURITY.md).

[10] [LiteLLM README](https://github.com/BerriAI/litellm)؛ [proxy UI](https://docs.litellm.ai/docs/proxy/ui).

[11] [LiteLLM supported endpoints](https://docs.litellm.ai/docs/supported_endpoints)؛ [Responses API](https://docs.litellm.ai/docs/response_api)؛ [Gemini generateContent](https://docs.litellm.ai/docs/generateContent).

[12] [LiteLLM Anthropic unified endpoint](https://docs.litellm.ai/docs/anthropic_unified/).

[13] [LiteLLM provider routing source](https://raw.githubusercontent.com/BerriAI/litellm/main/litellm/router.py).

[14] [LiteLLM routing](https://docs.litellm.ai/docs/routing).

[15] [LiteLLM security best practices](https://docs.litellm.ai/docs/proxy/security_best_practices)؛ [secret managers](https://docs.litellm.ai/docs/secret_managers/overview).

[16] [LiteLLM URL validation source](https://raw.githubusercontent.com/BerriAI/litellm/main/litellm/litellm_core_utils/url_utils.py).

[17] [Bifrost supported providers](https://docs.getbifrost.ai/providers/supported-providers/overview)؛ [default observability](https://docs.getbifrost.ai/features/observability/default).

[18] [Bifrost gateway streaming](https://docs.getbifrost.ai/quickstart/gateway/streaming).

[19] [Bifrost governance routing](https://docs.getbifrost.ai/features/governance/routing)؛ [retries and fallbacks](https://docs.getbifrost.ai/features/retries-and-fallbacks).

[20] [Bifrost SSRF dialer source](https://github.com/maximhq/bifrost/blob/dev/core/network/ssrf.go)؛ [SecretVar source](https://github.com/maximhq/bifrost/blob/dev/core/schemas/secretvar.go).

[21] [Bifrost security guidance](https://docs.getbifrost.ai/security).

[22] [OmniRoute features](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/docs/guides/FEATURES.md)؛ [setup guide](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/docs/guides/SETUP_GUIDE.md).

[23] [OmniRoute architecture](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/docs/architecture/ARCHITECTURE.md).

[24] [OmniRoute resilience guide](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/docs/architecture/RESILIENCE_GUIDE.md).

[25] [OmniRoute SECURITY.md](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/SECURITY.md)؛ [CodeQL workflow](https://raw.githubusercontent.com/diegosouzapw/OmniRoute/release/v3.8.51/.github/workflows/codeql.yml).

[26] [9Router Responses regression tests](https://raw.githubusercontent.com/decolua/9router/f01fb909e37189008080632ddaf404f096345cde/tests/translator/bugs-codexCli-responses.test.js).

[27] [9Router security-audit test](https://raw.githubusercontent.com/decolua/9router/f01fb909e37189008080632ddaf404f096345cde/tests/unit/security-audit.test.js).

[28] [NexaRoute tool-fidelity audit](https://github.com/ali-shortcuts/nexaroute/blob/dcaeba7993bfb08d9b51e698624da1033899d5a0/TOOL_FIDELITY_AUDIT_REPORT.md).

[29] [NexaRoute CI](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/.github/workflows/ci.yml)؛ [security workflow](https://raw.githubusercontent.com/ali-shortcuts/nexaroute/dcaeba7993bfb08d9b51e698624da1033899d5a0/.github/workflows/security.yml)؛ [LiteLLM CodeQL](https://raw.githubusercontent.com/BerriAI/litellm/main/.github/workflows/codeql.yml).
