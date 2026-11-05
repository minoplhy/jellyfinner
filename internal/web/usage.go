package web

import (
	"net/http"

	"jellyfinner/internal/orchestrator"
)

// usageView is the Usage page: one UTC month of work, traffic and cloud
// spending, plus the backlog as it stands now.
type usageView struct {
	orchestrator.Summary
	Billing []orchestrator.ProviderBilling
}

// usageData builds the page for ?month=YYYY-MM, or the current month. A bad
// month is the caller's mistake, reported with ok false.
func (s *Server) usageData(w http.ResponseWriter, r *http.Request) (usageView, bool) {
	month, err := orchestrator.ParseMonth(r.URL.Query().Get("month"), s.now())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return usageView{}, false
	}
	var v usageView
	if v.Summary, err = s.svc.Summary(r.Context(), month, s.svc.Cloud); err != nil {
		s.fail(w, err)
		return v, false
	}
	if s.svc.Cloud != nil {
		v.Billing = s.svc.Cloud.Billing()
	}
	return v, true
}

func (s *Server) usagePage(w http.ResponseWriter, r *http.Request, sess *session) {
	if v, ok := s.usageData(w, r); ok {
		s.render(w, sess, "usage", "Usage "+v.Month.Format("January 2006"), v)
	}
}

// usageFragment refreshes the current month in place.
func (s *Server) usageFragment(w http.ResponseWriter, r *http.Request, _ *session) {
	if v, ok := s.usageData(w, r); ok {
		s.fragment(w, "usage", "usagebody", v)
	}
}
