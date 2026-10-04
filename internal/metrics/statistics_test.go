package metrics

import (
	"encoding/json"
	"math"
	"testing"
)

func TestWilsonReferencesAndBounds(t *testing.T) {
	// Independent Python score-test root bisection, not the implementation formula.
	for _, c := range []struct {
		counts       Counts
		lower, upper float64
	}{
		{Counts{20, 4}, 0.08065766257979806, 0.4160174322518936},
		{Counts{200, 26}, 0.0902820227966932, 0.1836635187224841},
		{Counts{10, 0}, 0, 0.27753279986288926}, {Counts{10, 10}, 0.7224672001371109, 1},
	} {
		p := proportion(c.counts)
		if p.Status != "available" || math.Abs(p.ConfidenceInterval.Lower-c.lower) > 1e-12 || math.Abs(p.ConfidenceInterval.Upper-c.upper) > 1e-12 {
			t.Fatalf("Wilson %+v: %+v", c.counts, p)
		}
	}
	// NIST's published one-sided example: 26/200, z=1.645, lower 0.09577.
	lower, _ := wilson(Counts{200, 26}, 1.645)
	if math.Abs(lower-0.09577) > 1e-5 {
		t.Fatalf("NIST Wilson reference %.8f", lower)
	}
	for n := int64(1); n <= 100; n++ {
		for x := int64(0); x <= n; x++ {
			p := proportion(Counts{n, x})
			i := p.ConfidenceInterval
			if i.Lower < 0 || i.Upper > 1 || i.Lower > *p.Rate || i.Upper < *p.Rate {
				t.Fatalf("unbounded Wilson interval n=%d x=%d interval=%+v", n, x, i)
			}
		}
	}
	if p := proportion(Counts{}); p.Status != "insufficient_data" || p.Rate != nil || p.ConfidenceInterval != nil {
		t.Fatal("no data produced rate")
	}
	if proportion(Counts{1, 2}).Status != "invalid_counts" {
		t.Fatal("impossible count accepted")
	}
}

func TestTwoProportionNISTReferenceAndSmallCells(t *testing.T) {
	// NIST Dataplot 32/38 vs 39/44: Z=-0.58640, two-sided p=0.55760.
	control, treatment := Counts{44, 39}, Counts{38, 32}
	result := compare(control, treatment, SampleRatio{Status: "balanced"})
	if result.Status != "available" || math.Abs(*result.ZStatistic+0.58640) > 1e-5 || math.Abs(*result.PValue-0.55760) > 1e-5 {
		t.Fatalf("NIST two-proportion %+v", result)
	}
	swapped := compare(treatment, control, SampleRatio{Status: "balanced"})
	if math.Abs(*result.PValue-*swapped.PValue) > 1e-15 || math.Abs(*result.ZStatistic+*swapped.ZStatistic) > 1e-15 {
		t.Fatal("comparison not symmetric")
	}
	aa := compare(Counts{100, 50}, Counts{100, 50}, SampleRatio{Status: "balanced"})
	if aa.Status != "available" || *aa.PValue != 1 || *aa.AbsoluteLift != 0 || *aa.RelativeLift != 0 {
		t.Fatal("A/A produced difference")
	}
	for _, counts := range []Counts{{0, 0}, {10, 0}, {10, 10}, {100, 4}, {100, 96}} {
		r := compare(counts, Counts{100, 50}, SampleRatio{Status: "balanced"})
		if r.Status != "insufficient_data" || r.PValue != nil || r.ZStatistic != nil {
			t.Fatal("small cells produced significance")
		}
		if _, err := json.Marshal(r); err != nil {
			t.Fatal("nonfinite comparison JSON")
		}
	}
	zero := compare(Counts{100, 0}, Counts{100, 20}, SampleRatio{Status: "balanced"})
	if zero.RelativeLift != nil || zero.RelativeLiftReason != "zero_control_rate" {
		t.Fatal("division by zero relative lift")
	}
	mismatch := compare(Counts{100, 50}, Counts{100, 70}, SampleRatio{Status: "mismatch"})
	if mismatch.Status != "data_quality_warning" || mismatch.PValue != nil {
		t.Fatal("SRM did not suppress inference")
	}
}

func TestChiSquareNISTCriticalValuesAndUnequalWeights(t *testing.T) {
	// NIST 95th-percentile table is rounded to three decimals; use its precision.
	for df, critical := range []float64{3.841, 5.991, 7.815, 9.488, 11.070, 12.592, 14.067, 15.507, 16.919} {
		if p := chiSquareTail(critical, df+1); math.Abs(p-0.05) > 2e-5 {
			t.Fatalf("df %d tail %.9f", df+1, p)
		}
		if chiSquareTail(0, df+1) != 1 || chiSquareTail(1e6, df+1) != 0 {
			t.Fatal("chi-square tail limits")
		}
	}
	variants := []Variant{{ID: "control", WeightBP: 3000, Total: Counts{30, 15}}, {ID: "treatment", WeightBP: 7000, Total: Counts{70, 35}}}
	srm := sampleRatio(variants, "total")
	if srm.Status != "balanced" || *srm.PValue != 1 || *srm.Statistic != 0 {
		t.Fatal("unequal allocation incorrectly warned")
	}
	variants[0].Total.Exposed = 80
	variants[1].Total.Exposed = 20
	if srm = sampleRatio(variants, "total"); srm.Status != "mismatch" {
		t.Fatal("severe sample loss not warned")
	}
	variants[0].Total.Exposed = 1
	variants[1].Total.Exposed = 2
	if srm = sampleRatio(variants, "total"); srm.Status != "insufficient_data" || srm.PValue != nil {
		t.Fatal("tiny SRM sample analyzed")
	}
	multi := []Variant{{WeightBP: 2000, Total: Counts{20, 10}}, {WeightBP: 3000, Total: Counts{30, 15}}, {WeightBP: 5000, Total: Counts{50, 25}}}
	if srm = sampleRatio(multi, "total"); srm.Status != "balanced" || srm.DegreesOfFreedom != 2 || *srm.PValue != 1 {
		t.Fatal("multivariant SRM failed")
	}
}
