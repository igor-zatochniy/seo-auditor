# Матриця політик AI-краулерів

## Що вимірюється

Для кожної збереженої цілі SEO-запуску Auditor читає вже отриманий `robots.txt` і окремо обчислює правила для дев'яти офіційно документованих product tokens:

| Провайдер | Токен | Призначення |
| --- | --- | --- |
| Google | `Googlebot` | Google Search та AI-функції пошуку |
| Google | `Google-Extended` | використання для визначених моделей/grounding-продуктів |
| OpenAI | `OAI-SearchBot` | пошук ChatGPT |
| OpenAI | `GPTBot` | можливе використання для навчання моделей |
| Anthropic | `Claude-SearchBot` | пошук у Claude |
| Anthropic | `ClaudeBot` | збір даних для покращення моделей |
| Perplexity | `PerplexityBot` | пошукове індексування Perplexity |
| Apple | `Applebot` | Spotlight, Siri та пошук Apple |
| Apple | `Applebot-Extended` | використання для навчання базових моделей Apple |

`Google-Extended` та `Applebot-Extended` — керувальні токени `robots.txt`, а не окремі HTTP User-Agent. Їхня директива оцінюється за правилами груп robots; у журналах запитів шукати окремий User-Agent для них не слід. Користувацькі fetchers на кшталт `ChatGPT-User` та `Perplexity-User` не трактуються як звичайні пошукові краулери в цій матриці.

Для `Applebot` враховується правило провайдера: якщо окремої групи Applebot немає, але є Googlebot, застосовуються правила Googlebot, а не `*`. Власна група Applebot має пріоритет; `Applebot-Extended` не підміняє її.

## Стани та довіра

- **Дозволено правилами** — для URL не знайдено ефективної `Disallow`-директиви у вибраній групі.
- **Заборонено правилами** — URL збігається з ефективною `Disallow`-директивою.
- **Не перевірено** — немає збереженого знімка, перевірку зупинив контекст/ліміт або джерело недоступне.
- **Конфлікт** — `Allow` і `Disallow` однакової специфічності одночасно збігаються. За правилами зіставлення перемагає `Allow`, але конфігурація окремо підсвічується.

Для кожного токена зберігаються безпечний URL `robots.txt`, час перевірки, HTTP-статус, рівень упевненості, вибрана директива та пояснення. Значення query у директивах маскуються до збереження. Старі запуски без `ai_policies` показують unknown, а не вигадану дозвільну політику.

HTTP 4xx для `robots.txt` позначається як недоступна політика з неявним дозволом за RFC 9309; це не доводить, що провайдер може реально завантажити сторінку. Мережева помилка та HTTP 5xx дають unknown. Основний crawler не послаблює власну fail-closed поведінку через необов'язковий AI-аналіз.

Матриця не підтверджує запит із мережі провайдера, доступність через WAF, фактичне сканування, індексацію, snippet або цитування. Зокрема, дозвіл `Googlebot` не гарантує індексацію, а `Google-Extended` не є окремим fetcher. Google, OpenAI та інші провайдери розділяють пошукове використання і правила для навчання/інших продуктів; ці наміри не об'єднуються в один SEO-score.

## Бажана політика та зміни

У GEO-профілі для кожного токена можна вибрати бажаний стан: allowed, blocked або unknown. Unknown є типовим значенням і не створює порушення. `Policy mismatch` рахується лише коли власник явно вибрав allowed/blocked і фактичне правило відомо. Заборона навчального бота сама по собі не є SEO-помилкою.

Для кожного планового GEO-запуску бажана політика зберігається окремим незмінним snapshot. Нові налаштування профілю застосовуються до наступного запуску й не переписують історію. `Policy drift` порівнює відомі стани одного URL між поточним і попереднім готовим запуском профілю. Порівняння використовує HMAC fingerprint і той самий `fingerprint_key_id`; після ротації ключа або за неповних даних результат — «не порівнюється», не помилкова зміна. Drift не пояснює, хто і коли змінив файл на сервері.

## Ліміти й API

Текст robots.txt завантажується один раз на origin. Основні правила crawler компілюються окремо; додаткова GEO-матриця обмежена 4096 правилами на вибрану групу, 2 MiB оцінених compiled policies та 250 ms. Ідентичні групи перевикористовуються. Перевищення робить лише відповідний AI-стан unknown і не повинно переривати звичайний SEO-запуск. Кеш має загальний обмежений бюджет.

- `GET /api/geo/ai-policy-specs` — підтримувані токени, призначення й офіційні сторінки документації.
- `GET /api/geo/reports/{id}/ai-discoverability?limit=50&after=...` — зведення та keyset-сторінка URL з фактичними директивами, бажаним станом, конфліктами й порівнюваним drift.
- `POST /api/geo/profiles` — приймає `desired_ai_policy` як map `token -> allowed|blocked|unknown`.
- `POST /api/geo/profiles/{id}/update` — оновлює бажану політику для наступних запусків.

Міграція `042_geo_ai_policy.sql` додає поля налаштування і snapshots, не переписуючи попередні міграції або SEO-результати. Перед оновленням наявної PostgreSQL-БД створіть backup і застосовуйте сумісний binary із міграцією 42.

## Документація провайдерів

- [Google crawlers and fetchers](https://developers.google.com/crawling/docs/crawlers-fetchers/google-common-crawlers) та [Googlebot](https://developers.google.com/search/docs/crawling-indexing/googlebot)
- [OpenAI bots](https://developers.openai.com/api/docs/bots)
- [Anthropic web crawler controls](https://support.anthropic.com/en/articles/8896518-does-anthropic-crawl-data-from-the-web-and-how-can-site-owners-block-their-web-crawler)
- [Perplexity crawlers](https://docs.perplexity.ai/docs/resources/perplexity-crawlers)
- [Applebot](https://support.apple.com/en-us/119829)
- [RFC 9309](https://www.rfc-editor.org/rfc/rfc9309)
