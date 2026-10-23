// Package store defines persistence for patients, readings, evaluations and the
// audit trail. Postgres is the production backend; Memory is for tests and
// local development.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/purplestack/glucoguard/internal/clinical"
)

var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

type Patient struct {
	ID                   string                `json:"id"`
	DisplayName          string                `json:"display_name"`
	AgeGroup             clinical.AgeGroup     `json:"age_group"`
	DiabetesType         clinical.DiabetesType `json:"diabetes_type"`
	Pregnant             bool                  `json:"pregnant"`
	SpecialCircumstances bool                  `json:"special_circumstances"`
	PreferredUnit        clinical.Unit         `json:"preferred_unit"`
	Language             string                `json:"language"`
	CaregiverName        string                `json:"caregiver_name,omitempty"`
	CaregiverPhone       string                `json:"caregiver_phone,omitempty"`
	ConsentVersion       string                `json:"consent_version"`
	ConsentGivenAt       time.Time             `json:"consent_given_at"`
	CreatedAt            time.Time             `json:"created_at"`
	UpdatedAt            time.Time             `json:"updated_at"`
}

func (p Patient) Clinical() clinical.Patient {
	return clinical.Patient{
		AgeGroup: p.AgeGroup, DiabetesType: p.DiabetesType,
		Pregnant: p.Pregnant, SpecialCircumstances: p.SpecialCircumstances,
	}
}

type CarePlan struct {
	ID        string `json:"id"`
	PatientID string `json:"patient_id"`
	clinical.CarePlan
	StaleAfterMinutes       int       `json:"stale_after_minutes"`
	LowRecheckAfterMinutes  int       `json:"low_recheck_after_minutes"`
	HighRecheckAfterMinutes int       `json:"high_recheck_after_minutes"`
	EffectiveFrom           time.Time `json:"effective_from"`
	CreatedAt               time.Time `json:"created_at"`
}

// SyncDurations fills the minute fields from the durations, or the reverse.
func (c *CarePlan) SyncDurations(fromMinutes bool) {
	if fromMinutes {
		c.StaleAfter = time.Duration(c.StaleAfterMinutes) * time.Minute
		c.LowRecheckAfter = time.Duration(c.LowRecheckAfterMinutes) * time.Minute
		c.HighRecheckAfter = time.Duration(c.HighRecheckAfterMinutes) * time.Minute
		return
	}
	c.StaleAfterMinutes = int(c.StaleAfter.Minutes())
	c.LowRecheckAfterMinutes = int(c.LowRecheckAfter.Minutes())
	c.HighRecheckAfterMinutes = int(c.HighRecheckAfter.Minutes())
}

type MealContext string

var ValidMealContexts = map[MealContext]bool{
	"unspecified": true, "fasting": true, "before_meal": true, "after_meal": true, "bedtime": true, "overnight": true,
}

type Reading struct {
	ID              string             `json:"id"`
	PatientID       string             `json:"patient_id"`
	ClientReadingID string             `json:"client_reading_id"`
	Value           float64            `json:"value"`
	Unit            clinical.Unit      `json:"unit"`
	MeterFlag       clinical.MeterFlag `json:"meter_flag,omitempty"`
	ValueMgDL       float64            `json:"value_mg_dl"`
	Source          clinical.Source    `json:"source"`
	DeviceID        string             `json:"device_id,omitempty"`
	MealContext     MealContext        `json:"meal_context"`
	Symptoms        []clinical.Symptom `json:"symptoms"`
	Notes           string             `json:"notes,omitempty"`
	MeasuredAt      time.Time          `json:"measured_at"`
	ReceivedAt      time.Time          `json:"received_at"`
}

func (r Reading) Clinical() clinical.Reading {
	return clinical.Reading{
		Value: r.Value, Unit: r.Unit, MeterFlag: r.MeterFlag, Source: r.Source,
		MeasuredAt: r.MeasuredAt, ReceivedAt: r.ReceivedAt,
	}
}

type Evaluation struct {
	ID             string            `json:"id"`
	PatientID      string            `json:"patient_id"`
	ReadingID      string            `json:"reading_id"`
	Decision       clinical.Decision `json:"decision"`
	AcknowledgedAt *time.Time        `json:"acknowledged_at,omitempty"`
	CreatedAt      time.Time         `json:"created_at"`
}

type FollowUpKind string

var ValidFollowUpKinds = map[FollowUpKind]bool{
	"acknowledged": true, "recheck": true, "symptom_update": true, "escalated": true, "resolved": true,
}

type FollowUp struct {
	ID               string             `json:"id"`
	PatientID        string             `json:"patient_id"`
	EvaluationID     string             `json:"evaluation_id"`
	Kind             FollowUpKind       `json:"kind"`
	RecheckReadingID string             `json:"recheck_reading_id,omitempty"`
	Symptoms         []clinical.Symptom `json:"symptoms"`
	Note             string             `json:"note,omitempty"`
	CreatedAt        time.Time          `json:"created_at"`
}

type ReminderStatus string

const (
	ReminderPending   ReminderStatus = "pending"
	ReminderSent      ReminderStatus = "sent"
	ReminderFailed    ReminderStatus = "failed"
	ReminderCancelled ReminderStatus = "cancelled"
	ReminderCompleted ReminderStatus = "completed"
)

type Reminder struct {
	ID           string         `json:"id"`
	PatientID    string         `json:"patient_id"`
	EvaluationID string         `json:"evaluation_id"`
	DueAt        time.Time      `json:"due_at"`
	Status       ReminderStatus `json:"status"`
	Attempts     int            `json:"attempts"`
	LastError    string         `json:"last_error,omitempty"`
}

type AuditEvent struct {
	ID         int64           `json:"id"`
	PatientID  string          `json:"patient_id,omitempty"`
	Actor      string          `json:"actor"`
	Action     string          `json:"action"`
	EntityType string          `json:"entity_type"`
	EntityID   string          `json:"entity_id,omitempty"`
	Detail     json.RawMessage `json:"detail,omitempty"`
	At         time.Time       `json:"at"`
}

type TimeRange struct {
	From, To time.Time // zero means unbounded
	Limit    int
}

// Store is implemented by Postgres and Memory. Implementations must be safe
// for concurrent use.
type Store interface {
	CreatePatient(ctx context.Context, p Patient) (Patient, error)
	GetPatient(ctx context.Context, id string) (Patient, error)
	UpdatePatient(ctx context.Context, p Patient) (Patient, error)
	// DeletePatient erases the patient and all personal data. Audit rows are
	// kept with the patient reference removed.
	DeletePatient(ctx context.Context, id string) error

	CreateCarePlan(ctx context.Context, c CarePlan) (CarePlan, error)
	CurrentCarePlan(ctx context.Context, patientID string) (CarePlan, error)

	// InsertReading is idempotent on (patient_id, client_reading_id). It
	// returns created=false and the existing reading on a duplicate.
	InsertReading(ctx context.Context, r Reading) (out Reading, created bool, err error)
	GetReading(ctx context.Context, patientID, id string) (Reading, error)
	ListReadings(ctx context.Context, patientID string, tr TimeRange) ([]Reading, error)

	InsertEvaluation(ctx context.Context, e Evaluation) (Evaluation, error)
	GetEvaluation(ctx context.Context, patientID, id string) (Evaluation, error)
	// LatestEvaluationForReading returns the newest evaluation of one reading.
	LatestEvaluationForReading(ctx context.Context, patientID, readingID string) (Evaluation, error)
	// LatestEvaluationBefore returns the newest evaluation whose reading was
	// measured strictly before t, excluding readingID.
	LatestEvaluationBefore(ctx context.Context, patientID string, t time.Time, excludeReadingID string) (Evaluation, Reading, error)
	ListEvaluations(ctx context.Context, patientID string, tr TimeRange) ([]Evaluation, error)
	AcknowledgeEvaluation(ctx context.Context, patientID, id string, at time.Time) error

	InsertFollowUp(ctx context.Context, f FollowUp) (FollowUp, error)
	ListFollowUps(ctx context.Context, patientID string, tr TimeRange) ([]FollowUp, error)

	UpsertReminder(ctx context.Context, r Reminder) (Reminder, error)
	ClaimDueReminders(ctx context.Context, now time.Time, lease time.Duration, limit int) ([]Reminder, error)
	UpdateReminder(ctx context.Context, r Reminder) error
	CompleteRemindersForPatient(ctx context.Context, patientID string, before time.Time) error

	AppendAudit(ctx context.Context, e AuditEvent) error
	ListAudit(ctx context.Context, patientID string, tr TimeRange) ([]AuditEvent, error)

	Close()
}
