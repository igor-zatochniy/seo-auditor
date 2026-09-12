# Фіксація перевіреного релізу

Аудит від 8 вересня 2026 року дозволяє заморозити код на commit
`581205d8e350e3015e1288bdbd5098d4b8934d66`: підтверджених P0/P1 немає.
Цей висновок стосується конкретного SHA. Після будь-яких змін, зокрема CI,
обирайте новий SHA лише після успішних перевірок цього commit.

## Повторні перевірки

У GitHub Actions відкрийте workflow `CI`, виберіть потрібну гілку або наявний
tag у `Run workflow` та ввімкніть `freeze_evidence`.
Workflow має вже бути опублікований у default branch.

Ручний режим виконує всі звичайні CI-перевірки, а також:

- усі unit tests із `-shuffle=on -count=10`;
- усі unit tests із `-race -shuffle=on -count=10`;
- дев'ять regression tests для claim, resume, generation fencing, heartbeat,
  shutdown і locked targets із `-tags=integration -race -shuffle=on -count=10`.

Integration tests запускаються на тому самому PostgreSQL image, який пройшов
сканування: Compose використовує готовий образ без повторної збірки.
Після помилки будь-якої перевірки run залишається невдалим. Seed випадкового
порядку записується в логи Go, тому проблемний порядок можна повторити через
`-shuffle=<seed>`. Великий recovery-тест на 250 000 targets та migration matrix
виконуються один раз у повному integration suite.

## Захист у GitHub

Створіть активний branch ruleset для `main`:

- вимагайте pull request та успішний check `Go checks and Docker build` від GitHub Actions;
- вимагайте актуальну базову гілку перед merge;
- забороніть force push і видалення гілки;
- не додавайте bypass actors, якщо freeze має діяти також для адміністратора.

Для release tags, наприклад `freeze-*`, створіть окремий tag ruleset:
`Restrict updates` і `Restrict deletions`, також без bypass actors.
Git tag сам по собі не є незмінним. Підпис засвідчує його походження, але не
забороняє переміщення або видалення tag; це забезпечують правила GitHub.

Після перевірки SHA створіть annotated tag. За наявності налаштованого
GPG/SSH signing key використовуйте signed tag. Перед публікацією звірте
`git rev-parse <tag>^{commit}` із SHA успішного CI run. Workflow не створює tags
і не змінює rulesets автоматично.

## Довгострокове зберігання

Для кожного freeze зафіксуйте source SHA та URL успішного CI run.
Збереження Docker images, їхніх ідентифікаторів, SBOM і SHA-256 контрольних
сум архівів є додатковою можливістю відновлення, а не обов'язковою умовою freeze.
Локальний Docker image ID є digest конфігурації image, а не registry manifest
digest. Не підставляйте його в `registry/image@sha256:...` без публікації образу
і перевірки фактичного registry digest.

Dockerfile frontend закріплений immutable digest так само, як base images.
Exact APK versions захищають від непомітної заміни пакета новішою версією,
але live Alpine repository може перестати зберігати потрібну revision.
Збережений image archive або образ у registry дозволяє відновити перевірений
runtime без повторного завантаження APK. Для довгострокового зберігання
використовуйте сховище з контрольованим строком retention; GitHub Actions
artifacts можуть бути видалені або прострочені.

Freeze вважається оформленим, коли release tag вказує на перевірений SHA,
захищений від зміни й видалення через tag ruleset, а успішний CI run цього SHA
зафіксований. Архівування build artifacts необов'язкове.

Офіційні джерела: [правила GitHub rulesets](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/available-rules-for-rulesets),
[ручний запуск workflow](https://docs.github.com/en/actions/reference/workflows-and-actions/events-that-trigger-workflows#workflow_dispatch).
