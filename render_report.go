package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
)

func (s *webServer) renderingDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	target, err := strconv.ParseInt(r.PathValue("target"), 10, 64)
	if err != nil {
		writeWebError(w, 400, "invalid_target", "Некоректний Target ID")
		return
	}
	var data map[string]any
	if err = s.pool.QueryRow(r.Context(), "SELECT rendering FROM audit_results WHERE run_id=$1 AND target_id=$2", id, target).Scan(&data); err != nil {
		webDBError(w, err)
		return
	}
	sanitizeRenderingRecord(data)
	writeWebJSON(w, 200, data)
}

func sanitizeRenderingRecord(record map[string]any) {
	if metrics, ok := record["performance"].(map[string]any); ok {
		sanitizeDiagnosticRecord(metrics)
	}
	for _, key := range []string{"error", "browser"} {
		if v, ok := record[key].(string); ok {
			record[key] = sanitizeGEOTextForStorage(v, 8192)
		}
	}
	if warnings, ok := record["warnings"].([]any); ok {
		for i, v := range warnings {
			warnings[i] = sanitizeGEOTextForStorage(fmt.Sprint(v), 8192)
		}
	}
	for _, key := range []string{"raw", "rendered"} {
		if snapshot, ok := record[key].(map[string]any); ok {
			for k, v := range snapshot {
				if s, ok := v.(string); ok {
					if k == "canonical" {
						snapshot[k] = redactURL(s)
					} else {
						snapshot[k] = sanitizeGEOTextForStorage(s, 8192)
					}
				}
			}
			if links, ok := snapshot["hreflang"].([]any); ok {
				for i, v := range links {
					links[i] = sanitizeGEOTextForStorage(fmt.Sprint(v), 8192)
				}
			}
		}
	}
}

func renderingReportDetails(value any) []reportDetail {
	r, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	group := "JavaScript: HTML відповіді → DOM"
	items := []reportDetail{{"Статус", reportValue(r["status"]), group}, {"Помилка", reportValue(r["error"]), group}, {"Застереження", reportValue(r["warnings"]), group}}
	raw, _ := r["raw"].(map[string]any)
	dom, _ := r["rendered"].(map[string]any)
	for _, f := range []struct{ key, label string }{{"title", "Title"}, {"description", "Description"}, {"canonical", "Canonical"}, {"robots", "Meta robots"}, {"x_robots_tag", "X-Robots-Tag"}, {"h1", "H1"}, {"h1_count", "Кількість H1"}, {"words", "Слова"}, {"text_sample", "Вибірка тексту"}, {"internal_links", "Внутрішні посилання"}, {"external_links", "Зовнішні посилання"}, {"json_ld_count", "Блоки JSON-LD"}, {"hreflang", "Hreflang"}} {
		items = append(items, reportDetail{f.label, reportValue(raw[f.key]) + " → " + reportValue(dom[f.key]), group})
	}
	items = append(items, reportDetail{"Різниця (JSON)", reportValue(r["delta"]), group})
	return items
}

func structuredReportValue(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return ""
	}
	return string(b)
}
