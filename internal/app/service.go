// Package app holds the application services that sit between HTTP and the
// store. It owns the evaluation flow: persist, evaluate, record, remind, audit.
package app

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/purplestack/glucoguard/internal/clinical"
	"github.com/purplestack/glucoguard/internal/store"
)

// ConsentVersion is the current privacy notice version (DPDP Act 2023).
const ConsentVersion = "privacy-notice-2026-10"

type Service struct {
	Store store.Store
	Now   func() time.Time
	Log   *slog.Logger
}

func New(s store.Store, log *slog.Logger) *Service {
	return &Service{Store: s, Now: func() time.Time { return time.Now().UTC() }, Log: log}
}

// Actor identifies who performed an action, for the audit trail.
type Actor struct {
	ID   string
	Role string // "patient" | "clinician" | "system"
}

func (a Actor) String() string { return a.Role + ":" + a.ID }

// ValidationError is returned for bad client input (HTTP 422).
type ValidationError struct{ Msg string }

func (e *ValidationError) Error() string { return e.Msg }

func invalid(format string, args ...any) error { return &ValidationError{fmt.Sprintf(format, args...)} }

// EvaluationFailure means the safety engine could not produce a decision.
// The API must show FailSafe guidance rather than nothing.
type EvaluationFailure struct{ Err error }

func (e *EvaluationFailure) Error() string { return "evaluation failed: " + e.Err.Error() }
func (e *EvaluationFailure) Unwrap() error { return e.Err }

// FailSafe is what clients show when evaluation fails for any reason.
var FailSafe = map[string]any{
	"urgency":         "urgent",
	"action_code":     "EVALUATION_UNAVAILABLE",
	"message":         "We couldn't check this reading. If you feel unwell, follow your care plan and contact your care team. In an emergency call 112.",
	"emergency_phone": clinical.EmergencyNumberIN,
}

// ---- patients ---------------------------------------------------------------

type PatientInput struct {
	DisplayName          string                `json:"display_name"`
	AgeGroup             clinical.AgeGroup     `json:"age_group"`
	DiabetesType         clinical.DiabetesType `json:"diabetes_type"`
	Pregnant             bool                  `json:"pregnant"`
	SpecialCircumstances bool                  `json:"special_circumstances"`
	PreferredUnit        clinical.Unit         `json:"preferred_unit"`
	Language             string                `json:"language"`
	CaregiverName        string                `json:"caregiver_name"`
	CaregiverPhone       string                `json:"caregiver_phone"`
	ConsentVersion       string                `json:"consent_version"`
	ConsentGiven         bool                  `json:"consent_given"`
}

func (in *PatientInput) normalise() error {
	in.DisplayName = strings.TrimSpace(in.DisplayName)
	if in.DisplayName == "" || len(in.DisplayName) > 100 {
		return invalid("display_name is required (max 100 chars)")
	}
	switch in.AgeGroup {
	case clinical.AgeGroupAdult, clinical.AgeGroupOlderAdult, clinical.AgeGroupAdolescent, clinical.AgeGroupChild:
	default:
		return invalid("age_group must be adult, older_adult, adolescent or child")
	}
	switch in.DiabetesType {
	case clinical.DiabetesType1, clinical.DiabetesType2, clinical.DiabetesTypeGestational, clinical.DiabetesTypeOther:
	default:
		return invalid("diabetes_type must be type1, type2, gestational or other")
	}
	if in.PreferredUnit == "" {
		in.PreferredUnit = clinical.UnitMgDL
	}
	if in.PreferredUnit != clinical.UnitMgDL && in.PreferredUnit != clinical.UnitMmolL {
		return invalid("preferred_unit must be mg/dL or mmol/L")
	}
	if in.Language == "" {
		in.Language = "en"
	}
	if in.Language != "en" && in.Language != "hi" {
		return invalid("language must be en or hi")
	}
	if in.CaregiverPhone != "" && !validIndianPhone(in.CaregiverPhone) {
		return invalid("caregiver_phone must be an Indian mobile number in +91XXXXXXXXXX format")
	}
	return nil
}

func validIndianPhone(p string) bool {
	if len(p) != 13 || !strings.HasPrefix(p, "+91") || !strings.ContainsAny(p[3:4], "6789") {
		return false
	}
	for _, c := range p[3:] {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func (s *Service) RegisterPatient(ctx context.Context, in PatientInput) (store.Patient, error) {
	if err := in.normalise(); err != nil {
		return store.Patient{}, err
	}
	if !in.ConsentGiven || in.ConsentVersion != ConsentVersion {
		return store.Patient{}, invalid("explicit consent to privacy notice %q is required", ConsentVersion)
	}
	p, err := s.Store.CreatePatient(ctx, store.Patient{
		DisplayName: in.DisplayName, AgeGroup: in.AgeGroup, DiabetesType: in.DiabetesType,
		Pregnant: in.Pregnant, SpecialCircumstances: in.SpecialCircumstances, PreferredUnit: in.PreferredUnit,
		Language: in.Language, CaregiverName: in.CaregiverName, CaregiverPhone: in.CaregiverPhone,
		ConsentVersion: in.ConsentVersion, ConsentGivenAt: s.Now(),
	})
	if err != nil {
		return store.Patient{}, err
	}
	s.audit(ctx, p.ID, Actor{p.ID, "patient"}, "patient.registered", "patient", p.ID, map[string]any{"consent_version": in.ConsentVersion})
	return p, nil
}

func (s *Service) UpdatePatient(ctx context.Context, actor Actor, id string, in PatientInput) (store.Patient, error) {
	if err := in.normalise(); err != nil {
		return store.Patient{}, err
	}
	old, err := s.Store.GetPatient(ctx, id)
	if err != nil {
		return store.Patient{}, err
	}
	old.DisplayName, old.AgeGroup, old.DiabetesType = in.DisplayName, in.AgeGroup, in.DiabetesType
	old.Pregnant, old.SpecialCircumstances, old.PreferredUnit = in.Pregnant, in.SpecialCircumstances, in.PreferredUnit
	old.Language, old.CaregiverName, old.CaregiverPhone = in.Language, in.CaregiverName, in.CaregiverPhone
	p, err := s.Store.UpdatePatient(ctx, old)
	if err != nil {
		return store.Patient{}, err
	}
	s.audit(ctx, id, actor, "patient.updated", "patient", id, map[string]any{
		"age_group": p.AgeGroup, "pregnant": p.Pregnant, "special_circumstances": p.SpecialCircumstances,
	})
	return p, nil
}

func (s *Service) DeletePatient(ctx context.Context, actor Actor, id string) error {
	// Audit first so the deletion itself is on record (the patient reference is
	// cleared from all audit rows by the delete).
	s.audit(ctx, id, actor, "patient.deletion_requested", "patient", id, nil)
	if err := s.Store.DeletePatient(ctx, id); err != nil {
		return err
	}
	s.audit(ctx, "", actor, "patient.deleted", "patient", "", nil)
	return nil
}

// ---- care plans -------------------------------------------------------------

type CarePlanInput struct {
	Version                 string    `json:"version"`
	LowMgDL                 float64   `json:"low_mg_dl"`
	SevereLowMgDL           float64   `json:"severe_low_mg_dl"`
	HighMgDL                float64   `json:"high_mg_dl"`
	VeryHighMgDL            float64   `json:"very_high_mg_dl"`
	StaleAfterMinutes       int       `json:"stale_after_minutes"`
	LowRecheckAfterMinutes  int       `json:"low_recheck_after_minutes"`
	HighRecheckAfterMinutes int       `json:"high_recheck_after_minutes"`
	MaxLowTreatmentCycles   int       `json:"max_low_treatment_cycles"`
	Approve                 bool      `json:"approve"`
	EffectiveFrom           time.Time `json:"effective_from"`
}

// CreateCarePlan may only be called by a clinician. An approved plan records
// the clinician as approver.
func (s *Service) CreateCarePlan(ctx context.Context, clinician Actor, patientID string, in CarePlanInput) (store.CarePlan, error) {
	if clinician.Role != "clinician" {
		return store.CarePlan{}, ErrForbidden
	}
	c := store.CarePlan{PatientID: patientID, EffectiveFrom: in.EffectiveFrom,
		StaleAfterMinutes: in.StaleAfterMinutes, LowRecheckAfterMinutes: in.LowRecheckAfterMinutes,
		HighRecheckAfterMinutes: in.HighRecheckAfterMinutes}
	c.Version, c.LowMgDL, c.SevereLowMgDL = in.Version, in.LowMgDL, in.SevereLowMgDL
	c.HighMgDL, c.VeryHighMgDL, c.MaxLowTreatmentCycles = in.HighMgDL, in.VeryHighMgDL, in.MaxLowTreatmentCycles
	c.SyncDurations(true)
	if in.Approve {
		c.ClinicianApproved, c.ApprovedBy, c.ApprovedAt = true, clinician.ID, s.Now()
	}
	if err := c.CarePlan.Validate(); err != nil {
		return store.CarePlan{}, invalid("care plan: %v", err)
	}
	out, err := s.Store.CreateCarePlan(ctx, c)
	if err != nil {
		return store.CarePlan{}, err
	}
	s.audit(ctx, patientID, clinician, "care_plan.created", "care_plan", out.ID, out)
	return out, nil
}

// CurrentPlan returns the effective plan, or nil if the patient has none.
func (s *Service) CurrentPlan(ctx context.Context, patientID string) (*store.CarePlan, error) {
	p, err := s.Store.CurrentCarePlan(ctx, patientID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ---- readings and evaluation ------------------------------------------------

type ReadingInput struct {
	ClientReadingID string             `json:"client_reading_id"`
	Value           float64            `json:"value"`
	Unit            clinical.Unit      `json:"unit"`
	MeterFlag       clinical.MeterFlag `json:"meter_flag"`
	Source          clinical.Source    `json:"source"`
	DeviceID        string             `json:"device_id"`
	MealContext     store.MealContext  `json:"meal_context"`
	Symptoms        []clinical.Symptom `json:"symptoms"`
	Notes           string             `json:"notes"`
	MeasuredAt      time.Time          `json:"measured_at"`
}

type EvaluatedReading struct {
	Reading    store.Reading    `json:"reading"`
	Evaluation store.Evaluation `json:"evaluation"`
	Message    clinical.Message `json:"message"`
	Duplicate  bool             `json:"duplicate"`
}

var ErrForbidden = errors.New("forbidden")

func (s *Service) SubmitReading(ctx context.Context, actor Actor, patientID string, in ReadingInput) (EvaluatedReading, error) {
	now := s.Now()
	in.ClientReadingID = strings.TrimSpace(in.ClientReadingID)
	if in.ClientReadingID == "" || len(in.ClientReadingID) > 64 {
		return EvaluatedReading{}, invalid("client_reading_id is required (max 64 chars); generate one per reading so retries are safe")
	}
	if in.Source == "" {
		in.Source = clinical.SourceManualMeter
	}
	if in.MealContext == "" {
		in.MealContext = "unspecified"
	}
	if !store.ValidMealContexts[in.MealContext] {
		return EvaluatedReading{}, invalid("unknown meal_context %q", in.MealContext)
	}
	if len(in.Notes) > 500 {
		return EvaluatedReading{}, invalid("notes max 500 chars")
	}
	in.Symptoms = dedupe(in.Symptoms)
	if _, err := s.Store.GetPatient(ctx, patientID); err != nil {
		return EvaluatedReading{}, err
	}
	// Structural validation through the engine so API and engine agree.
	probe := clinical.Input{Reading: clinical.Reading{Value: in.Value, Unit: in.Unit, MeterFlag: in.MeterFlag,
		Source: in.Source, MeasuredAt: in.MeasuredAt, ReceivedAt: now}, Symptoms: in.Symptoms, Now: now}
	if err := clinical.ValidateInput(probe); err != nil {
		return EvaluatedReading{}, invalid("%s", strings.TrimPrefix(err.Error(), clinical.ErrInvalidInput.Error()+": "))
	}
	var mg float64
	switch in.MeterFlag {
	case clinical.MeterFlagLO:
		mg, in.Value = clinical.MeterMinMgDL, 0
	case clinical.MeterFlagHI:
		mg, in.Value = clinical.MeterMaxMgDL, 0
	default:
		mg, _ = clinical.ToMgDL(in.Value, in.Unit)
	}

	r, created, err := s.Store.InsertReading(ctx, store.Reading{
		PatientID: patientID, ClientReadingID: in.ClientReadingID, Value: in.Value, Unit: in.Unit,
		MeterFlag: in.MeterFlag, ValueMgDL: mg, Source: in.Source, DeviceID: in.DeviceID,
		MealContext: in.MealContext, Symptoms: in.Symptoms, Notes: in.Notes,
		MeasuredAt: in.MeasuredAt.UTC(), ReceivedAt: now,
	})
	if err != nil {
		return EvaluatedReading{}, err
	}
	if !created {
		// Idempotent retry: return what the patient was already shown.
		if e, err := s.Store.LatestEvaluationForReading(ctx, patientID, r.ID); err == nil {
			return EvaluatedReading{Reading: r, Evaluation: e, Message: clinical.Catalog[e.Decision.ApprovedMessageID], Duplicate: true}, nil
		}
	} else {
		s.audit(ctx, patientID, actor, "reading.created", "glucose_reading", r.ID,
			map[string]any{"source": r.Source, "value_mg_dl": r.ValueMgDL, "measured_at": r.MeasuredAt})
	}
	out, err := s.evaluate(ctx, actor, r, r.Symptoms)
	out.Duplicate = !created
	return out, err
}

// EvaluateReading re-runs the engine on a stored reading at the current time.
// A reading that was current at submission can be stale now, and the decision
// will say so.
func (s *Service) EvaluateReading(ctx context.Context, actor Actor, patientID, readingID string, extraSymptoms []clinical.Symptom) (EvaluatedReading, error) {
	r, err := s.Store.GetReading(ctx, patientID, readingID)
	if err != nil {
		return EvaluatedReading{}, err
	}
	sym := dedupe(append(append([]clinical.Symptom{}, r.Symptoms...), extraSymptoms...))
	for _, x := range sym {
		if !clinical.ValidSymptom(x) {
			return EvaluatedReading{}, invalid("unknown symptom %q", x)
		}
	}
	return s.evaluate(ctx, actor, r, sym)
}

func (s *Service) evaluate(ctx context.Context, actor Actor, r store.Reading, symptoms []clinical.Symptom) (EvaluatedReading, error) {
	now := s.Now()
	patient, err := s.Store.GetPatient(ctx, r.PatientID)
	if err != nil {
		return EvaluatedReading{}, err
	}
	plan, err := s.CurrentPlan(ctx, r.PatientID)
	if err != nil {
		return EvaluatedReading{}, &EvaluationFailure{err}
	}
	in := clinical.Input{Reading: r.Clinical(), Symptoms: symptoms, Patient: patient.Clinical(), Now: now}
	if plan != nil {
		cp := plan.CarePlan
		in.Plan = &cp
	}
	if pe, pr, err := s.Store.LatestEvaluationBefore(ctx, r.PatientID, r.MeasuredAt, r.ID); err == nil {
		in.Prior = &clinical.PriorEvent{ActionCode: pe.Decision.ActionCode, Cycle: pe.Decision.LowTreatmentCycle,
			ValueMgDL: pr.ValueMgDL, MeasuredAt: pr.MeasuredAt}
	} else if !errors.Is(err, store.ErrNotFound) {
		return EvaluatedReading{}, &EvaluationFailure{err}
	}

	d, err := clinical.Evaluate(in)
	if err != nil {
		s.Log.Error("SAFETY: evaluation failed", "patient_id", r.PatientID, "reading_id", r.ID, "err", err)
		s.audit(ctx, r.PatientID, actor, "reading.evaluation_failed", "glucose_reading", r.ID, map[string]any{"error": err.Error()})
		return EvaluatedReading{Reading: r}, &EvaluationFailure{err}
	}
	e, err := s.Store.InsertEvaluation(ctx, store.Evaluation{PatientID: r.PatientID, ReadingID: r.ID, Decision: d, CreatedAt: now})
	if err != nil {
		return EvaluatedReading{}, &EvaluationFailure{err}
	}

	// A new evaluation supersedes earlier recheck reminders.
	if err := s.Store.CompleteRemindersForPatient(ctx, r.PatientID, e.CreatedAt); err != nil {
		s.Log.Warn("complete reminders", "err", err)
	}
	if d.FollowUp.Required && d.FollowUp.RecheckAfterMinutes > 0 {
		due := now.Add(time.Duration(d.FollowUp.RecheckAfterMinutes) * time.Minute)
		if _, err := s.Store.UpsertReminder(ctx, store.Reminder{PatientID: r.PatientID, EvaluationID: e.ID, DueAt: due}); err != nil {
			// The client also schedules a local reminder, so a failure here is
			// logged loudly but does not block the safety guidance.
			s.Log.Error("SAFETY: reminder not persisted", "evaluation_id", e.ID, "err", err)
		}
	}

	s.audit(ctx, r.PatientID, actor, "rule.executed", "evaluation", e.ID, map[string]any{
		"reading_id": r.ID, "rule_set_version": d.RuleSetVersion, "care_plan_version": d.CarePlanVersion,
		"urgency": d.Urgency.String(), "action_code": d.ActionCode, "message_id": d.ApprovedMessageID,
		"matched_rules": d.MatchedRules, "symptoms": symptoms, "data_status": d.DataStatus,
	})
	if d.Escalation.NotifyCaregiver && patient.CaregiverPhone != "" {
		s.audit(ctx, r.PatientID, Actor{"notifier", "system"}, "caregiver.notification_queued", "evaluation", e.ID, nil)
	}
	return EvaluatedReading{Reading: r, Evaluation: e, Message: clinical.Catalog[d.ApprovedMessageID]}, nil
}

// ---- follow-ups ---------------------------------------------------------------

type FollowUpInput struct {
	EvaluationID string             `json:"evaluation_id"`
	Kind         store.FollowUpKind `json:"kind"`
	Symptoms     []clinical.Symptom `json:"symptoms"`
	Note         string             `json:"note"`
	Recheck      *ReadingInput      `json:"recheck,omitempty"`
}

type FollowUpResult struct {
	FollowUp      store.FollowUp    `json:"follow_up"`
	NewEvaluation *EvaluatedReading `json:"new_evaluation,omitempty"`
}

func (s *Service) RecordFollowUp(ctx context.Context, actor Actor, patientID string, in FollowUpInput) (FollowUpResult, error) {
	if !store.ValidFollowUpKinds[in.Kind] {
		return FollowUpResult{}, invalid("kind must be acknowledged, recheck, symptom_update, escalated or resolved")
	}
	if len(in.Note) > 500 {
		return FollowUpResult{}, invalid("note max 500 chars")
	}
	ev, err := s.Store.GetEvaluation(ctx, patientID, in.EvaluationID)
	if err != nil {
		return FollowUpResult{}, err
	}
	in.Symptoms = dedupe(in.Symptoms)
	for _, x := range in.Symptoms {
		if !clinical.ValidSymptom(x) {
			return FollowUpResult{}, invalid("unknown symptom %q", x)
		}
	}

	var res FollowUpResult
	switch in.Kind {
	case "recheck":
		if in.Recheck == nil {
			return FollowUpResult{}, invalid("recheck follow-ups need a recheck reading")
		}
		in.Recheck.Symptoms = dedupe(append(in.Recheck.Symptoms, in.Symptoms...))
		er, err := s.SubmitReading(ctx, actor, patientID, *in.Recheck)
		if err != nil {
			return FollowUpResult{}, err
		}
		res.NewEvaluation = &er
	case "symptom_update":
		if len(in.Symptoms) == 0 {
			return FollowUpResult{}, invalid("symptom_update needs at least one symptom")
		}
		// Re-run the engine on the original reading with the new symptoms.
		// Emergency symptoms escalate even though the reading may now be stale.
		er, err := s.EvaluateReading(ctx, actor, patientID, ev.ReadingID, in.Symptoms)
		if err != nil {
			return FollowUpResult{}, err
		}
		res.NewEvaluation = &er
	case "acknowledged":
		if err := s.Store.AcknowledgeEvaluation(ctx, patientID, ev.ID, s.Now()); err != nil {
			return FollowUpResult{}, err
		}
	}

	f := store.FollowUp{PatientID: patientID, EvaluationID: ev.ID, Kind: in.Kind, Symptoms: in.Symptoms, Note: in.Note}
	if res.NewEvaluation != nil && in.Kind == "recheck" {
		f.RecheckReadingID = res.NewEvaluation.Reading.ID
	}
	if f.Symptoms == nil {
		f.Symptoms = []clinical.Symptom{}
	}
	res.FollowUp, err = s.Store.InsertFollowUp(ctx, f)
	if err != nil {
		return FollowUpResult{}, err
	}
	s.audit(ctx, patientID, actor, "follow_up.recorded", "follow_up", res.FollowUp.ID, map[string]any{"kind": in.Kind, "evaluation_id": ev.ID})
	return res, nil
}

// ---- helpers ------------------------------------------------------------------

func (s *Service) audit(ctx context.Context, patientID string, actor Actor, action, entity, entityID string, detail any) {
	var raw json.RawMessage
	if detail != nil {
		raw, _ = json.Marshal(detail)
	}
	if err := s.Store.AppendAudit(ctx, store.AuditEvent{PatientID: patientID, Actor: actor.String(), Action: action,
		EntityType: entity, EntityID: entityID, Detail: raw, At: s.Now()}); err != nil {
		s.Log.Error("audit write failed", "action", action, "err", err)
	}
}

func dedupe(xs []clinical.Symptom) []clinical.Symptom {
	seen := map[clinical.Symptom]bool{}
	out := make([]clinical.Symptom, 0, len(xs))
	for _, x := range xs {
		if !seen[x] {
			seen[x] = true
			out = append(out, x)
		}
	}
	return out
}
