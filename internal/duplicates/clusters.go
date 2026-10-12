package duplicates

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strconv"
	"strings"

	"github.com/mfonda/simhash"
)

const MaxPages = 50_000
const MaxComparisons = 500_000

type Document struct {
	TargetID      int64
	Signature     *Signature
	Canonical     string
	KeyID         string
	TitleKey      string
	H1Key         string
	Hreflang      bool
	Product       bool
	CanonicalLost bool
}

type Summary struct {
	Model              string `json:"model"`
	Pages              int    `json:"pages"`
	Eligible           int    `json:"eligible"`
	Analyzed           int    `json:"analyzed"`
	Unverified         int    `json:"unverified"`
	ShortContent       int    `json:"short_content"`
	CapacityExcluded   int    `json:"capacity_excluded"`
	SimilarPages       int    `json:"similar_pages"`
	ExactClusters      int    `json:"exact_clusters"`
	NearClusters       int    `json:"near_clusters"`
	CanonicalReviews   int    `json:"canonical_reviews"`
	PotentialOverlaps  int    `json:"potential_overlaps"`
	Comparisons        int    `json:"comparisons"`
	NearSearchComplete bool   `json:"near_search_complete"`
	SignaturesComplete bool   `json:"signatures_complete"`
}

type Cluster struct {
	ID               int64   `json:"id"`
	Kind             string  `json:"kind"`
	Count            int     `json:"count"`
	Context          string  `json:"context"`
	Canonical        string  `json:"canonical"`
	PotentialOverlap bool    `json:"potential_overlap"`
	ExactGroups      int     `json:"exact_groups"`
	TargetIDs        []int64 `json:"-"`
}

type Detector struct {
	documents []Document
	summary   Summary
}

func NewDetector() *Detector {
	return &Detector{summary: Summary{Model: Model, NearSearchComplete: true, SignaturesComplete: true}}
}

func (d *Detector) Add(p Document) {
	d.summary.Pages++
	s := p.Signature
	if s == nil || !s.Complete || s.Model != Model || !validHash(s.SHA256) {
		d.summary.Unverified++
		d.summary.SignaturesComplete = false
		return
	}
	if _, err := strconv.ParseUint(s.SimHash, 16, 64); err != nil {
		d.summary.Unverified++
		d.summary.SignaturesComplete = false
		return
	}
	if s.Words < MinWords {
		d.summary.ShortContent++
		return
	}
	d.summary.Eligible++
	if len(d.documents) >= MaxPages {
		d.summary.CapacityExcluded++
		d.summary.NearSearchComplete = false
		return
	}
	d.documents = append(d.documents, p)
}

func validHash(s string) bool {
	if len(s) != 64 {
		return false
	}
	_, err := hex.DecodeString(s)
	return err == nil
}

type hashGroup struct {
	hash    uint64
	parent  int
	members []int
}

func (d *Detector) Analyze(ctx context.Context) (Summary, []Cluster, error) {
	sort.Slice(d.documents, func(i, j int) bool { return d.documents[i].TargetID < d.documents[j].TargetID })
	summary := d.summary
	summary.Analyzed = len(d.documents)
	groups := []hashGroup{}
	exact := map[string]int{}
	for i, p := range d.documents {
		if err := ctx.Err(); err != nil {
			return Summary{}, nil, err
		}
		g, found := exact[p.Signature.SHA256]
		if !found {
			g = len(groups)
			h, _ := strconv.ParseUint(p.Signature.SimHash, 16, 64)
			groups = append(groups, hashGroup{hash: h, parent: g})
			exact[p.Signature.SHA256] = g
		}
		groups[g].members = append(groups[g].members, i)
	}
	root := func(i int) int {
		for groups[i].parent != i {
			groups[i].parent = groups[groups[i].parent].parent
			i = groups[i].parent
		}
		return i
	}
	join := func(a, b int) {
		a, b = root(a), root(b)
		if a != b {
			if a > b {
				a, b = b, a
			}
			groups[b].parent = a
		}
	}
	// Однаковий SimHash з'єднується за O(n), а не попарно для великих груп.
	byHash := map[uint64]int{}
	representatives := []int{}
	for i := range groups {
		if g, ok := byHash[groups[i].hash]; ok {
			join(g, i)
		} else {
			byHash[groups[i].hash] = i
			representatives = append(representatives, i)
		}
	}
	var bands [4]map[uint16][]int
	for i := range bands {
		bands[i] = map[uint16][]int{}
	}
search:
	for _, i := range representatives {
		if err := ctx.Err(); err != nil {
			return Summary{}, nil, err
		}
		seen := map[int]bool{}
		for band := range bands {
			key := uint16(groups[i].hash >> (16 * band))
			for _, j := range bands[band][key] {
				if seen[j] {
					continue
				}
				seen[j] = true
				if summary.Comparisons >= MaxComparisons {
					summary.NearSearchComplete = false
					break search
				}
				summary.Comparisons++
				if summary.Comparisons%256 == 0 {
					if err := ctx.Err(); err != nil {
						return Summary{}, nil, err
					}
				}
				if simhash.Compare(groups[i].hash, groups[j].hash) <= 3 {
					join(i, j)
				}
			}
		}
		for band := range bands {
			key := uint16(groups[i].hash >> (16 * band))
			bands[band][key] = append(bands[band][key], i)
		}
	}
	components := map[int][]int{}
	for i := range groups {
		r := root(i)
		components[r] = append(components[r], i)
		if len(groups[i].members) > 1 {
			summary.ExactClusters++
		}
	}
	clusters := []Cluster{}
	for _, component := range components {
		members := []int{}
		exactGroups := 0
		for _, g := range component {
			members = append(members, groups[g].members...)
			if len(groups[g].members) > 1 {
				exactGroups++
			}
		}
		if len(members) < 2 {
			continue
		}
		sort.Ints(members)
		c := Cluster{ID: d.documents[members[0]].TargetID, Count: len(members), Kind: "exact", ExactGroups: exactGroups, Context: "technical_candidate"}
		if len(component) > 1 {
			c.Kind = "near"
			summary.NearClusters++
		}
		languages := map[string]bool{}
		canonical := map[string]bool{}
		keys := map[string]bool{}
		titles, h1s := map[string]int{}, map[string]int{}
		missing, lost, regional, product := false, false, false, false
		for _, index := range members {
			p := d.documents[index]
			c.TargetIDs = append(c.TargetIDs, p.TargetID)
			if p.Signature.Language != "" {
				languages[p.Signature.Language] = true
			}
			regional = regional || p.Hreflang
			product = product || p.Product
			lost = lost || p.CanonicalLost
			keys[p.KeyID] = true
			if p.Canonical == "" {
				missing = true
			} else {
				canonical[p.Canonical] = true
			}
			if p.TitleKey != "" {
				titles[p.TitleKey]++
			}
			if p.H1Key != "" {
				h1s[p.H1Key]++
			}
		}
		if regional || len(languages) > 1 {
			c.Context = "regional_review"
		} else if product {
			c.Context = "product_review"
		}
		c.Canonical = "consistent"
		if lost || len(keys) > 1 {
			c.Canonical = "unverified"
		} else if len(canonical) > 1 {
			c.Canonical = "divergent"
			summary.CanonicalReviews++
		} else if missing {
			c.Canonical = "missing"
		}
		if c.Context == "technical_candidate" {
			for _, count := range titles {
				c.PotentialOverlap = c.PotentialOverlap || count > 1
			}
			for _, count := range h1s {
				c.PotentialOverlap = c.PotentialOverlap || count > 1
			}
		}
		if c.PotentialOverlap {
			summary.PotentialOverlaps++
		}
		summary.SimilarPages += c.Count
		clusters = append(clusters, c)
	}
	sort.Slice(clusters, func(i, j int) bool { return clusters[i].ID < clusters[j].ID })
	return summary, clusters, nil
}

func HeadingKey(text string) string {
	if strings.HasPrefix(text, "[Missing ") {
		return ""
	}
	text = strings.Join(strings.Fields(strings.ToLower(text)), " ")
	if text == "" {
		return ""
	}
	// У пам'яті масового аналізу залишаємо сталий ключ, не копію metadata.
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}
