// Package clinical contains the deterministic safety engine. It never calls the
// network or an LLM. Every decision can be traced to a versioned rule and an
// approved message ID.
package clinical

import (
	"encoding/json"
	"fmt"
	"time"
)

// RuleSetVersion identifies the rules and message catalog in this build. Bump it
// whenever a rule, threshold default or message text changes.
const RuleSetVersion = "in-adult-2026.10.0-draft"

// RuleSetClinicallyApproved stays false until a qualified diabetes clinician has
// signed off every rule and message in this package. The server refuses to
// start in production while it is false.
const RuleSetClinicallyApproved = false

// EmergencyNumberIN is India's single emergency number.
const EmergencyNumberIN = "112"

type Unit string

const (
	UnitMgDL  Unit = "mg/dL"
	UnitMmolL Unit = "mmol/L"
)

type Source string

const (
	SourceManualMeter Source = "manual_meter"
	SourceCGM         Source = "cgm"
)

// MeterFlag carries "LO"/"HI" readings, where the meter shows no number because
// the value is outside its measuring range.
type MeterFlag string

const (
	MeterFlagNone MeterFlag = ""
	MeterFlagLO   MeterFlag = "LO"
	MeterFlagHI   MeterFlag = "HI"
)

type Symptom string

const (
	// Emergency: oral treatment is unsafe.
	SymptomUnconscious     Symptom = "unconscious"
	SymptomSeizure         Symptom = "seizure"
	SymptomUnableToSwallow Symptom = "unable_to_swallow"
	// Red flags (possible DKA/HHS or other emergencies).
	SymptomVomiting            Symptom = "vomiting"
	SymptomAbdominalPain       Symptom = "abdominal_pain"
	SymptomDifficultyBreathing Symptom = "difficulty_breathing"
	SymptomConfusion           Symptom = "confusion"
	// Typical hypoglycemia symptoms.
	SymptomShaky    Symptom = "shaky"
	SymptomSweating Symptom = "sweating"
	SymptomDizzy    Symptom = "dizzy"
	SymptomHungry   Symptom = "hungry"
	// Typical hyperglycemia symptoms.
	SymptomExtremeThirst     Symptom = "extreme_thirst"
	SymptomFrequentUrination Symptom = "frequent_urination"
	SymptomBlurredVision     Symptom = "blurred_vision"
)

var knownSymptoms = map[Symptom]bool{
	SymptomUnconscious: true, SymptomSeizure: true, SymptomUnableToSwallow: true,
	SymptomVomiting: true, SymptomAbdominalPain: true, SymptomDifficultyBreathing: true,
	SymptomConfusion: true, SymptomShaky: true, SymptomSweating: true, SymptomDizzy: true,
	SymptomHungry: true, SymptomExtremeThirst: true, SymptomFrequentUrination: true,
	SymptomBlurredVision: true,
}

func ValidSymptom(s Symptom) bool { return knownSymptoms[s] }

type AgeGroup string

const (
	AgeGroupAdult      AgeGroup = "adult"
	AgeGroupOlderAdult AgeGroup = "older_adult"
	AgeGroupAdolescent AgeGroup = "adolescent"
	AgeGroupChild      AgeGroup = "child"
)

type DiabetesType string

const (
	DiabetesType1           DiabetesType = "type1"
	DiabetesType2           DiabetesType = "type2"
	DiabetesTypeGestational DiabetesType = "gestational"
	DiabetesTypeOther       DiabetesType = "other"
)

// Patient is the clinical context the engine needs. It is not the full profile.
type Patient struct {
	AgeGroup     AgeGroup     `json:"age_group"`
	DiabetesType DiabetesType `json:"diabetes_type"`
	Pregnant     bool         `json:"pregnant"`
	// SpecialCircumstances covers anything a clinician says needs its own
	// protocol, such as dialysis or hypoglycemia unawareness.
	SpecialCircumstances bool `json:"special_circumstances"`
}

// InAdultScope reports whether the generic adult pathway may be applied.
func (p Patient) InAdultScope() bool {
	if p.AgeGroup != AgeGroupAdult && p.AgeGroup != AgeGroupOlderAdult {
		return false
	}
	return !p.Pregnant && p.DiabetesType != DiabetesTypeGestational && !p.SpecialCircumstances
}

// CarePlan holds the clinician-set thresholds for one patient. All glucose
// values are in mg/dL.
type CarePlan struct {
	Version               string        `json:"version"`
	LowMgDL               float64       `json:"low_mg_dl"`
	SevereLowMgDL         float64       `json:"severe_low_mg_dl"`
	HighMgDL              float64       `json:"high_mg_dl"`
	VeryHighMgDL          float64       `json:"very_high_mg_dl"`
	StaleAfter            time.Duration `json:"-"`
	LowRecheckAfter       time.Duration `json:"-"`
	HighRecheckAfter      time.Duration `json:"-"`
	MaxLowTreatmentCycles int           `json:"max_low_treatment_cycles"`
	ClinicianApproved     bool          `json:"clinician_approved"`
	ApprovedBy            string        `json:"approved_by,omitempty"`
	ApprovedAt            time.Time     `json:"approved_at,omitempty"`
}

// DefaultUnapprovedPlan is used when a patient has no approved plan. Its low
// thresholds follow the ADA definitions of level 1 (<70) and level 2 (<54)
// hypoglycemia. Its high values exist only so that high readings can be
// flagged; with no approved plan, the engine tells the patient to contact their
// care team rather than "follow your plan".
func DefaultUnapprovedPlan() CarePlan {
	return CarePlan{
		Version:               "default-unapproved",
		LowMgDL:               70,
		SevereLowMgDL:         54,
		HighMgDL:              250, // REVIEW: placeholder pending clinician review
		VeryHighMgDL:          300, // REVIEW: placeholder pending clinician review
		StaleAfter:            15 * time.Minute,
		LowRecheckAfter:       15 * time.Minute,
		HighRecheckAfter:      2 * time.Hour, // REVIEW
		MaxLowTreatmentCycles: 2,             // REVIEW
		ClinicianApproved:     false,
	}
}

// Validate rejects plans whose thresholds are internally inconsistent. A safety
// engine must not run on an incoherent plan.
func (p CarePlan) Validate() error {
	switch {
	case p.Version == "":
		return fmt.Errorf("care plan version is required")
	case p.SevereLowMgDL <= 0 || p.LowMgDL <= 0 || p.HighMgDL <= 0 || p.VeryHighMgDL <= 0:
		return fmt.Errorf("all thresholds must be positive")
	case !(p.SevereLowMgDL < p.LowMgDL && p.LowMgDL < p.HighMgDL && p.HighMgDL <= p.VeryHighMgDL):
		return fmt.Errorf("thresholds must satisfy severe_low < low < high <= very_high")
	case p.LowMgDL > 100:
		return fmt.Errorf("low threshold above 100 mg/dL is implausible")
	case p.StaleAfter <= 0 || p.LowRecheckAfter <= 0 || p.HighRecheckAfter <= 0:
		return fmt.Errorf("durations must be positive")
	case p.MaxLowTreatmentCycles < 1:
		return fmt.Errorf("max_low_treatment_cycles must be >= 1")
	case p.ClinicianApproved && p.ApprovedBy == "":
		return fmt.Errorf("approved plans must record approved_by")
	}
	return nil
}

type Reading struct {
	Value      float64   `json:"value"`
	Unit       Unit      `json:"unit"`
	MeterFlag  MeterFlag `json:"meter_flag,omitempty"`
	Source     Source    `json:"source"`
	MeasuredAt time.Time `json:"measured_at"`
	ReceivedAt time.Time `json:"received_at"`
}

// PriorEvent describes the patient's most recent evaluated reading. The engine
// uses it to track low-treatment cycles and rapid changes.
type PriorEvent struct {
	ActionCode ActionCode `json:"action_code"`
	Cycle      int        `json:"cycle"`
	ValueMgDL  float64    `json:"value_mg_dl"`
	MeasuredAt time.Time  `json:"measured_at"`
}

type Input struct {
	Reading  Reading
	Symptoms []Symptom
	Patient  Patient
	Plan     *CarePlan // nil means the patient has no plan
	Prior    *PriorEvent
	Now      time.Time
}

type Urgency int

const (
	UrgencyInfo Urgency = iota
	UrgencyMonitor
	UrgencyAction
	UrgencyUrgent
	UrgencyEmergency
)

var urgencyNames = [...]string{"info", "monitor", "action", "urgent", "emergency"}

func (u Urgency) String() string {
	if int(u) < len(urgencyNames) {
		return urgencyNames[u]
	}
	return "unknown"
}

func (u Urgency) MarshalJSON() ([]byte, error) { return json.Marshal(u.String()) }

func (u *Urgency) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	for i, n := range urgencyNames {
		if n == s {
			*u = Urgency(i)
			return nil
		}
	}
	return fmt.Errorf("unknown urgency %q", s)
}

type ActionCode string

const (
	ActionEmergencyNoOral        ActionCode = "EMERGENCY_NO_ORAL"
	ActionEmergencyAssessment    ActionCode = "EMERGENCY_ASSESSMENT"
	ActionSeekUrgentCare         ActionCode = "SEEK_URGENT_CARE"
	ActionTreatSevereLow         ActionCode = "TREAT_SEVERE_LOW"
	ActionTreatLow               ActionCode = "TREAT_LOW"
	ActionPersistentLowSeekHelp  ActionCode = "PERSISTENT_LOW_SEEK_HELP"
	ActionCheckNowStale          ActionCode = "CHECK_NOW_STALE_DATA"
	ActionVerifyDeviceTime       ActionCode = "VERIFY_DEVICE_TIME"
	ActionVerifyReading          ActionCode = "VERIFY_READING"
	ActionRecheckSymptoms        ActionCode = "RECHECK_SYMPTOMS_CONFLICT"
	ActionFollowHighPlan         ActionCode = "FOLLOW_HIGH_PLAN"
	ActionFollowVeryHighPlan     ActionCode = "FOLLOW_VERY_HIGH_PLAN"
	ActionHighContactCareTeam    ActionCode = "HIGH_CONTACT_CARE_TEAM"
	ActionSpecialContactCareTeam ActionCode = "SPECIAL_PROTOCOL_CONTACT_CARE_TEAM"
	ActionSpecialRouteProtocol   ActionCode = "SPECIAL_PROTOCOL_ROUTE"
	ActionInRange                ActionCode = "IN_RANGE"
	ActionSetUpCarePlan          ActionCode = "SET_UP_CARE_PLAN"
)

type ReasonCode string

const (
	ReasonNoOralSymptoms   ReasonCode = "SYMPTOM_NO_ORAL_INTAKE"
	ReasonHighWithRedFlags ReasonCode = "HIGH_WITH_RED_FLAG_SYMPTOMS"
	ReasonRedFlagSymptoms  ReasonCode = "RED_FLAG_SYMPTOMS"
	ReasonSevereLow        ReasonCode = "GLUCOSE_BELOW_SEVERE_LOW"
	ReasonLow              ReasonCode = "GLUCOSE_BELOW_LOW"
	ReasonLowWithConfusion ReasonCode = "LOW_WITH_CONFUSION"
	ReasonPersistentLow    ReasonCode = "LOW_AFTER_MAX_TREATMENT_CYCLES"
	ReasonMeterLimit       ReasonCode = "READING_AT_METER_LIMIT"
	ReasonStaleData        ReasonCode = "DATA_STALE"
	ReasonStaleLastLow     ReasonCode = "DATA_STALE_LAST_VALUE_LOW"
	ReasonFutureTimestamp  ReasonCode = "MEASURED_AT_IN_FUTURE"
	ReasonRapidChange      ReasonCode = "RAPID_CHANGE_FROM_PRIOR"
	ReasonSymptomConflict  ReasonCode = "HYPO_SYMPTOMS_WITH_NON_LOW_READING"
	ReasonHigh             ReasonCode = "GLUCOSE_ABOVE_HIGH"
	ReasonVeryHigh         ReasonCode = "GLUCOSE_ABOVE_VERY_HIGH"
	ReasonNoApprovedPlan   ReasonCode = "NO_APPROVED_CARE_PLAN"
	ReasonOutOfAdultScope  ReasonCode = "OUT_OF_ADULT_SCOPE"
	ReasonInRange          ReasonCode = "GLUCOSE_IN_TARGET_RANGE"
)

type DataStatus string

const (
	DataCurrent DataStatus = "current"
	DataStale   DataStatus = "stale"
	DataInvalid DataStatus = "invalid_timestamp"
)

type FollowUp struct {
	Required            bool `json:"required"`
	RecheckAfterMinutes int  `json:"recheck_after_minutes"`
}

type Escalation struct {
	CallEmergencyServices bool   `json:"call_emergency_services"`
	EmergencyNumber       string `json:"emergency_number,omitempty"`
	ContactCareTeam       bool   `json:"contact_care_team"`
	NotifyCaregiver       bool   `json:"notify_caregiver"`
}

// Decision is the authoritative safety outcome. Clients render it and must not
// recompute it.
type Decision struct {
	Urgency           Urgency      `json:"urgency"`
	ActionCode        ActionCode   `json:"action_code"`
	ApprovedMessageID string       `json:"approved_message_id"`
	ReasonCodes       []ReasonCode `json:"reason_codes"`
	MatchedRules      []string     `json:"matched_rules"`
	CarePlanVersion   string       `json:"care_plan_version"`
	CarePlanApproved  bool         `json:"care_plan_approved"`
	RuleSetVersion    string       `json:"rule_set_version"`
	FollowUp          FollowUp     `json:"follow_up_policy"`
	Escalation        Escalation   `json:"escalation"`
	ValueMgDL         float64      `json:"value_mg_dl"`
	DataStatus        DataStatus   `json:"data_status"`
	LowTreatmentCycle int          `json:"low_treatment_cycle,omitempty"`
}
