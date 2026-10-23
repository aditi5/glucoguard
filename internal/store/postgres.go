package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/purplestack/glucoguard/internal/clinical"
	"github.com/purplestack/glucoguard/migrations"
)

type Postgres struct{ pool *pgxpool.Pool }

func NewPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}
	p := &Postgres{pool: pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return p, nil
}

func (p *Postgres) Close() { p.pool.Close() }

// migrate applies embedded *.sql files once each, in name order, under an
// advisory lock so concurrent instances don't race.
func (p *Postgres) migrate(ctx context.Context) error {
	conn, err := p.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock(727274)`); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock(727274)`) //nolint:errcheck
	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (name TEXT PRIMARY KEY, applied_at TIMESTAMPTZ NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		var exists bool
		if err := conn.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM schema_migrations WHERE name=$1)`, name).Scan(&exists); err != nil {
			return err
		}
		if exists {
			continue
		}
		sql, err := migrations.FS.ReadFile(name)
		if err != nil {
			return err
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			_ = tx.Rollback(ctx)
			return fmt.Errorf("migration %s: %w", name, err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO schema_migrations(name) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback(ctx)
			return err
		}
		if err := tx.Commit(ctx); err != nil {
			return err
		}
	}
	return nil
}

func notFound(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrNotFound
	}
	var pg *pgconn.PgError
	if errors.As(err, &pg) {
		switch pg.Code {
		case "23505":
			return fmt.Errorf("%w: %s", ErrConflict, pg.Detail)
		case "22P02": // invalid UUID text
			return ErrNotFound
		}
	}
	return err
}

func nullStr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func symptomsToText(s []clinical.Symptom) []string {
	out := make([]string, len(s))
	for i, x := range s {
		out[i] = string(x)
	}
	return out
}

func textToSymptoms(s []string) []clinical.Symptom {
	out := make([]clinical.Symptom, len(s))
	for i, x := range s {
		out[i] = clinical.Symptom(x)
	}
	return out
}

// ---- patients ---------------------------------------------------------------

const patientCols = `id::text, display_name, age_group, diabetes_type, pregnant, special_circumstances,
	preferred_unit, language, caregiver_name, caregiver_phone, consent_version, consent_given_at, created_at, updated_at`

func scanPatient(row pgx.Row) (Patient, error) {
	var p Patient
	var cn, cp *string
	err := row.Scan(&p.ID, &p.DisplayName, &p.AgeGroup, &p.DiabetesType, &p.Pregnant, &p.SpecialCircumstances,
		&p.PreferredUnit, &p.Language, &cn, &cp, &p.ConsentVersion, &p.ConsentGivenAt, &p.CreatedAt, &p.UpdatedAt)
	p.CaregiverName, p.CaregiverPhone = deref(cn), deref(cp)
	return p, notFound(err)
}

func (s *Postgres) CreatePatient(ctx context.Context, p Patient) (Patient, error) {
	return scanPatient(s.pool.QueryRow(ctx, `INSERT INTO patients
		(display_name, age_group, diabetes_type, pregnant, special_circumstances, preferred_unit, language,
		 caregiver_name, caregiver_phone, consent_version, consent_given_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+patientCols,
		p.DisplayName, p.AgeGroup, p.DiabetesType, p.Pregnant, p.SpecialCircumstances, p.PreferredUnit, p.Language,
		nullStr(p.CaregiverName), nullStr(p.CaregiverPhone), p.ConsentVersion, p.ConsentGivenAt))
}

func (s *Postgres) GetPatient(ctx context.Context, id string) (Patient, error) {
	return scanPatient(s.pool.QueryRow(ctx, `SELECT `+patientCols+` FROM patients WHERE id=$1`, id))
}

func (s *Postgres) UpdatePatient(ctx context.Context, p Patient) (Patient, error) {
	return scanPatient(s.pool.QueryRow(ctx, `UPDATE patients SET display_name=$2, age_group=$3, diabetes_type=$4,
		pregnant=$5, special_circumstances=$6, preferred_unit=$7, language=$8, caregiver_name=$9, caregiver_phone=$10,
		updated_at=now() WHERE id=$1 RETURNING `+patientCols,
		p.ID, p.DisplayName, p.AgeGroup, p.DiabetesType, p.Pregnant, p.SpecialCircumstances, p.PreferredUnit,
		p.Language, nullStr(p.CaregiverName), nullStr(p.CaregiverPhone)))
}

func (s *Postgres) DeletePatient(ctx context.Context, id string) error {
	tag, err := s.pool.Exec(ctx, `DELETE FROM patients WHERE id=$1`, id)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- care plans -------------------------------------------------------------

const planCols = `id::text, patient_id::text, version, low_mg_dl::float8, severe_low_mg_dl::float8, high_mg_dl::float8,
	very_high_mg_dl::float8, stale_after_seconds, low_recheck_seconds, high_recheck_seconds, max_low_treatment_cycles,
	clinician_approved, approved_by, approved_at, effective_from, created_at`

func scanPlan(row pgx.Row) (CarePlan, error) {
	var c CarePlan
	var stale, lowR, highR int
	var by *string
	var at *time.Time
	err := row.Scan(&c.ID, &c.PatientID, &c.Version, &c.LowMgDL, &c.SevereLowMgDL, &c.HighMgDL, &c.VeryHighMgDL,
		&stale, &lowR, &highR, &c.MaxLowTreatmentCycles, &c.ClinicianApproved, &by, &at, &c.EffectiveFrom, &c.CreatedAt)
	if err != nil {
		return CarePlan{}, notFound(err)
	}
	c.StaleAfter = time.Duration(stale) * time.Second
	c.LowRecheckAfter = time.Duration(lowR) * time.Second
	c.HighRecheckAfter = time.Duration(highR) * time.Second
	c.ApprovedBy = deref(by)
	if at != nil {
		c.ApprovedAt = *at
	}
	c.SyncDurations(false)
	return c, nil
}

func (s *Postgres) CreateCarePlan(ctx context.Context, c CarePlan) (CarePlan, error) {
	var approvedAt *time.Time
	if c.ClinicianApproved {
		approvedAt = &c.ApprovedAt
	}
	eff := c.EffectiveFrom
	if eff.IsZero() {
		eff = time.Now().UTC()
	}
	return scanPlan(s.pool.QueryRow(ctx, `INSERT INTO care_plans (patient_id, version, low_mg_dl, severe_low_mg_dl,
		high_mg_dl, very_high_mg_dl, stale_after_seconds, low_recheck_seconds, high_recheck_seconds,
		max_low_treatment_cycles, clinician_approved, approved_by, approved_at, effective_from)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14) RETURNING `+planCols,
		c.PatientID, c.Version, c.LowMgDL, c.SevereLowMgDL, c.HighMgDL, c.VeryHighMgDL,
		int(c.StaleAfter.Seconds()), int(c.LowRecheckAfter.Seconds()), int(c.HighRecheckAfter.Seconds()),
		c.MaxLowTreatmentCycles, c.ClinicianApproved, nullStr(c.ApprovedBy), approvedAt, eff))
}

func (s *Postgres) CurrentCarePlan(ctx context.Context, patientID string) (CarePlan, error) {
	return scanPlan(s.pool.QueryRow(ctx, `SELECT `+planCols+` FROM care_plans
		WHERE patient_id=$1 AND effective_from <= now() ORDER BY effective_from DESC, created_at DESC LIMIT 1`, patientID))
}

// ---- readings -----------------------------------------------------------------

const readingCols = `id::text, patient_id::text, client_reading_id, COALESCE(value,0)::float8, unit, meter_flag,
	value_mg_dl::float8, source, device_id, meal_context, symptoms, notes, measured_at, received_at`

func scanReading(row pgx.Row) (Reading, error) {
	var r Reading
	var dev, notes *string
	var sym []string
	err := row.Scan(&r.ID, &r.PatientID, &r.ClientReadingID, &r.Value, &r.Unit, &r.MeterFlag, &r.ValueMgDL,
		&r.Source, &dev, &r.MealContext, &sym, &notes, &r.MeasuredAt, &r.ReceivedAt)
	r.DeviceID, r.Notes, r.Symptoms = deref(dev), deref(notes), textToSymptoms(sym)
	return r, notFound(err)
}

func (s *Postgres) InsertReading(ctx context.Context, r Reading) (Reading, bool, error) {
	var value *float64
	if r.MeterFlag == clinical.MeterFlagNone {
		value = &r.Value
	}
	out, err := scanReading(s.pool.QueryRow(ctx, `INSERT INTO glucose_readings (patient_id, client_reading_id, value,
		unit, meter_flag, value_mg_dl, source, device_id, meal_context, symptoms, notes, measured_at, received_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13)
		ON CONFLICT (patient_id, client_reading_id) DO NOTHING RETURNING `+readingCols,
		r.PatientID, r.ClientReadingID, value, r.Unit, r.MeterFlag, r.ValueMgDL, r.Source, nullStr(r.DeviceID),
		r.MealContext, symptomsToText(r.Symptoms), nullStr(r.Notes), r.MeasuredAt, r.ReceivedAt))
	if errors.Is(err, ErrNotFound) { // conflict: return the existing row
		existing, err := scanReading(s.pool.QueryRow(ctx, `SELECT `+readingCols+` FROM glucose_readings
			WHERE patient_id=$1 AND client_reading_id=$2`, r.PatientID, r.ClientReadingID))
		return existing, false, err
	}
	return out, err == nil, err
}

func (s *Postgres) GetReading(ctx context.Context, patientID, id string) (Reading, error) {
	return scanReading(s.pool.QueryRow(ctx, `SELECT `+readingCols+` FROM glucose_readings WHERE patient_id=$1 AND id=$2`, patientID, id))
}

func rangeArgs(tr TimeRange) (from, to *time.Time, lim int) {
	if !tr.From.IsZero() {
		from = &tr.From
	}
	if !tr.To.IsZero() {
		to = &tr.To
	}
	lim = tr.Limit
	if lim <= 0 {
		lim = 10000
	}
	return
}

func collect[T any](rows pgx.Rows, scan func(pgx.Row) (T, error)) ([]T, error) {
	defer rows.Close()
	var out []T
	for rows.Next() {
		x, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

func (s *Postgres) ListReadings(ctx context.Context, patientID string, tr TimeRange) ([]Reading, error) {
	from, to, lim := rangeArgs(tr)
	rows, err := s.pool.Query(ctx, `SELECT `+readingCols+` FROM glucose_readings WHERE patient_id=$1
		AND ($2::timestamptz IS NULL OR measured_at >= $2) AND ($3::timestamptz IS NULL OR measured_at < $3)
		ORDER BY measured_at DESC LIMIT $4`, patientID, from, to, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanReading)
}

// ---- evaluations --------------------------------------------------------------

const evalCols = `e.id::text, e.patient_id::text, e.reading_id::text, e.decision, e.acknowledged_at, e.created_at`

func scanEval(row pgx.Row) (Evaluation, error) {
	var e Evaluation
	var raw []byte
	err := row.Scan(&e.ID, &e.PatientID, &e.ReadingID, &raw, &e.AcknowledgedAt, &e.CreatedAt)
	if err != nil {
		return Evaluation{}, notFound(err)
	}
	return e, json.Unmarshal(raw, &e.Decision)
}

func (s *Postgres) InsertEvaluation(ctx context.Context, e Evaluation) (Evaluation, error) {
	d := e.Decision
	raw, err := json.Marshal(d)
	if err != nil {
		return Evaluation{}, err
	}
	reasons := make([]string, len(d.ReasonCodes))
	for i, r := range d.ReasonCodes {
		reasons[i] = string(r)
	}
	return scanEval(s.pool.QueryRow(ctx, `INSERT INTO evaluations AS e (patient_id, reading_id, urgency, action_code,
		approved_message_id, reason_codes, matched_rules, rule_set_version, care_plan_version, low_treatment_cycle, decision)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11) RETURNING `+evalCols,
		e.PatientID, e.ReadingID, d.Urgency.String(), d.ActionCode, d.ApprovedMessageID, reasons, d.MatchedRules,
		d.RuleSetVersion, d.CarePlanVersion, d.LowTreatmentCycle, raw))
}

func (s *Postgres) GetEvaluation(ctx context.Context, patientID, id string) (Evaluation, error) {
	return scanEval(s.pool.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations e WHERE e.patient_id=$1 AND e.id=$2`, patientID, id))
}

func (s *Postgres) LatestEvaluationForReading(ctx context.Context, patientID, readingID string) (Evaluation, error) {
	return scanEval(s.pool.QueryRow(ctx, `SELECT `+evalCols+` FROM evaluations e WHERE e.patient_id=$1 AND e.reading_id=$2
		ORDER BY e.created_at DESC LIMIT 1`, patientID, readingID))
}

func (s *Postgres) LatestEvaluationBefore(ctx context.Context, patientID string, t time.Time, exclude string) (Evaluation, Reading, error) {
	row := s.pool.QueryRow(ctx, `SELECT e.id::text FROM evaluations e JOIN glucose_readings r ON r.id = e.reading_id
		WHERE e.patient_id=$1 AND r.measured_at < $2 AND e.reading_id::text <> $3
		ORDER BY r.measured_at DESC, e.created_at DESC LIMIT 1`, patientID, t, exclude)
	var id string
	if err := row.Scan(&id); err != nil {
		return Evaluation{}, Reading{}, notFound(err)
	}
	e, err := s.GetEvaluation(ctx, patientID, id)
	if err != nil {
		return Evaluation{}, Reading{}, err
	}
	r, err := s.GetReading(ctx, patientID, e.ReadingID)
	return e, r, err
}

func (s *Postgres) ListEvaluations(ctx context.Context, patientID string, tr TimeRange) ([]Evaluation, error) {
	from, to, lim := rangeArgs(tr)
	rows, err := s.pool.Query(ctx, `SELECT `+evalCols+` FROM evaluations e WHERE e.patient_id=$1
		AND ($2::timestamptz IS NULL OR e.created_at >= $2) AND ($3::timestamptz IS NULL OR e.created_at < $3)
		ORDER BY e.created_at DESC LIMIT $4`, patientID, from, to, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanEval)
}

func (s *Postgres) AcknowledgeEvaluation(ctx context.Context, patientID, id string, at time.Time) error {
	tag, err := s.pool.Exec(ctx, `UPDATE evaluations SET acknowledged_at = COALESCE(acknowledged_at, $3)
		WHERE patient_id=$1 AND id=$2`, patientID, id, at)
	if err != nil {
		return notFound(err)
	}
	if tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- follow-ups ---------------------------------------------------------------

func scanFollow(row pgx.Row) (FollowUp, error) {
	var f FollowUp
	var sym []string
	var note *string
	err := row.Scan(&f.ID, &f.PatientID, &f.EvaluationID, &f.Kind, &f.RecheckReadingID, &sym, &note, &f.CreatedAt)
	f.Symptoms, f.Note = textToSymptoms(sym), deref(note)
	return f, notFound(err)
}

func followSelect() string {
	return `SELECT id::text, patient_id::text, evaluation_id::text, kind, COALESCE(recheck_reading_id::text,''),
		symptoms, note, created_at FROM follow_up_events`
}

func (s *Postgres) InsertFollowUp(ctx context.Context, f FollowUp) (FollowUp, error) {
	var rr *string
	if f.RecheckReadingID != "" {
		rr = &f.RecheckReadingID
	}
	return scanFollow(s.pool.QueryRow(ctx, `INSERT INTO follow_up_events (patient_id, evaluation_id, kind,
		recheck_reading_id, symptoms, note) VALUES ($1,$2,$3,$4,$5,$6)
		RETURNING id::text, patient_id::text, evaluation_id::text, kind, COALESCE(recheck_reading_id::text,''),
		symptoms, note, created_at`,
		f.PatientID, f.EvaluationID, f.Kind, rr, symptomsToText(f.Symptoms), nullStr(f.Note)))
}

func (s *Postgres) ListFollowUps(ctx context.Context, patientID string, tr TimeRange) ([]FollowUp, error) {
	from, to, lim := rangeArgs(tr)
	rows, err := s.pool.Query(ctx, followSelect()+` WHERE patient_id=$1
		AND ($2::timestamptz IS NULL OR created_at >= $2) AND ($3::timestamptz IS NULL OR created_at < $3)
		ORDER BY created_at DESC LIMIT $4`, patientID, from, to, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, scanFollow)
}

// ---- reminders ----------------------------------------------------------------

const reminderCols = `id::text, patient_id::text, evaluation_id::text, due_at, status, attempts, COALESCE(last_error,'')`

func scanReminder(row pgx.Row) (Reminder, error) {
	var r Reminder
	err := row.Scan(&r.ID, &r.PatientID, &r.EvaluationID, &r.DueAt, &r.Status, &r.Attempts, &r.LastError)
	return r, notFound(err)
}

func (s *Postgres) UpsertReminder(ctx context.Context, r Reminder) (Reminder, error) {
	out, err := scanReminder(s.pool.QueryRow(ctx, `INSERT INTO reminders (patient_id, evaluation_id, due_at)
		VALUES ($1,$2,$3) ON CONFLICT (evaluation_id) DO NOTHING RETURNING `+reminderCols, r.PatientID, r.EvaluationID, r.DueAt))
	if errors.Is(err, ErrNotFound) {
		return scanReminder(s.pool.QueryRow(ctx, `SELECT `+reminderCols+` FROM reminders WHERE evaluation_id=$1`, r.EvaluationID))
	}
	return out, err
}

// ClaimDueReminders atomically leases due reminders: it bumps attempts and
// pushes due_at forward by lease, so several workers never double-send and a
// crashed worker's reminders become due again once the lease expires.
func (s *Postgres) ClaimDueReminders(ctx context.Context, now time.Time, lease time.Duration, n int) ([]Reminder, error) {
	rows, err := s.pool.Query(ctx, `UPDATE reminders SET attempts = attempts + 1, due_at = $1::timestamptz + make_interval(secs => $3::float8)
		WHERE id IN (SELECT id FROM reminders WHERE status IN ('pending','failed') AND due_at <= $1
		             ORDER BY due_at LIMIT $2 FOR UPDATE SKIP LOCKED)
		RETURNING `+reminderCols, now, n, lease.Seconds())
	if err != nil {
		return nil, err
	}
	return collect(rows, scanReminder)
}

func (s *Postgres) UpdateReminder(ctx context.Context, r Reminder) error {
	_, err := s.pool.Exec(ctx, `UPDATE reminders SET status=$2, attempts=$3, last_error=$4, due_at=$5 WHERE evaluation_id=$1`,
		r.EvaluationID, r.Status, r.Attempts, nullStr(r.LastError), r.DueAt)
	return err
}

func (s *Postgres) CompleteRemindersForPatient(ctx context.Context, patientID string, before time.Time) error {
	_, err := s.pool.Exec(ctx, `UPDATE reminders r SET status='completed' FROM evaluations e
		WHERE r.evaluation_id = e.id AND r.patient_id=$1 AND r.status IN ('pending','failed','sent') AND e.created_at < $2`,
		patientID, before)
	return err
}

// ---- audit ------------------------------------------------------------------

func (s *Postgres) AppendAudit(ctx context.Context, e AuditEvent) error {
	detail := e.Detail
	if len(detail) == 0 {
		detail = json.RawMessage(`{}`)
	}
	_, err := s.pool.Exec(ctx, `INSERT INTO audit_events (patient_id, actor, action, entity_type, entity_id, detail)
		VALUES ($1,$2,$3,$4,$5,$6)`, nullStr(e.PatientID), e.Actor, e.Action, e.EntityType, nullStr(e.EntityID), detail)
	return err
}

func (s *Postgres) ListAudit(ctx context.Context, patientID string, tr TimeRange) ([]AuditEvent, error) {
	from, to, lim := rangeArgs(tr)
	rows, err := s.pool.Query(ctx, `SELECT id, COALESCE(patient_id::text,''), actor, action, entity_type,
		COALESCE(entity_id,''), detail, at FROM audit_events
		WHERE (CASE WHEN $1 = '' THEN patient_id IS NULL ELSE patient_id::text = $1 END)
		AND ($2::timestamptz IS NULL OR at >= $2) AND ($3::timestamptz IS NULL OR at < $3)
		ORDER BY at DESC LIMIT $4`, patientID, from, to, lim)
	if err != nil {
		return nil, err
	}
	return collect(rows, func(r pgx.Row) (AuditEvent, error) {
		var a AuditEvent
		err := r.Scan(&a.ID, &a.PatientID, &a.Actor, &a.Action, &a.EntityType, &a.EntityID, &a.Detail, &a.At)
		return a, err
	})
}

var _ Store = (*Postgres)(nil)
var _ Store = (*Memory)(nil)
