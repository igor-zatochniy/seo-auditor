# Приклад результату аудиту

Цей приклад показує, як один запис аудиту виглядає після обробки тестової сторінки `https://example.com`.

![Audit summary table](audit-summary.svg)

## Summary

| Metric | Value |
| --- | --- |
| Target ID | `42` |
| Target status | `completed` |
| Attempts | `1` |
| Safe URL | `https://example.com` |
| Fingerprint key ID | `local-dev` |
| Status | `200 OK` |
| Scan status | `completed` |
| Title | `Example Domain` |
| Title status | `Too Short` |
| Title truncated | `false` |
| Description | `[Missing]` |
| Description status | `Missing` |
| H1 | `Example Domain` |
| H1 count | `1` |
| Canonical | `[Missing]` |
| Meta robots | `[Missing]` |
| X-Robots-Tag | `[Missing]` |
| robots.txt | `Allowed` |
| Robots outcome | `allowed` |
| Error code | `[Empty]` |
| Error message | `[Empty]` |
| Images without alt | `0 / 0` |
| Internal links | `0` |
| External links | `1` |
| JSON-LD | `No` |
| Viewport | `Yes` |
| Word count | `21` |

## Скорочений SQL-зріз

```text
target_id            | 42
target_status        | completed
attempts             | 1
safe_url             | https://example.com
fingerprint_key_id   | local-dev
status_code          | 200
scan_status          | completed
error_code           |
error_message        |
title                | Example Domain
title_status         | Too Short
title_truncated      | false
title_original_length | 0
description_status   | Missing
h1                   | Example Domain
h1_count             | 1
canonical_url        |
meta_robots          |
x_robots_tag         |
robots_allowed       | true
robots_outcome       | allowed
images_missing_alt   | 0
internal_links_count | 0
external_links_count | 1
links_count          | 1
has_json_ld          | false
has_viewport         | true
word_count           | 21
```

## Підсумкові проблеми

- `title` коротший за рекомендований діапазон `40-65` символів.
- `meta description` відсутній.
- `canonical` tag відсутній.
- JSON-LD structured data не знайдено.
- Сторінка має дуже малий обсяг текстового контенту.

## Що вважається нормальним

- HTTP status успішний: `200`.
- `robots.txt` не блокує сканування.
- H1 присутній рівно один раз.
- Зображень без `alt` немає, тому що на сторінці немає зображень.
- Є один зовнішній link, внутрішніх links немає.
