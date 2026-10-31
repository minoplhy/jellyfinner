package orchestrator

import (
	"context"
	"errors"
	"sort"

	"jellyfinner/internal/config"
	"jellyfinner/internal/queue"
)

// backfillWindow is how many backfill jobs sit in the queue at once. Feeding a
// few at a time keeps newest-first order and makes a pause take effect quickly.
const backfillWindow = 3

// SetBackfill starts or pauses the backfill. Nothing else turns it on.
func (s *Service) SetBackfill(active bool) error {
	if active && s.Cfg.Mode != config.ModeAuto {
		return errors.New("backfill only runs in auto mode")
	}
	value := "0"
	if active {
		value = "1"
		if err := s.Store.SetMeta("backfill_finished", ""); err != nil {
			return err
		}
	}
	return s.Store.SetMeta("backfill_active", value)
}

// BackfillStatus summarises the work left.
type BackfillStatus struct {
	Active         bool
	Remaining      int
	RemainingBytes int64
	InProgress     int
	Failed         int
	UsedToday      int64
}

func (s *Service) needsBackfill(src *queue.Source) bool {
	if src.MissingSince != 0 {
		return false
	}
	if !src.Encoded() {
		return src.Backlog
	}
	return src.EncodedProfile != s.Profile
}

// backfillPlan lists sources still needing work, newest first, and classifies
// any job each already has.
func (s *Service) backfillPlan() (todo []*queue.Source, st BackfillStatus, err error) {
	sources, err := s.Store.Sources()
	if err != nil {
		return nil, st, err
	}
	states, err := s.Store.JobsByID()
	if err != nil {
		return nil, st, err
	}
	for _, src := range sources {
		if !s.needsBackfill(src) {
			continue
		}
		switch states[s.newJob(src, queue.PriorityBackfill).ID] {
		case "":
			todo = append(todo, src)
			st.Remaining++
			st.RemainingBytes += src.Size
		case queue.StateFailed, queue.StateCancelled:
			st.Failed++
		case queue.StateDone:
		default:
			st.InProgress++
		}
	}
	sort.SliceStable(todo, func(a, b int) bool { return todo[a].MtimeNS > todo[b].MtimeNS })
	return todo, st, nil
}

// BackfillStatus reports progress.
func (s *Service) BackfillStatus() (BackfillStatus, error) {
	_, st, err := s.backfillPlan()
	if err != nil {
		return st, err
	}
	active, err := s.Store.Meta("backfill_active")
	if err != nil {
		return st, err
	}
	st.Active = active == "1"
	st.UsedToday, err = s.Store.BudgetUsedToday()
	return st, err
}

func (s *Service) feedBackfill(ctx context.Context) error {
	if s.Cfg.Mode != config.ModeAuto {
		return nil
	}
	active, err := s.Store.Meta("backfill_active")
	if err != nil || active != "1" {
		return err
	}
	todo, st, err := s.backfillPlan()
	if err != nil {
		return err
	}
	for i := 0; i < len(todo) && st.InProgress+i < backfillWindow; i++ {
		if _, err := s.Store.Enqueue(s.newJob(todo[i], queue.PriorityBackfill)); err != nil {
			return err
		}
	}
	if len(todo) > 0 || st.InProgress > 0 {
		return nil
	}
	finished, err := s.Store.Meta("backfill_finished")
	if err != nil || finished == "1" {
		return err
	}
	if err := s.Store.SetMeta("backfill_finished", "1"); err != nil {
		return err
	}
	message := "Backfill finished, ready for cutover."
	if st.Failed > 0 {
		message = "Backfill finished with failed titles. Check the queue before cutover."
	}
	s.send(ctx, message)
	return nil
}
