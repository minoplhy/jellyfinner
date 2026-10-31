package orchestrator

import (
	"fmt"
	"net/http"
	"sort"

	"jellyfinner/internal/queue"
)

// MetricsHandler serves Prometheus text format.
func (s *Service) MetricsHandler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		counts, err := s.Store.StateCounts()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		st, err := s.BackfillStatus()
		if err != nil {
			http.Error(w, "internal error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/plain; version=0.0.4")
		fmt.Fprintln(w, "# TYPE jellyfinner_jobs gauge")
		for _, state := range sortedStates(counts) {
			fmt.Fprintf(w, "jellyfinner_jobs{state=%q} %d\n", state, counts[state])
		}
		fmt.Fprintln(w, "# TYPE jellyfinner_backfill_remaining_titles gauge")
		fmt.Fprintf(w, "jellyfinner_backfill_remaining_titles %d\n", st.Remaining)
		fmt.Fprintln(w, "# TYPE jellyfinner_backfill_remaining_bytes gauge")
		fmt.Fprintf(w, "jellyfinner_backfill_remaining_bytes %d\n", st.RemainingBytes)
		fmt.Fprintln(w, "# TYPE jellyfinner_backfill_used_today_bytes gauge")
		fmt.Fprintf(w, "jellyfinner_backfill_used_today_bytes %d\n", st.UsedToday)
	})
	return mux
}

func sortedStates(counts map[string]int) []string {
	all := []string{queue.StateQueued, queue.StateRunning, queue.StatePublishing, queue.StateDone, queue.StateFailed, queue.StateCancelled}
	for state := range counts {
		known := false
		for _, a := range all {
			known = known || a == state
		}
		if !known {
			all = append(all, state)
		}
	}
	sort.Strings(all)
	return all
}
