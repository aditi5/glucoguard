package clinical

import (
	"fmt"
	"sort"
)

// Evaluate runs the rules table against one reading. It is pure and
// deterministic: the same input always gives the same decision.
//
// Resolution:
//  1. Every rule in scope is evaluated.
//  2. The primary rule is the match with the highest urgency, with ties broken
//     by priority. The primary rule sets the action code and message.
//  3. Reason codes and escalations from all matches are merged. Escalations
//     only ever turn on, so no rule can suppress another rule's safety signal.
func Evaluate(in Input) (Decision, error) {
	if err := ValidateInput(in); err != nil {
		return Decision{}, err
	}
	c, err := newContext(in)
	if err != nil {
		return Decision{}, err
	}

	var matched []Rule
	for _, r := range Rules {
		if r.Scope == ScopeAdult && !c.adultScope {
			continue
		}
		if r.When(c) {
			matched = append(matched, r)
		}
	}
	if len(matched) == 0 {
		// Fail safe: an unmatched input is a gap in the rules table. Show the
		// patient the care-team route rather than nothing.
		return Decision{}, fmt.Errorf("no rule matched input (mg=%.1f); rules table has a gap", c.mg)
	}

	sort.SliceStable(matched, func(i, j int) bool {
		if matched[i].Then.Urgency != matched[j].Then.Urgency {
			return matched[i].Then.Urgency > matched[j].Then.Urgency
		}
		return matched[i].Priority < matched[j].Priority
	})
	primary := matched[0]

	d := Decision{
		Urgency:           primary.Then.Urgency,
		ActionCode:        primary.Then.Action,
		ApprovedMessageID: primary.Then.MessageID,
		CarePlanVersion:   c.plan.Version,
		CarePlanApproved:  c.planApproved,
		RuleSetVersion:    RuleSetVersion,
		ValueMgDL:         c.mg,
		DataStatus:        DataCurrent,
	}
	switch {
	case c.future:
		d.DataStatus = DataInvalid
	case c.stale:
		d.DataStatus = DataStale
	}

	// Report matches in priority order so reason codes read most-severe first.
	byPriority := append([]Rule(nil), matched...)
	sort.SliceStable(byPriority, func(i, j int) bool { return byPriority[i].Priority < byPriority[j].Priority })
	seen := map[ReasonCode]bool{}
	for _, r := range byPriority {
		d.MatchedRules = append(d.MatchedRules, r.ID)
		if !seen[r.Then.Reason] {
			seen[r.Then.Reason] = true
			d.ReasonCodes = append(d.ReasonCodes, r.Then.Reason)
		}
		e := r.Then.Escalation
		d.Escalation.CallEmergencyServices = d.Escalation.CallEmergencyServices || e.CallEmergencyServices
		d.Escalation.ContactCareTeam = d.Escalation.ContactCareTeam || e.ContactCareTeam
		d.Escalation.NotifyCaregiver = d.Escalation.NotifyCaregiver || e.NotifyCaregiver
	}
	if d.Escalation.CallEmergencyServices {
		d.Escalation.EmergencyNumber = EmergencyNumberIN
	}

	// Follow-up comes from the primary rule. If it has none, use the first
	// matched rule that does. Emergencies get no recheck: the instruction is
	// to get help, not to take another reading.
	if d.Urgency != UrgencyEmergency {
		rc := primary.Then.Recheck
		if rc == RecheckNone {
			for _, r := range byPriority {
				if r.Then.Recheck != RecheckNone {
					rc = r.Then.Recheck
					break
				}
			}
		}
		d.FollowUp = c.followUp(rc)
	}

	switch d.ActionCode {
	case ActionTreatLow, ActionTreatSevereLow, ActionPersistentLowSeekHelp:
		d.LowTreatmentCycle = 1
		if c.priorWasLowTreatment() {
			d.LowTreatmentCycle = in.Prior.Cycle + 1
		}
	}

	if _, ok := Catalog[d.ApprovedMessageID]; !ok {
		return Decision{}, fmt.Errorf("rule %s references unknown message %q", primary.ID, d.ApprovedMessageID)
	}
	return d, nil
}

func newContext(in Input) (*evalContext, error) {
	c := &evalContext{in: in, symptoms: map[Symptom]bool{}}
	for _, s := range in.Symptoms {
		c.symptoms[s] = true
	}

	switch in.Reading.MeterFlag {
	case MeterFlagLO:
		c.mg = MeterMinMgDL - 1 // below the meter floor: treat as severely low
	case MeterFlagHI:
		c.mg = MeterMaxMgDL + 1
	default:
		mg, err := ToMgDL(in.Reading.Value, in.Reading.Unit)
		if err != nil {
			return nil, err
		}
		c.mg = mg
	}

	if in.Plan != nil {
		c.plan = *in.Plan
		c.planApproved = in.Plan.ClinicianApproved
	} else {
		c.plan = DefaultUnapprovedPlan()
	}
	c.adultScope = in.Patient.InAdultScope()

	age := in.Now.Sub(in.Reading.MeasuredAt)
	c.future = age < -futureTolerance
	c.stale = !c.future && age > c.plan.StaleAfter
	return c, nil
}

func (c *evalContext) followUp(r Recheck) FollowUp {
	switch r {
	case RecheckNow:
		return FollowUp{Required: true, RecheckAfterMinutes: 0}
	case RecheckLowInterval:
		return FollowUp{Required: true, RecheckAfterMinutes: int(c.plan.LowRecheckAfter.Minutes())}
	case RecheckHighInterval:
		return FollowUp{Required: true, RecheckAfterMinutes: int(c.plan.HighRecheckAfter.Minutes())}
	}
	return FollowUp{}
}
