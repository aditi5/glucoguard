package store

import (
	"context"
	"crypto/rand"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is an in-process Store for tests and local development. Data is lost
// on restart.
type Memory struct {
	mu          sync.RWMutex
	patients    map[string]Patient
	plans       map[string][]CarePlan // patientID -> versions, oldest first
	readings    map[string]Reading
	readingKeys map[string]string // patientID|clientID -> readingID
	evals       map[string]Evaluation
	followUps   []FollowUp
	reminders   map[string]Reminder // evaluationID -> reminder
	audit       []AuditEvent
	auditSeq    int64
}

func NewMemory() *Memory {
	return &Memory{
		patients: map[string]Patient{}, plans: map[string][]CarePlan{},
		readings: map[string]Reading{}, readingKeys: map[string]string{},
		evals: map[string]Evaluation{}, reminders: map[string]Reminder{},
	}
}

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:])
}

func inRange(t time.Time, tr TimeRange) bool {
	return (tr.From.IsZero() || !t.Before(tr.From)) && (tr.To.IsZero() || t.Before(tr.To))
}

func limit[T any](xs []T, n int) []T {
	if n > 0 && len(xs) > n {
		return xs[:n]
	}
	return xs
}

func (m *Memory) Close() {}

func (m *Memory) CreatePatient(_ context.Context, p Patient) (Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if p.ID == "" {
		p.ID = newID()
	}
	now := time.Now().UTC()
	p.CreatedAt, p.UpdatedAt = now, now
	m.patients[p.ID] = p
	return p, nil
}

func (m *Memory) GetPatient(_ context.Context, id string) (Patient, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	p, ok := m.patients[id]
	if !ok {
		return Patient{}, ErrNotFound
	}
	return p, nil
}

func (m *Memory) UpdatePatient(_ context.Context, p Patient) (Patient, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	old, ok := m.patients[p.ID]
	if !ok {
		return Patient{}, ErrNotFound
	}
	p.CreatedAt, p.UpdatedAt = old.CreatedAt, time.Now().UTC()
	m.patients[p.ID] = p
	return p, nil
}

func (m *Memory) DeletePatient(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.patients[id]; !ok {
		return ErrNotFound
	}
	delete(m.patients, id)
	delete(m.plans, id)
	for rid, r := range m.readings {
		if r.PatientID == id {
			delete(m.readings, rid)
			delete(m.readingKeys, id+"|"+r.ClientReadingID)
		}
	}
	for eid, e := range m.evals {
		if e.PatientID == id {
			delete(m.evals, eid)
			delete(m.reminders, eid)
		}
	}
	kept := m.followUps[:0]
	for _, f := range m.followUps {
		if f.PatientID != id {
			kept = append(kept, f)
		}
	}
	m.followUps = kept
	for i := range m.audit {
		if m.audit[i].PatientID == id {
			m.audit[i].PatientID = ""
		}
	}
	return nil
}

func (m *Memory) CreateCarePlan(_ context.Context, c CarePlan) (CarePlan, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.patients[c.PatientID]; !ok {
		return CarePlan{}, ErrNotFound
	}
	for _, existing := range m.plans[c.PatientID] {
		if existing.Version == c.Version {
			return CarePlan{}, fmt.Errorf("%w: care plan version %q already exists", ErrConflict, c.Version)
		}
	}
	c.ID = newID()
	c.CreatedAt = time.Now().UTC()
	if c.EffectiveFrom.IsZero() {
		c.EffectiveFrom = c.CreatedAt
	}
	m.plans[c.PatientID] = append(m.plans[c.PatientID], c)
	return c, nil
}

func (m *Memory) CurrentCarePlan(_ context.Context, patientID string) (CarePlan, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	ps := m.plans[patientID]
	now := time.Now().UTC()
	var best *CarePlan
	for i := range ps {
		if ps[i].EffectiveFrom.After(now) {
			continue
		}
		if best == nil || !ps[i].EffectiveFrom.Before(best.EffectiveFrom) {
			best = &ps[i]
		}
	}
	if best == nil {
		return CarePlan{}, ErrNotFound
	}
	return *best, nil
}

func (m *Memory) InsertReading(_ context.Context, r Reading) (Reading, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := r.PatientID + "|" + r.ClientReadingID
	if id, ok := m.readingKeys[key]; ok {
		return m.readings[id], false, nil
	}
	r.ID = newID()
	m.readings[r.ID] = r
	m.readingKeys[key] = r.ID
	return r, true, nil
}

func (m *Memory) GetReading(_ context.Context, patientID, id string) (Reading, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.readings[id]
	if !ok || r.PatientID != patientID {
		return Reading{}, ErrNotFound
	}
	return r, nil
}

func (m *Memory) ListReadings(_ context.Context, patientID string, tr TimeRange) ([]Reading, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Reading
	for _, r := range m.readings {
		if r.PatientID == patientID && inRange(r.MeasuredAt, tr) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].MeasuredAt.After(out[j].MeasuredAt) })
	return limit(out, tr.Limit), nil
}

func (m *Memory) InsertEvaluation(_ context.Context, e Evaluation) (Evaluation, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e.ID = newID()
	if e.CreatedAt.IsZero() {
		e.CreatedAt = time.Now().UTC()
	}
	m.evals[e.ID] = e
	return e, nil
}

func (m *Memory) GetEvaluation(_ context.Context, patientID, id string) (Evaluation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.evals[id]
	if !ok || e.PatientID != patientID {
		return Evaluation{}, ErrNotFound
	}
	return e, nil
}

func (m *Memory) LatestEvaluationForReading(_ context.Context, patientID, readingID string) (Evaluation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var best Evaluation
	found := false
	for _, e := range m.evals {
		if e.PatientID == patientID && e.ReadingID == readingID && (!found || e.CreatedAt.After(best.CreatedAt)) {
			best, found = e, true
		}
	}
	if !found {
		return Evaluation{}, ErrNotFound
	}
	return best, nil
}

func (m *Memory) LatestEvaluationBefore(_ context.Context, patientID string, t time.Time, exclude string) (Evaluation, Reading, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var bestE Evaluation
	var bestR Reading
	found := false
	for _, e := range m.evals {
		if e.PatientID != patientID || e.ReadingID == exclude {
			continue
		}
		r := m.readings[e.ReadingID]
		if !r.MeasuredAt.Before(t) {
			continue
		}
		if !found || r.MeasuredAt.After(bestR.MeasuredAt) ||
			(r.MeasuredAt.Equal(bestR.MeasuredAt) && e.CreatedAt.After(bestE.CreatedAt)) {
			bestE, bestR, found = e, r, true
		}
	}
	if !found {
		return Evaluation{}, Reading{}, ErrNotFound
	}
	return bestE, bestR, nil
}

func (m *Memory) ListEvaluations(_ context.Context, patientID string, tr TimeRange) ([]Evaluation, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []Evaluation
	for _, e := range m.evals {
		if e.PatientID == patientID && inRange(e.CreatedAt, tr) {
			out = append(out, e)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return limit(out, tr.Limit), nil
}

func (m *Memory) AcknowledgeEvaluation(_ context.Context, patientID, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.evals[id]
	if !ok || e.PatientID != patientID {
		return ErrNotFound
	}
	if e.AcknowledgedAt == nil {
		e.AcknowledgedAt = &at
		m.evals[id] = e
	}
	return nil
}

func (m *Memory) InsertFollowUp(_ context.Context, f FollowUp) (FollowUp, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	f.ID = newID()
	f.CreatedAt = time.Now().UTC()
	m.followUps = append(m.followUps, f)
	return f, nil
}

func (m *Memory) ListFollowUps(_ context.Context, patientID string, tr TimeRange) ([]FollowUp, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []FollowUp
	for i := len(m.followUps) - 1; i >= 0; i-- {
		f := m.followUps[i]
		if f.PatientID == patientID && inRange(f.CreatedAt, tr) {
			out = append(out, f)
		}
	}
	return limit(out, tr.Limit), nil
}

func (m *Memory) UpsertReminder(_ context.Context, r Reminder) (Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.reminders[r.EvaluationID]; ok {
		return existing, nil
	}
	r.ID = newID()
	if r.Status == "" {
		r.Status = ReminderPending
	}
	m.reminders[r.EvaluationID] = r
	return r, nil
}

func (m *Memory) ClaimDueReminders(_ context.Context, now time.Time, lease time.Duration, n int) ([]Reminder, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Reminder
	for _, r := range m.reminders {
		if (r.Status == ReminderPending || r.Status == ReminderFailed) && !r.DueAt.After(now) {
			out = append(out, r)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].DueAt.Before(out[j].DueAt) })
	out = limit(out, n)
	for i := range out {
		out[i].Attempts++
		out[i].DueAt = now.Add(lease)
		m.reminders[out[i].EvaluationID] = out[i]
	}
	return out, nil
}

func (m *Memory) UpdateReminder(_ context.Context, r Reminder) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.reminders[r.EvaluationID]; !ok {
		return ErrNotFound
	}
	m.reminders[r.EvaluationID] = r
	return nil
}

func (m *Memory) CompleteRemindersForPatient(_ context.Context, patientID string, before time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for k, r := range m.reminders {
		if r.PatientID == patientID && (r.Status == ReminderPending || r.Status == ReminderFailed || r.Status == ReminderSent) &&
			m.evals[r.EvaluationID].CreatedAt.Before(before) {
			r.Status = ReminderCompleted
			m.reminders[k] = r
		}
	}
	return nil
}

func (m *Memory) AppendAudit(_ context.Context, e AuditEvent) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.auditSeq++
	e.ID = m.auditSeq
	if e.At.IsZero() {
		e.At = time.Now().UTC()
	}
	m.audit = append(m.audit, e)
	return nil
}

func (m *Memory) ListAudit(_ context.Context, patientID string, tr TimeRange) ([]AuditEvent, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []AuditEvent
	for i := len(m.audit) - 1; i >= 0; i-- {
		if a := m.audit[i]; a.PatientID == patientID && inRange(a.At, tr) {
			out = append(out, a)
		}
	}
	return limit(out, tr.Limit), nil
}
