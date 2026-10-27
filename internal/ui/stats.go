package ui

import (
	"net/http"
	"time"

	"commandcenter/internal/meta"
)

// GET /-/api/stats/requests?range=24h|7d|30d&repo= (admin): request counts and
// response bytes, hourly for 24h and daily (UTC) for 7d and 30d, with zero
// buckets filled in so a chart can plot them directly (plan 11 Decision 2).
func (u *UI) requestStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	q := r.URL.Query()
	var step int64
	var n int
	switch q.Get("range") {
	case "", "24h":
		step, n = 3600, 24
	case "7d":
		step, n = 86400, 7
	case "30d":
		step, n = 86400, 30
	default:
		writeError(w, http.StatusBadRequest, "range must be 24h, 7d or 30d")
		return
	}
	repoName := q.Get("repo")
	if repoName != "" && u.Repos.Get(repoName) == nil {
		writeError(w, http.StatusNotFound, "repository not found")
		return
	}
	end := u.clock().UTC().Unix()/step*step + step // exclusive: the end of the current bucket
	start := end - int64(n)*step
	got, err := u.DB.RequestStats(r.Context(), start, end, step, repoName)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	byT := make(map[int64][2]int64, len(got))
	for _, b := range got {
		byT[b.T] = [2]int64{b.Requests, b.Bytes}
	}
	type bucket struct {
		T        string `json:"t"`
		Requests int64  `json:"requests"`
		Bytes    int64  `json:"bytes"`
	}
	buckets := make([]bucket, 0, n)
	var reqs, bytes int64
	for t := start; t < end; t += step {
		v := byT[t]
		buckets = append(buckets, bucket{T: time.Unix(t, 0).UTC().Format(time.RFC3339), Requests: v[0], Bytes: v[1]})
		reqs += v[0]
		bytes += v[1]
	}
	rng := q.Get("range")
	if rng == "" {
		rng = "24h"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range":   rng,
		"repo":    repoName,
		"step":    step,
		"buckets": buckets,
		"totals":  map[string]int64{"requests": reqs, "bytes": bytes},
	})
}

// GET /-/api/stats/repositories?range=1h|24h|7d (admin): every repository's
// traffic over the period, from the hourly statistics (so "1h" is the current
// and the previous hour; "since" says exactly). Proxies also report their
// remote fetches and cache lookups. Counters are flushed every
// metricsFlushSeconds, so the last few seconds are missing.
func (u *UI) repositoryStats(w http.ResponseWriter, r *http.Request) {
	if _, ok := requireAdmin(w, r); !ok {
		return
	}
	rng := r.URL.Query().Get("range")
	var hours int64
	switch rng {
	case "1h":
		hours = 2
	case "", "24h":
		rng, hours = "24h", 24
	case "7d":
		hours = 7 * 24
	default:
		writeError(w, http.StatusBadRequest, "range must be 1h, 24h or 7d")
		return
	}
	now := u.clock().UTC()
	end := now.Unix()/3600*3600 + 3600
	start := end - hours*3600
	got, err := u.DB.RepoTrafficStats(r.Context(), start, end)
	if err != nil {
		u.fail(w, r, err)
		return
	}
	byRepo := make(map[string]meta.RepoTraffic, len(got))
	for _, s := range got {
		byRepo[s.Repo] = s
	}
	avg := func(sum, n int64) int64 {
		if n == 0 {
			return 0
		}
		return sum / n
	}
	var out []map[string]any
	for _, rp := range u.Repos.Snapshot().Sorted {
		s := byRepo[rp.Name]
		row := map[string]any{
			"name": rp.Name, "format": rp.Format, "type": rp.Type,
			"requests": s.Requests, "errors": s.Errors, "errorRate": 0.0,
			"bytesIn": s.BytesIn, "bytesOut": s.Bytes,
			"latencyAvgMs": avg(s.Ms, s.Requests), "latencyMaxMs": s.MaxMs,
		}
		if s.Requests > 0 {
			row["errorRate"] = float64(s.Errors) / float64(s.Requests)
		}
		if rp.Type == "proxy" {
			row["upstreamRequests"] = s.Upstream
			row["upstreamFailures"] = s.UpstreamFailures
			row["upstreamLatencyAvgMs"] = avg(s.UpstreamMs, s.Upstream)
			row["upstreamLatencyMaxMs"] = s.UpstreamMaxMs
			row["cacheHits"] = s.CacheHits
			row["cacheMisses"] = s.CacheMisses
		}
		out = append(out, row)
	}
	if out == nil {
		out = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"range": rng, "since": time.Unix(start, 0).UTC().Format(time.RFC3339),
		"collectedAt": now.Format(time.RFC3339), "repositories": out,
	})
}
