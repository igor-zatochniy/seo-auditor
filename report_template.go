package main

const reportHeaderHTML = `<!doctype html>
<html lang="uk">
<head>
  <meta charset="utf-8">
  <meta name="viewport" content="width=device-width, initial-scale=1">
  <title>SEO Auditor - звіт {{.RunID}}</title>
  <style>
    :root {
      color-scheme: light;
      --page: #f4f6f8;
      --surface: #ffffff;
      --text: #17212b;
      --muted: #5e6b78;
      --line: #d9e0e7;
      --accent: #1769aa;
      --success: #176b47;
      --success-bg: #e5f5ed;
      --warning: #8a5700;
      --warning-bg: #fff2cc;
      --danger: #a12622;
      --danger-bg: #fde8e7;
      --neutral: #465563;
      --neutral-bg: #edf1f4;
    }
    * { box-sizing: border-box; }
    body {
      margin: 0;
      background: var(--page);
      color: var(--text);
      font-family: Inter, ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif;
      font-size: 14px;
      line-height: 1.45;
      letter-spacing: 0;
    }
    main { width: min(100% - 32px, 1680px); margin: 28px auto 48px; }
    header { margin-bottom: 22px; }
    h1 { margin: 0 0 8px; font-size: 30px; line-height: 1.2; letter-spacing: 0; }
    .subtitle { margin: 0; color: var(--muted); overflow-wrap: anywhere; }
    .summary {
      display: grid;
      grid-template-columns: repeat(4, minmax(0, 1fr));
      gap: 12px;
      margin-bottom: 18px;
    }
    .metric {
      min-width: 0;
      padding: 15px 16px;
      background: var(--surface);
      border: 1px solid var(--line);
      border-radius: 6px;
    }
    .metric-label { display: block; margin-bottom: 5px; color: var(--muted); font-size: 12px; }
    .metric-value { font-size: 22px; font-weight: 700; overflow-wrap: anywhere; }
    .run-meta {
      display: grid;
      grid-template-columns: repeat(3, minmax(0, 1fr));
      gap: 10px 20px;
      margin: 0 0 18px;
      padding: 14px 16px;
      background: var(--surface);
      border: 1px solid var(--line);
      border-radius: 6px;
    }
    .run-meta div { min-width: 0; }
    .run-meta dt { color: var(--muted); font-size: 12px; }
    .run-meta dd { margin: 3px 0 0; font-weight: 600; overflow-wrap: anywhere; }
    .pill {
      display: inline-flex;
      align-items: center;
      min-height: 26px;
      padding: 4px 8px;
      border-radius: 6px;
      font-size: 12px;
      font-weight: 700;
      white-space: normal;
    }
    .tone-success { color: var(--success); background: var(--success-bg); }
    .tone-warning { color: var(--warning); background: var(--warning-bg); }
    .tone-danger { color: var(--danger); background: var(--danger-bg); }
    .tone-neutral { color: var(--neutral); background: var(--neutral-bg); }
    .table-shell {
      overflow-x: auto;
      background: var(--surface);
      border: 1px solid var(--line);
      border-radius: 6px;
    }
    table { width: 100%; min-width: 1660px; border-collapse: collapse; }
    th, td { padding: 11px 12px; border-bottom: 1px solid var(--line); text-align: left; vertical-align: top; }
    th {
      position: sticky;
      top: 0;
      z-index: 1;
      background: #eaf0f5;
      color: #344451;
      font-size: 12px;
      font-weight: 700;
    }
    tbody tr:hover { background: #f8fafb; }
    tbody tr:last-child td { border-bottom: 0; }
    .url { width: 260px; color: var(--accent); font-weight: 600; overflow-wrap: anywhere; }
    .text-cell { width: 220px; max-width: 300px; white-space: pre-wrap; overflow-wrap: anywhere; }
    .compact { width: 110px; white-space: pre-wrap; }
    .error { width: 230px; color: var(--danger); white-space: pre-wrap; overflow-wrap: anywhere; }
    footer { margin-top: 14px; color: var(--muted); font-size: 12px; }
    @media (max-width: 900px) {
      main { width: min(100% - 20px, 1680px); margin-top: 18px; }
      h1 { font-size: 24px; }
      .summary { grid-template-columns: repeat(2, minmax(0, 1fr)); }
      .run-meta { grid-template-columns: 1fr; }
    }
    @media (max-width: 480px) {
      .summary { grid-template-columns: 1fr; }
      .metric-value { font-size: 20px; }
    }
  </style>
</head>
<body>
<main>
  <header>
    <h1>SEO Auditor</h1>
    <p class="subtitle">Технічний звіт для запуску {{.RunID}}</p>
  </header>
  <section class="summary" aria-label="Підсумок аудиту">
    <div class="metric"><span class="metric-label">Статус</span><span class="pill {{.StatusTone}}">{{.Status}}</span></div>
    <div class="metric"><span class="metric-label">URL у запуску</span><span class="metric-value">{{.TotalURLs}}</span></div>
    <div class="metric"><span class="metric-label">Успішно</span><span class="metric-value">{{.SuccessfulURLs}}</span></div>
    <div class="metric"><span class="metric-label">З помилками</span><span class="metric-value">{{.FailedURLs}}</span></div>
  </section>
  <dl class="run-meta">
    <div><dt>Початок</dt><dd>{{.StartedAt}}</dd></div>
    <div><dt>Завершення</dt><dd>{{.FinishedAt}}</dd></div>
    <div><dt>Звіт створено</dt><dd>{{.GeneratedAt}}</dd></div>
  </dl>
  <div class="table-shell">
    <table>
      <thead>
        <tr>
          <th>URL</th>
          <th>HTTP</th>
          <th>Статус</th>
          <th>Title</th>
          <th>Description</th>
          <th>H1</th>
          <th>Посилання</th>
          <th>Без alt</th>
          <th>Robots</th>
          <th>Слова</th>
          <th>Час</th>
          <th>Помилки</th>
        </tr>
      </thead>
      <tbody>
`

const reportRowHTML = `        <tr>
          <td class="url">{{.URL}}</td>
          <td class="compact">{{.HTTPCode}}</td>
          <td class="compact"><span class="pill {{.StatusTone}}">{{.Status}}</span></td>
          <td class="text-cell">{{.Title}}</td>
          <td class="text-cell">{{.Description}}</td>
          <td class="text-cell">{{.H1}}</td>
          <td class="compact">{{.Links}}</td>
          <td class="compact">{{.ImagesMissingAlt}}</td>
          <td class="text-cell">{{.Robots}}</td>
          <td class="compact">{{.WordCount}}</td>
          <td class="compact">{{.Duration}}</td>
          <td class="error">{{.Error}}</td>
        </tr>
`

const reportFooterHTML = `      </tbody>
    </table>
  </div>
  <footer>Дані звіту отримано з PostgreSQL. Усі значення URL та HTML-метаданих екрановано під час генерації.</footer>
</main>
</body>
</html>
`
