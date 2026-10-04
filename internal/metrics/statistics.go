package metrics

import "math"

const confidenceZ = 1.959963984540054
const sampleRatioThreshold = 0.0005

type Interval struct {
	Method string  `json:"method"`
	Level  float64 `json:"level"`
	Lower  float64 `json:"lower"`
	Upper  float64 `json:"upper"`
}
type Proportion struct {
	Status             string    `json:"status"`
	Rate               *float64  `json:"rate"`
	ConfidenceInterval *Interval `json:"confidence_interval"`
}
type SampleRatio struct {
	Status           string   `json:"status"`
	Statistic        *float64 `json:"statistic"`
	DegreesOfFreedom int      `json:"degrees_of_freedom"`
	PValue           *float64 `json:"p_value"`
	WarningThreshold float64  `json:"warning_threshold"`
}
type SampleRatios struct {
	Provisional SampleRatio `json:"provisional"`
	Finalized   SampleRatio `json:"finalized"`
	Total       SampleRatio `json:"total"`
}
type ComparisonResult struct {
	Status             string   `json:"status"`
	Reason             string   `json:"reason,omitempty"`
	AbsoluteLift       *float64 `json:"absolute_lift"`
	RelativeLift       *float64 `json:"relative_lift"`
	RelativeLiftReason string   `json:"relative_lift_reason,omitempty"`
	ZStatistic         *float64 `json:"z_statistic"`
	PValue             *float64 `json:"p_value"`
}
type Comparison struct {
	ControlVariantID string           `json:"control_variant_id"`
	VariantID        string           `json:"variant_id"`
	Provisional      ComparisonResult `json:"provisional"`
	Finalized        ComparisonResult `json:"finalized"`
	Total            ComparisonResult `json:"total"`
}

func validCounts(c Counts) bool {
	return c.Exposed >= 0 && c.Converted >= 0 && c.Converted <= c.Exposed
}
func wilson(c Counts, z float64) (float64, float64) {
	n := float64(c.Exposed)
	p := float64(c.Converted) / n
	z2 := z * z
	center := (p + z2/(2*n)) / (1 + z2/n)
	half := z * math.Sqrt(p*(1-p)/n+z2/(4*n*n)) / (1 + z2/n)
	lower, upper := math.Max(0, center-half), math.Min(1, center+half)
	// Preserve exact endpoint coverage despite floating-point cancellation.
	if c.Converted == 0 {
		lower = 0
	}
	if c.Converted == c.Exposed {
		upper = 1
	}
	return lower, upper
}
func proportion(c Counts) Proportion {
	if !validCounts(c) {
		return Proportion{Status: "invalid_counts"}
	}
	if c.Exposed == 0 {
		return Proportion{Status: "insufficient_data"}
	}
	rate := float64(c.Converted) / float64(c.Exposed)
	lower, upper := wilson(c, confidenceZ)
	return Proportion{Status: "available", Rate: &rate, ConfidenceInterval: &Interval{Method: "wilson", Level: 0.95, Lower: lower, Upper: upper}}
}
func compare(control, treatment Counts, srm SampleRatio) ComparisonResult {
	result := ComparisonResult{Status: "insufficient_data", Reason: "no_exposures"}
	if !validCounts(control) || !validCounts(treatment) {
		result.Status = "invalid_counts"
		return result
	}
	if control.Exposed == 0 || treatment.Exposed == 0 {
		return result
	}
	p1 := float64(control.Converted) / float64(control.Exposed)
	p2 := float64(treatment.Converted) / float64(treatment.Exposed)
	lift := p2 - p1
	result.AbsoluteLift = &lift
	if p1 > 0 {
		relative := lift / p1
		result.RelativeLift = &relative
	} else {
		result.RelativeLiftReason = "zero_control_rate"
	}
	if srm.Status == "mismatch" {
		result.Status = "data_quality_warning"
		result.Reason = "sample_ratio_mismatch"
		return result
	}
	// Require at least five observed successes/failures per arm, and five expected
	// in each cell under the pooled null. Small samples receive no asymptotic p-value.
	n1, n2 := float64(control.Exposed), float64(treatment.Exposed)
	pooled := (float64(control.Converted) + float64(treatment.Converted)) / (n1 + n2)
	if control.Converted < 5 || treatment.Converted < 5 || control.Exposed-control.Converted < 5 || treatment.Exposed-treatment.Converted < 5 || n1*pooled < 5 || n2*pooled < 5 || n1*(1-pooled) < 5 || n2*(1-pooled) < 5 {
		result.Reason = "small_cell_counts"
		return result
	}
	z := lift / math.Sqrt(pooled*(1-pooled)*(1/n1+1/n2))
	p := math.Erfc(math.Abs(z) / math.Sqrt2)
	result.Status = "available"
	result.Reason = ""
	result.ZStatistic = &z
	result.PValue = &p
	return result
}

// Chi-square survival for integer degrees of freedom 1..9. Experiment runs have
// at most ten variants, so finite even/half-integer recurrences suffice.
func chiSquareTail(statistic float64, df int) float64 {
	if statistic == 0 {
		return 1
	}
	x := statistic / 2
	var tail float64
	if df%2 == 0 {
		term, sum := 1.0, 1.0
		for j := 1; j < df/2; j++ {
			term *= x / float64(j)
			sum += term
		}
		tail = math.Exp(-x) * sum
	} else {
		tail = math.Erfc(math.Sqrt(x))
		for a := 0.5; a < float64(df)/2; a++ {
			lgamma, _ := math.Lgamma(a + 1)
			tail += math.Exp(a*math.Log(x) - x - lgamma)
		}
	}
	return math.Max(0, math.Min(1, tail))
}
func sampleRatio(variants []Variant, cohort string) SampleRatio {
	result := SampleRatio{Status: "insufficient_data", DegreesOfFreedom: len(variants) - 1, WarningThreshold: sampleRatioThreshold}
	if len(variants) < 2 || len(variants) > 10 {
		return result
	}
	observed := func(v Variant) int64 {
		switch cohort {
		case "finalized":
			return v.Finalized.Exposed
		case "provisional":
			return v.Provisional.Exposed
		default:
			return v.Total.Exposed
		}
	}
	total := 0.0
	weights := 0
	for _, v := range variants {
		if observed(v) < 0 || v.WeightBP <= 0 {
			return result
		}
		total += float64(observed(v))
		weights += v.WeightBP
	}
	if total == 0 || weights != 10000 {
		return result
	}
	statistic := 0.0
	for _, v := range variants {
		expected := total * float64(v.WeightBP) / 10000
		if expected < 5 {
			return result
		}
		delta := float64(observed(v)) - expected
		statistic += delta * delta / expected
	}
	p := chiSquareTail(statistic, result.DegreesOfFreedom)
	result.Statistic = &statistic
	result.PValue = &p
	result.Status = "balanced"
	if p < sampleRatioThreshold {
		result.Status = "mismatch"
	}
	return result
}
func addStatistics(r *Results) {
	r.SampleRatio = SampleRatios{Provisional: sampleRatio(r.Variants, "provisional"), Finalized: sampleRatio(r.Variants, "finalized"), Total: sampleRatio(r.Variants, "total")}
	r.Comparisons = make([]Comparison, 0, len(r.Variants)-1)
	var control Variant
	for i := range r.Variants {
		v := &r.Variants[i]
		v.ProvisionalRate = proportion(v.Provisional)
		v.FinalizedRate = proportion(v.Finalized)
		v.TotalRate = proportion(v.Total)
		if v.ID == r.ControlVariantID {
			control = *v
		}
	}
	for _, v := range r.Variants {
		if v.ID != r.ControlVariantID {
			r.Comparisons = append(r.Comparisons, Comparison{ControlVariantID: control.ID, VariantID: v.ID, Provisional: compare(control.Provisional, v.Provisional, r.SampleRatio.Provisional), Finalized: compare(control.Finalized, v.Finalized, r.SampleRatio.Finalized), Total: compare(control.Total, v.Total, r.SampleRatio.Total)})
		}
	}
	r.InferenceNotes = []string{"Provisional conversions can change as attribution windows and late arrivals mature.", "P-values are descriptive fixed-analysis outputs; repeated dashboard checks are not a valid stopping rule.", "Pairwise p-values are unadjusted for multiple variants; they do not authorize rollout or rollback."}
}
