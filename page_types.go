package main

import (
	"context"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/igor-zatochniy/seo-auditor/internal/structureddata"
	"github.com/jackc/pgx/v5"
)

const pageTypeModel = "page-types-v1"

type pageTypeRule struct {
	Pattern string `json:"pattern"`
	Type    string `json:"type"`
}
type typeCheck struct {
	Code     string `json:"code"`
	State    string `json:"state"`
	Evidence string `json:"evidence"`
	Action   string `json:"action"`
}
type typedPage struct {
	ID         int64       `json:"target_id"`
	URL        string      `json:"url"`
	Type       string      `json:"type"`
	Source     string      `json:"source"`
	Confidence string      `json:"confidence"`
	Evidence   []string    `json:"evidence"`
	Checks     []typeCheck `json:"checks"`
	Revision   int64       `json:"revision"`
	HTTP       *int        `json:"http_status"`
	Status     string      `json:"scan_status"`
}
type typePageInput struct {
	ID                 int64
	URL, Status        string
	HTTP               *int
	Internal, External *int
	Schema             *structureddata.Report
	GEO                *geo.Signals
	Override           string
	Revision           int64
}

func validPageType(t string) bool {
	switch t {
	case "product", "category", "article", "service", "other":
		return true
	}
	return false
}

func classifyPage(in typePageInput, rules []pageTypeRule) typedPage {
	p := typedPage{ID: in.ID, URL: in.URL, Type: "unknown", Source: "automatic", Confidence: "unknown", Evidence: []string{}, Checks: []typeCheck{}, Revision: in.Revision, HTTP: in.HTTP, Status: in.Status}
	parsed, _ := url.Parse(in.URL)
	parsedOK := in.Status == scanStatusCompleted && in.HTTP != nil && *in.HTTP == 200
	if in.Override != "" && in.Override != "auto" {
		p.Type = in.Override
		p.Source = "manual"
		p.Confidence = "manual"
		p.Evidence = append(p.Evidence, "Тип підтверджено користувачем для цього target")
	} else if parsed != nil {
		for _, r := range rules {
			if auditRulePatternMatches(r.Pattern, parsed) {
				p.Type = r.Type
				p.Source = "rule"
				p.Confidence = "rule"
				p.Evidence = append(p.Evidence, "Правило шляху: "+r.Pattern)
				break
			}
		}
	}
	if p.Type == "unknown" && parsedOK && in.Schema != nil {
		candidates := map[string]bool{}
		for _, t := range in.Schema.Types {
			switch t {
			case "Product", "ProductGroup":
				candidates["product"] = true
			case "CollectionPage", "ItemList":
				candidates["category"] = true
			case "Article", "NewsArticle", "BlogPosting":
				candidates["article"] = true
			case "Service":
				candidates["service"] = true
			}
		}
		if len(candidates) == 1 {
			for t := range candidates {
				p.Type = t
			}
			p.Confidence = "medium"
			p.Evidence = append(p.Evidence, "Типи JSON-LD: "+strings.Join(in.Schema.Types, ", "))
		} else if len(candidates) > 1 {
			p.Evidence = append(p.Evidence, "Кілька типів JSON-LD можуть належати вкладеним сутностям; потрібне ручне визначення")
		}
	}
	if p.Type == "unknown" && len(p.Evidence) == 0 && parsedOK && parsed != nil {
		for _, part := range strings.Split(strings.ToLower(parsed.Path), "/") {
			t := ""
			switch part {
			case "product", "products", "товари", "товар":
				t = "product"
			case "category", "categories", "catalog", "каталог":
				t = "category"
			case "blog", "articles", "news", "статті":
				t = "article"
			case "services", "solutions", "послуги":
				t = "service"
			}
			if t != "" {
				p.Type = t
				p.Confidence = "low"
				p.Evidence = append(p.Evidence, "Евристика сегмента URL: "+part)
				break
			}
		}
	}
	if !parsedOK {
		p.Checks = append(p.Checks, typeCheck{"html", "unknown", "HTML HTTP 200 не збережено; відсутність метаданих не доведено", "Повторити перевірку після отримання повного HTML"})
		return p
	}
	add := func(code, state, evidence, action string) {
		p.Checks = append(p.Checks, typeCheck{code, state, evidence, action})
	}
	boolean := func(code string, known, present bool, evidence, action string) {
		state := "unknown"
		if known {
			state = "review"
			if present {
				state = "observed"
			}
		}
		add(code, state, evidence, action)
	}
	complete := in.Schema != nil && in.Schema.Complete && in.Schema.ValidBlocks == in.Schema.Blocks
	switch p.Type {
	case "product":
		product := in.Schema != nil && in.Schema.Products != nil && in.Schema.Products.Count > 0
		boolean("product_schema", complete, product, "Product JSON-LD у збереженому HTML", "Перевірити, чи потрібна Product schema саме цій сторінці")
		priceKnown := complete
		price := false
		availabilityKnown := false
		availability := false
		if product {
			v := in.Schema.Products
			priceKnown = priceKnown && v.Complete
			price = v.WithPrice == v.Count
			availabilityKnown = priceKnown && v.WithAvailability != nil
			availability = v.WithAvailability != nil && *v.WithAvailability == v.Count
		}
		boolean("product_price", priceKnown, price, "Наявність валідної ціни offers для всіх зібраних Product; не перевірка актуальності ціни", "Перевірити offers.price/lowPrice та відповідність видимому товару")
		boolean("product_availability", availabilityKnown, availability, "Availability перевіряється лише в нових повних даних JSON-LD", "Узгодити availability з фактичною наявністю; не змінювати статус автоматично")
	case "article":
		known := in.GEO != nil && in.GEO.Complete && !in.GEO.ContentIncomplete
		author := in.GEO != nil && in.GEO.HasAuthor
		if in.Schema != nil && in.Schema.Articles != nil && in.Schema.Articles.WithAuthor > 0 {
			author = true
		}
		boolean("article_author", known, author, "Метадані автора або author у JSON-LD; це не підтвердження експертності", "Перевірити автора й видиме авторство матеріалу")
		dateKnown := complete && in.Schema.Articles != nil && in.Schema.Articles.Complete && in.Schema.Articles.Count > 0
		boolean("article_date", dateKnown, dateKnown && in.Schema.Articles.WithDate == in.Schema.Articles.Count, "Валідна datePublished у JSON-LD; інші HTML-формати дати не перевірялися", "Перевірити дату публікації без вигаданого оновлення матеріалу")
		boolean("article_sources", known && in.External != nil, in.External != nil && *in.External > 0, "Зовнішні HTML-посилання — лише кандидати на джерела, не оцінка їхньої надійності", "Додати або перевірити першоджерела для фактичних тверджень, де це доречно")
	case "category":
		boolean("category_links", in.Internal != nil, in.Internal != nil && *in.Internal > 0, "Кількість внутрішніх посилань у response HTML", "Перевірити доступність товарів/підкатегорій через HTML-посилання")
		add("category_navigation", "info", "Фасетні сімейства, пагінація й глибина доступні у звітах «Фільтри URL» та «Граф посилань»; їхня відсутність не є універсальною помилкою", "Переглянути ці звіти в контексті категорії, без автоматичного блокування фільтрів")
	case "service":
		known := in.GEO != nil && in.GEO.Blocks != nil && in.GEO.Blocks.Complete
		boolean("service_answer", known, known && in.GEO.Blocks.Count > 0, "Зібрані змістові блоки 300–500 символів; це не гарантія відповіді на запит", "Перевірити прямий опис послуги й відповідність запитам у GEO-звіті")
	}
	return p
}

func (s *webServer) registerPageTypes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/audits/{id}/page-types", s.pageTypes)
	mux.HandleFunc("POST /api/audits/{id}/page-types/rules", s.pageTypeRules)
	mux.HandleFunc("POST /api/audits/{id}/results/{target}/page-type", s.pageTypeOverride)
}

func loadTypeRules(ctx context.Context, tx pgx.Tx, id, origin string) ([]pageTypeRule, int64, error) {
	rules := []pageTypeRule{}
	var revision int64
	err := tx.QueryRow(ctx, `SELECT rules,revision FROM audit_page_type_settings WHERE run_id=$1 AND origin=$2`, id, origin).Scan(&rules, &revision)
	if errors.Is(err, pgx.ErrNoRows) {
		return rules, 0, nil
	}
	return rules, revision, err
}

func walkTypedPages(ctx context.Context, tx pgx.Tx, id, origin string, rules []pageTypeRule, visit func(typedPage) error) error {
	after := int64(math.MinInt64)
	for {
		rows, err := tx.Query(ctx, `SELECT a.target_id,a.safe_url,a.scan_status,a.status_code,a.internal_links_count,a.external_links_count,a.structured_data,a.geo_signals,COALESCE(o.page_type,'auto'),COALESCE(o.revision,0)
 FROM audit_results a LEFT JOIN audit_page_type_overrides o ON o.run_id=a.run_id AND o.target_id=a.target_id
 WHERE a.run_id=$1 AND a.target_id>$2 ORDER BY a.target_id LIMIT 128`, id, after)
		if err != nil {
			return err
		}
		count := 0
		for rows.Next() {
			var in typePageInput
			if err = rows.Scan(&in.ID, &in.URL, &in.Status, &in.HTTP, &in.Internal, &in.External, &in.Schema, &in.GEO, &in.Override, &in.Revision); err != nil {
				rows.Close()
				return err
			}
			after = in.ID
			count++
			actual, e := searchOrigin(in.URL)
			if e == nil && actual == origin {
				if e = visit(classifyPage(in, rules)); e != nil {
					rows.Close()
					return e
				}
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if count < 128 {
			return nil
		}
		if err = ctx.Err(); err != nil {
			return err
		}
	}
}

func (s *webServer) pageTypes(w http.ResponseWriter, r *http.Request) {
	tx, id, origin, ok := s.insightRead(w, r)
	if !ok {
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	after, err := insightOffset(r)
	if err != nil {
		writeWebError(w, 400, "invalid_cursor", err.Error())
		return
	}
	filter := r.URL.Query().Get("type")
	if filter != "" && filter != "unknown" && !validPageType(filter) {
		writeWebError(w, 400, "invalid_type", "Невідомий тип сторінки")
		return
	}
	rules, revision, err := loadTypeRules(r.Context(), tx, id, origin)
	if err != nil {
		webDBError(w, err)
		return
	}
	summary := map[string]int{}
	reviews := map[string]int{}
	items := []typedPage{}
	matched := 0
	checked := 0
	err = walkTypedPages(r.Context(), tx, id, origin, rules, func(p typedPage) error {
		checked++
		summary[p.Type]++
		for _, c := range p.Checks {
			if c.State == "review" {
				reviews[c.Code]++
			}
		}
		if filter != "" && p.Type != filter {
			return nil
		}
		if matched >= after && len(items) < 51 {
			items = append(items, p)
		}
		matched++
		return nil
	})
	if err != nil {
		webDBError(w, err)
		return
	}
	more := len(items) > 50
	if more {
		items = items[:50]
	}
	writeWebJSON(w, 200, map[string]any{"model": pageTypeModel, "origin": origin, "checked": checked, "matched": matched, "summary": summary, "reviews": reviews, "items": items, "more": more, "rules": rules, "revision": revision, "note": "Евристична класифікація response HTML, не підтверджений тип шаблону. Вкладені schema-сутності можуть бути неоднозначними. Сигнали для перегляду не є автоматичними SEO-порушеннями. Правила й ручні типи діють лише в цьому аудиті; початкові SEO/GEO-дані не змінюються."})
}

func (s *webServer) pageTypeRules(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	var input struct {
		Origin   string         `json:"origin"`
		Revision int64          `json:"revision"`
		Rules    []pageTypeRule `json:"rules"`
	}
	if !decodeWebJSON(w, r, 32768, &input) {
		return
	}
	if len(input.Rules) > 32 || input.Revision < 0 {
		writeWebError(w, 400, "invalid_rules", "До 32 правил із коректною ревізією")
		return
	}
	for _, rule := range input.Rules {
		if !validPageType(rule.Type) || !validAuditRulePattern(rule.Pattern) || !strings.HasPrefix(rule.Pattern, "/") || strings.Count(rule.Pattern, "*") > 1 || strings.Contains(rule.Pattern, "*") && !strings.HasSuffix(rule.Pattern, "*") {
			writeWebError(w, 400, "invalid_rules", "Потрібен шлях /products/* або точний шлях і відомий тип")
			return
		}
	}
	if input.Rules == nil {
		input.Rules = []pageTypeRule{}
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if err = lockCoverageDestination(r.Context(), tx, id); err != nil {
		coverageMutationError(w, err)
		return
	}
	origins, err := insightOrigins(r.Context(), tx, id)
	if err != nil {
		webDBError(w, err)
		return
	}
	origin, err := selectedInsightOrigin(origins, input.Origin)
	if err != nil {
		writeWebError(w, 400, "invalid_origin", err.Error())
		return
	}
	_, revision, err := loadTypeRules(r.Context(), tx, id, origin)
	if err != nil {
		webDBError(w, err)
		return
	}
	if revision != input.Revision {
		writeWebError(w, 409, "revision_conflict", "Правила вже змінено; оновіть звіт")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_page_type_settings(run_id,origin,rules) VALUES($1,$2,$3) ON CONFLICT(run_id,origin) DO UPDATE SET rules=EXCLUDED.rules,revision=audit_page_type_settings.revision+1`, id, origin, input.Rules)
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]int64{"revision": revision + 1})
}

func (s *webServer) pageTypeOverride(w http.ResponseWriter, r *http.Request) {
	id, ok := webRunID(w, r)
	if !ok {
		return
	}
	target, parseErr := strconv.ParseInt(r.PathValue("target"), 10, 64)
	if parseErr != nil || target == 0 {
		writeWebError(w, 400, "invalid_target", "Некоректний target")
		return
	}
	var input struct {
		Type     string `json:"type"`
		Revision int64  `json:"revision"`
		Reason   string `json:"reason"`
	}
	if !decodeWebJSON(w, r, 4096, &input) {
		return
	}
	if (!validPageType(input.Type) && input.Type != "auto") || input.Revision < 0 || !utf8.ValidString(input.Reason) || utf8.RuneCountInString(strings.TrimSpace(input.Reason)) < 1 || utf8.RuneCountInString(input.Reason) > 500 || strings.ContainsFunc(input.Reason, unicode.IsControl) {
		writeWebError(w, 400, "invalid_override", "Потрібен тип, ревізія та причина до 500 символів")
		return
	}
	tx, err := s.pool.Begin(r.Context())
	if err != nil {
		webDBError(w, err)
		return
	}
	defer rollbackSiteTransaction(tx, s.cfg)
	if err = lockCoverageDestination(r.Context(), tx, id); err != nil {
		coverageMutationError(w, err)
		return
	}
	var exists bool
	if err = tx.QueryRow(r.Context(), `SELECT EXISTS(SELECT 1 FROM audit_results WHERE run_id=$1 AND target_id=$2)`, id, target).Scan(&exists); err != nil {
		webDBError(w, err)
		return
	}
	if !exists {
		writeWebError(w, 404, "not_found", "Target не знайдено")
		return
	}
	var revision int64
	err = tx.QueryRow(r.Context(), `SELECT revision FROM audit_page_type_overrides WHERE run_id=$1 AND target_id=$2`, id, target).Scan(&revision)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		webDBError(w, err)
		return
	}
	if revision != input.Revision {
		writeWebError(w, 409, "revision_conflict", "Тип уже змінено; оновіть звіт")
		return
	}
	_, err = tx.Exec(r.Context(), `INSERT INTO audit_page_type_overrides(run_id,target_id,page_type,reason) VALUES($1,$2,$3,$4) ON CONFLICT(run_id,target_id) DO UPDATE SET page_type=EXCLUDED.page_type,reason=EXCLUDED.reason,revision=audit_page_type_overrides.revision+1,updated_at=NOW()`, id, target, input.Type, sanitizeGEOTextForStorage(input.Reason, 500))
	if err == nil {
		err = tx.Commit(r.Context())
	}
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, 200, map[string]int64{"revision": revision + 1})
}
