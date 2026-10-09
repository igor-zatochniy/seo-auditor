package geo

import (
	"strings"
	"unicode"
)

type EntityRecord struct {
	Name   string   `json:"name"`
	Types  []string `json:"types,omitempty"`
	SameAs []string `json:"same_as,omitempty"`
}

// EntitySample описує розмітку, а не підтверджену юридичну особу чи експертність.
type EntitySample struct {
	Version       int            `json:"version"`
	Complete      bool           `json:"complete"`
	Organizations []EntityRecord `json:"organizations"`
	Authors       []EntityRecord `json:"authors"`
	Publishers    []EntityRecord `json:"publishers"`
	ProductBrands []EntityRecord `json:"product_brands"`
	HasProduct    bool           `json:"has_product"`
}

type EntityCheck struct {
	Key      string   `json:"key"`
	Name     string   `json:"name"`
	Status   string   `json:"status"`
	Evidence []string `json:"evidence"`
}

type EntityAssessment struct {
	Brand    string        `json:"brand"`
	Complete bool          `json:"complete"`
	Checks   []EntityCheck `json:"checks"`
}

func EvaluateEntities(brand string, s *Signals) *EntityAssessment {
	r := &EntityAssessment{Brand: brand}
	definitions := [][2]string{{"visible_brand", "Бренд в основному контенті"}, {"organization", "Назва організації"}, {"same_as", "Посилання sameAs організації"}, {"publisher", "Видавець"}, {"author", "Сутність автора"}, {"product_brand", "Бренд продукту"}}
	for _, d := range definitions {
		r.Checks = append(r.Checks, EntityCheck{Key: d[0], Name: d[1], Status: Unknown, Evidence: []string{}})
	}
	if s == nil || !s.Complete || s.Entities == nil || s.Entities.Version != 1 {
		return r
	}
	e := s.Entities
	r.Complete = e.Complete && !s.SchemaIncomplete && !s.ContentIncomplete
	brand = strings.TrimSpace(brand)
	if brand != "" && !s.ContentIncomplete {
		if containsBrand(s.Excerpt, brand) {
			r.Checks[0].Status = "found"
			r.Checks[0].Evidence = []string{brand}
		} else if !s.SampleTruncated {
			r.Checks[0].Status = "missing"
		}
	}
	assess := func(index int, records []EntityRecord, compare bool) {
		check := &r.Checks[index]
		if compare && brand == "" {
			return
		}
		matched := false
		for _, item := range records {
			if item.Name == "" {
				continue
			}
			check.Evidence = append(check.Evidence, item.Name)
			if !compare || normalizedBrand(item.Name) == normalizedBrand(brand) {
				matched = true
			}
		}
		if matched {
			check.Status = "found"
		} else if r.Complete {
			check.Status = "missing"
			if len(check.Evidence) > 0 {
				check.Status = "mismatch"
			}
		}
	}
	assess(1, e.Organizations, true)
	assess(3, e.Publishers, true)
	assess(4, e.Authors, false)
	assess(5, e.ProductBrands, true)
	if !e.HasProduct && r.Complete {
		r.Checks[5].Status = "not_applicable"
	}
	if brand != "" {
		for _, organization := range e.Organizations {
			if normalizedBrand(organization.Name) == normalizedBrand(brand) {
				r.Checks[2].Evidence = append(r.Checks[2].Evidence, organization.SameAs...)
			}
		}
		if len(r.Checks[2].Evidence) > 0 {
			r.Checks[2].Status = "found"
		} else if r.Complete {
			r.Checks[2].Status = "missing"
		}
	}
	return r
}

func normalizedBrand(value string) string { return strings.Join(strings.Fields(fold(value)), " ") }

func containsBrand(text, brand string) bool {
	words := func(value string) string {
		return strings.Join(strings.FieldsFunc(fold(value), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsNumber(r) }), " ")
	}
	b := words(brand)
	return b != "" && strings.Contains(" "+words(text)+" ", " "+b+" ")
}
