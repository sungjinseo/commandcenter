package ui

import (
	"net/http"
	"time"
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
