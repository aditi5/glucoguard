package app

import (
	"context"
	"log/slog"
	"time"

	"github.com/purplestack/glucoguard/internal/store"
)

// Notifier delivers a recheck reminder (push, SMS, WhatsApp). Push alone must
// never be the only safety mechanism: the mobile app also schedules a local
// notification from follow_up_policy when it receives a decision.
type Notifier interface {
	NotifyRecheck(ctx context.Context, r store.Reminder) error
}

// LogNotifier is a development stand-in that only logs.
type LogNotifier struct{ Log *slog.Logger }

func (n LogNotifier) NotifyRecheck(_ context.Context, r store.Reminder) error {
	n.Log.Info("recheck reminder (dev: not delivered)", "patient_id", r.PatientID, "evaluation_id", r.EvaluationID, "attempt", r.Attempts)
	return nil
}

type ReminderWorker struct {
	Svc         *Service
	Notifier    Notifier
	Interval    time.Duration
	Lease       time.Duration
	MaxAttempts int
}

func (w *ReminderWorker) Run(ctx context.Context) {
	if w.Interval == 0 {
		w.Interval = 15 * time.Second
	}
	if w.Lease == 0 {
		w.Lease = 2 * time.Minute
	}
	if w.MaxAttempts == 0 {
		w.MaxAttempts = 5
	}
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		w.Tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// Tick processes one batch of due reminders. Exported for tests.
func (w *ReminderWorker) Tick(ctx context.Context) {
	now := w.Svc.Now()
	due, err := w.Svc.Store.ClaimDueReminders(ctx, now, w.Lease, 100)
	if err != nil {
		w.Svc.Log.Error("claim reminders", "err", err)
		return
	}
	for _, r := range due {
		err := w.Notifier.NotifyRecheck(ctx, r)
		switch {
		case err == nil:
			r.Status, r.LastError = store.ReminderSent, ""
			w.Svc.audit(ctx, r.PatientID, Actor{"reminder-worker", "system"}, "reminder.sent", "reminder", r.ID, nil)
		case r.Attempts >= w.MaxAttempts:
			r.Status, r.LastError = store.ReminderFailed, err.Error()
			r.DueAt = now.Add(100 * 365 * 24 * time.Hour) // park it; needs human follow-up
			w.Svc.Log.Error("SAFETY: reminder delivery exhausted", "evaluation_id", r.EvaluationID, "err", err)
			w.Svc.audit(ctx, r.PatientID, Actor{"reminder-worker", "system"}, "reminder.delivery_exhausted", "reminder", r.ID,
				map[string]any{"attempts": r.Attempts, "error": err.Error()})
		default:
			r.Status, r.LastError = store.ReminderFailed, err.Error()
			r.DueAt = now.Add(time.Duration(1<<r.Attempts) * 15 * time.Second) // exponential backoff
		}
		if err := w.Svc.Store.UpdateReminder(ctx, r); err != nil {
			w.Svc.Log.Error("update reminder", "err", err)
		}
	}
}
