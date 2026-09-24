# ممیزی چرخهٔ Routing و Ready Queue در NexaRoute

## نتیجهٔ اجرایی

چرخه‌ای که برای NexaRoute تعریف کرده‌اید، در نسخهٔ فعلی تقریباً به‌صورت واقعی پیاده شده است. هنگام راه‌اندازی، gateway می‌تواند همهٔ deploymentهای فعال را probe کند. فقط deploymentهایی که probe موفق دارند به وضعیت `healthy` می‌رسند و در حالت `ready_queue` در صف انتخاب قرار می‌گیرند. اگر یک درخواست واقعی یا probe پس‌زمینه شکست بخورد، deployment بلافاصله quarantine می‌شود و از ready queue خارج می‌گردد. سپس supervisor برای آن deployment تا پنج recovery probe انجام می‌دهد. اگر همهٔ این تلاش‌ها شکست بخورد، deployment برای مقدار پیش‌فرض ۱۸۰۰ ثانیه، یعنی ۳۰ دقیقه، در cooldown می‌ماند و بعد دوباره وارد چرخهٔ recovery می‌شود.

یک تفاوت مهم با تصور «صف فیزیکی» وجود دارد: صف در کد یک کانتینر جداگانه نیست. ready queue به‌صورت محاسباتی از deploymentهایی ساخته می‌شود که state آن‌ها `healthy` است. این طراحی ساده‌تر است و بعد از reload تنظیمات stale queue باقی نمی‌گذارد، اما برای مشاهده‌پذیری بهتر باید snapshot صف آماده و دلایل خروج از صف صریح‌تر نمایش داده شوند.

در این ممیزی یک اصلاح مستقیم نیز اعمال شد. sweep دوره‌ای دیگر deploymentهای سالم را دوباره probe نمی‌کند. مدل‌های سالم در صف آماده باقی می‌مانند و supervisor بودجهٔ probe را به مدل‌های `unknown`، `degraded` و `half_open` اختصاص می‌دهد. probe اولیه و اجرای دستی همچنان همهٔ deploymentهای لازم را بررسی می‌کنند.

## چرخهٔ فعلی، مرحله‌به‌مرحله

### مرحلهٔ اول: ثبت deploymentها

هر provider فعال و هر model فعال آن به یک deployment مستقل تبدیل می‌شود. شناسهٔ deployment به شکل `provider-id/model-id` ساخته می‌شود. بنابراین یک provider با پنج مدل، پنج واحد مستقل در routing دارد. این نکته برای شکست، latency و recovery جداگانه اهمیت دارد.

کدهای اصلی این مرحله در `internal/router/router.go` قرار دارند. تابع `Reload` فهرست deploymentها را می‌سازد و آن را با تنظیمات جدید جایگزین می‌کند.

### مرحلهٔ دوم: probe اولیه

در `cmd/gateway/main.go`، اگر `probe.enabled` و `probe.on_start` فعال باشند، پیش از شروع listener وب، `pe.Prime(ctx)` اجرا می‌شود. به همین دلیل در حالت عادی اولین درخواست کاربر قبل از پایان health sweep اولیه پذیرفته نمی‌شود.

در probe اولیه، همهٔ deploymentهای فعال بررسی می‌شوند. مقدار پیش‌فرض `probe.max_tokens` برابر یک است. این مقدار برای اثبات دسترسی، احراز هویت، پاسخ‌گویی و latency مناسب است؛ اما معیار کیفیت هوش مدل نیست.

### مرحلهٔ سوم: ورود به ready queue

وقتی probe موفق باشد، `health.Manager.RecordSuccess` وضعیت deployment را به `healthy` تغییر می‌دهد. در strategy برابر `ready_queue`، router فقط deploymentهای `healthy` را به فهرست candidate اضافه می‌کند. deploymentهای `unknown`، `degraded`، `half_open` و `cooldown` وارد candidate عادی نمی‌شوند.

در داخل ready queue، ترتیب فعلی بر اساس priority کمتر، weight بیشتر و سپس شناسهٔ پایدار است. بنابراین priority و weight کنترل‌های اصلی ترتیب هستند و latency در ترتیب `ready_queue` نقش تعیین‌کننده ندارد.

### مرحلهٔ چهارم: استفادهٔ ترافیک واقعی

هر درخواست Claude یا OpenAI ابتدا candidateهای سالم و سازگار با capability را دریافت می‌کند. اگر پاسخ موفق باشد، state deployment با latency جدید به‌روزرسانی می‌شود. اگر transport error، timeout یا خطای failover-eligible دریافت شود، deployment در حالت `ready_queue` quarantine می‌شود و supervisor آن فعال می‌گردد.

درخواست فعلی می‌تواند به candidate بعدی failover کند، مشروط به اینکه هنوز response bytes به client ارسال نشده باشد. پس از شروع stream، gateway نمی‌تواند یک stream شکسته را به‌صورت صادقانه روی مدل دیگری ادامه دهد.

### مرحلهٔ پنجم: خروج فوری از صف

`health.Manager.Quarantine` deployment را فوراً از وضعیت آماده خارج می‌کند. این رفتار مهم است، چون درخواست بعدی نباید دوباره همان deployment خراب را انتخاب کند. در این مرحله مدل الزاماً هنوز cooldown نیست؛ ابتدا در recovery قرار می‌گیرد تا supervisor پنج probe مشخص را اجرا کند.

### مرحلهٔ ششم: supervisor recovery

`internal/probe/engine.go` برای هر deployment فقط یک recovery loop هم‌زمان اجازه می‌دهد. این loop تا پنج probe انجام می‌دهد؛ مقدار قابل تنظیم `probe.recovery_attempts` است و مقدار پیش‌فرض پنج است. فاصلهٔ probeهای recovery با `probe.recovery_retry_ms` کنترل می‌شود و مقدار پیش‌فرض آن ۵۰۰ میلی‌ثانیه است.

اگر هر probe موفق شود، deployment دوباره `healthy` می‌شود و بدون صبر برای پایان پنج تلاش به ready queue برمی‌گردد. اگر همهٔ تلاش‌ها شکست بخورند، `EnterCooldown` آن را برای `routing.cooldown_seconds` کنار می‌گذارد. مقدار پیش‌فرض این زمان ۱۸۰۰ ثانیه است.

### مرحلهٔ هفتم: بازگشت پس از cooldown

پس از پایان cooldown، state به `half_open` منتقل می‌شود. supervisor دوباره recovery budget را اجرا می‌کند. موفقیت باعث بازگشت به ready queue می‌شود و شکست کامل باعث cooldown بعدی می‌گردد. این چرخه تا حذف deployment از configuration یا shutdown سرویس ادامه پیدا می‌کند.

## اصلاح اعمال‌شده در این ممیزی

پیش از این، sweep دوره‌ای همهٔ deploymentها را دوباره probe می‌کرد؛ حتی deploymentهایی که سالم بودند و در ready queue قرار داشتند. این کار از نظر availability مفید بود، اما با چرخهٔ مدنظر شما یکسان نبود و در مقیاس ۱۰۰ مدل ترافیک probe غیرضروری ایجاد می‌کرد.

اکنون در `runOnce(ctx, false)`، deploymentهای `healthy` به‌عنوان `skipped_healthy` ثبت می‌شوند و probe نمی‌شوند. probe اولیه با `force=true` همچنان همهٔ deploymentها را بررسی می‌کند. برای این رفتار تست regression در `internal/probe/engine_ready_queue_test.go` اضافه شده است.

این تصمیم یک trade-off دارد: اگر deployment سالمی بدون هیچ درخواست یا recovery علامت خرابی بدهد، تا sweep بعدی از آن باخبر نمی‌شویم؛ اما چون sweep فقط deploymentهای خارج از ready queue را بررسی می‌کند، یک deployment سالم به‌طور عمدی داخل صف باقی می‌ماند. برای پوشش این حالت در مقیاس production، باید revalidation سبک و جداگانه‌ای با نرخ پایین‌تر یا passive signalهای provider اضافه شود، نه اینکه همهٔ مدل‌ها در هر sweep با یک probe کامل بررسی شوند.

## مقایسه با gatewayهای معتبر

| ابزار | چیزی که به NexaRoute می‌آموزد | تفاوت با چرخهٔ NexaRoute |
|---|---|---|
| LiteLLM | health-check routing، حذف proactive deployment خراب، policy جداگانه برای تعداد خطاهای هر نوع، نادیده‌گرفتن خطاهای transient مثل ۴۰۸ و ۴۲۹ | LiteLLM نیز health check پس‌زمینه دارد، اما cooldown و allowed-fails را به تفکیک نوع خطا قابل تنظیم می‌کند. NexaRoute فعلاً state machine اختصاصی پنج recovery probe و cooldown ۳۰ دقیقه‌ای دارد، ولی policy خطاها هنوز ساده‌تر است. |
| Envoy AI Gateway | endpoint picker با queue depth، KV-cache، وضعیت endpoint و اطلاعات runtime برای انتخاب مقصد | Envoy روی ظرفیت لحظه‌ای inference تمرکز بیشتری دارد. NexaRoute فعلاً priority، weight، health و latency را دارد، اما queue depth یا token throughput را وارد انتخاب نمی‌کند. |
| Kong AI Gateway | ترکیب round-robin، least-connections، lowest-usage، lowest-latency، priority و circuit breaker با `max_fails` و `fail_timeout` | NexaRoute circuit و failover را برای deployment پیاده کرده، اما least-busy و lowest-usage به‌صورت کامل ندارد. این مهم‌ترین مسیر بهبود performance است. |
| Portkey | fallbackهای composable که می‌توانند داخل load balancer یا conditional router تو در تو قرار بگیرند، و trace کامل تلاش‌ها | NexaRoute fallback را در request loop انجام می‌دهد و event ثبت می‌کند، اما زنجیرهٔ routing قابل ترکیب و policy graph عمومی ندارد. |
| OpenRouter | جداسازی انتخاب model از انتخاب provider، درنظرگرفتن outage اخیر، قیمت، throughput، latency و fallback providerها | NexaRoute deployment را واحد اصلی می‌داند که انتخاب model/provider را یکجا انجام می‌دهد. این برای self-hosted routing ساده‌تر است، اما برای routing cost/performance چندمدلی به metadata بیشتری نیاز دارد. |

## آیا این طراحی «بی‌سابقه» است؟

نمی‌توان به‌صورت قابل اثبات گفت هیچ gateway دیگری چنین چرخه‌ای نساخته است. LiteLLM health-check driven routing، Kong circuit breaker، Envoy endpoint picker و OpenRouter provider health هرکدام بخش‌هایی از این ایده را دارند. بخش متمایز NexaRoute در ترکیب این اجزاست: probe اولیهٔ کل deploymentها، ready queue محاسباتی، quarantine فوری پس از خطای واقعی، recovery budget دقیق پنج‌تایی، و cooldown سی‌دقیقه‌ای با بازگشت supervisor.

ادعای دقیق و قابل دفاع این است که NexaRoute یک **ترکیب self-hosted و deployment-centric از proactive readiness و supervised recovery** ارائه می‌کند؛ نه اینکه هیچ مشابهی در صنعت وجود نداشته باشد.

## معماری پیشنهادی برای نسخهٔ بعدی

### تفکیک سه نوع سیگنال سلامت

سیگنال اول، **active readiness probe** است که برای deploymentهای unknown، degraded و half-open اجرا می‌شود. سیگنال دوم، **passive request outcome** است که از درخواست‌های واقعی latency، timeout، status code و stream failure را می‌گیرد. سیگنال سوم، **capacity signal** است که active requests، queue depth، token rate و نرخ خطا را وارد تصمیم routing می‌کند.

این سه سیگنال نباید در یک شمارندهٔ ساده مخلوط شوند. خطای ۴۰۱ معمولاً credential را خراب می‌کند، خطای ۴۲۹ ظرفیت یا quota را نشان می‌دهد، timeout می‌تواند transient باشد، و ۵۰۰ ممکن است خرابی provider یا مدل باشد. برای هر نوع خطا threshold و cooldown جداگانه بهتر است.

### صف‌های منطقی

به‌جای یک صف فیزیکی، چهار مجموعهٔ منطقی نگهداری شود:

1. `Ready`: مدل‌هایی که probe اخیرشان موفق است و می‌توانند ترافیک بگیرند.
2. `Recovery`: مدل‌هایی که quarantine شده‌اند و supervisor برایشان تلاش می‌کند.
3. `Cooldown`: مدل‌هایی که budget recovery را تمام کرده‌اند.
4. `Unknown`: مدل‌های تازه‌وارد یا مدل‌هایی که staleness آن‌ها از حد مجاز گذشته است.

این مجموعه‌ها باید از state machine مرکزی تولید شوند تا reload و حذف provider باعث باقی‌ماندن pointer قدیمی نشود.

### انتخاب ظرفیت‌محور

پس از فیلتر `Ready`، router باید score را با این سیگنال‌ها بسازد: health band، priority، weight، EWMA latency، in-flight requests، estimated remaining capacity، token throughput و خطای اخیر. برای شروع، `least_busy` و `lowest_latency` ارزش بیشتری از semantic routing دارند و بدون وابستگی به embedding قابل پیاده‌سازی هستند.

### جلوگیری از thundering herd

برای ۱۰۰ مدل، همهٔ recoveryها نباید دقیقاً در یک لحظه آغاز شوند. recovery باید jitter محدود، concurrency budget مشترک و backoff داشته باشد. cooldown نیز باید از یک scheduler مرکزی با deadline heap استفاده کند تا برای هر مدل goroutine خوابیدهٔ طولانی ساخته نشود.

### معیارهای موفقیت

نسخهٔ بعدی باید این invariantها را تست کند:

- deploymentی که `healthy` نیست، در `ready_queue` انتخاب نمی‌شود.
- deploymentی که request آن failover-eligible است، قبل از candidate بعدی quarantine می‌شود.
- هر deployment حداکثر یک recovery loop هم‌زمان دارد.
- recovery موفق در تلاش اول، چهار تلاش باقی‌مانده را لغو می‌کند.
- بعد از پنج شکست، cooldown دقیقاً از زمان آخرین recovery failure محاسبه می‌شود.
- یک deployment سالم در sweep عادی probe نمی‌شود، مگر اینکه staleness policy آن را دوباره به `unknown` منتقل کند.
- در زمان reload، deployment حذف‌شده دیگر recovery probe دریافت نمی‌کند.
- در صورت unhealthy شدن همهٔ deploymentها، پاسخ باید دلیل قابل مشاهده داشته باشد و scheduler وارد loop بی‌نهایت retry نشود.

## جمع‌بندی نهایی

چرخهٔ اصلی موردنظر شما در NexaRoute وجود دارد و بخش‌های حساس آن با تست‌های ۲۰ provider و ۱۰۰ model، quarantine، recovery و ready-queue پوشش داده شده‌اند. اصلاح جدید، رفتار supervisor را به خواستهٔ شما نزدیک‌تر کرده است: مدل‌های آماده دوباره به‌صورت بی‌مورد probe نمی‌شوند و تمرکز روی مدل‌های خارج از صف است.

برای تبدیل NexaRoute به gatewayی واقعاً هوشمندتر، گام بعدی نباید صرفاً زیادکردن تعداد retry باشد. اول باید health signalها بر اساس نوع خطا تفکیک شوند. سپس باید ظرفیت لحظه‌ای و least-busy routing اضافه شود. در مرحلهٔ بعد، scheduler recovery با jitter و deadline queue باید جایگزین خواب مستقل برای هر مدل شود. این مسیر از الگوهای proven در LiteLLM، Envoy، Kong، Portkey و OpenRouter استفاده می‌کند، اما state machine پنج‌مرحله‌ای NexaRoute را حفظ می‌کند.

## منابع

[1]: https://docs.litellm.ai/docs/proxy/health_check_routing "LiteLLM Health Check Driven Routing"
[2]: https://docs.litellm.ai/docs/routing "LiteLLM Router and Load Balancing"
[3]: https://aigateway.envoyproxy.io/blog/endpoint-picker-for-inference-routing/ "Envoy AI Gateway Endpoint Picker for Inference Routing"
[4]: https://developer.konghq.com/ai-gateway/load-balancing/ "Kong AI Gateway Load Balancing"
[5]: https://docs.portkey.ai/docs/product/ai-gateway/fallbacks "Portkey AI Gateway Fallbacks"
[6]: https://openrouter.ai/docs/guides/routing/provider-selection "OpenRouter Provider Selection"
