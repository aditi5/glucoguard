package clinical

import (
	"math"
	"time"
)

// Scope controls which patients a rule applies to.
type Scope int

const (
	// ScopeUniversal rules apply to every patient. They are limited to
	// emergencies, data integrity and routing.
	ScopeUniversal Scope = iota
	// ScopeAdult rules give the generic adult pathway. They are suppressed
	// for children, pregnancy and other special circumstances.
	ScopeAdult
)

// Recheck says when the patient should take another reading.
type Recheck int

const (
	RecheckNone Recheck = iota
	RecheckNow
	RecheckLowInterval
	RecheckHighInterval
)

// Outcome is what a rule contributes when it matches.
type Outcome struct {
	Urgency    Urgency
	Action     ActionCode
	MessageID  string
	Reason     ReasonCode
	Escalation Escalation
	Recheck    Recheck
}

// Rule is one row of the clinical rules table. Priority decides the primary
// action (lower = more important). Every matched rule still contributes its
// reason code and escalations, so a lower-priority match can only add safety,
// never remove it.
type Rule struct {
	ID       string
	Priority int
	Scope    Scope
	When     func(c *evalContext) bool
	Then     Outcome
}

// evalContext holds facts derived once from the input.
type evalContext struct {
	in           Input
	mg           float64
	plan         CarePlan
	planApproved bool
	adultScope   bool
	stale        bool
	future       bool
	symptoms     map[Symptom]bool
}

// usable reports whether the glucose value can be treated as a current
// measurement.
func (c *evalContext) usable() bool { return !c.stale && !c.future }

func (c *evalContext) has(s ...Symptom) bool {
	for _, x := range s {
		if c.symptoms[x] {
			return true
		}
	}
	return false
}

func (c *evalContext) noOral() bool {
	return c.has(SymptomUnconscious, SymptomSeizure, SymptomUnableToSwallow)
}

func (c *evalContext) redFlag() bool {
	return c.has(SymptomVomiting, SymptomAbdominalPain, SymptomDifficultyBreathing, SymptomConfusion)
}

func (c *evalContext) hypoSymptoms() bool {
	return c.has(SymptomShaky, SymptomSweating, SymptomDizzy, SymptomHungry, SymptomConfusion)
}

func (c *evalContext) priorWasLowTreatment() bool {
	p := c.in.Prior
	if p == nil {
		return false
	}
	if p.ActionCode != ActionTreatLow && p.ActionCode != ActionTreatSevereLow && p.ActionCode != ActionPersistentLowSeekHelp {
		return false
	}
	// A treatment cycle only continues if the prior reading was recent.
	return c.in.Reading.MeasuredAt.Sub(p.MeasuredAt) <= lowCycleWindow
}

const (
	lowCycleWindow    = 60 * time.Minute
	rapidChangeWindow = 30 * time.Minute
	rapidChangeMgDL   = 100.0 // REVIEW
)

var (
	callEmergency = Escalation{CallEmergencyServices: true, EmergencyNumber: EmergencyNumberIN, NotifyCaregiver: true}
	careTeam      = Escalation{ContactCareTeam: true}
)

// Rules is the clinical rules table, ordered by priority. Add a test case in
// engine_test.go for every rule you add or change.
var Rules = []Rule{
	// ---- Tier 0: emergencies (universal) ---------------------------------
	{
		ID: "U01_NO_ORAL_EMERGENCY", Priority: 0, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return c.noOral() },
		Then: Outcome{UrgencyEmergency, ActionEmergencyNoOral, "EMG_NO_ORAL_001", ReasonNoOralSymptoms, callEmergency, RecheckNone},
	},
	{
		ID: "U02_HIGH_WITH_RED_FLAGS", Priority: 1, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return c.usable() && c.mg >= c.plan.HighMgDL && c.redFlag() },
		Then: Outcome{UrgencyEmergency, ActionEmergencyAssessment, "EMG_HYPER_001", ReasonHighWithRedFlags, callEmergency, RecheckNone},
	},
	{
		ID: "U03_RED_FLAG_SYMPTOMS", Priority: 5, Scope: ScopeUniversal,
		When: func(c *evalContext) bool {
			if !c.redFlag() {
				return false
			}
			// In an adult with a current low reading, confusion alone is
			// explained by the low and handled by A29. Treating the low is the
			// priority there, so it is not double-routed here.
			confusionOnly := c.has(SymptomConfusion) &&
				!c.has(SymptomVomiting, SymptomAbdominalPain, SymptomDifficultyBreathing)
			if confusionOnly && c.adultScope && c.usable() && c.mg < c.plan.LowMgDL {
				return false
			}
			return true
		},
		Then: Outcome{UrgencyUrgent, ActionSeekUrgentCare, "URG_REDFLAG_001", ReasonRedFlagSymptoms, careTeam, RecheckNone},
	},

	// ---- Tier 1: data integrity (universal) --------------------------------
	{
		ID: "U10_FUTURE_TIMESTAMP", Priority: 10, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return c.future },
		Then: Outcome{UrgencyAction, ActionVerifyDeviceTime, "FUTURE_TS_001", ReasonFutureTimestamp, Escalation{}, RecheckNow},
	},
	{
		ID: "U11_STALE_LAST_LOW", Priority: 11, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return c.stale && c.mg < c.plan.LowMgDL },
		Then: Outcome{UrgencyUrgent, ActionCheckNowStale, "STALE_LOW_001", ReasonStaleLastLow, Escalation{}, RecheckNow},
	},
	{
		ID: "U12_STALE", Priority: 12, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return c.stale },
		Then: Outcome{UrgencyAction, ActionCheckNowStale, "STALE_001", ReasonStaleData, Escalation{}, RecheckNow},
	},

	// ---- Tier 2: special populations (universal routing) -----------------
	{
		ID: "U20_SPECIAL_OUT_OF_RANGE", Priority: 20, Scope: ScopeUniversal,
		When: func(c *evalContext) bool {
			return !c.adultScope && c.usable() && (c.mg < c.plan.LowMgDL || c.mg >= c.plan.HighMgDL)
		},
		Then: Outcome{UrgencyUrgent, ActionSpecialContactCareTeam, "SPECIAL_OOR_001", ReasonOutOfAdultScope, careTeam, RecheckNone},
	},
	{
		ID: "U21_SPECIAL_ROUTE", Priority: 21, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return !c.adultScope },
		Then: Outcome{UrgencyInfo, ActionSpecialRouteProtocol, "SPECIAL_ROUTE_001", ReasonOutOfAdultScope, Escalation{}, RecheckNone},
	},

	// ---- Tier 3: adult hypoglycemia ---------------------------------------
	{
		ID: "A29_LOW_WITH_CONFUSION", Priority: 29, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.mg < c.plan.LowMgDL && c.has(SymptomConfusion) },
		Then: Outcome{UrgencyUrgent, ActionTreatSevereLow, "SEVERE_LOW_001", ReasonLowWithConfusion,
			Escalation{NotifyCaregiver: true}, RecheckLowInterval},
	},
	{
		ID: "A30_PERSISTENT_LOW", Priority: 30, Scope: ScopeAdult,
		When: func(c *evalContext) bool {
			return c.usable() && c.mg < c.plan.LowMgDL && c.priorWasLowTreatment() &&
				c.in.Prior.Cycle >= c.plan.MaxLowTreatmentCycles
		},
		Then: Outcome{UrgencyUrgent, ActionPersistentLowSeekHelp, "LOW_PERSIST_001", ReasonPersistentLow,
			Escalation{ContactCareTeam: true, NotifyCaregiver: true}, RecheckLowInterval},
	},
	{
		ID: "A31_SEVERE_LOW", Priority: 31, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.mg < c.plan.SevereLowMgDL },
		Then: Outcome{UrgencyUrgent, ActionTreatSevereLow, "SEVERE_LOW_001", ReasonSevereLow,
			Escalation{NotifyCaregiver: true}, RecheckLowInterval},
	},
	{
		ID: "A32_LOW", Priority: 32, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.mg < c.plan.LowMgDL },
		Then: Outcome{UrgencyAction, ActionTreatLow, "LOW_001", ReasonLow, Escalation{}, RecheckLowInterval},
	},

	// ---- Tier 4: verification (universal) ---------------------------------
	{
		ID: "U40_METER_LIMIT", Priority: 40, Scope: ScopeUniversal,
		When: func(c *evalContext) bool {
			r := c.in.Reading
			return r.MeterFlag != MeterFlagNone || c.mg < MeterMinMgDL || c.mg > MeterMaxMgDL
		},
		Then: Outcome{UrgencyUrgent, ActionVerifyReading, "VERIFY_001", ReasonMeterLimit, Escalation{}, RecheckNow},
	},
	{
		ID: "U41_RAPID_CHANGE", Priority: 41, Scope: ScopeUniversal,
		When: func(c *evalContext) bool {
			p := c.in.Prior
			if p == nil || !c.usable() {
				return false
			}
			dt := c.in.Reading.MeasuredAt.Sub(p.MeasuredAt)
			return dt >= 0 && dt <= rapidChangeWindow && math.Abs(c.mg-p.ValueMgDL) >= rapidChangeMgDL
		},
		Then: Outcome{UrgencyAction, ActionVerifyReading, "VERIFY_001", ReasonRapidChange, Escalation{}, RecheckNow},
	},
	{
		ID: "A42_SYMPTOM_CONFLICT", Priority: 42, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.mg >= c.plan.LowMgDL && c.hypoSymptoms() },
		Then: Outcome{UrgencyAction, ActionRecheckSymptoms, "CONFLICT_001", ReasonSymptomConflict, Escalation{}, RecheckNow},
	},

	// ---- Tier 5: adult hyperglycemia ---------------------------------------
	{
		ID: "A50_HIGH_NO_PLAN", Priority: 50, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && !c.planApproved && c.mg >= c.plan.HighMgDL },
		Then: Outcome{UrgencyAction, ActionHighContactCareTeam, "HIGH_NO_PLAN_001", ReasonHigh, careTeam, RecheckHighInterval},
	},
	{
		ID: "A51_VERY_HIGH", Priority: 51, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.planApproved && c.mg >= c.plan.VeryHighMgDL },
		Then: Outcome{UrgencyAction, ActionFollowVeryHighPlan, "VERY_HIGH_PLAN_001", ReasonVeryHigh, Escalation{}, RecheckHighInterval},
	},
	{
		ID: "A52_HIGH", Priority: 52, Scope: ScopeAdult,
		When: func(c *evalContext) bool { return c.usable() && c.planApproved && c.mg >= c.plan.HighMgDL },
		Then: Outcome{UrgencyMonitor, ActionFollowHighPlan, "HIGH_PLAN_001", ReasonHigh, Escalation{}, RecheckHighInterval},
	},

	// ---- Tier 6: informational --------------------------------------------
	{
		ID: "U60_NO_APPROVED_PLAN", Priority: 60, Scope: ScopeUniversal,
		When: func(c *evalContext) bool { return !c.planApproved },
		Then: Outcome{UrgencyInfo, ActionSetUpCarePlan, "NO_PLAN_001", ReasonNoApprovedPlan, Escalation{}, RecheckNone},
	},
	{
		ID: "A61_IN_RANGE", Priority: 61, Scope: ScopeAdult,
		When: func(c *evalContext) bool {
			return c.usable() && c.mg >= c.plan.LowMgDL && c.mg < c.plan.HighMgDL
		},
		Then: Outcome{UrgencyInfo, ActionInRange, "IN_RANGE_001", ReasonInRange, Escalation{}, RecheckNone},
	},
}
