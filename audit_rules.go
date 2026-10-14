package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"github.com/jackc/pgx/v5"
)

type customAuditRule struct {
	ID              string `json:"id"`
	Revision        int    `json:"revision"`
	Name            string `json:"name"`
	Pattern         string `json:"pattern"`
	Kind            string `json:"kind"`
	Value           string `json:"value"`
	Operator        string `json:"operator,omitempty"`
	Threshold       int    `json:"threshold,omitempty"`
	ConsecutiveRuns int    `json:"consecutive_runs,omitempty"`
	Severity        string `json:"severity"`
	Enabled         bool   `json:"enabled"`
}

func validateAuditRule(rule customAuditRule) error {
	if !runIDPattern.MatchString(rule.ID) || !utf8.ValidString(rule.Name) || utf8.RuneCountInString(rule.Name) < 1 || utf8.RuneCountInString(rule.Name) > 120 || strings.ContainsRune(rule.Name, 0) {
		return fmt.Errorf("потрібні UUID та назва до 120 символів")
	}
	validPath := func(p string) bool {
		return len(p) > 0 && len(p) <= 256 && strings.HasPrefix(p, "/") && !strings.HasPrefix(p, "//") && !strings.ContainsAny(p, "?#\x00\r\n\\") && utf8.ValidString(p)
	}
	if !validAuditRulePattern(rule.Pattern) || strings.Count(rule.Pattern, "*") > 1 || strings.Contains(rule.Pattern, "*") && (!strings.HasSuffix(rule.Pattern, "*") || !strings.HasPrefix(rule.Pattern, "/")) {
		return fmt.Errorf("область: шлях або точний URL без query; для шляху дозволено один * наприкінці")
	}
	if rule.Severity != "critical" && rule.Severity != "warning" {
		return fmt.Errorf("пріоритет: critical або warning")
	}
	if rule.ConsecutiveRuns < 0 || rule.ConsecutiveRuns > 10 {
		return fmt.Errorf("послідовні перевірки: від 1 до 10")
	}
	if rule.ConsecutiveRuns == 0 {
		rule.ConsecutiveRuns = 1
	}
	switch rule.Kind {
	case "http_200", "indexable", "self_canonical", "product_price", "oai_search_allowed":
		if rule.Value != "" {
			return fmt.Errorf("це правило не потребує додаткового значення")
		}
		if rule.Operator != "" && rule.Operator != "violates" {
			return fmt.Errorf("для цього правила підтримується лише оператор violates")
		}
	case "canonical_prefix":
		if !validPath(rule.Value) || strings.Contains(rule.Value, "*") {
			return fmt.Errorf("потрібен префікс canonical, наприклад /pl/")
		}
		if rule.Operator != "" && rule.Operator != "outside_prefix" {
			return fmt.Errorf("для canonical підтримується лише оператор outside_prefix")
		}
	case "canonical_changed":
		if rule.Value != "" || rule.Operator != "" && rule.Operator != "changed" {
			return fmt.Errorf("для зміни canonical підтримується оператор changed")
		}
	case "content_drop":
		if rule.Value != "" || rule.Operator != "" && rule.Operator != "decrease_percent" || rule.Threshold < 1 || rule.Threshold > 90 {
			return fmt.Errorf("скорочення контенту: поріг від 1 до 90 відсотків")
		}
	default:
		return fmt.Errorf("непідтримуваний тип правила")
	}
	return nil
}

func validAuditRulePattern(pattern string) bool {
	if strings.HasPrefix(pattern, "/") {
		return len(pattern) <= 256 && !strings.HasPrefix(pattern, "//") && !strings.ContainsAny(pattern, "?#\x00\r\n\\") && utf8.ValidString(pattern)
	}
	if len(pattern) > 256 || strings.ContainsAny(pattern, "?#\x00\r\n\\") || !utf8.ValidString(pattern) {
		return false
	}
	u, err := url.ParseRequestURI(pattern)
	if err != nil || u.User != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" || (u.Scheme != "http" && u.Scheme != "https") {
		return false
	}
	_, err = crawler.NormalizeAuthority(u)
	return err == nil
}

type auditRulePage struct {
	ID                                int64
	Fingerprint                       []byte
	URL, ScanStatus, Indexing, Robots string
	HTTP                              *int
	Complete                          bool
	WordCount                         int
	WordCountKnown                    bool
	OAISearchRules                    string
	SelfCanonical                     bool
	Signals                           *crawler.URLSignals
	Structured                        *structureddata.Report
}

type previousAuditRuleResult struct {
	state, evidence, observed string
	consecutive               int
	notified                  bool
}

func effectiveRuleRuns(rule customAuditRule) int {
	if rule.ConsecutiveRuns < 1 {
		return 1
	}
	return rule.ConsecutiveRuns
}

func auditRuleTransition(previous previousAuditRuleResult, state string, threshold int, excluded bool) (streak int, notified bool, event string) {
	if state == "fail" {
		streak = 1
		if previous.state == "fail" {
			streak = previous.consecutive + 1
			if streak > 10 {
				streak = 10
			}
			notified = previous.notified
		}
		if streak >= threshold && !previous.notified && !excluded {
			event = "failed"
			notified = true
		}
		return
	}
	if state == "pass" && previous.state == "fail" && previous.notified {
		event = "resolved"
	}
	return 0, false, event
}

func evaluateAuditRule(rule customAuditRule, p auditRulePage) (state, evidence string, matched bool) {
	state, evidence, _, matched = evaluateAuditRuleWithBaseline(rule, p, previousAuditRuleResult{})
	return
}

func evaluateAuditRuleWithBaseline(rule customAuditRule, p auditRulePage, previous previousAuditRuleResult) (state, evidence, observed string, matched bool) {
	u, e := url.Parse(p.URL)
	if e != nil {
		return "unknown", "Некоректна збережена адреса", "", false
	}
	if u.Path == "" {
		u.Path = "/"
	}
	match := auditRulePatternMatches(rule.Pattern, u)
	if !match {
		return "", "", "", false
	}
	state, matched = "unknown", true
	if rule.Kind == "http_200" {
		if p.HTTP == nil {
			return state, "HTTP-відповіді немає", "", true
		}
		if *p.HTTP == 200 {
			return "pass", "HTTP 200", fmt.Sprint(*p.HTTP), true
		}
		return "fail", fmt.Sprintf("HTTP %d", *p.HTTP), fmt.Sprint(*p.HTTP), true
	}
	if rule.Kind == "oai_search_allowed" {
		switch p.OAISearchRules {
		case "allowed":
			return "pass", "OAI-SearchBot дозволений для цієї сторінки", "allowed", true
		case "blocked":
			return "fail", "OAI-SearchBot заблокований для цієї сторінки", "blocked", true
		default:
			return "unknown", "Політику OAI-SearchBot не вдалося перевірити", "", true
		}
	}
	if p.Robots == "disallowed" {
		return state, "Сторінку не отримано через заборону robots.txt; HTML-директиви не перевірено", "", true
	}
	if p.HTTP == nil || *p.HTTP != 200 || p.ScanStatus != "completed" || !p.Complete {
		return state, "Повний HTML HTTP 200 не отримано", "", true
	}
	switch rule.Kind {
	case "indexable":
		switch p.Indexing {
		case "allowed":
			return "pass", "Технічної заборони Google indexing не виявлено; індексацію не підтверджено", "allowed", true
		case "blocked":
			return "fail", "Виявлено заборону індексації Google", "blocked", true
		}
		return state, "Директиви індексації не перевірено повністю", "", true
	case "product_price":
		if p.Structured == nil || p.Structured.Products == nil || !p.Structured.Products.Complete {
			return state, "Повних даних Product/offers немає; повторіть аудит", "", true
		}
		v := p.Structured.Products
		if v.Count > 0 && v.WithPrice == v.Count {
			return "pass", "Кожен зібраний Product має числову ціну offers", "present", true
		}
		return "fail", fmt.Sprintf("Product: %d; з ціною offers: %d", v.Count, v.WithPrice), "missing", true
	case "self_canonical", "canonical_prefix":
		if p.Signals == nil || !p.Signals.Complete {
			return state, "Canonical-сигнали неповні", "", true
		}
		refs := append([]crawler.URLReference{}, p.Signals.HTTP...)
		if p.Signals.HTML != nil {
			refs = append(refs, *p.Signals.HTML)
		}
		if len(refs) == 0 {
			return "fail", "Canonical не оголошено", "missing", true
		}
		for index, ref := range refs {
			if ref.Truncated {
				return state, "Canonical скорочено через ліміт", "", true
			}
			if !ref.Valid {
				return "fail", "Некоректний canonical", "invalid", true
			}
			if rule.Kind == "self_canonical" {
				self := false
				known := false
				if p.Signals.HTML != nil && index == len(refs)-1 {
					self = p.SelfCanonical
					known = true
				} else if ref.Self != nil {
					self = *ref.Self
					known = true
				}
				for _, fp := range ref.Fingerprints {
					if !known {
						self = self || fp == fmt.Sprintf("%x", p.Fingerprint)
					}
				}
				if !known && !self {
					return "unknown", "Історичний HTTP canonical не має повних даних self; повторіть аудит", "", true
				}
				if !self {
					return "fail", "Canonical не відповідає цій сторінці", "not-self", true
				}
			} else {
				c, e := url.Parse(ref.URL)
				if e == nil && c.Path == "" {
					c.Path = "/"
				}
				if e != nil {
					return "fail", "Некоректний canonical", "invalid", true
				}
				left, le := crawler.NormalizeAuthority(c)
				right, re := crawler.NormalizeAuthority(u)
				if le != nil || re != nil || left != right || c.Scheme != u.Scheme || !strings.HasPrefix(c.Path, rule.Value) {
					return "fail", "Canonical виходить за дозволений origin/префікс: " + redactURL(ref.URL), "outside-prefix", true
				}
			}
		}
		return "pass", "Canonical відповідає правилу", "valid", true
	}
	if rule.Kind == "canonical_changed" {
		observation, ok := canonicalRuleObservation(p.Signals)
		if !ok {
			return "unknown", "Canonical-сигнали неповні", "", true
		}
		if previous.observed == "" {
			return "pass", "Збережено початковий стан canonical", observation, true
		}
		if previous.observed != observation {
			return "fail", "Canonical відрізняється від попереднього аудиту", observation, true
		}
		return "pass", "Canonical не змінився", observation, true
	}
	if rule.Kind == "content_drop" {
		if !p.WordCountKnown || p.WordCount < 0 {
			return "unknown", "Кількість слів недоступна", "", true
		}
		observation := fmt.Sprint(p.WordCount)
		baseline, err := strconv.Atoi(previous.observed)
		if err != nil || baseline <= 0 {
			return "pass", "Збережено початкову кількість слів", observation, true
		}
		drop := (baseline - p.WordCount) * 100 / baseline
		if drop >= rule.Threshold {
			return "fail", fmt.Sprintf("Кількість слів сторінки скоротилася на %d%% (%d → %d)", drop, baseline, p.WordCount), observation, true
		}
		return "pass", fmt.Sprintf("Скорочення кількості слів %d%%, поріг %d%%", max(drop, 0), rule.Threshold), observation, true
	}
	return state, "Дані не перевірено", "", true
}

func auditRulePatternMatches(pattern string, target *url.URL) bool {
	if strings.HasPrefix(pattern, "/") {
		path := target.Path
		if path == "" {
			path = "/"
		}
		if strings.HasSuffix(pattern, "*") {
			return strings.HasPrefix(path, strings.TrimSuffix(pattern, "*"))
		}
		return path == pattern
	}
	scope, err := url.ParseRequestURI(pattern)
	if err != nil {
		return false
	}
	if scope.Path == "" {
		scope.Path = "/"
	}
	if target.Path == "" {
		target.Path = "/"
	}
	scopeAuthority, scopeErr := crawler.NormalizeAuthority(scope)
	targetAuthority, targetErr := crawler.NormalizeAuthority(target)
	return scopeErr == nil && targetErr == nil && scope.Scheme == target.Scheme && scopeAuthority == targetAuthority && scope.Path == target.Path
}

func canonicalRuleObservation(signals *crawler.URLSignals) (string, bool) {
	if signals == nil || !signals.Complete {
		return "", false
	}
	values := make([]string, 0, len(signals.HTTP)+1)
	appendReference := func(source string, ref crawler.URLReference) bool {
		if ref.Truncated {
			return false
		}
		identities := append([]string(nil), ref.Fingerprints...)
		sort.Strings(identities)
		if len(identities) == 0 {
			identities = append(identities, "url:"+ref.URL)
		}
		self := "unknown"
		if ref.Self != nil {
			self = strconv.FormatBool(*ref.Self)
		}
		values = append(values, source+"|valid="+strconv.FormatBool(ref.Valid)+"|self="+self+"|"+strings.Join(identities, ","))
		return true
	}
	for _, ref := range signals.HTTP {
		if !appendReference("http", ref) {
			return "", false
		}
	}
	if signals.HTML != nil {
		if !appendReference("html", *signals.HTML) {
			return "", false
		}
	}
	sort.Strings(values)
	sum := sha256.Sum256([]byte(strings.Join(values, "\x00")))
	return fmt.Sprintf("%x", sum[:]), true
}

func captureAuditRules(ctx context.Context, tx pgx.Tx, cfg Config, runID, scheduleID string, automatic bool) error {
	rows, e := tx.Query(ctx, `SELECT definition FROM audit_custom_rules WHERE schedule_id=$1 AND enabled ORDER BY id LIMIT 33`, scheduleID)
	if e != nil {
		return e
	}
	rules := []customAuditRule{}
	for rows.Next() {
		var rule customAuditRule
		if e = rows.Scan(&rule); e != nil {
			rows.Close()
			return e
		}
		rules = append(rules, rule)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return e
	}
	if len(rules) == 0 {
		return nil
	}
	if len(rules) > 32 {
		return fmt.Errorf("ліміт 32 правил")
	}
	_, e = tx.Exec(ctx, `INSERT INTO audit_rule_runs(run_id,schedule_id,rules,key_check,automatic) VALUES($1,$2,$3,$4,$5) ON CONFLICT(run_id) DO NOTHING`, runID, scheduleID, rules, fingerprintURL(cfg.TargetFingerprintKey, "rules:"+scheduleID), automatic)
	return e
}

// Кожен пакет результатів, cursor та сповіщення фіксуються однією транзакцією.
func (s *webServer) processAuditRules(ctx context.Context) (bool, error) {
	tx, e := s.pool.Begin(ctx)
	if e != nil {
		return false, e
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	var runID, scheduleID, status string
	var rules []customAuditRule
	var after int64
	var check []byte
	var automatic bool
	e = tx.QueryRow(ctx, `SELECT j.run_id::TEXT,j.schedule_id::TEXT,j.rules,j.after_target,j.key_check,r.status,j.automatic FROM audit_rule_runs j JOIN audit_runs r ON r.id=j.run_id
 WHERE j.status='pending' AND r.status<>'running' AND NOT EXISTS(SELECT 1 FROM audit_rule_runs p WHERE p.schedule_id=j.schedule_id AND p.status='pending' AND (p.created_at,p.run_id)<(j.created_at,j.run_id))
 ORDER BY j.created_at,j.run_id LIMIT 1 FOR UPDATE OF j SKIP LOCKED`).Scan(&runID, &scheduleID, &rules, &after, &check, &status, &automatic)
	if errors.Is(e, pgx.ErrNoRows) {
		return false, nil
	}
	if e != nil {
		return false, e
	}
	if status != auditRunStatusCompleted && status != auditRunStatusCompletedWithErrors {
		_, e = tx.Exec(ctx, `UPDATE audit_rule_runs SET status='interrupted' WHERE run_id=$1`, runID)
		if e != nil {
			return false, e
		}
		return true, tx.Commit(ctx)
	}
	rows, e := tx.Query(ctx, `SELECT target_id,target_fingerprint,safe_url,status_code,scan_status,COALESCE(geo_signals->'search'->>'google_indexing','unknown'),robots_outcome,is_self_canonical,html_size_complete,url_signals,structured_data,COALESCE(word_count,0),word_count IS NOT NULL,COALESCE(geo_signals->'search'->>'oai_searchbot_rules','unknown')
 FROM audit_results WHERE run_id=$1 AND target_id>$2 ORDER BY target_id LIMIT 100`, runID, after)
	if e != nil {
		return false, e
	}
	pages := []auditRulePage{}
	for rows.Next() {
		var p auditRulePage
		if e = rows.Scan(&p.ID, &p.Fingerprint, &p.URL, &p.HTTP, &p.ScanStatus, &p.Indexing, &p.Robots, &p.SelfCanonical, &p.Complete, &p.Signals, &p.Structured, &p.WordCount, &p.WordCountKnown, &p.OAISearchRules); e != nil {
			rows.Close()
			return false, e
		}
		pages = append(pages, p)
	}
	e = rows.Err()
	rows.Close()
	if e != nil {
		return false, e
	}
	previousResults := map[string]previousAuditRuleResult{}
	if automatic && len(pages) > 0 {
		fps := [][]byte{}
		for _, p := range pages {
			fps = append(fps, p.Fingerprint)
		}
		baseline, err := tx.Query(ctx, `SELECT DISTINCT ON(p.rule_id,p.fingerprint) p.rule_id::TEXT,p.fingerprint,p.state,p.evidence,p.observed_value,p.consecutive_failures,p.notified
		 FROM audit_rule_results p JOIN audit_rule_runs j ON j.run_id=p.run_id JOIN audit_rule_runs cur ON cur.run_id=$1
		 JOIN LATERAL jsonb_array_elements(cur.rules) wanted(def) ON wanted.def->>'id'=p.rule_id::TEXT AND (wanted.def->>'revision')::INT=p.revision
		 WHERE j.schedule_id=$2 AND j.automatic AND j.status='done' AND j.key_check=$3 AND (j.created_at,j.run_id)<(cur.created_at,cur.run_id)
		 AND p.fingerprint=ANY($4::BYTEA[]) ORDER BY p.rule_id,p.fingerprint,j.created_at DESC,j.run_id DESC,p.target_id LIMIT 3200`, runID, scheduleID, check, fps)
		if err != nil {
			return false, err
		}
		for baseline.Next() {
			var ruleID string
			var fp []byte
			var v previousAuditRuleResult
			if err = baseline.Scan(&ruleID, &fp, &v.state, &v.evidence, &v.observed, &v.consecutive, &v.notified); err != nil {
				baseline.Close()
				return false, err
			}
			previousResults[ruleID+string(fp)] = v
		}
		err = baseline.Err()
		baseline.Close()
		if err != nil {
			return false, err
		}
	}
	exceptions := map[string]bool{}
	if automatic && len(pages) > 0 {
		exceptionRows, err := tx.Query(ctx, `SELECT rule_id::TEXT,encode(fingerprint,'hex') FROM audit_rule_exceptions WHERE schedule_id=$1 AND expires_at>CURRENT_TIMESTAMP`, scheduleID)
		if err != nil {
			return false, err
		}
		for exceptionRows.Next() {
			var ruleID, fp string
			if err = exceptionRows.Scan(&ruleID, &fp); err != nil {
				exceptionRows.Close()
				return false, err
			}
			exceptions[ruleID+":"+fp] = true
		}
		err = exceptionRows.Err()
		exceptionRows.Close()
		if err != nil {
			return false, err
		}
	}
	var stored, events [][]any
	for _, p := range pages {
		for _, rule := range rules {
			if e = validateAuditRule(rule); e != nil {
				return false, e
			}
			previous := previousResults[rule.ID+string(p.Fingerprint)]
			state, evidence, observed, matched := evaluateAuditRuleWithBaseline(rule, p, previous)
			if !matched {
				continue
			}
			evidence, _, _ = limitStorageString(redactText(evidence), 1000)
			if !automatic || state == "unknown" {
				stored = append(stored, []any{runID, rule.ID, rule.Revision, p.ID, p.Fingerprint, redactURL(p.URL), state, evidence, observed, 0, false})
				continue
			}
			excluded := exceptions[rule.ID+":"+fmt.Sprintf("%x", p.Fingerprint)]
			streak, notified, transition := auditRuleTransition(previous, state, effectiveRuleRuns(rule), excluded)
			stored = append(stored, []any{runID, rule.ID, rule.Revision, p.ID, p.Fingerprint, redactURL(p.URL), state, evidence, observed, streak, notified})
			if transition != "" {
				label := "Виправлено порушення"
				if transition == "failed" {
					label = "З’явилося порушення"
					if previous.state == "" || previous.state == "unknown" {
						label = "Вперше спостережене порушення"
					}
				}
				events = append(events, []any{runID, rule.ID, p.ID, redactURL(p.URL), "rule_" + strings.ReplaceAll(rule.ID, "-", ""), rule.Severity, previous.evidence, label + ": " + rule.Name + ". " + evidence})
			}
		}
	}
	if len(stored) > 0 {
		if _, e = tx.CopyFrom(ctx, pgx.Identifier{"audit_rule_results"}, []string{"run_id", "rule_id", "revision", "target_id", "fingerprint", "safe_url", "state", "evidence", "observed_value", "consecutive_failures", "notified"}, pgx.CopyFromRows(stored)); e != nil {
			return false, e
		}
	}
	if len(events) > 0 {
		var scheduled bool
		if e = tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM audit_schedule_runs WHERE run_id=$1)`, runID).Scan(&scheduled); e != nil {
			return false, e
		}
		if scheduled {
			if _, e = tx.CopyFrom(ctx, pgx.Identifier{"audit_change_events"}, []string{"run_id", "rule_id", "target_id", "safe_url", "kind", "severity", "before_value", "after_value"}, pgx.CopyFromRows(events)); e != nil {
				return false, e
			}
			if _, e = tx.Exec(ctx, `UPDATE audit_custom_rules c SET last_notified_at=CURRENT_TIMESTAMP FROM (SELECT DISTINCT rule_id FROM audit_change_events WHERE run_id=$1 AND rule_id IS NOT NULL) e WHERE c.id=e.rule_id AND c.schedule_id=$2`, runID, scheduleID); e != nil {
				return false, e
			}
		}
	}
	if len(pages) == 0 {
		if _, e = tx.Exec(ctx, `UPDATE audit_rule_runs SET status='done' WHERE run_id=$1`, runID); e != nil {
			return false, e
		}
		if _, e = tx.Exec(ctx, `UPDATE audit_schedule_runs SET change_count=(SELECT COUNT(*) FROM audit_change_events WHERE run_id=$1) WHERE run_id=$1`, runID); e != nil {
			return false, e
		}
	} else {
		if _, e = tx.Exec(ctx, `UPDATE audit_rule_runs SET after_target=$2,checked=checked+$3 WHERE run_id=$1`, runID, pages[len(pages)-1].ID, len(pages)); e != nil {
			return false, e
		}
	}
	return true, tx.Commit(ctx)
}

func equalRuleDefinition(a, b customAuditRule) bool {
	a.Revision, b.Revision = 0, 0
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}
