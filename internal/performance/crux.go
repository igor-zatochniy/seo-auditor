package performance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/igor-zatochniy/seo-auditor/internal/crawler"
	"io"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const CrUXEndpoint = "https://chromeuxreport.googleapis.com/v1/records:queryRecord"

type FieldMetric struct {
	P75    float64 `json:"p75"`
	Rating string  `json:"rating"`
	Unit   string  `json:"unit"`
}
type CrUXReport struct {
	Status     string                 `json:"status"`
	Scope      string                 `json:"scope"`
	Device     string                 `json:"device"`
	Requested  string                 `json:"requested"`
	Identifier string                 `json:"identifier,omitempty"`
	FirstDate  string                 `json:"first_date,omitempty"`
	LastDate   string                 `json:"last_date,omitempty"`
	Metrics    map[string]FieldMetric `json:"metrics"`
}

type cruxDate struct{ Year, Month, Day int }

func (d cruxDate) value() string {
	t := time.Date(d.Year, time.Month(d.Month), d.Day, 0, 0, 0, 0, time.UTC)
	if d.Year < 2000 || t.Year() != d.Year || int(t.Month()) != d.Month || t.Day() != d.Day {
		return ""
	}
	return t.Format("2006-01-02")
}

func QueryCrUX(ctx context.Context, client *http.Client, key, address, device, scope string) (*CrUXReport, error) {
	if key == "" || len(key) > 256 || strings.ContainsAny(key, "\r\n") {
		return nil, errors.New("invalid CrUX API key configuration")
	}
	if (device != "PHONE" && device != "DESKTOP") || (scope != "url" && scope != "origin") {
		return nil, errors.New("invalid CrUX query")
	}
	normalized, err := crawler.NormalizeTargetURL(address, false)
	if err != nil {
		return nil, errors.New("CrUX requires a public URL")
	}
	address = normalized
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("CrUX requires a public query-free URL")
	}
	if scope == "origin" {
		address = u.Scheme + "://" + u.Host
	}
	payload, _ := json.Marshal(map[string]any{scope: address, "formFactor": device, "metrics": []string{"largest_contentful_paint", "interaction_to_next_paint", "cumulative_layout_shift"}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, CrUXEndpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", key)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("CrUX network request failed")
	}
	defer resp.Body.Close()
	r := &CrUXReport{Status: "no_data", Scope: scope, Device: device, Requested: address, Metrics: map[string]FieldMetric{}}
	if resp.StatusCode == 404 {
		return r, nil
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("CrUX returned HTTP %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (64<<10)+1))
	if err != nil || len(raw) > 64<<10 {
		return nil, errors.New("CrUX response is incomplete or oversized")
	}
	var response struct {
		Record struct {
			Key              struct{ URL, Origin, FormFactor string }
			Metrics          map[string]struct{ Percentiles struct{ P75 json.RawMessage } }
			CollectionPeriod struct{ FirstDate, LastDate cruxDate }
		}
	}
	if json.Unmarshal(raw, &response) != nil {
		return nil, errors.New("invalid CrUX response")
	}
	record := response.Record
	r.Identifier = record.Key.URL
	if scope == "origin" {
		r.Identifier = record.Key.Origin
	}
	actual, e := url.Parse(r.Identifier)
	if e != nil || actual.User != nil || actual.RawQuery != "" || actual.Fragment != "" || actual.Scheme != u.Scheme || !strings.EqualFold(actual.Host, u.Host) || record.Key.FormFactor != device {
		return nil, errors.New("CrUX response scope mismatch")
	}
	r.FirstDate, r.LastDate = record.CollectionPeriod.FirstDate.value(), record.CollectionPeriod.LastDate.value()
	if r.FirstDate == "" || r.LastDate == "" || r.FirstDate > r.LastDate {
		return nil, errors.New("invalid CrUX collection period")
	}
	for _, m := range []struct {
		Key, Unit  string
		Good, Poor float64
	}{{"largest_contentful_paint", "ms", 2500, 4000}, {"interaction_to_next_paint", "ms", 200, 500}, {"cumulative_layout_shift", "", 0.1, 0.25}} {
		value, exists := record.Metrics[m.Key]
		if !exists {
			continue
		}
		encoded := string(value.Percentiles.P75)
		if len(encoded) == 0 {
			continue
		}
		if encoded[0] == '"' {
			if json.Unmarshal(value.Percentiles.P75, &encoded) != nil {
				continue
			}
		}
		number, e := strconv.ParseFloat(encoded, 64)
		if e != nil || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
			continue
		}
		r.Metrics[m.Key] = FieldMetric{number, Rating(&number, m.Good, m.Poor), m.Unit}
	}
	r.Status = "complete"
	if len(r.Metrics) != 3 {
		r.Status = "partial"
	}
	return r, nil
}
