package geo

type SignalGap struct {
	Signal   string `json:"signal"`
	Evidence string `json:"evidence"`
}

// Виявлена структура не доводить якість, рейтинг чи перемогу конкурента у видачі.
func ComparePageSignals(own, other Result) []SignalGap {
	out := []SignalGap{}
	if own.TargetID == nil || other.TargetID == nil {
		return out
	}
	checks := map[string]Check{}
	for _, c := range own.Checks {
		checks[c.Category+"\x00"+c.Name] = c
	}
	for _, c := range other.Checks {
		if p, ok := checks[c.Category+"\x00"+c.Name]; ok && !p.Passed && c.Passed {
			out = append(out, SignalGap{c.Name, c.Evidence})
		}
	}
	if own.Entities != nil && other.Entities != nil {
		old := map[string]EntityCheck{}
		for _, c := range own.Entities.Checks {
			old[c.Key] = c
		}
		for _, c := range other.Entities.Checks {
			if p, ok := old[c.Key]; ok && (p.Status == "missing" || p.Status == "mismatch") && c.Status == "found" {
				out = append(out, SignalGap{c.Name, "На сторінці конкурента виявлено відповідний сигнал сутності."})
			}
		}
	}
	if own.Eligibility != nil && other.Eligibility != nil {
		if own.Eligibility.GoogleAI.Status == Blocked && other.Eligibility.GoogleAI.Status == "allowed" {
			out = append(out, SignalGap{"Технічні правила Google AI", "На нашій сторінці виявлено обмеження; у конкурента заборони не виявлено."})
		}
		if own.Eligibility.ChatGPTSearch.Status == Blocked && other.Eligibility.ChatGPTSearch.Status == "allowed" {
			out = append(out, SignalGap{"Технічні правила ChatGPT Search", "На нашій сторінці виявлено обмеження; у конкурента заборони не виявлено."})
		}
	}
	if own.Blocks != nil && other.Blocks != nil && own.Blocks.Complete && other.Blocks.Complete && relevantBlockCount(own) == 0 && relevantBlockCount(other) > 0 {
		out = append(out, SignalGap{"Блок 300–500 символів зі збігом запиту", "У конкурента є блок із ≥50% термінів запиту. Це евристика, не доказ повноти відповіді."})
	}
	return out
}
