//go:build qualitygate

package geo

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
)

// Набір надходить від людини; синтетичні очікування не видаються за незалежну оцінку.
func TestQualityGateHumanLabels(t *testing.T) {
	path := os.Getenv("GEO_HUMAN_DATASET")
	if path == "" {
		t.Skip("NOT VERIFIED: independent human labels were not supplied")
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var data struct {
		Kind             string      `json:"kind"`
		AnnotationID     string      `json:"annotation_id"`
		Model            string      `json:"model"`
		TopicModel       string      `json:"topic_model"`
		OpportunityModel string      `json:"opportunity_model"`
		Pages            []Page      `json:"pages"`
		Facts            []PageFacts `json:"facts"`
		Cases            []struct {
			Query          string   `json:"query"`
			TargetURL      string   `json:"target_url"`
			Cluster        string   `json:"cluster"`
			TasksReviewed  bool     `json:"tasks_reviewed"`
			RequiredTasks  []string `json:"required_tasks"`
			ForbiddenTasks []string `json:"forbidden_tasks"`
		} `json:"cases"`
	}
	d := json.NewDecoder(io.LimitReader(f, 8<<20))
	d.DisallowUnknownFields()
	if err = d.Decode(&data); err != nil {
		t.Fatal(err)
	}
	if data.Kind != "human" || strings.TrimSpace(data.AnnotationID) == "" || data.Model != Model || data.TopicModel != TopicModel || data.OpportunityModel != OpportunityModel || len(data.Cases) < 100 || len(data.Cases) > 200 || len(data.Pages) > MaxPages {
		t.Fatal("requires 100–200 independent human cases tied to current models")
	}
	queries := []string{}
	seen := map[string]bool{}
	for _, c := range data.Cases {
		if c.Query == "" || c.Cluster == "" || !c.TasksReviewed || seen[c.Query] {
			t.Fatal("incomplete or duplicate annotation")
		}
		seen[c.Query] = true
		queries = append(queries, c.Query)
	}
	r, err := Analyze(context.Background(), queries, data.Pages)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := BuildOpportunityPlan(context.Background(), PlanInput{Results: r, Pages: data.Pages, Facts: data.Facts})
	if err != nil {
		t.Fatal(err)
	}
	group := map[int]int{}
	for _, topic := range plan.Topics {
		for _, q := range topic.Queries {
			group[q.Ordinal] = topic.ID
		}
	}
	correct, tp, fp, fn, required, found, forbidden := 0, 0, 0, 0, 0, 0, 0
	for i, c := range data.Cases {
		if r[i].TargetURL == c.TargetURL {
			correct++
		}
		kinds := map[string]bool{}
		for _, task := range plan.Tasks {
			for _, q := range task.Queries {
				if q == c.Query {
					kinds[task.Kind] = true
				}
			}
		}
		for _, v := range c.RequiredTasks {
			required++
			if kinds[v] {
				found++
			}
		}
		for _, v := range c.ForbiddenTasks {
			if kinds[v] {
				forbidden++
			}
		}
		for j := i + 1; j < len(data.Cases); j++ {
			want, got := c.Cluster == data.Cases[j].Cluster, group[i+1] == group[j+1]
			if want && got {
				tp++
			}
			if !want && got {
				fp++
			}
			if want && !got {
				fn++
			}
		}
	}
	if required == 0 || tp+fn == 0 || fp+fn+tp == 0 {
		t.Fatal("dataset must assess both recommendations and cluster membership")
	}
	accuracy := float64(correct) / float64(len(r))
	f1 := float64(2*tp) / float64(2*tp+fp+fn)
	recall := float64(found) / float64(required)
	t.Logf("human validation: model=%s topics=%s opportunities=%s cases=%d top1=%.3f clusterF1=%.3f requiredTaskRecall=%.3f forbiddenTasks=%d", Model, TopicModel, OpportunityModel, len(r), accuracy, f1, recall, forbidden)
	if accuracy < 0.85 || f1 < 0.8 || recall < 0.9 || forbidden > 0 {
		t.Fatal("human-labeled quality threshold was not met")
	}
}
