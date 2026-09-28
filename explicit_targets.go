package main

import (
	"context"
	"crypto/rand"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func newWebRunID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	b[6] = (b[6] & 15) | 64
	b[8] = (b[8] & 63) | 128
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func parseWebURLs(input string, limit int, allowPrivate bool) ([]string, error) {
	urls := make([]string, 0)
	seen := make(map[string]bool)
	i := 0
	for line := range strings.Lines(input) {
		i++
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !utf8.ValidString(line) || strings.ContainsRune(line, 0) || utf8.RuneCountInString(line) > 2048 {
			return nil, fmt.Errorf("рядок %d: URL перевищує 2048 символів або містить недопустимі символи", i)
		}
		normalized, err := normalizeTargetURL(line, allowPrivate)
		if err != nil || utf8.RuneCountInString(normalized) > 2048 {
			return nil, fmt.Errorf("рядок %d: потрібен коректний публічний HTTP(S) URL без облікових даних", i)
		}
		if !seen[normalized] {
			seen[normalized] = true
			urls = append(urls, normalized)
			if len(urls) > limit {
				return nil, fmt.Errorf("дозволено не більше %d URL", limit)
			}
		}
	}
	if len(urls) == 0 {
		return nil, fmt.Errorf("додайте принаймні один URL")
	}
	return urls, nil
}

// A browser submission never touches the global batch source.
func createExplicitAuditRun(ctx context.Context, pool *pgxpool.Pool, cfg *Config, urls []string) error {
	if cfg == nil || len(urls) == 0 {
		return fmt.Errorf("explicit targets and configuration are required")
	}
	ctx, cancel := context.WithTimeout(ctx, effectiveDBFetchTimeout(*cfg)+effectiveDBWriteTimeout(*cfg))
	defer cancel()
	tx, err := pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer func() {
		rollbackCtx, stop := context.WithTimeout(context.Background(), effectiveDBWriteTimeout(*cfg))
		defer stop()
		_ = tx.Rollback(rollbackCtx)
	}()
	_, err = tx.Exec(ctx, `INSERT INTO audit_runs
		(id, started_at, heartbeat_at, worker_instance_id, owner_generation, status, targets_captured_at, total_urls)
		VALUES ($1, CURRENT_TIMESTAMP, CURRENT_TIMESTAMP, $2, 1, 'running', CURRENT_TIMESTAMP, $3)`,
		cfg.RunID, effectiveWorkerInstanceID(*cfg), len(urls))
	if err != nil {
		return err
	}
	_, err = tx.CopyFrom(ctx, pgx.Identifier{"audit_run_targets"}, []string{"run_id", "target_id", "request_url"},
		pgx.CopyFromSlice(len(urls), func(i int) ([]any, error) { return []any{cfg.RunID, int64(i + 1), urls[i]}, nil }))
	if err != nil {
		return err
	}
	if err = tx.Commit(ctx); err != nil {
		return err
	}
	cfg.OwnerGeneration = 1
	return nil
}
