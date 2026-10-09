package main

import (
	"context"
	"errors"
	"log/slog"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

var errAuditBusy = errors.New("another audit is active")
var errAuditNotResumable = errors.New("audit is not resumable")

type AuditManager struct {
	mu      sync.Mutex
	root    context.Context
	pool    *pgxpool.Pool
	cfg     Config
	active  string
	cancel  context.CancelFunc
	closed  bool
	done    chan struct{}
	execute func(context.Context, *pgxpool.Pool, Config, bool) int
	publish func(context.Context, *pgxpool.Pool, Config, bool)
}

func newAuditManager(ctx context.Context, pool *pgxpool.Pool, cfg Config) *AuditManager {
	return &AuditManager{root: ctx, pool: pool, cfg: cfg, execute: executeCapturedAuditRun, publish: publishAuditReportContext}
}

func (m *AuditManager) start(ctx context.Context, urls []string, resumeID string, siteOptions ...siteCrawlOptions) (string, error) {
	return m.startWithRendering(ctx, urls, resumeID, m.cfg.RenderJavaScript, siteOptions...)
}

func (m *AuditManager) startWithRendering(ctx context.Context, urls []string, resumeID string, renderJS bool, siteOptions ...siteCrawlOptions) (string, error) {
	ctx, cancelInit := context.WithCancel(ctx)
	stopInit := context.AfterFunc(m.root, cancelInit)
	defer stopInit()
	defer cancelInit()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.root.Err() != nil || m.active != "" {
		return "", errAuditBusy
	}
	cfg := m.cfg
	cfg.RenderJavaScript = renderJS
	cfg.RunID = newWebRunID()
	if resumeID != "" {
		cfg.RunID = resumeID
		if _, err := abandonStaleAuditRuns(ctx, m.pool, cfg); err != nil {
			return "", err
		}
		run, err := loadWebRun(ctx, m.pool, cfg, resumeID)
		if err != nil {
			return "", err
		}
		if !run.Resumable {
			return "", errAuditNotResumable
		}
		cfg.RenderJavaScript = run.RenderJavaScript
		if _, err := loadSiteCrawl(ctx, m.pool, cfg); err != nil {
			return "", err
		}
		if err := createAuditRun(ctx, m.pool, &cfg); err != nil {
			return "", err
		}
	} else if err := validateRendering(cfg); err != nil {
		return "", err
	} else if len(siteOptions) > 0 {
		if err := createSiteAuditRun(ctx, m.pool, &cfg, siteOptions[0]); err != nil {
			return "", err
		}
	} else if err := createExplicitAuditRun(ctx, m.pool, &cfg, urls); err != nil {
		return "", err
	}
	m.launchLocked(cfg)
	return cfg.RunID, nil
}

// Викликається лише під m.mu після успішного commit запуску та його цілей.
func (m *AuditManager) launchLocked(cfg Config) {
	runCtx, cancel := context.WithCancel(m.root)
	m.active, m.cancel, m.done = cfg.RunID, cancel, make(chan struct{})
	go func() {
		defer cancel()
		code := m.execute(runCtx, m.pool, cfg, false)
		if code == exitSuccess && runCtx.Err() == nil {
			m.publish(runCtx, m.pool, cfg, false)
		}
		slog.Info("Web-аудит завершився", "run_id", cfg.RunID, "exit_code", code)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.active, m.cancel = "", nil
		close(m.done)
	}()
}

func (m *AuditManager) cancelRun(id string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.active != id || m.cancel == nil {
		return false
	}
	m.cancel()
	return true
}

func (m *AuditManager) shutdown() {
	m.mu.Lock()
	m.closed = true
	done := m.done
	if m.cancel != nil {
		m.cancel()
	}
	m.mu.Unlock()
	if done != nil {
		<-done
	}
}
