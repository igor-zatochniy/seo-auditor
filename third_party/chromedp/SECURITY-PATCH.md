# Локальний security-патч chromedp

Основа: `github.com/chromedp/chromedp v0.14.2`, production-файли та MIT license
з Go module cache. Версії cdproto й інших залежностей не змінені.

Upstream module checksum: `h1:r3b/WtwM50RsBZHMUm9fsNhhzRStTHrKdr2zmwbZSzM=`.

Єдина зміна upstream Go-коду: `browser.go` додає `WithTransportDialer` для
підключення окремого transport на один browser. Типовий transport не змінено.
Глобальні WebSocket dialers не модифікуються.

SEO Auditor використовує цей hook у `internal/render/cdp_transport.go`, щоб
обмежити вхідні CDP frames, fragmented messages, сумарні bytes і кількість
повідомлень **до** накопичення та JSON-декодування. Це захищає також від
великих console events, незалежно від HTML/DOM/AX лімітів.

Цю локальну копію можна прибрати після переходу на сумісний upstream release
із публічним transport hook; не прибирайте її без regression tests transport
і rendering на Chromium.
