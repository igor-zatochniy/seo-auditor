package main

import (
	"fmt"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func sanitizeDiagnosticRecord(record map[string]any) {
	for key, value := range record {
		switch v := value.(type) {
		case string:
			record[key] = sanitizeGEOTextForStorage(v, 2000)
		case map[string]any:
			sanitizeDiagnosticRecord(v)
		case []any:
			for i, item := range v {
				if s, ok := item.(string); ok {
					v[i] = sanitizeGEOTextForStorage(s, 2000)
				}
				if m, ok := item.(map[string]any); ok {
					sanitizeDiagnosticRecord(m)
				}
			}
		}
	}
}

func diagnosticReportDetails(value any) []reportDetail {
	s, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	items := []reportDetail{}
	if eligibility, ok := s["eligibility"].(map[string]any); ok {
		for _, item := range []struct{ key, label string }{{"google_ai", "Google AI: технічні передумови"}, {"chatgpt_search", "ChatGPT Search: технічні передумови"}} {
			check, _ := eligibility[item.key].(map[string]any)
			state, _ := check["status"].(string)
			reasons := []string{}
			if list, ok := check["reasons"].([]any); ok {
				for _, reason := range list {
					if text, ok := reason.(string); ok {
						reasons = append(reasons, text)
					}
				}
			}
			items = append(items, reportDetail{item.label, geoEligibilityLabel(geo.EligibilityCheck{Status: state, Reasons: reasons}), "AI-пошук"})
		}
	}
	search, _ := s["search"].(map[string]any)
	for _, f := range []struct{ key, label string }{{"googlebot_rules", "Googlebot"}, {"oai_searchbot_rules", "OAI-SearchBot (пошук)"}, {"gptbot_rules", "GPTBot (навчання)"}, {"google_indexing", "Google: індексація"}, {"google_snippet", "Google: snippet"}} {
		items = append(items, reportDetail{f.label, geoRuleLabel(fmt.Sprint(search[f.key])), "Правила доступу"})
	}
	if h, ok := s["http"].(map[string]any); ok {
		items = append(items, reportDetail{"Перший байт після надсилання запиту, мс", reportValue(h["response_ms"]), "HTTP нашого клієнта"})
	}
	if p, ok := s["performance"].(map[string]any); ok {
		for _, f := range []struct{ key, label string }{{"status", "Статус"}, {"lcp_ms", "LCP, мс"}, {"cls", "CLS"}, {"blocked", "Заблоковані ресурси"}, {"error", "Помилка"}, {"warnings", "Застереження"}} {
			items = append(items, reportDetail{f.label, reportValue(p[f.key]), "Лабораторна швидкість"})
		}
	}
	if b, ok := s["blocks"].(map[string]any); ok {
		items = append(items, reportDetail{"Кількість абзаців 300–500 символів", reportValue(b["count"]), "Фрагменти контенту"})
		if blocks, ok := b["items"].([]any); ok {
			for _, v := range blocks {
				if item, ok := v.(map[string]any); ok {
					items = append(items, reportDetail{reportValue(item["heading"]), reportValue(item["text"]), "Вибірка абзаців"})
				}
			}
		}
	}
	return append(items, reportDetail{"Межі перевірки", "Правила не доводять AI-видимості. LCP/CLS — локальний desktop-вимір через брокер, не польові CWV. Блоки 300–500 символів — евристика, не вимога AI.", "Методика"})
}
