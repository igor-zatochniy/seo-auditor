package main

import (
	"context"
	"crypto/subtle"
	"embed"
	"encoding/json"
	"errors"
	"html/template"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	appconfig "github.com/igor-zatochniy/seo-auditor/internal/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed web/templates/*.html web/static/*
var webAssets embed.FS

type webServer struct {
	pool        *pgxpool.Pool
	cfg         Config
	web         appconfig.WebConfig
	manager     *AuditManager
	queries     chan struct{}
	submissions chan struct{}
}

func serveAuditor(ctx context.Context, pool *pgxpool.Pool, cfg Config) int {
	wc, err := appconfig.LoadWeb()
	if err != nil {
		slog.Error("Некоректна web-конфігурація", "error", err)
		return exitFatal
	}
	manager := newAuditManager(ctx, pool, cfg)
	app := &webServer{pool: pool, cfg: cfg, web: wc, manager: manager, queries: make(chan struct{}, 3), submissions: make(chan struct{}, 1)}
	server := &http.Server{Addr: wc.Addr, Handler: app.handler(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second,
		WriteTimeout: cfg.ReportExportTimeout + 5*time.Second, IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	listener, err := net.Listen("tcp", wc.Addr)
	if err != nil {
		slog.Error("Не вдалося відкрити web-порт", "error", err)
		return exitFatal
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	slog.Info("SEO Auditor web запущено", "address", wc.Addr)
	select {
	case <-ctx.Done():
	case err := <-done:
		manager.shutdown()
		if !errors.Is(err, http.ErrServerClosed) {
			slog.Error("Web-сервер зупинився", "error", err)
			return exitFatal
		}
		return exitSuccess
	}
	manager.shutdown()
	stopCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(stopCtx); err != nil {
		_ = server.Close()
	}
	<-done
	return exitSuccess
}

func (s *webServer) handler() http.Handler {
	mux := http.NewServeMux()
	s.registerGEO(mux)
	assets, _ := fs.Sub(webAssets, "web/static")
	mux.Handle("GET /static/", http.StripPrefix("/static/", http.FileServer(http.FS(assets))))
	page := template.Must(template.ParseFS(webAssets, "web/templates/index.html"))
	pageHandler := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_ = page.ExecuteTemplate(w, "index.html", nil)
	}
	mux.HandleFunc("GET /{$}", pageHandler)
	mux.HandleFunc("GET /audits", pageHandler)
	mux.HandleFunc("GET /audits/{id}", func(w http.ResponseWriter, r *http.Request) {
		if !runIDPattern.MatchString(r.PathValue("id")) {
			writeWebError(w, 404, "not_found", "Аудит не знайдено")
			return
		}
		pageHandler(w, r)
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeWebJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", s.ready)
	mux.HandleFunc("POST /api/session", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{Name: "seo_session", Value: s.web.AccessToken, Path: "/api/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
		writeWebJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("GET /api/schema", func(w http.ResponseWriter, r *http.Request) {
		writeWebJSON(w, 200, map[string]any{"fields": reportFields, "max_urls": s.web.MaxURLs})
	})
	mux.HandleFunc("GET /api/audits", s.history)
	mux.HandleFunc("POST /api/audits", s.submit)
	mux.HandleFunc("GET /api/audits/{id}", s.runInfo)
	mux.HandleFunc("GET /api/audits/{id}/progress", s.progress)
	mux.HandleFunc("GET /api/audits/{id}/analytics", s.analytics)
	mux.HandleFunc("GET /api/audits/{id}/results", s.results)
	mux.HandleFunc("GET /api/audits/{id}/graph", s.graph)
	mux.HandleFunc("POST /api/audits/{id}/cancel", s.cancel)
	mux.HandleFunc("POST /api/audits/{id}/resume", s.resume)
	mux.HandleFunc("GET /api/audits/{id}/export/{format}", s.export)
	mux.HandleFunc("/api/", func(w http.ResponseWriter, r *http.Request) {
		writeWebError(w, 404, "not_found", "Endpoint не знайдено")
	})
	return s.secure(mux)
}

func validLocalHost(authority string) bool {
	host := authority
	if h, _, err := net.SplitHostPort(authority); err == nil {
		host = h
	}
	return host == "127.0.0.1" || host == "localhost" || host == "::1" || host == "[::1]"
}

func (s *webServer) secure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; base-uri 'none'; frame-ancestors 'none'; form-action 'self'")
		if !validLocalHost(r.Host) {
			writeWebError(w, 403, "invalid_host", "Доступ дозволено лише через localhost")
			return
		}
		// The token protects API data even if a container is accidentally exposed.
		if strings.HasPrefix(r.URL.Path, "/api/") {
			token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if token == "" {
				if cookie, err := r.Cookie("seo_session"); err == nil {
					token = cookie.Value
				}
			}
			if len(s.web.AccessToken) < 32 || subtle.ConstantTimeCompare([]byte(token), []byte(s.web.AccessToken)) != 1 {
				writeWebError(w, 401, "unauthorized", "Відкрийте інтерфейс через start-auditor.cmd")
				return
			}
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			origin, err := url.Parse(r.Header.Get("Origin"))
			if err != nil || origin.Scheme != "http" || origin.Host != r.Host || origin.Path != "" || origin.RawQuery != "" || origin.User != nil ||
				r.Header.Get("X-SEO-Auditor-Request") != "1" || strings.Split(r.Header.Get("Content-Type"), ";")[0] != "application/json" {
				writeWebError(w, 403, "invalid_origin", "Потрібен same-origin JSON запит")
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/readyz" {
			select {
			case s.queries <- struct{}{}:
				defer func() { <-s.queries }()
			default:
				writeWebError(w, 429, "busy", "Забагато запитів. Спробуйте ще раз")
				return
			}
			timeout := effectiveDBFetchTimeout(s.cfg)
			if strings.Contains(r.URL.Path, "/export/") {
				timeout = s.cfg.ReportExportTimeout
			}
			if r.Method == "POST" {
				timeout = s.cfg.FinalizationTimeout
			}
			ctx, cancel := context.WithTimeout(r.Context(), timeout)
			defer cancel()
			r = r.WithContext(ctx)
		}
		next.ServeHTTP(w, r)
	})
}

func writeWebJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func writeWebError(w http.ResponseWriter, status int, code, message string) {
	writeWebJSON(w, status, map[string]any{"error": map[string]string{"code": code, "message": message}})
}
func webDBError(w http.ResponseWriter, err error) {
	if errors.Is(err, pgx.ErrNoRows) {
		writeWebError(w, 404, "not_found", "Аудит не знайдено")
		return
	}
	slog.Error("Помилка web-запиту до сховища", "error", sanitizeError(err))
	writeWebError(w, 503, "storage_unavailable", "Сховище тимчасово недоступне")
}

func webRunID(w http.ResponseWriter, r *http.Request) (string, bool) {
	id := r.PathValue("id")
	if !runIDPattern.MatchString(id) {
		writeWebError(w, 400, "invalid_run_id", "Некоректний ID аудиту")
		return "", false
	}
	return id, true
}
