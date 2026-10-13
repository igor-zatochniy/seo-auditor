package crawler

import "context"

const MaxRouteHops = 10

type RouteNode struct {
	ID          int64       `json:"target_id"`
	URL         string      `json:"url"`
	Fingerprint string      `json:"-"`
	KeyID       string      `json:"-"`
	HTTP        *int        `json:"http"`
	Status      string      `json:"status"`
	Noindex     bool        `json:"noindex"`
	Sitemap     bool        `json:"in_sitemap"`
	Signals     *URLSignals `json:"-"`
	Ambiguous   bool        `json:"-"`
}

type URLRoute struct {
	State       string      `json:"state"`
	Steps       []RouteNode `json:"steps"`
	Destination string      `json:"destination,omitempty"`
}

func (n RouteNode) Reference(mode string) *URLReference {
	if n.Signals == nil {
		return nil
	}
	if n.Status == "redirect" {
		return n.Signals.Redirect
	}
	if mode == "canonical" {
		if n.Signals.HTML != nil {
			return n.Signals.HTML
		}
		if len(n.Signals.HTTP) > 0 {
			return &n.Signals.HTTP[0]
		}
	}
	return nil
}

func TraceURLRoute(ctx context.Context, start RouteNode, mode string, lookup func(string, string) (RouteNode, bool)) URLRoute {
	r := URLRoute{Steps: []RouteNode{start}}
	current := start
	seen := map[int64]bool{start.ID: true}
	for hops := 0; hops <= MaxRouteHops; hops++ {
		if ctx.Err() != nil {
			r.State = "canceled"
			return r
		}
		if current.Ambiguous {
			r.State = "ambiguous"
			return r
		}
		ref := current.Reference(mode)
		if hops == 0 && mode == "redirect" && current.Status != "redirect" {
			r.State = "not_redirect"
			return r
		}
		if ref == nil {
			switch {
			case current.HTTP == nil || current.Status == "failed" || current.Status == "blocked_by_robots":
				r.State = "unverified"
			case *current.HTTP >= 400:
				r.State = "http_error"
			case current.Noindex:
				r.State = "noindex"
			case current.Status == "redirect" || current.Signals == nil || !current.Signals.Complete:
				r.State = "incomplete"
			case hops == 0 && mode == "canonical":
				r.State = "not_declared"
			default:
				r.State = "resolved"
			}
			return r
		}
		r.Destination = ref.URL
		if ref.Truncated {
			r.State = "incomplete"
			return r
		}
		if !ref.Valid || len(ref.Fingerprints) == 0 {
			r.State = "invalid_reference"
			return r
		}
		if hops == MaxRouteHops {
			r.State = "hop_limit"
			return r
		}
		var next RouteNode
		found := false
		for _, fp := range ref.Fingerprints {
			if next, found = lookup(current.KeyID, fp); found {
				break
			}
		}
		if !found {
			r.State = "not_audited"
			return r
		}
		// Self-canonical завершує маршрут; self-redirect є циклом.
		if next.ID == current.ID && current.Status != "redirect" {
			switch {
			case current.Noindex:
				r.State = "noindex"
			case current.HTTP != nil && *current.HTTP >= 400:
				r.State = "http_error"
			default:
				r.State = "resolved"
			}
			return r
		}
		r.Steps = append(r.Steps, next)
		if seen[next.ID] {
			r.State = "loop"
			return r
		}
		seen[next.ID] = true
		current = next
	}
	return r
}
