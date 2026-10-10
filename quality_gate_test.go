//go:build integration && qualitygate

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5/pgxpool"
	"golang.org/x/net/dns/dnsmessage"
)

func qualityDB(t *testing.T) (context.Context, *pgxpool.Pool, Config) {
	t.Helper()
	u, err := url.Parse(os.Getenv("DATABASE_URL"))
	if err != nil || u == nil || !strings.HasPrefix(strings.TrimPrefix(u.Path, "/"), "seo_quality_") || os.Getenv("SEO_QUALITY_GATE") != "isolated" {
		t.Fatal("quality gate requires SEO_QUALITY_GATE=isolated and a dedicated seo_quality_* database")
	}
	ctx, pool, cfg := webIntegration(t, 12*time.Minute)
	cfg.Workers = 2
	cfg.DBWriteTimeout = 3 * time.Second
	cfg.FinalizationTimeout = 30 * time.Second
	cfg.RateLimitInterval = time.Millisecond
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(io.Discard, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	return ctx, pool, cfg
}

// DNS змінюється лише у тестовому процесі; системні hosts та Docker DNS не редагуються.
func qualityFixtureDNS(t *testing.T) {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	original := net.DefaultResolver
	go func() {
		buffer := make([]byte, 4096)
		for {
			n, addr, err := conn.ReadFrom(buffer)
			if err != nil {
				return
			}
			var query dnsmessage.Message
			if query.Unpack(buffer[:n]) != nil {
				continue
			}
			response := dnsmessage.Message{Header: dnsmessage.Header{ID: query.ID, Response: true, RecursionAvailable: true}, Questions: query.Questions}
			for _, q := range query.Questions {
				host := strings.TrimSuffix(q.Name.String(), ".")
				var ips []net.IPAddr
				if host == "quality-own.test" || host == "quality-1.test" || host == "quality-2.test" || host == "quality-3.test" || host == "quality-4.test" || host == "quality-5.test" {
					ips = []net.IPAddr{{IP: net.ParseIP("127.0.0.1")}}
				} else {
					ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
					ips, _ = original.LookupIPAddr(ctx, host)
					cancel()
				}
				for _, ip := range ips {
					if v := ip.IP.To4(); v != nil && q.Type == dnsmessage.TypeA {
						response.Answers = append(response.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: q.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET}, Body: &dnsmessage.AResource{A: [4]byte{v[0], v[1], v[2], v[3]}}})
					}
				}
			}
			if b, err := response.Pack(); err == nil {
				_, _ = conn.WriteTo(b, addr)
			}
		}
	}()
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, "udp", conn.LocalAddr().String())
	}}
	t.Cleanup(func() { net.DefaultResolver = original; _ = conn.Close() })
}

func TestQualityGateTenThousandURLsThousandQueriesFiveCompetitors(t *testing.T) {
	ctx, pool, cfg := qualityDB(t)
	qualityFixtureDNS(t)
	start := time.Now()
	var requests atomic.Int64
	fixture := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		topic := strings.TrimPrefix(r.URL.Path, "/page/")
		fmt.Fprintf(w, `<html><head><title>Topic %s protection guide</title><meta name="description" content="Topic %s protection evidence"><link rel="canonical" href="http://%s%s"></head><body><main><h1>Topic %s protection</h1><p>Topic %s protection guide provides a direct answer, examples and independent evidence.</p><a href="/page/00000">Reference</a></main></body></html>`, topic, topic, r.Host, r.URL.Path, topic, topic)
	}))
	l, err := net.Listen("tcp", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	fixture.Listener = l
	fixture.Start()
	defer fixture.Close()
	port := strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
	base := "http://quality-own.test:" + port
	if _, err := net.LookupHost("quality-own.test"); err != nil {
		t.Fatal("configure isolated fixture aliases", err)
	}
	urls := make([]string, 10000)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/page/%05d", base, i)
	}
	urls[9998] += "?token=quality-secret-alpha"
	urls[9999] += "?token=quality-secret-beta"
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal("10k pipeline", code)
	}
	large := cfg.RunID
	var count, distinct, retained int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*),COUNT(DISTINCT target_id) FROM audit_results WHERE run_id=$1`, large).Scan(&count, &distinct); err != nil || count != 10000 || distinct != 10000 {
		t.Fatal("lost or duplicated targets", count, distinct, err)
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_run_targets WHERE run_id=$1 AND request_url<>''", large).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("raw URL retention", retained, err)
	}
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM audit_results WHERE run_id=$1 AND (safe_url LIKE '%quality-secret-%' OR international::text LIKE '%quality-secret-%' OR error_message LIKE '%quality-secret-%')`, large).Scan(&retained); err != nil || retained != 0 {
		t.Fatal("secret leaked", retained, err)
	}
	app := testWebApp()
	app.pool = pool
	app.cfg = cfg
	queries := make([]string, 1000)
	for i := range queries {
		queries[i] = fmt.Sprintf("topic %05d protection", i)
	}
	input := geo.Input{ID: newWebRunID(), SourceRunID: large, Domain: "quality-own.test", Brand: "Quality", Queries: strings.Join(queries, "\n")}
	if _, err := app.createGEO(ctx, input, queries); err == nil {
		t.Fatal("unbounded 10k GEO corpus accepted")
	}
	// GEO навмисно обмежений 1000 сторінками, без непомітного усічення великого джерела.
	cfg.RunID = newWebRunID()
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls[:1000]); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, false); code != exitSuccess {
		t.Fatal(code)
	}
	app.cfg = cfg
	input.ID = newWebRunID()
	input.SourceRunID = cfg.RunID
	report, err := app.createGEO(ctx, input, queries)
	if err != nil {
		t.Fatal("1000-query analysis", err)
	}
	if report.QueryCount != 1000 {
		t.Fatal("query count", report)
	}
	app.manager = newAuditManager(ctx, pool, cfg)
	app.manager.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	t.Cleanup(app.manager.shutdown)
	competitors := []competitorInput{}
	for i := 1; i <= 5; i++ {
		competitors = append(competitors, competitorInput{URL: fmt.Sprintf("http://quality-%d.test:%s/page/00000", i, port), Brand: fmt.Sprintf("Competitor %d", i)})
	}
	for pass := 0; pass < 2; pass++ {
		set := competitorSetInput{ID: newWebRunID(), Competitors: competitors, MaxPages: 2, FreshHours: 24}
		if err := app.storeCompetitorSet(ctx, report.ID, set); err != nil {
			t.Fatal(err)
		}
		before := requests.Load()
		for range 5 {
			if _, err := app.manager.startCompetitor(ctx); err != nil {
				t.Fatal(err)
			}
			waitScheduled(t, app.manager)
			if ok, err := app.processCompetitorReport(ctx); err != nil || !ok {
				t.Fatal("competitor report", ok, err)
			}
		}
		if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM geo_competitor_jobs WHERE set_id=$1 AND status='ready' AND reused=$2", set.ID, pass == 1).Scan(&count); err != nil || count != 5 {
			t.Fatal("five competitors", count, err)
		}
		if pass == 1 && requests.Load() != before {
			t.Fatal("fresh competitors were scanned again")
		}
	}
	for _, path := range []string{"/topics", "/opportunities", "/competitors", "/tasks.csv"} {
		w := strategyRequest(app, "GET", "/api/geo/reports/"+report.ID+path, "")
		if w.Code != 200 {
			t.Fatal(path, w.Code, w.Body.String())
		}
	}
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", large).Scan(&count); err != nil || count != 10000 {
		t.Fatal("GEO changed SEO history", count, err)
	}
	var memory runtime.MemStats
	runtime.ReadMemStats(&memory)
	t.Logf("quality gate: 10000 URLs, 1000 queries, 5 crawled + 5 reused competitors; duration=%s heap_alloc=%d heap_sys=%d", time.Since(start), memory.HeapAlloc, memory.HeapSys)
	if b, err := os.ReadFile("/proc/self/status"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if strings.HasPrefix(line, "VmHWM:") {
				t.Log(line)
				fields := strings.Fields(line)
				v, _ := strconv.ParseInt(fields[1], 10, 64)
				if v > 384*1024 {
					t.Fatal("quality gate process exceeded 384 MiB RSS")
				}
			}
		}
	}
	if time.Since(start) > 10*time.Minute {
		t.Fatal("quality gate exceeded 10-minute execution budget")
	}
}

func TestQualityGateDatabaseContainerRestartRecovery(t *testing.T) {
	ctx, pool, cfg := qualityDB(t)
	container, nonce := os.Getenv("QUALITY_DB_CONTAINER"), os.Getenv("QUALITY_CONTAINER_NONCE")
	if container == "" || nonce == "" {
		t.Skip("physical Docker restart requires an explicitly labeled isolated container")
	}
	inspect := exec.CommandContext(ctx, "docker", "inspect", "--format", `{{index .Config.Labels "seo-auditor.quality-gate"}}`, container)
	b, err := inspect.Output()
	if err != nil || strings.TrimSpace(string(b)) != nonce {
		t.Fatal("refusing to stop an unverified container", err)
	}
	fixture := webFixture()
	defer fixture.Close()
	urls := []string{}
	for i := 0; i < 100; i++ {
		urls = append(urls, fmt.Sprintf("%s/page/%d", fixture.URL, i))
	}
	if err := createExplicitAuditRun(ctx, pool, &cfg, urls); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `UPDATE audit_run_targets SET status='running',claimed_by=$2,claimed_at=NOW(),lease_until=NOW()+INTERVAL '2 minutes',started_at=NOW(),attempts=1 WHERE run_id=$1 AND target_id<=10`, cfg.RunID, effectiveWorkerInstanceID(cfg)); err != nil {
		t.Fatal(err)
	}
	if b, err := exec.CommandContext(ctx, "docker", "stop", "--time", "1", container).CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	defer exec.Command("docker", "start", container).Run()
	if err := pool.Ping(ctx); err == nil {
		t.Fatal("PostgreSQL interruption did not occur")
	}
	if b, err := exec.CommandContext(ctx, "docker", "start", container).CombinedOutput(); err != nil {
		t.Fatal(string(b), err)
	}
	for until := time.Now().Add(30 * time.Second); ; {
		if pool.Ping(ctx) == nil {
			break
		}
		if time.Now().After(until) {
			t.Fatal("database did not recover")
		}
		time.Sleep(100 * time.Millisecond)
	}
	if _, err := pool.Exec(ctx, "UPDATE audit_runs SET heartbeat_at=NOW()-INTERVAL '1 hour' WHERE id=$1", cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := abandonStaleAuditRuns(ctx, pool, cfg); err != nil {
		t.Fatal(err)
	}
	if err := createAuditRun(ctx, pool, &cfg); err != nil {
		t.Fatal(err)
	}
	if code := executeCapturedAuditRun(ctx, pool, cfg, true); code != exitSuccess {
		t.Fatal("resume", code)
	}
	var count int
	if err := pool.QueryRow(ctx, "SELECT COUNT(*) FROM audit_results WHERE run_id=$1", cfg.RunID).Scan(&count); err != nil || count != 100 {
		t.Fatal("recovery lost targets", count, err)
	}
	var state string
	if err := pool.QueryRow(ctx, "SELECT status FROM audit_runs WHERE id=$1", cfg.RunID).Scan(&state); err != nil || state != "completed" {
		t.Fatal(state, err)
	}
	// Модель процесу, який загинув: running targets + відсутній heartbeat, без читання приватних даних.
	raw, _ := json.Marshal(map[string]any{"targets": count, "status": state})
	t.Log(string(raw))
}

func TestQualityGateBrowserWorkflow(t *testing.T) {
	if os.Getenv("QUALITY_BROWSER") != "1" {
		t.Skip("browser workflow requires QUALITY_BROWSER=1 and Playwright")
	}
	ctx, pool, cfg := qualityDB(t)
	qualityFixtureDNS(t)
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/html")
		if r.URL.Path == "/plain" {
			_, _ = io.WriteString(w, `<head><title>Plain page without language alternatives</title></head><body><main><h1>Plain page</h1><p>Regular content without hreflang.</p></main></body>`)
			return
		}
		fmt.Fprintf(w, `<head><title>Chargeback protection</title><link rel="alternate" hreflang="en" href="http://%s/en"><link rel="alternate" hreflang="es" href="http://%s/es"><link rel="alternate" hreflang="&lt;img src=x onerror=window.qualityXss=1&gt;" href="http://%s/en"><link rel="canonical" href="http://%s%s"></head><body><main><h1>Chargeback protection</h1><p>Chargeback protection for Stripe with evidence and references.</p></main></body>`, r.Host, r.Host, r.Host, r.Host, r.URL.Path)
	}))
	defer fixture.Close()
	u, _ := url.Parse(fixture.URL)
	u.Host = "quality-own.test:" + u.Port()
	app := testWebApp()
	app.web.MaxURLs = 3
	app.cfg = cfg
	app.pool = pool
	app.manager = newAuditManager(ctx, pool, cfg)
	app.manager.publish = func(context.Context, *pgxpool.Pool, Config, bool) {}
	defer app.manager.shutdown()
	server := httptest.NewServer(app.handler())
	defer server.Close()
	node := os.Getenv("QUALITY_NODE_EXECUTABLE")
	if node == "" {
		node = "node"
	}
	checkCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(checkCtx, node, "web/e2e/quality-gate.cjs", server.URL, u.String(), testWebToken)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("browser workflow: %v\n%s", err, b)
	} else {
		t.Log(string(b))
	}
}
