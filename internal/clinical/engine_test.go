package clinical

import (
	"math/rand"
	"slices"
	"testing"
	"time"
)

var now = time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)

func approvedPlan() *CarePlan {
	return &CarePlan{
		Version: "plan-v3", LowMgDL: 70, SevereLowMgDL: 54, HighMgDL: 180, VeryHighMgDL: 300,
		StaleAfter: 15 * time.Minute, LowRecheckAfter: 15 * time.Minute, HighRecheckAfter: 2 * time.Hour,
		MaxLowTreatmentCycles: 2, ClinicianApproved: true, ApprovedBy: "dr-test",
	}
}

var adult = Patient{AgeGroup: AgeGroupAdult, DiabetesType: DiabetesType1}

func reading(mg float64, ago time.Duration) Reading {
	return Reading{Value: mg, Unit: UnitMgDL, Source: SourceManualMeter, MeasuredAt: now.Add(-ago), ReceivedAt: now}
}

func in(r Reading, p Patient, plan *CarePlan, sym ...Symptom) Input {
	return Input{Reading: r, Patient: p, Plan: plan, Symptoms: sym, Now: now}
}

// coveredRules records which rules the table tests exercised as primary or
// secondary matches. TestEveryRuleIsCovered fails if any rule is never hit.
var coveredRules = map[string]bool{}

func TestClinicalScenarios(t *testing.T) {
	pregnant := Patient{AgeGroup: AgeGroupAdult, DiabetesType: DiabetesType1, Pregnant: true}
	child := Patient{AgeGroup: AgeGroupChild, DiabetesType: DiabetesType1}
	priorLow := func(cycle int) *PriorEvent {
		return &PriorEvent{ActionCode: ActionTreatLow, Cycle: cycle, ValueMgDL: 62, MeasuredAt: now.Add(-16 * time.Minute)}
	}
	withPrior := func(i Input, p *PriorEvent) Input { i.Prior = p; return i }
	mmol := func(v float64) Reading {
		r := reading(0, time.Minute)
		r.Value, r.Unit = v, UnitMmolL
		return r
	}
	flag := func(f MeterFlag) Reading { r := reading(0, time.Minute); r.MeterFlag = f; return r }

	cases := []struct {
		name        string
		input       Input
		urgency     Urgency
		action      ActionCode
		mustReasons []ReasonCode
		callEMS     bool
		recheck     int // -1 = no follow-up required
		cycle       int
	}{
		// Emergencies
		{"unconscious overrides normal reading", in(reading(110, time.Minute), adult, approvedPlan(), SymptomUnconscious),
			UrgencyEmergency, ActionEmergencyNoOral, []ReasonCode{ReasonNoOralSymptoms}, true, -1, 0},
		{"seizure with low: no oral treatment", in(reading(40, time.Minute), adult, approvedPlan(), SymptomSeizure),
			UrgencyEmergency, ActionEmergencyNoOral, []ReasonCode{ReasonNoOralSymptoms, ReasonSevereLow}, true, -1, 0},
		{"unable to swallow with stale data still emergency", in(reading(65, 3*time.Hour), adult, approvedPlan(), SymptomUnableToSwallow),
			UrgencyEmergency, ActionEmergencyNoOral, []ReasonCode{ReasonNoOralSymptoms, ReasonStaleLastLow}, true, -1, 0},
		{"no-oral emergency applies to children too", in(reading(45, time.Minute), child, approvedPlan(), SymptomUnconscious),
			UrgencyEmergency, ActionEmergencyNoOral, []ReasonCode{ReasonNoOralSymptoms, ReasonOutOfAdultScope}, true, -1, 0},
		{"high with vomiting is an emergency", in(reading(320, time.Minute), adult, approvedPlan(), SymptomVomiting),
			UrgencyEmergency, ActionEmergencyAssessment, []ReasonCode{ReasonHighWithRedFlags, ReasonVeryHigh}, true, -1, 0},
		{"high with difficulty breathing, no plan", in(reading(260, time.Minute), adult, nil, SymptomDifficultyBreathing),
			UrgencyEmergency, ActionEmergencyAssessment, []ReasonCode{ReasonHighWithRedFlags, ReasonNoApprovedPlan}, true, -1, 0},
		{"red flag symptoms with normal reading are urgent", in(reading(120, time.Minute), adult, approvedPlan(), SymptomAbdominalPain),
			UrgencyUrgent, ActionSeekUrgentCare, []ReasonCode{ReasonRedFlagSymptoms}, false, -1, 0},
		{"red flag with stale data does not wait for a reading", in(reading(200, time.Hour), adult, approvedPlan(), SymptomConfusion),
			UrgencyUrgent, ActionSeekUrgentCare, []ReasonCode{ReasonRedFlagSymptoms, ReasonStaleData}, false, 0, 0},

		// Hypoglycemia
		{"69 is low", in(reading(69, time.Minute), adult, approvedPlan()),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow}, false, 15, 1},
		{"70 is not low", in(reading(70, time.Minute), adult, approvedPlan()),
			UrgencyInfo, ActionInRange, []ReasonCode{ReasonInRange}, false, -1, 0},
		{"53 is severe low and notifies caregiver", in(reading(53, time.Minute), adult, approvedPlan()),
			UrgencyUrgent, ActionTreatSevereLow, []ReasonCode{ReasonSevereLow, ReasonLow}, false, 15, 1},
		{"54 is low, not severe", in(reading(54, time.Minute), adult, approvedPlan()),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow}, false, 15, 1},
		{"low with confusion escalates", in(reading(62, time.Minute), adult, approvedPlan(), SymptomConfusion),
			UrgencyUrgent, ActionTreatSevereLow, []ReasonCode{ReasonLowWithConfusion, ReasonLow}, false, 15, 1},
		{"low with vomiting routes to urgent care", in(reading(62, time.Minute), adult, approvedPlan(), SymptomVomiting),
			UrgencyUrgent, ActionSeekUrgentCare, []ReasonCode{ReasonRedFlagSymptoms, ReasonLow}, false, 15, 0},
		{"still low after one cycle: treat again, cycle 2", withPrior(in(reading(64, time.Minute), adult, approvedPlan()), priorLow(1)),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow}, false, 15, 2},
		{"still low after max cycles: seek help", withPrior(in(reading(64, time.Minute), adult, approvedPlan()), priorLow(2)),
			UrgencyUrgent, ActionPersistentLowSeekHelp, []ReasonCode{ReasonPersistentLow, ReasonLow}, false, 15, 3},
		{"old prior low does not continue the cycle",
			withPrior(in(reading(64, time.Minute), adult, approvedPlan()),
				&PriorEvent{ActionCode: ActionTreatLow, Cycle: 2, ValueMgDL: 60, MeasuredAt: now.Add(-3 * time.Hour)}),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow}, false, 15, 1},
		{"no plan still uses ADA low thresholds", in(reading(60, time.Minute), adult, nil),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow, ReasonNoApprovedPlan}, false, 15, 1},

		// Units and meter limits
		{"mmol/L 3.5 is low", in(mmol(3.5), adult, approvedPlan()),
			UrgencyAction, ActionTreatLow, []ReasonCode{ReasonLow}, false, 15, 1},
		{"mmol/L 2.8 is severe low", in(mmol(2.8), adult, approvedPlan()),
			UrgencyUrgent, ActionTreatSevereLow, []ReasonCode{ReasonSevereLow}, false, 15, 1},
		{"meter LO is treated as severe low and flagged", in(flag(MeterFlagLO), adult, approvedPlan()),
			UrgencyUrgent, ActionTreatSevereLow, []ReasonCode{ReasonSevereLow, ReasonMeterLimit}, false, 15, 1},
		{"meter HI must be verified", in(flag(MeterFlagHI), adult, approvedPlan()),
			UrgencyUrgent, ActionVerifyReading, []ReasonCode{ReasonVeryHigh, ReasonMeterLimit}, false, 0, 0},
		{"likely unit mix-up (5.5 mg/dL) is never dismissed", in(reading(5.5, time.Minute), adult, approvedPlan()),
			UrgencyUrgent, ActionTreatSevereLow, []ReasonCode{ReasonSevereLow, ReasonMeterLimit}, false, 15, 1},

		// Data integrity
		{"stale normal reading is not presented as current", in(reading(110, 40*time.Minute), adult, approvedPlan()),
			UrgencyAction, ActionCheckNowStale, []ReasonCode{ReasonStaleData}, false, 0, 0},
		{"stale low reading is urgent", in(reading(58, 40*time.Minute), adult, approvedPlan()),
			UrgencyUrgent, ActionCheckNowStale, []ReasonCode{ReasonStaleLastLow}, false, 0, 0},
		{"future timestamp is invalid", in(reading(110, -2*time.Hour), adult, approvedPlan()),
			UrgencyAction, ActionVerifyDeviceTime, []ReasonCode{ReasonFutureTimestamp}, false, 0, 0},
		{"small clock skew is tolerated", in(reading(110, -2*time.Minute), adult, approvedPlan()),
			UrgencyInfo, ActionInRange, []ReasonCode{ReasonInRange}, false, -1, 0},
		{"rapid change asks for verification",
			withPrior(in(reading(260, time.Minute), adult, approvedPlan()),
				&PriorEvent{ActionCode: ActionInRange, ValueMgDL: 120, MeasuredAt: now.Add(-11 * time.Minute)}),
			UrgencyAction, ActionVerifyReading, []ReasonCode{ReasonRapidChange, ReasonHigh}, false, 0, 0},
		{"hypo symptoms with normal reading: recheck", in(reading(95, time.Minute), adult, approvedPlan(), SymptomShaky, SymptomSweating),
			UrgencyAction, ActionRecheckSymptoms, []ReasonCode{ReasonSymptomConflict, ReasonInRange}, false, 0, 0},

		// Hyperglycemia
		{"high with approved plan", in(reading(220, time.Minute), adult, approvedPlan()),
			UrgencyMonitor, ActionFollowHighPlan, []ReasonCode{ReasonHigh}, false, 120, 0},
		{"very high with approved plan", in(reading(350, time.Minute), adult, approvedPlan()),
			UrgencyAction, ActionFollowVeryHighPlan, []ReasonCode{ReasonVeryHigh, ReasonHigh}, false, 120, 0},
		{"high without approved plan: contact care team", in(reading(270, time.Minute), adult, nil),
			UrgencyAction, ActionHighContactCareTeam, []ReasonCode{ReasonHigh, ReasonNoApprovedPlan}, false, 120, 0},

		// Special populations never get the adult pathway
		{"pregnant low routes to personal plan", in(reading(62, time.Minute), pregnant, approvedPlan()),
			UrgencyUrgent, ActionSpecialContactCareTeam, []ReasonCode{ReasonOutOfAdultScope}, false, -1, 0},
		{"child in range is saved and routed", in(reading(110, time.Minute), child, approvedPlan()),
			UrgencyInfo, ActionSpecialRouteProtocol, []ReasonCode{ReasonOutOfAdultScope}, false, -1, 0},
		{"child with low and confusion is still urgent", in(reading(60, time.Minute), child, approvedPlan(), SymptomConfusion),
			UrgencyUrgent, ActionSeekUrgentCare, []ReasonCode{ReasonRedFlagSymptoms, ReasonOutOfAdultScope}, false, -1, 0},

		// Informational
		{"in range, approved plan", in(reading(120, time.Minute), adult, approvedPlan()),
			UrgencyInfo, ActionInRange, []ReasonCode{ReasonInRange}, false, -1, 0},
		{"in range, no plan prompts set-up", in(reading(120, time.Minute), adult, nil),
			UrgencyInfo, ActionSetUpCarePlan, []ReasonCode{ReasonNoApprovedPlan, ReasonInRange}, false, -1, 0},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d, err := Evaluate(tc.input)
			if err != nil {
				t.Fatalf("Evaluate: %v", err)
			}
			for _, r := range d.MatchedRules {
				coveredRules[r] = true
			}
			if d.Urgency != tc.urgency {
				t.Errorf("urgency = %s, want %s (rules %v)", d.Urgency, tc.urgency, d.MatchedRules)
			}
			if d.ActionCode != tc.action {
				t.Errorf("action = %s, want %s (rules %v)", d.ActionCode, tc.action, d.MatchedRules)
			}
			for _, r := range tc.mustReasons {
				if !slices.Contains(d.ReasonCodes, r) {
					t.Errorf("missing reason %s; got %v", r, d.ReasonCodes)
				}
			}
			if d.Escalation.CallEmergencyServices != tc.callEMS {
				t.Errorf("call EMS = %v, want %v", d.Escalation.CallEmergencyServices, tc.callEMS)
			}
			if tc.callEMS && d.Escalation.EmergencyNumber != EmergencyNumberIN {
				t.Errorf("emergency number = %q", d.Escalation.EmergencyNumber)
			}
			if tc.recheck < 0 && d.FollowUp.Required {
				t.Errorf("unexpected follow-up %+v", d.FollowUp)
			}
			if tc.recheck >= 0 && (!d.FollowUp.Required || d.FollowUp.RecheckAfterMinutes != tc.recheck) {
				t.Errorf("follow-up = %+v, want recheck in %d min", d.FollowUp, tc.recheck)
			}
			if d.LowTreatmentCycle != tc.cycle {
				t.Errorf("low cycle = %d, want %d", d.LowTreatmentCycle, tc.cycle)
			}
			if d.RuleSetVersion != RuleSetVersion {
				t.Errorf("rule set version missing")
			}
		})
	}
}

func TestEveryRuleIsCovered(t *testing.T) {
	t.Run("scenarios", TestClinicalScenarios)
	for _, r := range Rules {
		if !coveredRules[r.ID] {
			t.Errorf("rule %s is not exercised by any scenario", r.ID)
		}
	}
}

func TestRulesTableIntegrity(t *testing.T) {
	ids := map[string]bool{}
	prios := map[int]string{}
	for _, r := range Rules {
		if ids[r.ID] {
			t.Errorf("duplicate rule id %s", r.ID)
		}
		ids[r.ID] = true
		if other, ok := prios[r.Priority]; ok {
			t.Errorf("rules %s and %s share priority %d", r.ID, other, r.Priority)
		}
		prios[r.Priority] = r.ID
		if _, ok := Catalog[r.Then.MessageID]; !ok {
			t.Errorf("rule %s references unknown message %s", r.ID, r.Then.MessageID)
		}
		if r.Then.Urgency == UrgencyEmergency && !r.Then.Escalation.CallEmergencyServices {
			t.Errorf("emergency rule %s must call emergency services", r.ID)
		}
	}
}

// TestSafetyInvariants fuzzes random inputs and checks properties that must
// hold for every input, not just the hand-picked scenarios.
func TestSafetyInvariants(t *testing.T) {
	rng := rand.New(rand.NewSource(42))
	allSymptoms := make([]Symptom, 0, len(knownSymptoms))
	for s := range knownSymptoms {
		allSymptoms = append(allSymptoms, s)
	}
	slices.Sort(allSymptoms)
	patients := []Patient{
		adult,
		{AgeGroup: AgeGroupOlderAdult, DiabetesType: DiabetesType2},
		{AgeGroup: AgeGroupChild, DiabetesType: DiabetesType1},
		{AgeGroup: AgeGroupAdult, DiabetesType: DiabetesTypeGestational},
	}

	for i := 0; i < 50000; i++ {
		var sym []Symptom
		for _, s := range allSymptoms {
			if rng.Float64() < 0.08 {
				sym = append(sym, s)
			}
		}
		var plan *CarePlan
		if rng.Intn(3) > 0 {
			plan = approvedPlan()
		}
		r := reading(float64(5+rng.Intn(700)), time.Duration(rng.Intn(120)-10)*time.Minute)
		input := in(r, patients[rng.Intn(len(patients))], plan, sym...)
		if rng.Intn(4) == 0 {
			input.Prior = &PriorEvent{ActionCode: ActionTreatLow, Cycle: rng.Intn(4), ValueMgDL: float64(40 + rng.Intn(300)),
				MeasuredAt: r.MeasuredAt.Add(-time.Duration(rng.Intn(90)) * time.Minute)}
		}

		d, err := Evaluate(input)
		if err != nil {
			t.Fatalf("case %d: unexpected error %v for %+v", i, err, input)
		}
		set := map[Symptom]bool{}
		for _, s := range sym {
			set[s] = true
		}
		if set[SymptomUnconscious] || set[SymptomSeizure] || set[SymptomUnableToSwallow] {
			if d.ActionCode != ActionEmergencyNoOral || !d.Escalation.CallEmergencyServices {
				t.Fatalf("case %d: no-oral symptom did not produce emergency: %+v", i, d)
			}
		}
		if d.ActionCode == ActionTreatLow || d.ActionCode == ActionTreatSevereLow {
			if set[SymptomUnconscious] || set[SymptomSeizure] || set[SymptomUnableToSwallow] {
				t.Fatalf("case %d: oral treatment suggested with no-oral symptom", i)
			}
		}
		if d.DataStatus == DataCurrent && d.ValueMgDL < 70 && d.Urgency < UrgencyAction {
			t.Fatalf("case %d: current low reading got urgency %s", i, d.Urgency)
		}
		if d.DataStatus != DataCurrent && d.ActionCode == ActionInRange {
			t.Fatalf("case %d: stale/invalid data presented as in range", i)
		}
		if !input.Patient.InAdultScope() {
			switch d.ActionCode {
			case ActionTreatLow, ActionTreatSevereLow, ActionFollowHighPlan, ActionFollowVeryHighPlan, ActionInRange:
				t.Fatalf("case %d: adult pathway %s applied to out-of-scope patient", i, d.ActionCode)
			}
		}
		if d.Urgency == UrgencyEmergency && d.FollowUp.Required {
			t.Fatalf("case %d: emergency should not ask for a recheck", i)
		}
	}
}

func TestDeterministic(t *testing.T) {
	i := in(reading(63, time.Minute), adult, approvedPlan(), SymptomShaky)
	a, _ := Evaluate(i)
	for n := 0; n < 100; n++ {
		b, _ := Evaluate(i)
		if a.ActionCode != b.ActionCode || !slices.Equal(a.ReasonCodes, b.ReasonCodes) {
			t.Fatal("non-deterministic evaluation")
		}
	}
}

func TestValidation(t *testing.T) {
	bad := []Input{
		in(Reading{Value: 100, Unit: "mg", Source: SourceManualMeter, MeasuredAt: now}, adult, nil),
		in(Reading{Value: -4, Unit: UnitMgDL, Source: SourceManualMeter, MeasuredAt: now}, adult, nil),
		in(Reading{Value: 100, Unit: UnitMgDL, Source: SourceManualMeter}, adult, nil),
		in(Reading{Value: 120, Unit: UnitMmolL, Source: SourceManualMeter, MeasuredAt: now}, adult, nil), // 2162 mg/dL
		in(reading(100, 0), adult, nil, "itchy"),
		in(reading(100, 0), adult, &CarePlan{Version: "x", LowMgDL: 90, SevereLowMgDL: 100, HighMgDL: 180, VeryHighMgDL: 300,
			StaleAfter: time.Minute, LowRecheckAfter: time.Minute, HighRecheckAfter: time.Minute, MaxLowTreatmentCycles: 1}),
	}
	for i, b := range bad {
		if _, err := Evaluate(b); err == nil {
			t.Errorf("case %d: expected validation error", i)
		}
	}
}

func TestUnitConversion(t *testing.T) {
	for _, c := range []struct {
		v    float64
		u    Unit
		want float64
	}{{5.5, UnitMmolL, 99.1}, {3.9, UnitMmolL, 70.3}, {3.0, UnitMmolL, 54.1}, {100, UnitMgDL, 100}} {
		got, err := ToMgDL(c.v, c.u)
		if err != nil || got != c.want {
			t.Errorf("ToMgDL(%v,%s) = %v,%v want %v", c.v, c.u, got, err, c.want)
		}
	}
}
