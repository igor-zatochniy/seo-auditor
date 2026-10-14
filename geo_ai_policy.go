package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/igor-zatochniy/seo-auditor/internal/geo"
	"github.com/jackc/pgx/v5/pgtype"
)

type aiPolicySummary struct {
	Token      string `json:"token"`
	Allowed    int64  `json:"allowed"`
	Blocked    int64  `json:"blocked"`
	Unknown    int64  `json:"unknown"`
	Mismatches int64  `json:"mismatches"`
	Conflicts  int64  `json:"conflicts"`
	Drift      int64  `json:"drift"`
}

type aiPolicyComparison struct {
	Spec           geo.AIPolicySpec `json:"spec"`
	Evidence       geo.AIControl    `json:"evidence"`
	Desired        string           `json:"desired"`
	Mismatch       bool             `json:"mismatch"`
	PreviousStatus string           `json:"previous_status"`
	Drift          string           `json:"drift"`
}

type aiPolicyPage struct {
	TargetID      int64                `json:"target_id"`
	URL           string               `json:"url"`
	ScanStatus    string               `json:"scan_status"`
	HTTPStatus    *int                 `json:"http_status,omitempty"`
	Controls      []aiPolicyComparison `json:"controls"`
	Mismatches    int                  `json:"mismatches"`
	Conflicts     int                  `json:"conflicts"`
	PolicyChanges int                  `json:"policy_changes"`
}

type aiDiscoverabilityResponse struct {
	SourceRunID       string            `json:"source_run_id"`
	Domain            string            `json:"domain"`
	DesiredConfigured bool              `json:"desired_configured"`
	DesiredPolicy     map[string]string `json:"desired_policy"`
	BaselineRunID     string            `json:"baseline_run_id,omitempty"`
	PagesTotal        int64             `json:"pages_total"`
	PagesWithMatrix   int64             `json:"pages_with_matrix"`
	Summary           []aiPolicySummary `json:"summary"`
	Items             []aiPolicyPage    `json:"items"`
	NextAfter         *int64            `json:"next_after,omitempty"`
	Limit             int               `json:"limit"`
}

func (s *webServer) registerAIDiscoverability(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/geo/ai-policy-specs", func(w http.ResponseWriter, r *http.Request) {
		writeWebJSON(w, http.StatusOK, geo.SupportedAIPolicySpecs)
	})
	mux.HandleFunc("GET /api/geo/reports/{id}/ai-discoverability", s.geoAIDiscoverability)
}

func (s *webServer) geoAIDiscoverability(w http.ResponseWriter, r *http.Request) {
	reportID, ok := webRunID(w, r)
	if !ok {
		return
	}
	limit := 50
	if raw := r.URL.Query().Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 || parsed > 100 {
			writeWebError(w, http.StatusBadRequest, "invalid_limit", "Ліміт сторінки має бути від 1 до 100")
			return
		}
		limit = parsed
	}
	var after *int64
	if raw := r.URL.Query().Get("after"); raw != "" {
		parsed, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			writeWebError(w, http.StatusBadRequest, "invalid_cursor", "Некоректний курсор сторінки")
			return
		}
		after = &parsed
	}

	var response aiDiscoverabilityResponse
	var desiredJSON []byte
	var baselineSource pgtype.Text
	err := s.pool.QueryRow(r.Context(), `SELECT report.source_run_id::text,report.domain,
		COALESCE(profile_run.desired_ai_policy,'{}'::jsonb),baseline.source_run_id::text,
		EXISTS(SELECT 1 FROM jsonb_each_text(COALESCE(profile_run.desired_ai_policy,'{}'::jsonb)) policy
			WHERE policy.value IN ('allowed','blocked'))
		FROM geo_reports report
		LEFT JOIN geo_profile_runs profile_run ON profile_run.report_id=report.id
		LEFT JOIN geo_reports baseline ON baseline.id=profile_run.baseline_report_id
		WHERE report.id=$1`, reportID).Scan(&response.SourceRunID, &response.Domain, &desiredJSON, &baselineSource, &response.DesiredConfigured)
	if err != nil {
		webDBError(w, err)
		return
	}
	if err = json.Unmarshal(desiredJSON, &response.DesiredPolicy); err != nil {
		webDBError(w, err)
		return
	}
	response.DesiredPolicy, err = geo.NormalizeDesiredAIPolicy(response.DesiredPolicy)
	if err != nil {
		webDBError(w, err)
		return
	}
	var baselineRunID *string
	if baselineSource.Valid {
		response.BaselineRunID = baselineSource.String
		baselineRunID = &response.BaselineRunID
	}
	response.Limit = limit

	if err = s.pool.QueryRow(r.Context(), `SELECT COUNT(*),COUNT(*) FILTER (
		WHERE CASE WHEN jsonb_typeof(geo_signals->'search'->'ai_policies')='array'
		THEN jsonb_array_length(geo_signals->'search'->'ai_policies')>0 ELSE FALSE END)
		FROM audit_results WHERE run_id=$1`, response.SourceRunID).Scan(&response.PagesTotal, &response.PagesWithMatrix); err != nil {
		webDBError(w, err)
		return
	}
	response.Summary, err = s.aiPolicySummary(r, response.SourceRunID, baselineRunID, response.DesiredPolicy)
	if err != nil {
		webDBError(w, err)
		return
	}
	response.Items, response.NextAfter, err = s.aiPolicyPage(r, response.SourceRunID, baselineRunID, response.DesiredPolicy, after, limit)
	if err != nil {
		webDBError(w, err)
		return
	}
	writeWebJSON(w, http.StatusOK, response)
}

const aiPoliciesJSON = `CASE WHEN jsonb_typeof(%s)='array' THEN %s ELSE '[]'::jsonb END`

func aiPolicyJSONExpr(column string) string {
	return fmt.Sprintf(aiPoliciesJSON, column, column)
}

func (s *webServer) aiPolicySummary(r *http.Request, runID string, baselineRunID *string, desired map[string]string) ([]aiPolicySummary, error) {
	desiredBytes, err := json.Marshal(desired)
	if err != nil {
		return nil, err
	}
	previousExpr := aiPolicyJSONExpr("old.geo_signals->'search'->'ai_policies'")
	query := `WITH observations AS (
		SELECT token.key AS token,current_control.status,current_control.conflict,
			($3::jsonb)->>(token.key) AS desired,prior.status AS previous_status
		FROM audit_results current_result
		CROSS JOIN LATERAL jsonb_object_keys($3::jsonb) token(key)
		LEFT JOIN LATERAL (
			SELECT current_control.value->>'status' AS status,COALESCE(current_control.value->>'conflict'='true',false) AS conflict
			FROM jsonb_array_elements(` + aiPolicyJSONExpr("current_result.geo_signals->'search'->'ai_policies'") + `) current_control(value)
			WHERE lower(current_control.value->>'token')=token.key LIMIT 1
		) current_control ON TRUE
		LEFT JOIN LATERAL (
			SELECT ` + previousExpr + ` AS policies FROM audit_results old
			WHERE old.run_id=$2::uuid AND old.target_fingerprint=current_result.target_fingerprint
			AND old.fingerprint_key_id=current_result.fingerprint_key_id ORDER BY old.target_id LIMIT 1
		) previous_row ON TRUE
		LEFT JOIN LATERAL (
			SELECT old_control.value->>'status' AS status
			FROM jsonb_array_elements(previous_row.policies) old_control(value)
			WHERE lower(old_control.value->>'token')=token.key LIMIT 1
		) prior ON TRUE
		WHERE current_result.run_id=$1
	)
	SELECT token,COUNT(*) FILTER(WHERE status='allowed'),COUNT(*) FILTER(WHERE status='blocked'),
		COUNT(*) FILTER(WHERE status IS NULL OR status='unknown'),
		COUNT(*) FILTER(WHERE desired IN ('allowed','blocked') AND status IN ('allowed','blocked') AND desired<>status),
		COUNT(*) FILTER(WHERE conflict),
		COUNT(*) FILTER(WHERE status IN ('allowed','blocked') AND previous_status IN ('allowed','blocked') AND status<>previous_status)
	FROM observations GROUP BY token`
	rows, err := s.pool.Query(r.Context(), query, runID, baselineRunID, string(desiredBytes))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]aiPolicySummary, len(geo.SupportedAIPolicySpecs))
	for rows.Next() {
		var item aiPolicySummary
		if err = rows.Scan(&item.Token, &item.Allowed, &item.Blocked, &item.Unknown, &item.Mismatches, &item.Conflicts, &item.Drift); err != nil {
			return nil, err
		}
		counts[strings.ToLower(item.Token)] = item
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result := make([]aiPolicySummary, 0, len(geo.SupportedAIPolicySpecs))
	for _, spec := range geo.SupportedAIPolicySpecs {
		item := counts[strings.ToLower(spec.Token)]
		item.Token = spec.Token
		result = append(result, item)
	}
	return result, nil
}

func (s *webServer) aiPolicyPage(r *http.Request, runID string, baselineRunID *string, desired map[string]string, after *int64, limit int) ([]aiPolicyPage, *int64, error) {
	currentExpr := aiPolicyJSONExpr("current_result.geo_signals->'search'->'ai_policies'")
	previousExpr := aiPolicyJSONExpr("old.geo_signals->'search'->'ai_policies'")
	query := `SELECT current_result.target_id,current_result.safe_url,current_result.scan_status,current_result.status_code,
		` + currentExpr + `,COALESCE(previous.policies,'[]'::jsonb)
	FROM audit_results current_result
	LEFT JOIN LATERAL (
		SELECT ` + previousExpr + ` AS policies FROM audit_results old
		WHERE old.run_id=$3::uuid AND old.target_fingerprint=current_result.target_fingerprint
		AND old.fingerprint_key_id=current_result.fingerprint_key_id ORDER BY old.target_id LIMIT 1
	) previous ON TRUE
	WHERE current_result.run_id=$1 AND ($2::bigint IS NULL OR current_result.target_id>$2)
	ORDER BY current_result.target_id LIMIT $4`
	rows, err := s.pool.Query(r.Context(), query, runID, after, baselineRunID, limit+1)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	items := make([]aiPolicyPage, 0, limit+1)
	for rows.Next() {
		var item aiPolicyPage
		var httpStatus pgtype.Int4
		var currentJSON, previousJSON []byte
		if err = rows.Scan(&item.TargetID, &item.URL, &item.ScanStatus, &httpStatus, &currentJSON, &previousJSON); err != nil {
			return nil, nil, err
		}
		if httpStatus.Valid {
			value := int(httpStatus.Int32)
			item.HTTPStatus = &value
		}
		item.Controls = compareAIPolicies(currentJSON, previousJSON, desired, baselineRunID != nil)
		for _, control := range item.Controls {
			if control.Mismatch {
				item.Mismatches++
			}
			if control.Evidence.Conflict {
				item.Conflicts++
			}
			if control.Drift == "changed" {
				item.PolicyChanges++
			}
		}
		items = append(items, item)
	}
	if err = rows.Err(); err != nil {
		return nil, nil, err
	}
	var next *int64
	if len(items) > limit {
		items = items[:limit]
		cursor := items[len(items)-1].TargetID
		next = &cursor
	}
	return items, next, nil
}

func compareAIPolicies(currentJSON, previousJSON []byte, desired map[string]string, baselineAvailable bool) []aiPolicyComparison {
	current := make(map[string]geo.AIControl)
	previous := make(map[string]geo.AIControl)
	var currentControls []geo.AIControl
	var previousControls []geo.AIControl
	_ = json.Unmarshal(currentJSON, &currentControls)
	_ = json.Unmarshal(previousJSON, &previousControls)
	for _, control := range currentControls {
		current[strings.ToLower(control.Token)] = control
	}
	for _, control := range previousControls {
		previous[strings.ToLower(control.Token)] = control
	}
	result := make([]aiPolicyComparison, 0, len(geo.SupportedAIPolicySpecs))
	for _, spec := range geo.SupportedAIPolicySpecs {
		key := strings.ToLower(spec.Token)
		evidence, found := current[key]
		if !found {
			evidence = geo.AIControl{Token: spec.Token, Provider: spec.Provider, Purpose: spec.Purpose, Status: geo.Unknown, Confidence: "unknown", Explanation: "У цьому аудиті матрицю ще не збережено."}
		}
		prior, hasPrior := previous[key]
		drift := "no_baseline"
		if baselineAvailable {
			drift = "not_comparable"
		}
		previousStatus := geo.Unknown
		if hasPrior && baselineStatusComparable(evidence.Status, prior.Status) {
			previousStatus = prior.Status
			drift = "stable"
			if evidence.Status != prior.Status {
				drift = "changed"
			}
		} else if hasPrior {
			previousStatus = prior.Status
			drift = "not_comparable"
		}
		want := desired[key]
		mismatch := (want == geo.Allowed || want == geo.Blocked) && baselineStatusKnown(evidence.Status) && want != evidence.Status
		result = append(result, aiPolicyComparison{Spec: spec, Evidence: evidence, Desired: want, Mismatch: mismatch, PreviousStatus: previousStatus, Drift: drift})
	}
	return result
}

func baselineStatusKnown(status string) bool { return status == geo.Allowed || status == geo.Blocked }

func baselineStatusComparable(current, previous string) bool {
	return baselineStatusKnown(current) && baselineStatusKnown(previous)
}
