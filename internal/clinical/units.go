package clinical

import (
	"errors"
	"fmt"
	"math"
	"time"
)

// MgDLPerMmolL converts glucose units (molar mass of glucose 180.16 g/mol).
const MgDLPerMmolL = 18.0182

// Typical glucometer measuring range. Values outside it are clamped and flagged
// rather than rejected, so a dangerous reading is never dismissed.
const (
	MeterMinMgDL = 20.0
	MeterMaxMgDL = 600.0
)

// ToMgDL normalises a reading to mg/dL, rounded to one decimal place.
func ToMgDL(value float64, unit Unit) (float64, error) {
	switch unit {
	case UnitMgDL:
		return round1(value), nil
	case UnitMmolL:
		return round1(value * MgDLPerMmolL), nil
	default:
		return 0, fmt.Errorf("unsupported unit %q (use %q or %q)", unit, UnitMgDL, UnitMmolL)
	}
}

func round1(v float64) float64 { return math.Round(v*10) / 10 }

var ErrInvalidInput = errors.New("invalid input")

// ValidateInput checks structure only. It makes no clinical judgement.
func ValidateInput(in Input) error {
	r := in.Reading
	if r.Unit != UnitMgDL && r.Unit != UnitMmolL {
		return fmt.Errorf("%w: unit must be %q or %q", ErrInvalidInput, UnitMgDL, UnitMmolL)
	}
	if r.Source != SourceManualMeter && r.Source != SourceCGM {
		return fmt.Errorf("%w: unknown source %q", ErrInvalidInput, r.Source)
	}
	switch r.MeterFlag {
	case MeterFlagNone:
		if math.IsNaN(r.Value) || math.IsInf(r.Value, 0) || r.Value <= 0 {
			return fmt.Errorf("%w: value must be a positive number (use meter_flag LO/HI for out-of-range meters)", ErrInvalidInput)
		}
		if mg, _ := ToMgDL(r.Value, r.Unit); mg > 2000 {
			return fmt.Errorf("%w: value %.1f %s is not a plausible glucose reading; check the unit", ErrInvalidInput, r.Value, r.Unit)
		}
	case MeterFlagLO, MeterFlagHI:
	default:
		return fmt.Errorf("%w: meter_flag must be LO, HI or empty", ErrInvalidInput)
	}
	if r.MeasuredAt.IsZero() {
		return fmt.Errorf("%w: measured_at is required", ErrInvalidInput)
	}
	if in.Now.IsZero() {
		return fmt.Errorf("%w: evaluation time is required", ErrInvalidInput)
	}
	for _, s := range in.Symptoms {
		if !ValidSymptom(s) {
			return fmt.Errorf("%w: unknown symptom %q", ErrInvalidInput, s)
		}
	}
	if in.Plan != nil {
		if err := in.Plan.Validate(); err != nil {
			return fmt.Errorf("%w: care plan: %v", ErrInvalidInput, err)
		}
	}
	return nil
}

// futureTolerance absorbs small clock skew between device and server.
const futureTolerance = 5 * time.Minute
