package geo

import "fmt"

const Limited = "limited"

type EligibilityCheck struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
}

type AIEligibility struct {
	GoogleAI      EligibilityCheck `json:"google_ai"`
	ChatGPTSearch EligibilityCheck `json:"chatgpt_search"`
}

// Це висновок за спостереженими обмеженнями, не доказ індексації чи цитування.
func EvaluateEligibility(s *Signals) *AIEligibility {
	result := &AIEligibility{}
	if s == nil {
		s = &Signals{}
	}
	check := func(bot string, google bool) EligibilityCheck {
		c := EligibilityCheck{Status: Allowed, Reasons: []string{}}
		blocked, unknown, limited := false, false, false
		if bot == Blocked {
			blocked = true
			c.Reasons = append(c.Reasons, "Обхід URL заборонено правилами robots.txt для цього бота.")
		} else if bot != Allowed {
			unknown = true
			c.Reasons = append(c.Reasons, "Правила robots.txt для цього бота не перевірено.")
		}
		if s.HTTP == nil || s.HTTP.Status == 0 {
			unknown = true
			c.Reasons = append(c.Reasons, "Немає HTTP-спостереження нашого клієнта.")
		} else if s.HTTP.Status != 200 {
			blocked = true
			c.Reasons = append(c.Reasons, fmt.Sprintf("Наш клієнт отримав HTTP %d замість HTML-сторінки з HTTP 200.", s.HTTP.Status))
		}
		if !s.Complete || s.Version < 2 || s.Version > 3 {
			unknown = true
			c.Reasons = append(c.Reasons, "Немає повного набору перевірених HTML-сигналів цієї методики.")
		}
		if google {
			if s.Search.MaxSnippet != nil && *s.Search.MaxSnippet == 0 && s.Search.GoogleSnippet != Blocked {
				blocked = true
				c.Reasons = append(c.Reasons, "max-snippet:0 забороняє текстовий snippet Google.")
			}
			for _, rule := range []struct{ state, reason string }{
				{s.Search.GoogleIndexing, "Директиви noindex / none забороняють індексацію Google."},
				{s.Search.GoogleSnippet, "Директиви nosnippet / max-snippet:0 забороняють текстовий snippet Google."},
			} {
				if rule.state == Blocked {
					blocked = true
					c.Reasons = append(c.Reasons, rule.reason)
				} else if rule.state != Allowed {
					unknown = true
					c.Reasons = append(c.Reasons, "Не всі директиви індексації та snippet Google перевірено.")
				}
			}
			if s.Search.MaxSnippet != nil && *s.Search.MaxSnippet > 0 {
				limited = true
				c.Reasons = append(c.Reasons, fmt.Sprintf("max-snippet:%d обмежує текстовий фрагмент; це не повна заборона AI-функцій.", *s.Search.MaxSnippet))
			}
			if s.Search.DataNoSnippet {
				limited = true
				c.Reasons = append(c.Reasons, "data-nosnippet обмежує окремі фрагменти, не всю сторінку.")
			}
		}
		switch {
		case blocked:
			c.Status = Blocked
		case unknown:
			c.Status = Unknown
		case limited:
			c.Status = Limited
		}
		if len(c.Reasons) == 0 {
			c.Reasons = append(c.Reasons, "У перевірених правилах і відповіді нашого клієнта технічних заборон не виявлено.")
		}
		return c
	}
	result.GoogleAI = check(s.Search.GooglebotRules, true)
	result.ChatGPTSearch = check(s.Search.OAISearchBotRules, false)
	return result
}
