# گزارش بررسی وضعیت NexaRoute — ۱۰ اکتبر ۲۰۲۶

## جمع‌بندی اجرایی

مخزن در وضعیت «کاملاً تمام‌شده» نیست، اما گلوگاه مهم شاخهٔ فعال WP4 برطرف شده است: پوشش statement سراسری Go از **75.6٪** به **85.12٪ (16,885 از 19,837 statement)** رسیده و گیت مستندشدهٔ 85٪ پاس می‌شود. گیت‌های محلی و چهار چک GitHub نیز روی head `aa5a864` سبز شدند. PR شمارهٔ 224 برای review آماده است، اما ادغام نشده است.

## چه کاری انجام شد

در این ادامه، بدون تغییر منطق تولیدی برنامه، آزمون‌های رفتاری جدید برای مسیرهای دارای ریسک و پوشش پایین افزوده شد: احراز هویت و نشست OIDC/RBAC و audit پایدار، اعتبارسنجی پیکربندی و تصمیم‌گیری، APIهای مدیریتی و endpointهای مجازی، ترجمهٔ پروتکل‌های OpenAI/Anthropic/Responses، probe و سلامت provider، استخراج ویژگی ورودی، ارزیابی، cache/authz و کل زیرسیستم ویدئو شامل API، هزینه، صف، ذخیره‌سازی، provider جعلی، composition و orchestrator. آزمون‌های provider شبکهٔ واقعی مصرف نمی‌کنند و برای مسیرهای HTTP از `httptest` استفاده شده است.

## نتایج راستی‌آزمایی محلی

| گیت | نتیجه |
|---|---|
| `go test -count=1 -coverprofile=... ./...` | **موفق** — پوشش 85.12٪ |
| `go test -race -count=1 ./...` | **موفق** |
| `./scripts/verify.sh` | **موفق** — شامل vet، race، browser control-plane، Live Visual Agent، fuzz محدود و buildهای Linux amd64/arm64 |
| `./scripts/smoke-local.sh` | **موفق** — runtime، UI، admin persistence، redaction و token-count fallback |
| `./scripts/build-release.sh v0.7.0` | **موفق** — artifactها فقط محلی ساخته شدند و منتشر نشدند |
| `./scripts/test-install.sh` | **موفق** — نصب پاک، checksum، انتخاب معماری، ارتقا و حفظ تنظیمات |
| `git diff --check` و قالب‌بندی Go | **موفق** |

در نخستین اجرای کامل `verify.sh` یک‌بار آزمون shutdown برنامه در race suite از مهلت پنج‌ثانیه‌ای گذشت. اجرای همان آزمون ۱۰ بار، اجرای دوبارهٔ race سراسری و اجرای دوم `verify.sh` همگی موفق شدند. چک تازهٔ `CI/verify` روی head `aa5a864` نیز سبز شد. این یک مشاهدهٔ flake در timeout آزمون بود؛ هر push بعدی باید دوباره CI همان head را بگذراند.

ابزار `govulncheck` در همین sandbox در دسترس نبود، اما اسکن تازهٔ GitHub روی head `aa5a864` سبز شد. همچنین IdP واقعی شخص ثالث آزمایش نشد؛ مسیر OIDC با provider درون‌فرآیندی آزموده شده است.

## وضعیت GitHub و اقدام بعدی

آزمون‌ها با commit `9a837a8` به PR #224 ارسال شدند و گزارش وضعیت با commit مستنداتی `aa5a864` به همان شاخه رفت. چهار چک روی head نهایی `aa5a864` سبز شدند: `CI/verify`، `Go vulnerability scan`، `CodeQL (Go)` و `CodeQL`. PR اکنون **Ready for review** است و merge نشده. هر push بعدی head را عوض می‌کند و باید پیش از merge دوباره بررسی شود.

## آنچه هنوز در کل پروژه باز است

گذر از 85٪، باقی roadmap را به‌تنهایی کامل نمی‌کند. طبق handoff مخزن، کارهای مجزای بعدی شامل **WP5** (سیاست egress/SSRF، حفاظت IP خصوصی/metadata، جلوگیری از DNS rebinding و کنترل redirect/proxy)، **WP6** (اتصال ذخیره‌سازی پایدار/اشتراکی به runtime و آزمون بازیابی/هم‌زمانی)، **WP7** (abstraction برای keyring/KMS و چرخش امن کلید) و **WP8** (پذیرش نهایی از clean clone، پاکسازی مستندات و ممیزی شاخه‌ها) هستند. همچنین هیچ adapter ویدئویی واقعی خارجی در این checkpoint تأیید نشده است.

پس نتیجهٔ دقیق این است: **گلوگاه پوشش و گیت‌های محلی و راه‌دور شاخهٔ WP4 بسته شد؛ PR #224 روی head `aa5a864` آمادهٔ review است ولی merge نشده؛ و کل roadmap محصول هنوز تمام نشده است.**
