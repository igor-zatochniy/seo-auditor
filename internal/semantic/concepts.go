package semantic

import (
	"strings"
	"unicode"

	"golang.org/x/text/cases"
	"golang.org/x/text/unicode/norm"
)

const Model = "local-concepts-en-uk-ru-pl-es-ro-v1"
const MaxUnits = 24

type Unit struct {
	Key      string   `json:"key"`
	Label    string   `json:"label"`
	Observed string   `json:"observed"`
	Aliases  []string `json:"-"`
	Known    bool     `json:"known"`
}

type concept struct{ key, label, aliases string }

// Версіонований контрольований словник, не перекладач і не embedding-модель.
// Значення іншими мовами є даними для зіставлення, а не текстом інтерфейсу.
var concepts = []concept{
	{"chargeback", "чарджбек", "chargeback|chargebacks|чарджбек|чарджбеки|чарджбеків|чарджбеков|chargebacki|chargebacków|contracargo|contracargos"},
	{"prevention", "запобігання", "prevention|prevent|preventing|preventive|запобігання|запобігти|профілактика|предотвращение|предотвратить|zapobieganie|zapobiegać|prevención|prevenir|prevenire|preveni"},
	{"protection", "захист", "protection|protect|protected|protecting|захист|захисту|захистити|защита|защиты|защитить|ochrona|ochrony|chronić|protección|proteger|protecție|protecția|proteja"},
	{"payment", "платіж", "payment|payments|pay|платіж|платежі|платежів|оплата|платеж|платежи|платежей|płatność|płatności|pago|pagos|plată|plăți|plăților"},
	{"fraud", "шахрайство", "fraud|fraudulent|шахрайство|шахрайства|мошенничество|мошенничества|oszustwo|oszustwa|fraude|fraudes|fraudă"},
	{"software", "програмне забезпечення", "software|tool|tools|інструмент|інструменти|инструмент|инструменты|narzędzie|narzędzia|herramienta|herramientas|instrument|instrumente|програма|програми|програмне забезпечення|программное обеспечение|программа|программы|oprogramowanie|programa|programas|program"},
	{"management", "управління", "management|manage|managing|управління|керування|управление|zarządzanie|gestión|gestionar|gestionare|administrare"},
	{"automation", "автоматизація", "automation|automate|automated|автоматизація|автоматизований|автоматизация|автоматизированный|automatyzacja|automatyczny|automatización|automatizar|automatizare|automatizat"},
	{"integration", "інтеграція", "integration|integrations|integrate|інтеграція|інтеграції|интеграция|интеграции|integracja|integracje|integración|integraciones|integrare|integrări"},
	{"solution", "рішення", "solution|solutions|рішення|решение|решения|rozwiązanie|rozwiązania|solución|soluciones|soluție|soluții"},
	{"comparison", "порівняння", "comparison|compare|comparing|порівняння|порівняти|сравнение|сравнить|porównanie|porównać|comparación|comparar|comparație|comparare"},
	{"alternative", "альтернатива", "alternative|alternatives|альтернатива|альтернативи|альтернативы|alternatywa|alternatywy|alternativa|alternativas|alternativele"},
	{"price", "ціна", "price|prices|pricing|cost|ціна|ціни|вартість|цена|цены|стоимость|cena|ceny|koszt|precio|precios|coste|preț|prețuri|costul"},
	{"review", "огляд", "review|reviews|огляд|огляди|обзор|обзоры|recenzja|recenzje|reseña|reseñas|recenzie|recenzii"},
	{"security", "безпека", "security|secure|безпека|безпеки|безпечний|безопасность|безопасный|bezpieczeństwo|bezpieczny|seguridad|seguro|securitate|siguranță"},
	{"customer", "клієнт", "customer|customers|client|clients|клієнт|клієнти|клієнтів|клиент|клиенты|клиентов|klient|klienci|klientów|cliente|clientes|clienți"},
	{"support", "підтримка", "support|підтримка|підтримки|поддержка|поддержки|wsparcie|soporte|asistencia|asistență|suport"},
	{"business", "бізнес", "business|businesses|бізнес|бізнесу|бизнес|biznes|negocio|negocios|afacere|afaceri"},
	{"commerce", "електронна торгівля", "ecommerce|e-commerce|електронна торгівля|электронная торговля|comercio electrónico|comerț electronic|handel elektroniczny"},
	{"shipping", "доставка", "shipping|delivery|доставка|доставки|доставку|dostawa|wysyłka|envío|entrega|livrare"},
	{"refund", "повернення коштів", "refund|refunds|повернення коштів|возврат средств|zwrot pieniędzy|reembolso|reembolsos|restituire|restituiri"},
	{"dispute", "спір", "dispute|disputes|спір|спори|спорів|спор|споры|споров|spór|spory|disputa|disputas|litigiu|litigii"},
	{"alert", "сповіщення", "alert|alerts|сповіщення|оповіщення|уведомление|уведомления|powiadomienie|powiadomienia|alerta|alertas|alerte|notificare"},
	{"subscription", "підписка", "subscription|subscriptions|підписка|підписки|подписка|подписки|subskrypcja|subskrypcje|suscripción|suscripciones|abonament|abonamente"},
	{"analytics", "аналітика", "analytics|аналітика|аналитика|analityka|analítica|analitică"},
	{"report", "звіт", "report|reports|звіт|звіти|отчет|отчеты|отчёт|отчёты|raport|raporty|informe|informes|rapoarte"},
	{"audit", "аудит", "audit|audits|аудит|аудити|аудиты|audyt|audyty|auditoría|auditorías|auditare"},
	{"content", "контент", "content|контент|зміст|содержание|treść|contenido|conținut"},
	{"optimization", "оптимізація", "optimization|optimisation|optimize|оптимізація|оптимизации|оптимизация|optymalizacja|optimización|optimizare"},
	{"indexing", "індексація", "indexing|indexation|індексація|индексация|indeksowanie|indexación|indexare"},
	{"search", "пошук", "search|пошук|поиск|wyszukiwanie|búsqueda|căutare"},
	{"canonical", "канонічний URL", "canonical|канонічний|канонический|kanoniczny|canónico|canonic"},
	{"duplicate", "дубль", "duplicate|duplicates|дубль|дублі|дубликат|дубликаты|duplikat|duplikaty|duplicado|duplicados|duplicat|duplicatele"},
	{"language", "мова", "language|languages|мова|мови|язык|языки|język|języki|idioma|idiomas|limbă|limbi"},
	{"speed", "швидкість", "speed|швидкість|скорость|szybkość|velocidad|viteză"},
	{"loading", "завантаження", "loading|load|завантаження|загрузка|ładowanie|carga|încărcare"},
	{"image", "зображення", "image|images|зображення|изображение|изображения|obraz|obrazy|imagen|imágenes|imagine|imagini"},
	{"link", "посилання", "link|links|посилання|ссылка|ссылки|łącze|łącza|enlace|enlaces|legătură|legături"},
	{"internal", "внутрішній", "internal|внутрішній|внутрішні|внутренний|внутренние|wewnętrzny|wewnętrzne|interno|internos|internă|interne"},
	{"product", "товар", "product|products|товар|товари|товары|produkt|produkty|producto|productos|produs|produse"},
	{"service", "послуга", "service|services|послуга|послуги|услуга|услуги|usługa|usługi|servicio|servicios|serviciu|servicii"},
}

var stop = strings.Fields("a an the and or for to of in on with by is are how what why best top як що чому для та і й або в у на з із це найкращий найкращі как что почему для и или в на с лучший лучшие jak co dlaczego dla i lub w na z najlepszy najlepsze el la los las un una y o de del en para con cómo qué mejor mejores cel cea și sau de din în pentru cu cum ce mai bun")

func words(s string) []string {
	s = cases.Fold().String(norm.NFKC.String(s))
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) })
}

var conceptLookup = func() map[string]Unit {
	lookup := map[string]Unit{}
	for _, c := range concepts {
		aliases := strings.Split(c.aliases, "|")
		for _, alias := range aliases {
			lookup[strings.Join(words(alias), " ")] = Unit{Key: c.key, Label: c.label, Aliases: aliases, Known: true}
		}
	}
	return lookup
}()

// Порожні чи невідомі поняття не вигадуються; власні назви залишаються термінами.
func Extract(text string) ([]Unit, bool) {
	input := words(text)
	seen, out := map[string]bool{}, []Unit{}
	for i := 0; i < len(input); {
		u := Unit{Key: input[i], Label: input[i], Aliases: []string{input[i]}, Observed: input[i]}
		step := 1
		for n := min(3, len(input)-i); n > 0; n-- {
			key := strings.Join(input[i:i+n], " ")
			if known, ok := conceptLookup[key]; ok {
				u = known
				u.Observed = key
				step = n
				break
			}
		}
		i += step
		ignored := false
		if !u.Known {
			for _, s := range stop {
				if u.Key == s {
					ignored = true
					break
				}
			}
		}
		if ignored || seen[u.Key] || len([]rune(u.Key)) < 2 || len([]rune(u.Key)) > 64 {
			continue
		}
		if len(out) == MaxUnits {
			return out, true
		}
		seen[u.Key] = true
		out = append(out, u)
	}
	return out, false
}

func SingleTerms(u Unit) []string {
	seen, out := map[string]bool{}, []string{}
	for _, a := range u.Aliases {
		w := words(a)
		if len(w) == 1 && !seen[w[0]] {
			seen[w[0]] = true
			out = append(out, w[0])
		}
	}
	return out
}
