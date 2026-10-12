//go:build integration

package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/igor-zatochniy/seo-auditor/internal/duplicates"
)

func TestDuplicateReportTenThousandPagesAndMemberPagination(t *testing.T) {
	app, cfg := duplicateAuditFixture(t)
	if _, err := app.pool.Exec(t.Context(), `INSERT INTO audit_run_targets(run_id,target_id,request_url,status,attempts,finished_at)
	 SELECT $1,1000+i,'','completed',1,CURRENT_TIMESTAMP FROM generate_series(1,10000) i`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(t.Context(), `INSERT INTO audit_results(run_id,target_id,safe_url,target_fingerprint,fingerprint_key_id,status_code,scan_status,title,h1,international,content_signature)
	 SELECT a.run_id,1000+i,'https://example.com/copy/'||i,decode(md5(i::text)||md5(i::text),'hex'),a.fingerprint_key_id,200,'completed',a.title,a.h1,a.international,a.content_signature
	 FROM audit_results a CROSS JOIN generate_series(1,10000) i WHERE a.run_id=$1 AND a.target_id=1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	if _, err := app.pool.Exec(t.Context(), `UPDATE audit_runs SET total_urls=10002,successful_urls=10002,failed_urls=0 WHERE id=$1`, cfg.RunID); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	w := strategyRequest(app, "GET", "/api/audits/"+cfg.RunID+"/duplicates", "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	var response struct {
		Summary  duplicates.Summary `json:"summary"`
		Clusters []duplicateCluster `json:"clusters"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Summary.Analyzed != 10002 || !response.Summary.NearSearchComplete || response.Summary.ExactClusters != 1 || len(response.Clusters) != 1 || response.Clusters[0].Count != 10002 || len(response.Clusters[0].Members) != 5 || response.Clusters[0].MemberNext == 0 {
		t.Fatal(response.Summary, len(response.Clusters))
	}
	t.Logf("10002 pages: %s", time.Since(started))
	cluster := response.Clusters[0].ID
	w = strategyRequest(app, "GET", fmt.Sprintf("/api/audits/%s/duplicates?cluster=%d", cfg.RunID, cluster), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Clusters[0].Members) != 50 {
		t.Fatal("member page not bounded", len(response.Clusters[0].Members))
	}
	after := response.Clusters[0].MemberNext
	w = strategyRequest(app, "GET", fmt.Sprintf("/api/audits/%s/duplicates?cluster=%d&member_after=%d", cfg.RunID, cluster, after), "")
	if w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Clusters[0].Members) != 50 || response.Clusters[0].Members[0].TargetID <= after {
		t.Fatal("member cursor repeated data", response.Clusters[0])
	}
}
