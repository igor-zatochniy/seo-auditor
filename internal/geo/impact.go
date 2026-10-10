package geo

type ImpactSample struct {
	Result      Result
	Mode        string
	KeyID       string
	Fingerprint string
}
type ImpactChange struct {
	Query          string   `json:"query"`
	BeforeURL      string   `json:"before_url"`
	AfterURL       string   `json:"after_url"`
	TargetChanged  *bool    `json:"target_changed"`
	RelevanceDelta *int     `json:"relevance_delta"`
	Comparable     bool     `json:"comparable"`
	Reason         string   `json:"reason"`
	Changes        []Change `json:"changes"`
}

// Порівняння спостережених змін не встановлює причинний ефект SEO-робіт.
func CompareImpact(beforeModel, afterModel string, before, after []ImpactSample) []ImpactChange {
	old := map[string]ImpactSample{}
	for _, s := range before {
		old[QueryKey(s.Result.Query)] = s
	}
	out := []ImpactChange{}
	for _, now := range after {
		p, exists := old[QueryKey(now.Result.Query)]
		c := ImpactChange{Query: now.Result.Query, BeforeURL: p.Result.TargetURL, AfterURL: now.Result.TargetURL, Changes: []Change{}}
		if !exists {
			c.Reason = "Запиту немає у попередньому звіті."
			out = append(out, c)
			continue
		}
		switch {
		case beforeModel != afterModel:
			c.Reason = "Версії алгоритму різні: числові оцінки не порівнюються."
		case p.Mode != now.Mode:
			c.Reason = "Змінився спосіб вибору цілі: ручне рішення не порівнюється з автоматичною оцінкою."
		case p.Result.TargetID == nil || now.Result.TargetID == nil:
			c.Reason = "Одна з цілей не зіставлена; різниця оцінок невідома."
		case p.KeyID == "" || p.KeyID != now.KeyID || p.Fingerprint == "" || now.Fingerprint == "":
			c.Reason = "Немає порівнюваної захищеної ідентичності цілей, зокрема після ротації ключа."
		case p.Fingerprint != now.Fingerprint:
			c.Reason = "Змінилася ціль: це не порівняння тієї самої сторінки."
		case p.Result.Matching == nil || now.Result.Matching == nil:
			c.Reason = "Немає числових даних зіставлення."
		case p.Result.Matching.Limited || now.Result.Matching.Limited:
			c.Reason = "Обмежена вибірка не дозволяє надійно порівняти оцінки."
		case p.Result.Matching.CorpusPages != now.Result.Matching.CorpusPages:
			c.Reason = "Кількість сторінок корпусу змінилася: IDF-залежні оцінки не порівнюються."
		default:
			c.Comparable = true
			delta := now.Result.Matching.Relevance - p.Result.Matching.Relevance
			c.RelevanceDelta = &delta
			c.Reason = "Та сама модель, спосіб вибору й ідентичність цілі. IDF також залежить від контенту корпусу; зміна оцінки не доводить причинний ефект чи зростання AI-видимості."
			c.Changes = CompareResults([]Result{p.Result}, []Result{now.Result})
		}
		if p.Result.TargetID != nil && now.Result.TargetID != nil && p.KeyID != "" && p.KeyID == now.KeyID && p.Fingerprint != "" && now.Fingerprint != "" {
			changed := p.Fingerprint != now.Fingerprint
			c.TargetChanged = &changed
		}
		out = append(out, c)
	}
	return out
}
