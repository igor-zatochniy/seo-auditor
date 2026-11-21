module github.com/igor-zatochniy/seo-auditor

go 1.26.11

// v0.14.2 with a per-browser transport hook; see third_party/chromedp/SECURITY-PATCH.md.
replace github.com/chromedp/chromedp => ./third_party/chromedp

require (
	github.com/chromedp/cdproto v0.0.0-20250724212937-08a3db8b4327
	github.com/chromedp/chromedp v0.14.2
	github.com/go-json-experiment/json v1.27.2
	github.com/gobwas/ws v1.5.0
	github.com/jackc/pgx/v5 v5.13.0
	github.com/mfonda/simhash v0.0.0-20151007195837-79f94a1100d6
	github.com/pressly/goose/v3 v3.30.1
	golang.org/x/image v0.48.0
	golang.org/x/net v0.63.0
	golang.org/x/text v0.44.0
	golang.org/x/time v0.16.0
)

require (
	github.com/chromedp/sysutil v1.1.0 // indirect
	github.com/gobwas/httphead v0.1.0 // indirect
	github.com/jackc/pgpassfile v1.0.0 // indirect
	github.com/jackc/pgservicefile v0.0.0-20240606120523-5a60cdf6a761 // indirect
	github.com/jackc/puddle/v2 v2.2.3 // indirect
	github.com/mfridman/interpolate v0.2.0 // indirect
	github.com/sethvargo/go-retry v0.5.0 // indirect
	go.uber.org/multierr v1.11.0 // indirect
	golang.org/x/sync v0.25.0 // indirect
)
