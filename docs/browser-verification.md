# Перевірка browser-first інтерфейсу

Дата: 28 вересня 2026 року.

Базовий commit: `dcd6aea0d3b30f2197c402ba35218b62ac02b257`.
Новий інтерфейс реалізовано у робочому дереві; нового commit SHA та exact-head
GitHub Actions run ще немає. Попередній freeze tag не змінювався.

## Межі змін

```text
start-auditor.cmd -> Docker Compose -> PostgreSQL + seo-auditor serve
                                               |
                      localhost HTTP + embedded HTML/CSS/JS
                                               |
                     AuditManager: один активний web audit
                                               |
                  transaction: audit_runs + explicit run targets
                                               |
run -> pages_to_scan snapshot ------> executeCapturedAuditRun
                                               |
                  generation-fenced claim -> workers -> saveResults
                                               |
                      heartbeat / leases / bounded shutdown
                                               |
                     terminal state -> retained URL cleanup
                                               |
            PostgreSQL -> paginated API / SQL analytics / HTML / CSV
```

Web input не записується до `pages_to_scan`. Створення запуску й усіх його
targets атомарне. Resume використовує збережений snapshot і наявний механізм
ownership takeover. Crawler, SSRF dialer, SEO parser, robots policies,
generation fencing і transactional result persistence не переписувалися.

Міграції та залежності не змінено: використовується schema version 11.
Запуск binary без аргументів і команда `run` зберігають batch semantics.
Compose за замовчуванням запускає `serve`; старий `run-audit.ps1` явно обирає `run`.

## Файли

| Область | Файли |
| --- | --- |
| Спільний engine | `main.go`, `audit_manager.go`, `explicit_targets.go` |
| HTTP / API | `web_server.go`, `web_handlers.go`, `web_models.go`, `web_queries.go`, `web_analytics.go` |
| Frontend | `web/templates/index.html`, `web/static/app.css`, `web/static/app.js` |
| Експорт | `report.go`, `report_template.go`, `report_extended.go` |
| Конфігурація | `internal/config/web.go`, `.env.example`, `docker-compose.yml` |
| Windows | `start-auditor.cmd`, `start-auditor.ps1`, `stop-auditor.cmd`, `run-audit.ps1` |
| Тести | `web_test.go`, `web_integration_test.go`, `internal/config/web_test.go`, `start-auditor_test.ps1`, `report_integration_test.go` |
| CI / документація | `.github/workflows/ci.yml`, `README.md`, цей документ |

## HTTP API

| Метод | Шлях | Призначення |
| --- | --- | --- |
| GET | `/healthz`, `/readyz` | Liveness та готовність PostgreSQL |
| POST | `/api/session` | Обмін bearer token на локальну session cookie |
| GET | `/api/schema` | Публічні поля звіту та ліміт URL |
| GET / POST | `/api/audits` | Історія / створення аудиту |
| GET | `/api/audits/{id}` | Метадані запуску |
| GET | `/api/audits/{id}/progress` | Поточні стани targets і відсоток завершення |
| GET | `/api/audits/{id}/analytics` | SQL-агрегати та розподіли |
| GET | `/api/audits/{id}/results` | Keyset pagination, пошук, whitelist фільтрів |
| POST | `/api/audits/{id}/cancel` | Graceful cancel |
| POST | `/api/audits/{id}/resume` | Відновлення resumable запуску |
| GET | `/api/audits/{id}/export/html` | Повний автономний HTML |
| GET | `/api/audits/{id}/export/csv` | Повний CSV |

Сторінки UI: `/`, `/audits`, `/audits/{id}`. Таблиця містить до 200 рядків
на сторінку; типове значення 50. Експорт охоплює весь запуск незалежно від
поточних UI-фільтрів. Running-запуски не експортуються як завершений звіт.

## Можливості UI

- URL form із лічильниками, deduplication та серверною валідацією.
- Polling progress, статуси targets, час виконання, cancel і resume.
- Історія запусків, технічні розподіли без штучного SEO score.
- Пошук, 21 фільтр, keyset pagination, вибір колонок у localStorage.
- Деталі кожної сторінки: усі 58 публічних persisted SEO / telemetry полів.
- HTML і CSV export; автоматичні `latest-report.html` та archive reports.
- Адаптивний desktop/mobile layout, без CDN, npm та frontend runtime.

## Security та privacy

- Порт Compose прив'язаний до `127.0.0.1`. Для API потрібен випадковий access
  token навіть за помилкового зовнішнього port publishing.
- Launcher генерує секрети криптографічним RNG. Наявні DB credentials не
  перезаписуються. Token передається через URL fragment; JS видаляє його з
  адреси перед API-запитом. Cookie має `HttpOnly` та `SameSite=Strict`.
- Змінювальні запити потребують same-origin JSON, додаткового request header
  і автентифікації. CORS не відкривається. Є CSP, nosniff, no-referrer, DENY.
- URL проходять ту саму нормалізацію, що й crawler. Transport-level DNS/IP
  validation залишається основною SSRF boundary; private targets заборонені.
- API та export використовують явний allowlist полів: без `request_url`,
  HMAC key та fingerprint. URL і URL усередині metadata повторно маскуються.
- HTML генерує `html/template`; JS використовує `textContent`, не `innerHTML`.
  CSV нейтралізує formula prefixes, включно з leading whitespace/control.
- SQL values параметризовані; filters та columns походять із compile-time
  allowlists. Немає user-controlled SQL expression.
- PostgreSQL лишається довіреним сховищем: raw URL потрібні до завершення
  або відновлення запуску. Existing retention/cleanup semantics збережено.

## Конкурентність

AuditManager дозволяє один активний web audit; наступний POST отримує 409.
Скасування не закриває channels довільно: спільний engine припиняє scheduling,
очікує producers, зберігає результати та виконує bounded finalization.
Web-сервер не змінює global logger для окремого запуску.

HTTP query concurrency обмежена трьома запитами, приймання списку URL одним.
Input body має ліміт до 8 MiB; максимум 10000 URL, по 2048 символів.
Pool у serve має `WORKERS + 6` з'єднань. Агрегати обчислює PostgreSQL;
HTML/CSV читають результати потоково. Повний export використовує read-only
repeatable-read transaction для узгодженого набору даних.

## Виконані перевірки

| Перевірка | Результат |
| --- | --- |
| `go mod verify` | PASS |
| `go test -shuffle=on -count=10 ./...` | PASS |
| `go test -race -shuffle=on -count=10 ./...` | PASS |
| `go test -tags=integration -count=1 ./...` | PASS, повторний повний прогін 118 s |
| Web integration з `-race -count=10` | PASS |
| Ownership/shutdown + web stress, однаковий shuffle seed | PASS, `-race -count=10`, 35 s |
| `go vet ./...`, `staticcheck ./...` | PASS |
| `govulncheck ./...` | Уразливостей не знайдено |
| Обидва PowerShell launcher test suites | PASS |
| `docker compose config --quiet` | PASS |
| Application і PostgreSQL image build | PASS |
| Штатний Windows launcher без test overrides | PASS, відкриття браузера після readiness |
| Trivy HIGH/CRITICAL, `--ignore-unfixed`, обидва образи | PASS |
| Playwright desktop 1440 / mobile 390 | PASS для основного сценарію та Resume |
| SBOM | Створено локальний SPDX JSON для фінального application image |
| Exact-head GitHub CI / freeze_evidence | Ще не запускався для цих незакомічених змін |

Нові тести перевіряють atomic initialization/rollback, відсутність змішування
global targets, повний набір полів, агрегати, pagination, privacy, HTML escaping,
CSV formulas, cancel до першого snapshot read, HTTP cancel, 409 для другого
аудиту та resume після crash зі збереженням уже готових результатів.

Один повний прогін під час Docker build отримав timeout у незміненому recovery
тесті з 1-секундним DB budget. Окремий повтор цього тесту 10 разів і послідовний
повний integration suite пройшли без зміни timeout або production recovery.
Паралельний stress/Trivy прогін також спричинив DB timeout у browser-created
run. Він залишився resumable: перевірка кнопки Resume відновила той самий run,
зберегла всі три результати та відкрила повний звіт без JavaScript-помилок.
Перезапуск Docker під час паузи зупинив тестові контейнери; вони були відновлені
без видалення volumes. Помилки недоступної тестової БД не рахуються як PASS.
На цій Windows-машині порт 5432 недоступний для bind. У локальному ігнорованому
`.env` встановлено `DB_PORT=55436`; credentials не змінено. Штатний Compose
project `seoauditor` запускає UI на 8080, не змінюючи сторонню БД на 5433.

## Межі перевірки та verdict

Це локальний single-instance UI, не public multi-user service. Немає окремих
користувачів, RBAC, HTTPS termination або renderer для JavaScript-сторінок.
Одночасний запуск незалежного batch/container поверх тієї самої БД не
координується AuditManager як другий web audit. Не запускайте старий launcher
паралельно з web audit: він замінює parser container у тому самому Compose project.

Повний worst-case memory/soak тест web dashboard на 10000 великих записів
не виконувався. Усі 58 поля доступні, але пороги SEO parser та обмеження
source-level аналізу не змінено.

**NOT FREEZE READY:** нова версія ще не має commit SHA та успішного exact-head
CI з `freeze_evidence`. Локальний успішний тест не замінює цю release-перевірку.
