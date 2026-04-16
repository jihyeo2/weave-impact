package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"time"
)

type prCountResponse struct {
	Repo          string `json:"repo"`
	Days          int    `json:"days"`
	MergedPRCount int    `json:"merged_pr_count"`
}

type githubSearchResponse struct {
	TotalCount int `json:"total_count"`
}

type engineerStatsResponse struct {
	Repo      string         `json:"repo"`
	Days      int            `json:"days"`
	Top       []engineerStat `json:"top"`
	Generated time.Time      `json:"generated_at"`
}

type engineerStat struct {
	Login                string  `json:"login"`
	MergedPRCount        int     `json:"merged_pr_count"`
	OutputScore          float64 `json:"output_score"`
	MedianCycleTimeHours float64 `json:"median_cycle_time_hours"`
	MedianFirstReviewHours float64 `json:"median_time_to_first_review_hours"`
	AvgIterationCost       float64 `json:"avg_iteration_cost"`
	ImpactScore            float64 `json:"impact_score"`
	ScoreBreakdown         scoreBreakdown `json:"score_breakdown"`
}

type scoreBreakdown struct {
	OutputScorePercentile          float64 `json:"output_score_percentile"`
	CycleTimePercentile            float64 `json:"cycle_time_percentile"`
	TimeToFirstReviewPercentile    float64 `json:"time_to_first_review_percentile"`
	IterationCostPercentile        float64 `json:"iteration_cost_percentile"`
}

type graphqlEnvelope struct {
	Query     string                 `json:"query"`
	Variables map[string]any         `json:"variables,omitempty"`
}

type graphqlResponse[T any] struct {
	Data   T `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

type engineerAgg struct {
	Login         string
	MergedPRCount int
	OutputScore   float64
	CycleHours    []float64
	FirstReviewHours []float64
	IterationCostSum float64
	IterationCostCount int
}

type engineerStatsCacheEntry struct {
	ExpiresAt time.Time
	Payload   engineerStatsResponse
}

var (
	engineerStatsCacheMu sync.Mutex
	engineerStatsCache   = map[string]engineerStatsCacheEntry{}
	engineerStatsRefreshInFlight = map[string]bool{}
)

func engineerStatsCacheKey(days int, maxPRs int) string {
	return fmt.Sprintf("days=%d|max_prs=%d", days, maxPRs)
}

func engineerStatsCacheFilePath(days int, maxPRs int) string {
	name := fmt.Sprintf("engineer-stats_days=%d_maxprs=%d.json", days, maxPRs)
	return filepath.Join("cache", name)
}

func main() {
	loadPersistedEngineerStatsCache()

	mux := http.NewServeMux()
	mux.HandleFunc("/api/pr-count", handlePRCount)
	mux.HandleFunc("/api/engineer-stats", handleEngineerStats)

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}
	addr := ":" + port
	log.Printf("listening on %s", addr)
	if err := http.ListenAndServe(addr, withCORS(mux)); err != nil {
		log.Fatal(err)
	}
}

func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")

		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}

		next.ServeHTTP(w, r)
	})
}

func handlePRCount(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid days", http.StatusBadRequest)
			return
		}
		days = n
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		http.Error(w, "GITHUB_TOKEN env var is required", http.StatusInternalServerError)
		return
	}

	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format("2006-01-02")
	q := fmt.Sprintf("repo:PostHog/posthog is:pr is:merged merged:>=%s", cutoff)

	endpoint := "https://api.github.com/search/issues"
	u, err := url.Parse(endpoint)
	if err != nil {
		http.Error(w, "failed to build github url", http.StatusInternalServerError)
		return
	}
	params := u.Query()
	params.Set("q", q)
	u.RawQuery = params.Encode()

	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		http.Error(w, "failed to create request", http.StatusInternalServerError)
		return
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 20 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		http.Error(w, "github request failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		http.Error(w, "github api returned non-2xx", http.StatusBadGateway)
		return
	}

	var gh githubSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&gh); err != nil {
		http.Error(w, "failed to parse github response", http.StatusBadGateway)
		return
	}

	out := prCountResponse{
		Repo:          "PostHog/posthog",
		Days:          days,
		MergedPRCount: gh.TotalCount,
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
	log.Printf("/api/pr-count days=%d took=%s", days, time.Since(start).Truncate(time.Millisecond))
}

func handleEngineerStats(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	days := 90
	if v := r.URL.Query().Get("days"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid days", http.StatusBadRequest)
			return
		}
		days = n
	}

	force := false
	if v := r.URL.Query().Get("force"); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			http.Error(w, "invalid force", http.StatusBadRequest)
			return
		}
		force = b
	}

	maxPRs := 2000
	if v := r.URL.Query().Get("max_prs"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			http.Error(w, "invalid max_prs", http.StatusBadRequest)
			return
		}
		maxPRs = n
	}

	token := os.Getenv("GITHUB_TOKEN")
	if token == "" {
		http.Error(w, "GITHUB_TOKEN env var is required", http.StatusInternalServerError)
		return
	}

	// Stale-while-revalidate: serve cached snapshot immediately (even if stale), and refresh in background.
	cacheTTL := 30 * time.Minute
	if !force {
		if cached, ok, stale := getEngineerStatsFromCacheWithStale(days, maxPRs, cacheTTL); ok {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cached)
			if stale {
				triggerEngineerStatsRefresh(r.Context(), token, days, maxPRs)
				log.Printf("/api/engineer-stats days=%d max_prs=%d took=%s (served stale; refresh queued)", days, maxPRs, time.Since(start).Truncate(time.Millisecond))
				return
			}
			log.Printf("/api/engineer-stats days=%d max_prs=%d took=%s (cache hit)", days, maxPRs, time.Since(start).Truncate(time.Millisecond))
			return
		}
	} else {
		log.Printf("/api/engineer-stats days=%d max_prs=%d force=true (bypassing cache)", days, maxPRs)
	}

	cutoff := time.Now().AddDate(0, 0, -days).UTC().Format("2006-01-02")
	aggs, err := fetchAuthorAggs(r.Context(), token, cutoff, maxPRs)
	if err != nil {
		if snapshot, ok := loadPersistedEngineerStatsSnapshot(days, maxPRs); ok {
			cacheTTL := 30 * time.Minute
			setEngineerStatsCache(days, maxPRs, snapshot, cacheTTL)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(snapshot)
			triggerEngineerStatsRefresh(r.Context(), token, days, maxPRs)
			log.Printf("/api/engineer-stats days=%d max_prs=%d took=%s (served persisted snapshot; refresh queued; fetch err=%v)", days, maxPRs, time.Since(start).Truncate(time.Millisecond), err)
			return
		}
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}

	// Build per-author computed metrics first (needed for normalization/scoring)
	authorMetrics := make([]engineerStat, 0, len(aggs))
	for i := range aggs {
		avgIter := 0.0
		if aggs[i].IterationCostCount > 0 {
			avgIter = aggs[i].IterationCostSum / float64(aggs[i].IterationCostCount)
		}
		authorMetrics = append(authorMetrics, engineerStat{
			Login:                 aggs[i].Login,
			MergedPRCount:         aggs[i].MergedPRCount,
			OutputScore:           aggs[i].OutputScore,
			MedianCycleTimeHours:  median(aggs[i].CycleHours),
			MedianFirstReviewHours: median(aggs[i].FirstReviewHours),
			AvgIterationCost:       avgIter,
		})
	}

	// Percentile ranks across authors (0..1). For latency/cost metrics, lower is better so we'll invert.
	outputP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.OutputScore }, false)
	cycleP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.MedianCycleTimeHours }, true)
	firstReviewP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.MedianFirstReviewHours }, true)
	iterP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.AvgIterationCost }, true)

	for i := range authorMetrics {
		bd := scoreBreakdown{
			OutputScorePercentile:       outputP[i],
			CycleTimePercentile:         cycleP[i],
			TimeToFirstReviewPercentile: firstReviewP[i],
			IterationCostPercentile:     iterP[i],
		}
		authorMetrics[i].ScoreBreakdown = bd
		authorMetrics[i].ImpactScore = (bd.OutputScorePercentile + bd.CycleTimePercentile + bd.TimeToFirstReviewPercentile + bd.IterationCostPercentile) / 4
	}

	sort.Slice(authorMetrics, func(i, j int) bool {
		if authorMetrics[i].ImpactScore == authorMetrics[j].ImpactScore {
			return authorMetrics[i].Login < authorMetrics[j].Login
		}
		return authorMetrics[i].ImpactScore > authorMetrics[j].ImpactScore
	})

	topN := 5
	if len(authorMetrics) < topN {
		topN = len(authorMetrics)
	}
	top := make([]engineerStat, 0, topN)
	for i := 0; i < topN; i++ {
		top = append(top, authorMetrics[i])
	}

	out := engineerStatsResponse{
		Repo:      "PostHog/posthog",
		Days:      days,
		Top:       top,
		Generated: time.Now().UTC(),
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
	setEngineerStatsCache(days, maxPRs, out, cacheTTL)
	_ = persistEngineerStatsCache(days, maxPRs, out)
	log.Printf("/api/engineer-stats days=%d max_prs=%d took=%s authors=%d", days, maxPRs, time.Since(start).Truncate(time.Millisecond), len(aggs))
}

func fetchAuthorAggs(ctx context.Context, token string, cutoffDateYYYYMMDD string, maxPRs int) ([]engineerAgg, error) {
	queryText := fmt.Sprintf("repo:PostHog/posthog is:pr is:merged merged:>=%s", cutoffDateYYYYMMDD)

	type gqlData struct {
		Search struct {
			IssueCount int `json:"issueCount"`
			PageInfo   struct {
				EndCursor   *string `json:"endCursor"`
				HasNextPage bool    `json:"hasNextPage"`
			} `json:"pageInfo"`
			Nodes []struct {
				Typename string `json:"__typename"`
				Author   *struct {
					Login string `json:"login"`
				} `json:"author"`
				Additions    int       `json:"additions"`
				Deletions    int       `json:"deletions"`
				ChangedFiles int       `json:"changedFiles"`
				CreatedAt    time.Time `json:"createdAt"`
				MergedAt     time.Time `json:"mergedAt"`
				Reviews struct {
					Nodes []struct {
						SubmittedAt time.Time `json:"submittedAt"`
					} `json:"nodes"`
				} `json:"reviews"`
				Commits struct {
					TotalCount int `json:"totalCount"`
				} `json:"commits"`
			} `json:"nodes"`
		} `json:"search"`
	}

	gql := `query($query:String!, $after:String) {
  search(type: ISSUE, query: $query, first: 100, after: $after) {
    issueCount
    pageInfo { endCursor hasNextPage }
    nodes {
      __typename
      ... on PullRequest {
        author { login }
		 additions
		 deletions
		 changedFiles
		 createdAt
		 mergedAt
		 reviews(first: 1) { nodes { submittedAt } }
		 commits { totalCount }
      }
    }
  }
}`

	aggsByLogin := map[string]*engineerAgg{}
	var after *string
	client := &http.Client{Timeout: 30 * time.Second}
	processed := 0

	for {
		vars := map[string]any{"query": queryText, "after": after}
		env := graphqlEnvelope{Query: gql, Variables: vars}
		body, _ := json.Marshal(env)

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://api.github.com/graphql", bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("graphql request build failed: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
		req.Header.Set("User-Agent", "weave-impact")

		resp, err := client.Do(req)
		if err != nil {
			return nil, fmt.Errorf("graphql request failed: %w", err)
		}

		respBody, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, fmt.Errorf("graphql read failed: %w", readErr)
		}

		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			return nil, fmt.Errorf("graphql non-2xx: %s", string(respBody))
		}

		var parsed graphqlResponse[gqlData]
		if err := json.Unmarshal(respBody, &parsed); err != nil {
			return nil, fmt.Errorf("graphql parse failed: %w", err)
		}
		if len(parsed.Errors) > 0 {
			return nil, fmt.Errorf("graphql errors: %s", parsed.Errors[0].Message)
		}

		for _, n := range parsed.Data.Search.Nodes {
			if maxPRs > 0 && processed >= maxPRs {
				break
			}
			if n.Author == nil {
				continue
			}
			processed++
			login := n.Author.Login
			agg, ok := aggsByLogin[login]
			if !ok {
				agg = &engineerAgg{Login: login}
				aggsByLogin[login] = agg
			}
			agg.MergedPRCount++
			linesChanged := n.Additions + n.Deletions
			agg.OutputScore += math.Log1p(float64(linesChanged)) + 0.3*math.Log1p(float64(n.ChangedFiles))
			cycleHours := n.MergedAt.Sub(n.CreatedAt).Hours()
			if cycleHours >= 0 {
				agg.CycleHours = append(agg.CycleHours, cycleHours)
			}

			var firstReviewAt *time.Time
			for _, rv := range n.Reviews.Nodes {
				t := rv.SubmittedAt
				if firstReviewAt == nil || t.Before(*firstReviewAt) {
					firstReviewAt = &t
				}
			}
			if firstReviewAt != nil {
				frHours := firstReviewAt.Sub(n.CreatedAt).Hours()
				if frHours >= 0 {
					agg.FirstReviewHours = append(agg.FirstReviewHours, frHours)
				}

				// Approximation: without commit timestamps, treat all commits after the first commit as "post-review".
				postReviewCommits := n.Commits.TotalCount - 1
				if postReviewCommits < 0 {
					postReviewCommits = 0
				}
				denom := math.Log1p(float64(linesChanged))
				if denom > 0 {
					iterCost := float64(postReviewCommits) / denom
					agg.IterationCostSum += iterCost
					agg.IterationCostCount++
				}
			}
		}

		after = parsed.Data.Search.PageInfo.EndCursor
		if !parsed.Data.Search.PageInfo.HasNextPage || after == nil {
			break
		}
	}

	aggs := make([]engineerAgg, 0, len(aggsByLogin))
	for _, a := range aggsByLogin {
		aggs = append(aggs, *a)
	}
	return aggs, nil
}

func median(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	sorted := make([]float64, len(vals))
	copy(sorted, vals)
	sort.Float64s(sorted)
	m := len(sorted) / 2
	if len(sorted)%2 == 1 {
		return sorted[m]
	}
	return (sorted[m-1] + sorted[m]) / 2
}

func getEngineerStatsFromCache(days int, maxPRs int) (engineerStatsResponse, bool) {
	engineerStatsCacheMu.Lock()
	defer engineerStatsCacheMu.Unlock()
	key := engineerStatsCacheKey(days, maxPRs)
	e, ok := engineerStatsCache[key]
	if !ok {
		return engineerStatsResponse{}, false
	}
	if time.Now().After(e.ExpiresAt) {
		delete(engineerStatsCache, key)
		return engineerStatsResponse{}, false
	}
	return e.Payload, true
}

func getEngineerStatsFromCacheWithStale(days int, maxPRs int, ttl time.Duration) (engineerStatsResponse, bool, bool) {
	engineerStatsCacheMu.Lock()
	defer engineerStatsCacheMu.Unlock()
	key := engineerStatsCacheKey(days, maxPRs)
	e, ok := engineerStatsCache[key]
	if !ok {
		return engineerStatsResponse{}, false, false
	}
	stale := time.Now().After(e.ExpiresAt)
	return e.Payload, true, stale
}

func setEngineerStatsCache(days int, maxPRs int, payload engineerStatsResponse, ttl time.Duration) {
	engineerStatsCacheMu.Lock()
	defer engineerStatsCacheMu.Unlock()
	key := engineerStatsCacheKey(days, maxPRs)
	engineerStatsCache[key] = engineerStatsCacheEntry{ExpiresAt: time.Now().Add(ttl), Payload: payload}
}

func triggerEngineerStatsRefresh(ctx context.Context, token string, days int, maxPRs int) {
	key := engineerStatsCacheKey(days, maxPRs)
	engineerStatsCacheMu.Lock()
	if engineerStatsRefreshInFlight[key] {
		engineerStatsCacheMu.Unlock()
		return
	}
	engineerStatsRefreshInFlight[key] = true
	engineerStatsCacheMu.Unlock()

	go func() {
		defer func() {
			engineerStatsCacheMu.Lock()
			delete(engineerStatsRefreshInFlight, key)
			engineerStatsCacheMu.Unlock()
		}()

		cutoff := time.Now().AddDate(0, 0, -days).UTC().Format("2006-01-02")
		aggs, err := fetchAuthorAggs(ctx, token, cutoff, maxPRs)
		if err != nil {
			log.Printf("refresh engineer-stats failed: %v", err)
			return
		}

		// Reuse the same scoring logic by calling the handler's core logic locally.
		out, err := buildEngineerStatsResponse(days, aggs)
		if err != nil {
			log.Printf("refresh engineer-stats build failed: %v", err)
			return
		}

		cacheTTL := 30 * time.Minute
		setEngineerStatsCache(days, maxPRs, out, cacheTTL)
		_ = persistEngineerStatsCache(days, maxPRs, out)
		log.Printf("refresh engineer-stats completed days=%d max_prs=%d", days, maxPRs)
	}()
}

func persistEngineerStatsCache(days int, maxPRs int, payload engineerStatsResponse) error {
	path := engineerStatsCacheFilePath(days, maxPRs)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func loadPersistedEngineerStatsSnapshot(days int, maxPRs int) (engineerStatsResponse, bool) {
	path := engineerStatsCacheFilePath(days, maxPRs)
	b, err := os.ReadFile(path)
	if err != nil {
		return engineerStatsResponse{}, false
	}
	var payload engineerStatsResponse
	if err := json.Unmarshal(b, &payload); err != nil {
		return engineerStatsResponse{}, false
	}
	if payload.Days <= 0 || payload.Repo == "" {
		return engineerStatsResponse{}, false
	}
	return payload, true
}

func loadPersistedEngineerStatsCache() {
	// Best-effort: load any persisted snapshots from ./cache into memory.
	paths, err := filepath.Glob(engineerStatsCacheFilePath(0, 0))
	_ = paths
	if err != nil {
		return
	}
	files, err := filepath.Glob(filepath.Join("cache", "engineer-stats_*.json"))
	if err != nil {
		return
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var payload engineerStatsResponse
		if err := json.Unmarshal(b, &payload); err != nil {
			continue
		}
		// We don't know max_prs from the payload; derive from filename.
		var days, maxPRs int
		_, _ = fmt.Sscanf(filepath.Base(f), "engineer-stats_days=%d_maxprs=%d.json", &days, &maxPRs)
		if days <= 0 {
			continue
		}
		cacheTTL := 30 * time.Minute
		setEngineerStatsCache(days, maxPRs, payload, cacheTTL)
	}
}

func buildEngineerStatsResponse(days int, aggs []engineerAgg) (engineerStatsResponse, error) {
	// Build per-author computed metrics first (needed for normalization/scoring)
	authorMetrics := make([]engineerStat, 0, len(aggs))
	for i := range aggs {
		avgIter := 0.0
		if aggs[i].IterationCostCount > 0 {
			avgIter = aggs[i].IterationCostSum / float64(aggs[i].IterationCostCount)
		}
		authorMetrics = append(authorMetrics, engineerStat{
			Login:                  aggs[i].Login,
			MergedPRCount:          aggs[i].MergedPRCount,
			OutputScore:            aggs[i].OutputScore,
			MedianCycleTimeHours:   median(aggs[i].CycleHours),
			MedianFirstReviewHours: median(aggs[i].FirstReviewHours),
			AvgIterationCost:       avgIter,
		})
	}

	outputP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.OutputScore }, false)
	cycleP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.MedianCycleTimeHours }, true)
	firstReviewP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.MedianFirstReviewHours }, true)
	iterP := percentileBy(authorMetrics, func(e engineerStat) float64 { return e.AvgIterationCost }, true)

	for i := range authorMetrics {
		bd := scoreBreakdown{
			OutputScorePercentile:       outputP[i],
			CycleTimePercentile:         cycleP[i],
			TimeToFirstReviewPercentile: firstReviewP[i],
			IterationCostPercentile:     iterP[i],
		}
		authorMetrics[i].ScoreBreakdown = bd
		authorMetrics[i].ImpactScore = (bd.OutputScorePercentile + bd.CycleTimePercentile + bd.TimeToFirstReviewPercentile + bd.IterationCostPercentile) / 4
	}

	sort.Slice(authorMetrics, func(i, j int) bool {
		if authorMetrics[i].ImpactScore == authorMetrics[j].ImpactScore {
			return authorMetrics[i].Login < authorMetrics[j].Login
		}
		return authorMetrics[i].ImpactScore > authorMetrics[j].ImpactScore
	})

	topN := 5
	if len(authorMetrics) < topN {
		topN = len(authorMetrics)
	}
	top := make([]engineerStat, 0, topN)
	for i := 0; i < topN; i++ {
		top = append(top, authorMetrics[i])
	}

	return engineerStatsResponse{
		Repo:      "PostHog/posthog",
		Days:      days,
		Top:       top,
		Generated: time.Now().UTC(),
	}, nil
}

func percentileBy(items []engineerStat, valueFn func(engineerStat) float64, invert bool) []float64 {
	n := len(items)
	if n == 0 {
		return nil
	}

	type pair struct {
		idx int
		val float64
	}
	pairs := make([]pair, 0, n)
	for i := range items {
		v := valueFn(items[i])
		if math.IsNaN(v) || math.IsInf(v, 0) {
			v = 0
		}
		pairs = append(pairs, pair{idx: i, val: v})
	}

	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].val == pairs[j].val {
			return pairs[i].idx < pairs[j].idx
		}
		return pairs[i].val < pairs[j].val
	})

	out := make([]float64, n)
	if n == 1 {
		out[pairs[0].idx] = 1
		if invert {
			out[pairs[0].idx] = 1
		}
		return out
	}

	// Percentile based on average rank for ties.
	for i := 0; i < n; {
		j := i + 1
		for j < n && pairs[j].val == pairs[i].val {
			j++
		}
		avgRank := (float64(i) + float64(j-1)) / 2 // 0-based
		p := avgRank / float64(n-1)
		if invert {
			p = 1 - p
		}
		for k := i; k < j; k++ {
			out[pairs[k].idx] = p
		}
		i = j
	}

	return out
}
