//go:build js && wasm

// Command wasm exposes the clinical safety engine to the browser, so the
// static web app runs exactly the same rules (and the same tested code) as
// the server. Build:
//
//	GOOS=js GOARCH=wasm go build -o web/engine.wasm ./cmd/wasm
package main

import (
	"encoding/json"
	"syscall/js"
	"time"

	"github.com/purplestack/glucoguard/internal/clinical"
)

// planJSON mirrors clinical.CarePlan with durations in minutes.
type planJSON struct {
	Version                 string  `json:"version"`
	LowMgDL                 float64 `json:"low_mg_dl"`
	SevereLowMgDL           float64 `json:"severe_low_mg_dl"`
	HighMgDL                float64 `json:"high_mg_dl"`
	VeryHighMgDL            float64 `json:"very_high_mg_dl"`
	StaleAfterMinutes       int     `json:"stale_after_minutes"`
	LowRecheckAfterMinutes  int     `json:"low_recheck_after_minutes"`
	HighRecheckAfterMinutes int     `json:"high_recheck_after_minutes"`
	MaxLowTreatmentCycles   int     `json:"max_low_treatment_cycles"`
	ClinicianApproved       bool    `json:"clinician_approved"`
	ApprovedBy              string  `json:"approved_by"`
}

type inputJSON struct {
	Reading  clinical.Reading     `json:"reading"`
	Symptoms []clinical.Symptom   `json:"symptoms"`
	Patient  clinical.Patient     `json:"patient"`
	Plan     *planJSON            `json:"plan"`
	Prior    *clinical.PriorEvent `json:"prior"`
	Now      time.Time            `json:"now"`
}

func result(v any, err error) any {
	if err != nil {
		return map[string]any{"error": err.Error()}
	}
	b, _ := json.Marshal(v)
	return map[string]any{"ok": string(b)}
}

func evaluate(_ js.Value, args []js.Value) any {
	if len(args) != 1 {
		return map[string]any{"error": "evaluate expects one JSON string"}
	}
	var in inputJSON
	if err := json.Unmarshal([]byte(args[0].String()), &in); err != nil {
		return map[string]any{"error": "bad input: " + err.Error()}
	}
	ci := clinical.Input{Reading: in.Reading, Symptoms: in.Symptoms, Patient: in.Patient, Prior: in.Prior, Now: in.Now}
	if in.Plan != nil {
		p := in.Plan
		ci.Plan = &clinical.CarePlan{
			Version: p.Version, LowMgDL: p.LowMgDL, SevereLowMgDL: p.SevereLowMgDL, HighMgDL: p.HighMgDL,
			VeryHighMgDL: p.VeryHighMgDL, MaxLowTreatmentCycles: p.MaxLowTreatmentCycles,
			StaleAfter:        time.Duration(p.StaleAfterMinutes) * time.Minute,
			LowRecheckAfter:   time.Duration(p.LowRecheckAfterMinutes) * time.Minute,
			HighRecheckAfter:  time.Duration(p.HighRecheckAfterMinutes) * time.Minute,
			ClinicianApproved: p.ClinicianApproved, ApprovedBy: p.ApprovedBy,
		}
	}
	return result(clinical.Evaluate(ci))
}

func validatePlan(_ js.Value, args []js.Value) any {
	var p planJSON
	if err := json.Unmarshal([]byte(args[0].String()), &p); err != nil {
		return map[string]any{"error": err.Error()}
	}
	cp := clinical.CarePlan{Version: p.Version, LowMgDL: p.LowMgDL, SevereLowMgDL: p.SevereLowMgDL, HighMgDL: p.HighMgDL,
		VeryHighMgDL: p.VeryHighMgDL, MaxLowTreatmentCycles: p.MaxLowTreatmentCycles,
		StaleAfter:        time.Duration(p.StaleAfterMinutes) * time.Minute,
		LowRecheckAfter:   time.Duration(p.LowRecheckAfterMinutes) * time.Minute,
		HighRecheckAfter:  time.Duration(p.HighRecheckAfterMinutes) * time.Minute,
		ClinicianApproved: p.ClinicianApproved, ApprovedBy: p.ApprovedBy}
	return result("ok", cp.Validate())
}

func meta(js.Value, []js.Value) any {
	d := clinical.DefaultUnapprovedPlan()
	return result(map[string]any{
		"rule_set_version":             clinical.RuleSetVersion,
		"rule_set_clinically_approved": clinical.RuleSetClinicallyApproved,
		"emergency_number":             clinical.EmergencyNumberIN,
		"messages":                     clinical.Catalog,
		"default_plan": map[string]any{"low_mg_dl": d.LowMgDL, "severe_low_mg_dl": d.SevereLowMgDL,
			"high_mg_dl": d.HighMgDL, "very_high_mg_dl": d.VeryHighMgDL},
	}, nil)
}

func main() {
	js.Global().Set("glucoguard", js.ValueOf(map[string]any{
		"evaluate":     js.FuncOf(evaluate),
		"validatePlan": js.FuncOf(validatePlan),
		"meta":         js.FuncOf(meta),
	}))
	js.Global().Call("dispatchEvent", js.Global().Get("Event").New("glucoguard-ready"))
	select {}
}
