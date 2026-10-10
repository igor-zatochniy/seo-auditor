# Локальні межі безпеки

## Мережевий захист

Базовий Compose запускає `parser` у мережевому просторі `egress-guard`. Лише guard має `NET_ADMIN`; parser працює як UID 10001 без capabilities. До готовності IPv4/IPv6 firewall parser не запускається. Оновлення правил атомарне для кожної IP-сім’ї, кожні 15 с адреси сервісів перевіряються повторно. При помилці старі заборони не видаляються.

Нові TCP-з’єднання дозволено до публічних IPv4/IPv6, конкретної IPv4-адреси `postgres:5432` та, з rendering overlay, `browser:9222`. Docker DNS і loopback healthcheck мають окремі винятки. Приватні/службові діапазони, host gateway, metadata endpoint, IPv6 ULA/link-local і перехідні NAT64/6to4 мережі не дозволено. Native Go-запуск не має цього Docker-бар’єра. Firewall не захищає від адміністратора Docker і не замінює SafeDialContext, пароль БД чи web-authentication.

`ALLOW_PRIVATE_TARGETS` у базовому Compose завжди `false`. Інтеграційні тести локальних HTTP fixtures запускаються окремо, не в production parser. Не додавайте довільні IP до винятків. Після заміни guard/network через зовнішні Docker-команди перезапустіть parser через узгоджений `docker compose up -d --wait`; не використовуйте старий parser зі старим network namespace.

PostgreSQL не публікує порт на Windows. Коли прямий доступ справді потрібен:

```powershell
docker compose -f docker-compose.yml -f docker-compose.db-access.yml up -d postgres
```

Overlay обмежує порт адресою `127.0.0.1`; не використовуйте його для звичайного браузерного аудиту. Браузерний UI як і раніше доступний тільки через loopback.

## GitHub

Ruleset `Protect main with PR and CI` вимагає PR і успішний `Go checks and Docker build` від GitHub Actions; force push та видалення main заборонені. Bypass відсутній. Для сольного репозиторію кількість обов’язкових reviewer approvals дорівнює 0: автор не може затвердити власний PR. Зміни коду не публікуються автоматично; наступні зміни слід подавати через окрему гілку та PR. Новий workflow також містить `Windows backup checks`; додавати його до обов’язкових ruleset checks слід лише після публікації workflow й успішного виконання job, щоб не блокувати PR перевіркою, якої ще немає.

Dependabot security alerts/PR керуються налаштуваннями GitHub. `.github/dependabot.yml` додає щотижневі пропозиції Go modules, Docker images і GitHub Actions після публікації конфігурації. Автоматичне злиття не ввімкнено.

## Приватні файли та backup

Windows launcher прибирає inherited permissions `.env`: доступ мають поточний користувач, SYSTEM і Administrators. Linux launcher встановлює mode 600. Значення секретів не змінюються. Локальні адміністратори та Docker daemon залишаються довіреними. `.env` не є encrypted secret store.

`database-backup.ps1` потоком передає `pg_dump` у AES-256-CBC, додає HMAC-SHA-256 до заголовка та ciphertext і захищає випадкові ключі Windows DPAPI CurrentUser. Plaintext dump не створюється на диску. Backup має обмежений ACL; перезапис існуючого файлу заборонено. Цілісність перевіряється до decrypt/restore. Відновлення потребує `-ConfirmRestore`, виконується однією транзакцією без `--clean` у заздалегідь створену порожню БД.

```powershell
./database-backup.ps1 -File ./backups/seo-2026-10-10.seobak
./database-backup.ps1 -Mode Verify -File ./backups/seo-2026-10-10.seobak
./database-backup.ps1 -Mode Restore -ConfirmRestore -File ./backups/seo-2026-10-10.seobak -Database seo_restore
```

Для іншої назви БД/користувача передайте `-Database`, `-User`; для конкретного контейнера `-Container`. Паролі не передаються через аргументи: інструменти використовують локальне з’єднання всередині PostgreSQL container. Linux не підтримується DPAPI-скриптом.

**Межа відновлення:** DPAPI backup прив’язаний до Windows-профілю. Він не є переносним disaster-recovery backup. Втрата профілю/DPAPI-ключів може зробити його недоступним. Для окремого recovery host потрібен додатковий переносний backup, зашифрований стандартним age/GPG з офлайн recovery key. Завжди перевіряйте restore на окремій БД. Наявні незашифровані backup не конвертуються й не видаляються автоматично; вони також потребують приватного ACL. Backup не замінює контроль доступу до запущеної PostgreSQL.

## Контрольоване зберігання

Новий режим **retention** не запускається автоматично. Спочатку створіть і перевірте backup, оновіть binary та схему до поточної версії 23, потім виконайте суху перевірку:

```powershell
docker compose run --rm --no-deps parser retention --keep-days 90 --max-runs 100
docker compose run --rm --no-deps parser retention --keep-days 90 --max-runs 100 --apply
```

`--apply` безповоротно видаляє старі completed/completed_with_errors/canceled запуски разом із залежними SEO/GEO-результатами, імпортами та подіями. Кожен запуск видаляється окремою транзакцією; повторний виклик продовжує очищення. Running, failed/abandoned для resume, дві останні точки кожного розкладу/профілю, джерела pending порівнянь та актуальні competitor references захищені. До 100 запусків за виклик за замовчуванням, загальний deadline 5 хв. Невдалий виклик може вже завершити попередні транзакції; summary не означає атомарне видалення всього набору.

Очищення raw snapshot URL йде порціями до 100 лише для non-resumable terminal runs; failed/abandoned зберігають джерела для resume. `pages_to_scan` є постійним набором цілей, не transient queue: його URL не стираються без рішення власника. URL активних розкладів потрібні для наступного аудиту, включно з query; PostgreSQL залишається довіреним сховищем. Для остаточного забування конкретного вимкненого розкладу:

```powershell
docker compose run --rm --no-deps parser retention --forget-schedule SCHEDULE_UUID
docker compose run --rm --no-deps parser retention --forget-schedule SCHEDULE_UUID --apply
```

Видаляються source, пов’язані профілі й історія моніторингу лише цього розкладу. `--forget-schedule` не запускає загальне очищення аудитів чи snapshot URL; для нього потрібен окремий виклик retention без цього прапорця. Самі SEO-аудити залишаються, доки не спрацює їхня retention policy. Незавершена робота блокує забування. Пауза сама по собі не очищає source. Не додавайте signed/reset/OAuth URL до постійних цілей або розкладів. SQL-видалення не видаляє старі backup, HTML-архіви чи журнали: для них потрібна окрема файлова policy; HTML archive уже має REPORT_RETENTION_COUNT.
