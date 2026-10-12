# Production quality gate

Це **ручна ізольована перевірка**, а не твердження, що продукт уже перевірений на всіх клієнтських сайтах. Звичайний CI залишається швидшим. Workflow `Production quality gate` запускається через GitHub Actions → Run workflow; автоматичний push не потрібен для локальних перевірок.

## Автоматичні сценарії

- Реальні HTTP-запити для 10 000 URL локальної фікстури, рівно одна збережена результуюча строка на target; перевірка очищення request URL і redaction.
- PostgreSQL-backed GEO аналіз усіх 10 000 успішних сторінок із 1000 запитів без усічення корпусу; додатковий малий corpus для перевірки повторюваності. Окремі integration-тести перевіряють найкращу ціль після 10 000-ї сторінки, ручний вибір, пошук, імпорт видимості та технічні помилки наприкінці джерела.
- П'ять окремих конкурентних доменів-фікстур: справжні обмежені сканування, звіти, повторне використання свіжих аудитів без нових HTTP-запитів.
- Загальний час load-сценарію до 10 хвилин; у Linux піковий RSS **тестового процесу** до 384 MiB. Це не вимірювання контейнера production з лімітом 256 MiB і не worst-case HTML/Chromium stress test.
- PostgreSQL container stop/start та recovery стану загиблого власника: pending/running targets → stale recovery → resume → 100 результатів без дублювання. Це не доказ безпеки довільного SIGKILL точно під час COMMIT.
- Наявні integration-сценарії ownership fencing, shutdown, lock contention, retry, міграцій, scheduler, GEO і redaction; додатковий upgrade 22 → 23 зі збереженням SEO/GEO-звітів.
- Playwright: створення SEO-аудиту, міжнародний звіт, GEO, екранування, desktop/mobile і відсутність змін початкових SEO-результатів.

Робочий 8081 і його PostgreSQL не використовуються. Аварійний тест зупиняє лише контейнер, чий label `seo-auditor.quality-gate` точно відповідає переданому nonce. Він не перезапускає Docker daemon і не видаляє volumes.

## Локальний запуск

Потрібні Go, Docker, Node.js ≥20 і Playwright. Створіть **нову тестову** БД з ім'ям `seo_quality_*`; не підставляйте робочий DSN. Integration suite може очищати свої fixture-таблиці, тому спільна БД непридатна.

```powershell
$env:SEO_QUALITY_GATE = 'isolated'
$env:DATABASE_URL = 'postgres://postgres:quality-test-only@127.0.0.1:55439/seo_quality_local?sslmode=disable'
$env:TARGET_FINGERPRINT_KEY = 'quality-fixture-only-fingerprint-key'
$env:GOMAXPROCS = '2'
$env:GOFLAGS = '-p=1'
$env:GOMEMLIMIT = '384MiB'
go test '-tags=integration,qualitygate' -count=1 -timeout=25m ./...
```

Для load-тесту домени `quality-own.test`, `quality-1.test` … `quality-5.test` резолвляться у loopback через DNS-фікстуру лише **тестового процесу**. Системні hosts, глобальні DNS і Docker daemon не змінюються. `ALLOW_PRIVATE_TARGETS=true` використовується лише всередині тестової конфігурації; звичайний runtime залишається `false`.

Для фізичного DB restart задайте `QUALITY_DB_CONTAINER` і `QUALITY_CONTAINER_NONCE`; без них цей сценарій явно пропускається. Для браузера встановіть залежності у `web/e2e` через `npm ci`, виконайте `npx playwright install chromium` і задайте `QUALITY_BROWSER=1`. Опційні `QUALITY_NODE_EXECUTABLE` і `QUALITY_BROWSER_CHANNEL` дозволяють використати локальний Node/Edge. [Запуск Playwright у CI](https://playwright.dev/docs/ci).

## Незалежна ручна розмітка

Синтетичний benchmark не замінює людську оцінку. Якщо `GEO_HUMAN_DATASET` не задано, `TestQualityGateHumanLabels` позначає перевірку **NOT VERIFIED**. Зелений job у такому режимі підтверджує автоматичні сценарії, але не незалежну точність на реальних проєктах.

Потрібен JSON із 100–200 незалежно розміченими запитами та знімками реальних, дозволених до використання сторінок. Не додавайте приватні дані або секретні URL у Git. Формат:

```json
{
  "kind": "human",
  "annotation_id": "dataset-review-identifier",
  "model": "local-lexical-v4",
  "topic_model": "local-topic-v1",
  "opportunity_model": "local-opportunity-v1",
  "pages": [],
  "facts": [],
  "cases": [
    {
      "query": "запит користувача",
      "target_url": "https://example.com/relevant-page",
      "cluster": "ідентифікатор підтеми",
      "tasks_reviewed": true,
      "required_tasks": ["перевірений_kind_рекомендації"],
      "forbidden_tasks": []
    }
  ]
}
```

`pages` відповідають `geo.Page`, `facts` — `geo.PageFacts`, task kinds — `Opportunity.Kind`. Для запиту без релевантної сторінки `target_url` порожній. Усі cases мають бути незалежно переглянуті, не автоматично заповнені поточним алгоритмом. Пороги gate: top-1 ≥0.85, pairwise cluster F1 ≥0.80, recall обов'язкових рекомендацій ≥0.90, заборонених рекомендацій — 0. Результат прив'язаний до версій моделей.

Повний production verdict також потребує дозволених реальних проєктів, реального історичного volume та окремого worst-case memory/JS профілю. Ці перевірки не видаються за виконані фікстурами.
