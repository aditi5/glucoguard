package app

import (
	"context"
	"encoding/csv"
	"fmt"
	"io"
	"math"
	"strconv"
	"time"

	"github.com/purplestack/glucoguard/internal/clinical"
	"github.com/purplestack/glucoguard/internal/store"
)

type ReportStats struct {
	Readings          int     `json:"readings"`
	MeanMgDL          float64 `json:"mean_mg_dl"`
	MinMgDL           float64 `json:"min_mg_dl"`
	MaxMgDL           float64 `json:"max_mg_dl"`
	StdDevMgDL        float64 `json:"std_dev_mg_dl"`
	CoefficientOfVar  float64 `json:"coefficient_of_variation_pct"`
	TimeInRangePct    float64 `json:"readings_in_range_pct"`
	BelowLowPct       float64 `json:"readings_below_low_pct"`
	BelowSevereLowPct float64 `json:"readings_below_severe_low_pct"`
	AboveHighPct      float64 `json:"readings_above_high_pct"`
	LowEvents         int     `json:"low_events"`
	SevereLowEvents   int     `json:"severe_low_events"`
	UrgentOrWorse     int     `json:"urgent_or_emergency_evaluations"`
	RangeLowMgDL      float64 `json:"range_low_mg_dl"`
	RangeHighMgDL     float64 `json:"range_high_mg_dl"`
}

type ReportRow struct {
	Reading    store.Reading      `json:"reading"`
	Evaluation *clinical.Decision `json:"evaluation,omitempty"`
}

type Report struct {
	Patient        store.Patient    `json:"patient"`
	From           time.Time        `json:"from"`
	To             time.Time        `json:"to"`
	GeneratedAt    time.Time        `json:"generated_at"`
	CarePlan       *store.CarePlan  `json:"care_plan,omitempty"`
	RuleSetVersion string           `json:"rule_set_version"`
	Stats          ReportStats      `json:"stats"`
	Rows           []ReportRow      `json:"rows"`
	FollowUps      []store.FollowUp `json:"follow_ups"`
	Disclaimer     string           `json:"disclaimer"`
}

// GlucoseReport builds the clinician report. Percentages are per reading, not
// CGM time-in-range, because manual readings are not evenly spaced.
func (s *Service) GlucoseReport(ctx context.Context, actor Actor, patientID string, from, to time.Time) (Report, error) {
	if to.IsZero() {
		to = s.Now()
	}
	if from.IsZero() {
		from = to.AddDate(0, 0, -14)
	}
	if !from.Before(to) || to.Sub(from) > 366*24*time.Hour {
		return Report{}, invalid("report range must be positive and at most 1 year")
	}
	p, err := s.Store.GetPatient(ctx, patientID)
	if err != nil {
		return Report{}, err
	}
	plan, err := s.CurrentPlan(ctx, patientID)
	if err != nil {
		return Report{}, err
	}
	tr := store.TimeRange{From: from, To: to}
	readings, err := s.Store.ListReadings(ctx, patientID, tr)
	if err != nil {
		return Report{}, err
	}
	evals, err := s.Store.ListEvaluations(ctx, patientID, store.TimeRange{From: from})
	if err != nil {
		return Report{}, err
	}
	latest := map[string]*clinical.Decision{}
	for i := range evals { // evals are newest first; keep the first per reading
		if _, ok := latest[evals[i].ReadingID]; !ok {
			latest[evals[i].ReadingID] = &evals[i].Decision
		}
	}
	fus, err := s.Store.ListFollowUps(ctx, patientID, tr)
	if err != nil {
		return Report{}, err
	}

	low, severe, high := 70.0, 54.0, 180.0
	if plan != nil {
		low, severe, high = plan.LowMgDL, plan.SevereLowMgDL, plan.HighMgDL
	}
	rep := Report{Patient: p, From: from, To: to, GeneratedAt: s.Now(), CarePlan: plan,
		RuleSetVersion: clinical.RuleSetVersion, FollowUps: fus,
		Disclaimer: "Patient-entered and device data. Percentages are of readings, not continuous time in range."}
	if fus == nil {
		rep.FollowUps = []store.FollowUp{}
	}
	st := ReportStats{RangeLowMgDL: low, RangeHighMgDL: high}
	var sum, sumSq float64
	var inRange, below, belowSevere, above int
	for _, r := range readings {
		row := ReportRow{Reading: r, Evaluation: latest[r.ID]}
		rep.Rows = append(rep.Rows, row)
		v := r.ValueMgDL
		if st.Readings == 0 || v < st.MinMgDL {
			st.MinMgDL = v
		}
		if v > st.MaxMgDL {
			st.MaxMgDL = v
		}
		st.Readings++
		sum += v
		sumSq += v * v
		switch {
		case v < severe:
			belowSevere++
			below++
		case v < low:
			below++
		case v > high:
			above++
		default:
			inRange++
		}
		if d := row.Evaluation; d != nil {
			if d.Urgency >= clinical.UrgencyUrgent {
				st.UrgentOrWorse++
			}
			if d.LowTreatmentCycle == 1 {
				st.LowEvents++
				if d.ActionCode == clinical.ActionTreatSevereLow {
					st.SevereLowEvents++
				}
			}
		}
	}
	if n := float64(st.Readings); n > 0 {
		st.MeanMgDL = round1(sum / n)
		st.StdDevMgDL = round1(math.Sqrt(math.Max(0, sumSq/n-(sum/n)*(sum/n))))
		if st.MeanMgDL > 0 {
			st.CoefficientOfVar = round1(100 * st.StdDevMgDL / st.MeanMgDL)
		}
		st.TimeInRangePct = round1(100 * float64(inRange) / n)
		st.BelowLowPct = round1(100 * float64(below) / n)
		st.BelowSevereLowPct = round1(100 * float64(belowSevere) / n)
		st.AboveHighPct = round1(100 * float64(above) / n)
	}
	rep.Stats = st
	if rep.Rows == nil {
		rep.Rows = []ReportRow{}
	}
	s.audit(ctx, patientID, actor, "report.generated", "report", "",
		map[string]any{"from": from, "to": to, "readings": st.Readings})
	return rep, nil
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

// WriteCSV writes one row per reading for spreadsheet import.
func (r Report) WriteCSV(w io.Writer) error {
	cw := csv.NewWriter(w)
	_ = cw.Write([]string{"measured_at", "received_at", "value", "unit", "meter_flag", "value_mg_dl", "source",
		"meal_context", "symptoms", "urgency", "action_code", "reason_codes", "rule_set_version", "care_plan_version"})
	for _, row := range r.Rows {
		rd := row.Reading
		syms := ""
		for i, s := range rd.Symptoms {
			if i > 0 {
				syms += ";"
			}
			syms += string(s)
		}
		rec := []string{rd.MeasuredAt.Format(time.RFC3339), rd.ReceivedAt.Format(time.RFC3339),
			strconv.FormatFloat(rd.Value, 'f', -1, 64), string(rd.Unit), string(rd.MeterFlag),
			strconv.FormatFloat(rd.ValueMgDL, 'f', 1, 64), string(rd.Source), string(rd.MealContext), syms, "", "", "", "", ""}
		if d := row.Evaluation; d != nil {
			reasons := ""
			for i, rc := range d.ReasonCodes {
				if i > 0 {
					reasons += ";"
				}
				reasons += string(rc)
			}
			rec[9], rec[10], rec[11], rec[12], rec[13] = d.Urgency.String(), string(d.ActionCode), reasons, d.RuleSetVersion, d.CarePlanVersion
		}
		for i, f := range rec { // neutralise spreadsheet formula injection
			if f != "" && (f[0] == '=' || f[0] == '+' || f[0] == '-' || f[0] == '@') && i != 2 {
				rec[i] = "'" + f
			}
		}
		if err := cw.Write(rec); err != nil {
			return fmt.Errorf("write csv: %w", err)
		}
	}
	cw.Flush()
	return cw.Error()
}
