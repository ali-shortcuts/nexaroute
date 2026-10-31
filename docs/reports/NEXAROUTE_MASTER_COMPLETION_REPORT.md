# گزارش جامع مأموریت تکمیل NexaRoute — ۲۰۲۶-۱۰-۳۱

مأموریت: اجرای مستقیم در نشست اصلی (بدون واگذاری به OpenCode/DeepSeek)، بازخوانی تازهٔ GitHub،
اجرای تست‌های محلی روی سرشاخه‌های دقیق، و حکم صادقانهٔ انتشار.
این گزارش در `docs/reports/NEXAROUTE_MASTER_COMPLETION_REPORT.md` نیز ثبت می‌شود (PR مستندات).

## ۱. وضعیت مخزن

- SHA آغازین = SHA پایانی: `815513b861b3ba38bb51ba138dddc39071718753` (مرج #791؛ در این نوبت مرجی انجام نشد)
- ایشوها: ۲۱۷ در مجموع، ۱۰ باز، ۲۰۷ بسته (اسنپ‌شات مأموریت «۸ باز / ۲۰۹ بسته» بود؛
  اختلاف دقیقاً #648 و #653 است که در ۰۵:۰۶ تا ۰۵:۱۰ امروز با دستور مالک بازگشایی شدند — بخش ۶)
- PRها: ۵۷۴ در مجموع، ۶ باز، ۴۹۵ مرج‌شده، ۷۳ بستهٔ بدون مرج (۵۶۸ بسته منهای ۴۹۵)
- PRهای باز (همه روی base کهنه، همه نیازمند rebase + CI تازه — بخش ۳):
  #788 (head ‏0df788e‏، ۱۲ کامیت عقب)، #786 (draft، ‏ad130f0‏، ۱۲ عقب)،
  #781 (‏fc89201‏، ۳۶ عقب)، #779 (draft، ‏5cbdc29‏، ۴۴ عقب)،
  #773 (draft، ‏8146308‏، ۴۴ عقب)، #771 (‏a302201‏، ۴۴ عقب)
- CI پس‌ازمرج روی 815513b8: هر ۹ context قوانین (verify، ci-soak-smoke، fuzz-smoke،
  integration-postgres، integration-redis، container، Go vulnerability scan، CodeQL (Go)،
  Fresh Go coverage) با موفقیت ثبت شده‌اند (راستی‌آزمایی ۲۰۲۶-۱۰-۳۱ ۰۰:۳۸).

## ۲. وضعیت هر ایشو

| ایشو | پیاده‌سازی | تست محلی | CI | پذیرش زنده | مانع | نتیجه |
|---|---|---|---|---|---|---|
| #646 LLM Providers | harness + runbook روی main؛ #773 validator و #786 dialect pin در شاخه‌های draft | livecanary + providers سبز؛ dialectها و auth_failure هر ۶ خانواده سبز (بخش ۴) | سبز روی سرشاخه‌های draft | BLOCKED | مجوز کتبی اپراتور + حساب/مدل/سقف هزینه؛ صفر فراخوانی پولی تاکنون | BLOCKED |
| #647 OIDC | روی main؛ #772 + #791 مرج شده | OIDC-focused روی main سبز (۲۳٫۱ ثانیه) | سبز روی 815513b8 | BLOCKED | tenant سازمانی قابل‌حذف + client محرمانه + mappingها + کاربران آزمون | BLOCKED |
| #648 Vault KV | روی main؛ #780 + #787 مرج شده | secrets سبز؛ loopback PASS (محدوده‌دار) | سبز روی 815513b8 | BLOCKED | Vault غیرloopback با HTTPS + mount خالی KV v2 + توکن‌های کم‌اختیار | OPEN (بازگشایی‌شده؛ بسته‌شدن زودهنگام بود) |
| #649 Vault Transit | nxs2 روی main (ADR 0007)؛ #775 مرج شده | secrets سبز (Transit + timeout/cancel) | سبز روی 815513b8 | BLOCKED | Vault با Transit فعال + چرخش کلید + بازیابی | BLOCKED |
| #650 Kubernetes/Helm | chart + guard روی main؛ #762 + #777 + #784 مرج شده | آفلاین/self-test روی main: PASS (failures=0, not_run=0, helm v3.19.0 از ‎~/workspace/.tools‎) | سبز روی 815513b8 | BLOCKED | هاست مجاز با docker+kind+kubectl+helm؛ این جعبه cgroup ندارد | BLOCKED |
| #651 ComfyUI | harness + #748 + #778 روی main | video/api سبز (۰٫۳۵ ثانیه، fixture) | سبز روی 815513b8 | BLOCKED | سرور + مدل + workflow + mapping + مجوز کتبی | BLOCKED |
| #652 Multi-host HA | #776 روی main؛ #788 در REVIEW ONLY | HA self-test روی main سبز؛ self-test شاخهٔ #788 سبز | سبز روی 815513b8 | BLOCKED | توپولوژی دو میزبانهٔ مستقل و مجاز؛ RPO/RTO چندمیزبانه هنوز هیچ مقداری ندارد | BLOCKED |
| #653 کیفیت/پوشش | gate روی main (#756) + ruleset فعال | gate روی 815513b8: PASS ‏۹۵٫۴۶٪‏ (بخش ۴) | Fresh coverage سبز روی 815513b8 | soak روی 815513b8 هنوز اجرا نشده | گیت‌های خارجی #646–#652 باز؛ تصمیم soak تازه با مالک | OPEN (بازگشایی‌شده؛ بسته‌شدن زودهنگام بود) |
| #654 چتر انتشار | ماتریس به‌روز شد (دو نظر در ۲۰۲۶-۱۰-۳۰) | — | — | — | همهٔ موارد بالا | OPEN؛ آخر بسته می‌شود |
| #664 soak | گزارش soak روی 7daa36d در main | — | — | soak ‏41040550445‏ روی 7daa36d: PASS | — | CLOSED (مشروع؛ با تأیید مالک) |
| #765 NexaFlow wiring | سه کاندید باز؛ هیچ‌کدام در main نیست | هر سه روی سرشاخهٔ خود سبز (base کهنه) | سبز روی سرشاخه‌های کهنه، نه main فعلی | — | انتخاب مالک (بخش ۵) | NEEDS_OWNER_DECISION |

## ۳. وضعیت هر PR

| PR | head | base | عقب از main | وضعیت | نگه‌داشتن |
|---|---|---|---|---|---|
| #788 | 0df788e | 7a2623a9 | ۱۲ | آمادهٔ بررسی، ‎[REVIEW ONLY]‎ | بدون review کامل و مجوز، مرج ممنوع |
| #786 | ad130f0 | 7a2623a9 | ۱۲ | draft | نیازمند مجوز آماده‌شدن |
| #781 | fc89201 | fc89961 | ۳۶ | آمادهٔ بررسی | نامزد #765؛ بدون انتخاب مالک مرج ممنوع |
| #779 | 5cbdc29 | 7daa36d | ۴۴ | draft | نامزد #765؛ نقص memory-default؛ بدون انتخاب و اصلاح مرج ممنوع |
| #773 | 8146308 | 7daa36d | ۴۴ | draft | نیازمند مجوز آماده‌شدن |
| #771 | a302201 | 7daa36d | ۴۴ | آمادهٔ بررسی | نامزد #765؛ بدنه پاک‌سازی‌شده (بدون ‎Closes #654‎)؛ بدون انتخاب مالک مرج ممنوع |

همهٔ PRها ‎mergeable=true‎ و ‎mergeable_state=clean‎ روی base کهنهٔ خود دارند؛
این روی main فعلی اعتباری ندارد و پس از rebase باید CI کامل روی head تازه سبز شود.

## ۴. تست‌های اجراشده در این نوبت (خودم، نه لین دیگر)

همه روی سرشاخهٔ دقیقِ ذکرشده؛ ‎TMPDIR=$HOME/workspace/.tmp‎؛ ‏Go 1.27.0‎ محلی، ‏CI Go 1.27.3‎.

| دستور | سرشاخه | نتیجه | مدت |
|---|---|---|---|
| `gofmt -l cmd internal scripts` | 815513b8 | خالی (تمیز) | — |
| `go vet ./internal/secrets/ ./internal/providers/ ./internal/providers/livecanary/` | 815513b8 | exit 0 | — |
| `go test -count=1 ./internal/secrets/` | 815513b8 | ok، exit 0 | ۱۱٫۸ ثانیه |
| `go test -count=1 -run 'OIDC\|Callback\|PKCE\|JWKS\|Issuer\|Login' ./internal/httpapi/` | 815513b8 | ok، exit 0 | ۲۳٫۱ ثانیه |
| `go test -count=1 ./internal/providers/livecanary/ ./internal/providers/` | 815513b8 | ok، exit 0 | ۳٫۵ + ۰٫۶ ثانیه |
| `go test -count=1 ./internal/video/api/` | 815513b8 | ok، exit 0 | ۰٫۳۵ ثانیه |
| `python3 scripts/ha-two-host-acceptance.py --self-test` | 815513b8 | self-test PASS، exit 0 | — |
| `python3 -m py_compile scripts/ha-two-host-acceptance.py` | 815513b8 | ok | — |
| `bash scripts/k8s-acceptance.sh --offline-only --self-test` | 815513b8 | PASS (failures=0, not_run=0)، exit 0؛ helm v3.19.0 از ‎~/workspace/.tools‎ (اجرای اول بدون helm روی PATH شکست خورد — نقص محیط، اصلاح شد) | — |
| coverage artifact ‏13003750345‎ از run ‏41108800247‎ (success روی 815513b8) + محاسبهٔ awk + ‏scripts/coverage-gate.sh | 815513b8 | ‏۴۲٬۰۳۴/۴۴٬۰۳۴ = ۹۵٫۴۵۸٪‏، حاشیه ‎+۲۰۱‎، gate PASS، exit 0؛ ‎environment.txt‎: commit ‏815513b8‏، ‏ref refs/heads/main‎، ‏Go 1.27.3‎؛ ۶۳ پکیج (در 7a2623a9: ۶۲ پکیج، ‏۴۲٬۰۱۸/۴۴٬۰۱۶ = ۹۵٫۴۶٪‎) | — |
| `python3 scripts/ha-two-host-acceptance.py --self-test` | 0df788e (#788) | self-test PASS، exit 0 | — |
| `go test -count=1 -v -run 'Dialect\|ErrorEnvelope' ./internal/providers/livecanary/` | 8146308 (#773) | PASS با subtestهای واقعی (stream-usage dialects، encode paths، error-envelope bodies) | ۰٫۰۱۲ ثانیه |
| validator: گزارش valid حداقلی + loopback با ‎--require-live‎ + فایل گمشده + schema ناشناس | 8146308 (#773) | VALID (exit 0)؛ loopback INVALID با خطای دقیق (exit 1)؛ missing exit 2؛ unknown schema exit 2 | — |
| `go test -count=1 -v -run 'AuthFailure' ./internal/providers/livecanary/` | ad130f0 (#786) | PASS: هر ۶ dialect (openai_compatible، anthropic_compatible، gemini، vertex، azure_openai، bedrock) + guardها | ۰٫۰۱۳ ثانیه |

توجه: پوشش کامل ‎`go test ./...`‎ روی این ماشین ۲ هسته‌ای به‌علت timeout سه‌دقیقه‌ای
پکیج ‎httpapi‎ تحت بار، در CI معتبر است نه محلی؛ CI روی head دقیق 815513b8 سبز است (بخش ۱).
‎`go tool cover -func`‎ خارج از ‎go.mod‎ جواب نمی‌دهد؛ محاسبه با awk روی ‎coverage.out‎ انجام شد
و با خروجی ‎coverage-gate.sh‎ (‏۹۵٫۵٪‏) تطابق دارد. تلاش اول محاسبه با awk اشتباه بود
(جمع counts به‌جای numStmt و ‏۹۹٫۷۸٪‏ کاذب) و پیش از ثبت اصلاح شد.

## ۵. مقایسهٔ #765 (از brief راستی‌آزمایی‌شدهٔ ۲۰۲۶-۱۰-۳۰ ۱۸:۲۴ + وضعیت سرشاخه‌های فعلی)

سرشاخه‌ها از زمان brief تغییری نکرده‌اند، پس جدول پابرجاست:

| معیار | #771 | #779 | #781 |
|---|---|---|---|
| Default-off | بله | بله | بله |
| Persistent store | file (مسیر پیش‌فرض زیر config) | memory پیش‌فرض (نقص: state با restart از دست می‌رود) | file پیش‌فرض، ‎store_backend/store_path‎ |
| Startup recovery | engine.Recover() با خطای fatal | ندارد | svc.Recover() یک‌بار پیش از serve |
| Browser self-boot | دست‌نخورده → production class روی 404 می‌پرد | باید اصلاح شود (SKIP بی‌دلیل + flag) | بازنویسی‌شده؛ production class روی binary واقعی PASS (۱۵ مورد، دستی، نه CI) |
| Binary E2E | loopback CompositeBackend — backendها واقعاً اجرا می‌شوند (سطح حملهٔ تازه) | — | loopback E2E + restart persistence |
| Authentication | GatewayClient + ‎gateway_token_ref‎ (env-secret-ref) | — | — |
| Worker id | ‎gateway-<pid>‎ (با هر restart عوض می‌شود؛ اثر روی lease راستی‌آزمایی‌نشده) | ثابت ‎gateway‎ | — |
| نقص‌های باز | body پیش‌تر ‎Closes #654‎ داشت — پاک‌سازی شد (راستی‌آزمایی ۲۲:۱۷) | memory-default + نبود Recover + SKIP مرورگر | PostgreSQL رد می‌شود (validation)؛ فقط file/memory |

توصیهٔ فنی (نه تصمیم مالک): معماری #781 — file پیش‌فرض fail-closed، ‎store_path‎ الزامی برای file،
Recover در سطح سرویس، و مرورگر بازنویسی‌شده. مرج هر سه‌تای رقیب ممنوع؛
پس از انتخاب مالک، فقط یکی می‌ماند و دو تای دیگر کنترل‌شده superseded می‌شوند.
تا آن زمان، patchهای قابل‌انتقال (self-boot مرورگر، file-default، Recover) آماده‌اند.

## ۶. ممیزی بسته‌شدن #648 و #653

- #648: بسته‌شدن ۲۰۲۶-۱۰-۳۰T18:25:26Z، دو ثانیه پس از مرج #787، بدون توضیح و بدون
  exclusion بازبینی‌شده، درحالی‌که staging BLOCKED بود. مکانیزم: بدنهٔ #787 جملهٔ
  نفی‌شدهٔ «does not close #648» داشت و GitHub نفی را نادیده می‌گیرد (auto-close محتمل؛
  پیام مرج تمیز بود، ‎commit_id‎ رویداد بسته‌شدن null). با دستور مالک ۲۰۲۶-۱۰-۳۱ ۰۵:۰۶
  («مشکوک‌ها را بررسی و درست کن») بازگشایی شد؛ نظر ‏6536629329‏؛ اکنون ‎state=open‎،
  ‎reason=reopened‎، ‎closed_at=null‎.
- #653: بسته‌شدن ۲۰۲۶-۱۰-۳۰T18:31:10Z، دو ثانیه پس از مرج #790، با همان مکانیزم
  (بدنهٔ #790: «never closes #653»). در زمان بسته‌شدن، soak روی 7a2623a9 هنوز اعتبارسنجی
  نشده بود و گیت‌های خارجی باز بودند؛ پس بسته‌شدن با شرط خودش تناقض داشت.
  با همان دستور مالک بازگشایی شد؛ نظر ‏6536629520‏؛ اکنون ‎state=open‎، ‎reason=reopened‎.
  soak روی 7a2623a9 بعداً اعتبارسنجی شد (run ‏41101588099‏، PASS)، ولی فقط به همان درخت
  می‌چسبد؛ دلتای تا 815513b8 شامل یک تغییر رفتاری است (#787: bound خروجی helper).
  soak تازه روی 815513b8 نیازمند تصمیم مالک است (از هدررفت در treadmill تا تثبیت main).
- #664: بسته‌شدن مشروع و با تأیید مالک؛ دست نخورده باقی ماند.

## ۷. نقص‌های اصلاح‌شده در این نوبت

هیچ نقص کدی در تست‌های محلی این نوبت یافت نشد — همهٔ سوئیت‌های اجراشده سبز بودند.
موارد ثبت‌شده (نه نقص محصول):
- محاسبهٔ اول coverage با awk اشتباه بود (۹۹٫۷۸٪ کاذب)؛ پیش از ثبت اصلاح و با gate تطابق داده شد.
- ‎scripts/check-acceptance-report.py‎ روی main وجود ندارد (فقط شاخهٔ draft #773) — طبق طراحی؛
  اجرای validator روی همان شاخه انجام شد.
- ‎k8s-acceptance.sh‎ در اجرای اول به‌علت نبود helm روی PATH شکست خورد؛ با helm v3.19.0
  از ‎~/workspace/.tools‎ بازاجرا شد و PASS گرفت — نقص محیط بود، نه کد.

## ۸. محدودیت‌ها (دقیقاً چه چیزی مسیر را باز می‌کند)

- #646: مجوز کتبی اپراتور (providerها + مدل‌ها + tenant + سقف هزینه) و credentialها فقط با نام env var.
- #647: tenant قابل‌حذف + confidential client + audience/role-claim mapping + کاربران آزمون + مجوز §3–§7 و §8 runbook.
- #648: Vault غیرloopback با HTTPS + mount خالی KV v2 + توکن‌های کم‌اختیار + بازبینی دستی policy.
- #649: Vault با Transit فعال + اجرای (a)–(h) + چرخش و بازیابی.
- #650: هاست مجاز با docker+kind+kubectl+helm.
- #651: سرور + مدل سازگار + workflow + mapping + مجوز کتبی هر چهار مورد.
- #652: توپولوژی دو میزبانهٔ مستقل و مجاز؛ RPO/RTO فقط با اندازه‌گیری واقعی.
- #765: انتخاب مالک از میان #771/#779/#781.
- #653: تصمیم مالک برای soak تازه روی 815513b8 (یا پذیرش اتصال soak به 7a2623a9 با مستندسازی دلتا).
- انتشار: کلمهٔ صریح و جداگانهٔ مالک برای tag/release، پس از بسته‌شدن واقعی همهٔ گیت‌ها.

## ۹. حکم انتشار

**NO-GO**

دلایل (هرکدام به‌تنهایی کافی است):
1. شش پذیرش زنده (#646، #647، #649، #650، #651، #652) BLOCKED‌ اند؛ هیچ‌کدام با fixture جایگزین نمی‌شود.
2. #765 بدون انتخاب مالک، در main نیست؛ سه کاندید روی base کهنه‌اند.
3. #653 باز است و soak به main فعلی نمی‌چسبد؛ #654 چتر باز است.
4. هیچ tag/release ساخته نشد و Production-ready اعلام نمی‌شود.

آنچه در این نوبت کامل شد: بازخوانی تازه، coverage روی main فعلی با gate سبز،
همهٔ سوئیت‌های محلی قابل‌اجرا روی سرشاخه‌های دقیق، راستی‌آزمایی بازگشایی‌های #648/#653،
و این گزارش با جدول‌های مستند. کار محلیِ باقی‌مانده: بازاجرای #650 پس از helm و rebase
کاندیداها پس از انتخاب مالک؛ کار زنده فقط با ورودی‌های اپراتور باز می‌شود.
