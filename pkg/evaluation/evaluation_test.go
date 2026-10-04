package evaluation_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"switchyard/pkg/evaluation"
)

func boolean(v bool) evaluation.Value {
	return evaluation.Value{Type: "boolean", Data: json.RawMessage(fmt.Sprint(v))}
}
func base() evaluation.Definition {
	return evaluation.Definition{ProjectID: "prj_demo", EnvironmentID: "env_demo", FlagID: "flag_demo", Key: "new_listing", Revision: 1, Type: "boolean", Default: boolean(false), Safe: boolean(false), Rollout: &evaluation.Rollout{TrafficBP: 1000, Salt: "salt_demo", Value: boolean(true)}}
}
func TestGoldenBuckets(t *testing.T) {
	for _, tt := range []struct {
		purpose  string
		expected int
	}{{"eligibility", 8811}, {"variant", 3818}} {
		got := evaluation.Bucket("prj_demo", "env_demo", "flag_demo", "run_demo", tt.purpose, "salt_demo", "user_123")
		if got != tt.expected {
			t.Fatalf("%s=%d want=%d", tt.purpose, got, tt.expected)
		}
	}
}
func TestRolloutGrowthAndShrinkPreservePopulation(t *testing.T) {
	d := base()
	small, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	d.Rollout.TrafficBP = 2000
	large, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for i := range 10000 {
		user := fmt.Sprintf("user-%d", i)
		a, err := small.Evaluate(user, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := large.Evaluate(user, nil)
		if err != nil {
			t.Fatal(err)
		}
		if string(a.Value.Data) == "true" {
			count++
			if string(b.Value.Data) != "true" {
				t.Fatal("growth removed existing user")
			}
		}
	}
	if count < 850 || count > 1150 {
		t.Fatalf("unexpected broad distribution: %d", count)
	}
}
func TestKillSwitchAndTargetPrecedence(t *testing.T) {
	d := base()
	d.Rules = []evaluation.Rule{{Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: boolean(true)}}
	d.Experiment = &evaluation.Experiment{RunID: "run_demo", EligibilitySalt: "eligible", VariantSalt: "variant", TrafficBP: 10000, Variants: []evaluation.Variant{{ID: "control", Ordinal: 0, WeightBP: 5000, Value: boolean(false)}, {ID: "treatment", Ordinal: 1, WeightBP: 5000, Value: boolean(true)}}}
	c, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Evaluate("user-123", map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)})
	if err != nil || r.Reason != "targeting" || r.RunID != "" {
		t.Fatalf("target not excluded: %+v %v", r, err)
	}
	d.Killed = true
	c, err = evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err = c.Evaluate("user-123", map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)})
	if err != nil || r.Reason != "kill_switch" || string(r.Value.Data) != "false" {
		t.Fatalf("kill precedence: %+v %v", r, err)
	}
}
func TestVariantsStayStableWhenTrafficGrowsAndDefinitionIsImmutable(t *testing.T) {
	d := base()
	d.Rollout = nil
	d.Experiment = &evaluation.Experiment{RunID: "run_demo", EligibilitySalt: "eligible", VariantSalt: "variant", TrafficBP: 1000, Variants: []evaluation.Variant{{ID: "treatment", Ordinal: 1, WeightBP: 7000, Value: boolean(true)}, {ID: "control", Ordinal: 0, WeightBP: 3000, Value: boolean(false)}}}
	small, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	d.Experiment.TrafficBP = 5000
	d.Revision = 2
	large, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	treatmentUser := ""
	for i := range 10000 {
		user := fmt.Sprintf("user-%d", i)
		a, err := small.Evaluate(user, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := large.Evaluate(user, nil)
		if err != nil {
			t.Fatal(err)
		}
		if b.VariantID == "treatment" {
			treatmentUser = user
		}
		if a.RunID != "" && a.VariantID != b.VariantID {
			t.Fatal("traffic change reassigned variant")
		}
	}
	d.Experiment.Variants[0].Value.Data[0] = 'f'
	if treatmentUser == "" {
		t.Fatal("no treatment fixture")
	}
	a, err := large.Evaluate(treatmentUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(a.Value.Data) {
		t.Fatal("caller mutated compiled definition")
	}
	a.Value.Data[0] = 'x'
	b, err := large.Evaluate(treatmentUser, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !json.Valid(b.Value.Data) {
		t.Fatal("result mutated compiled definition")
	}
}
func TestVariantValidationAndOrderedTargets(t *testing.T) {
	for _, variants := range [][]evaluation.Variant{
		{{ID: "a", Ordinal: 0, WeightBP: 6000, Value: boolean(false)}, {ID: "b", Ordinal: 1, WeightBP: 6000, Value: boolean(true)}},
		{{ID: "a", Ordinal: 0, WeightBP: 5000, Value: boolean(false)}, {ID: "a", Ordinal: 1, WeightBP: 5000, Value: boolean(true)}},
		{{ID: "a", Ordinal: 0, WeightBP: 5000, Value: boolean(false)}, {ID: "b", Ordinal: 0, WeightBP: 5000, Value: boolean(true)}},
	} {
		d := base()
		d.Experiment = &evaluation.Experiment{RunID: "run", EligibilitySalt: "eligibility", VariantSalt: "variant", TrafficBP: 10000, Variants: variants}
		if _, err := evaluation.Compile(d); err == nil {
			t.Fatal("invalid weights/identity accepted")
		}
	}
	d := base()
	d.Rules = []evaluation.Rule{{Attribute: "country", Operator: "in", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: boolean(true)}, {Attribute: "country", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"JP"`)}, Value: boolean(false)}}
	c, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Evaluate("user", map[string]json.RawMessage{"country": json.RawMessage(`"JP"`)})
	if err != nil || string(r.Value.Data) != "true" {
		t.Fatal("ordered rules changed precedence")
	}
}
func TestTypedRulesAndValidation(t *testing.T) {
	for _, tt := range []struct {
		operator, attribute string
		target, input       json.RawMessage
		match               bool
	}{{"gte", "age", json.RawMessage(`18`), json.RawMessage(`19`), true}, {"gte", "age", json.RawMessage(`18`), json.RawMessage(`"19"`), false}, {"eq", "premium", json.RawMessage(`true`), json.RawMessage(`true`), true}, {"eq", "country", json.RawMessage(`"JP"`), json.RawMessage(`"US"`), false}} {
		d := base()
		d.Rollout = nil
		d.Rules = []evaluation.Rule{{Attribute: tt.attribute, Operator: tt.operator, Values: []json.RawMessage{tt.target}, Value: boolean(true)}}
		c, err := evaluation.Compile(d)
		if err != nil {
			t.Fatal(err)
		}
		r, err := c.Evaluate("user", map[string]json.RawMessage{tt.attribute: tt.input})
		if err != nil || (r.Reason == "targeting") != tt.match {
			t.Fatalf("%s matched=%s err=%v", tt.operator, r.Reason, err)
		}
	}
	for _, mutate := range []func(*evaluation.Definition){func(d *evaluation.Definition) { d.Safe = evaluation.Value{Type: "json", Data: json.RawMessage(`{}`)} }, func(d *evaluation.Definition) { d.Rollout.TrafficBP = 10001 }, func(d *evaluation.Definition) {
		d.Rules = []evaluation.Rule{{Attribute: "email", Operator: "eq", Values: []json.RawMessage{json.RawMessage(`"x"`)}, Value: boolean(true)}}
	}} {
		d := base()
		mutate(&d)
		if _, err := evaluation.Compile(d); err == nil {
			t.Fatal("invalid definition accepted")
		}
	}
	c, err := evaluation.Compile(base())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Evaluate("", nil); err == nil {
		t.Fatal("missing user ID accepted")
	}
}
func BenchmarkEvaluate(b *testing.B) {
	c, err := evaluation.Compile(base())
	if err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if _, err := c.Evaluate("user-123", nil); err != nil {
			b.Fatal(err)
		}
	}
}
func FuzzCompile(f *testing.F) {
	valid, err := json.Marshal(base())
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte(`{"type":"boolean"}`))
	f.Add([]byte(`null`))
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 65536 {
			return
		}
		var d evaluation.Definition
		if json.Unmarshal(body, &d) != nil {
			return
		}
		c, err := evaluation.Compile(d)
		if err == nil {
			_, _ = c.Evaluate("fuzz-user", nil)
		}
	})
}

func TestJSONSafetyAndBoundedNumbers(t *testing.T) {
	a := evaluation.Value{Type: "json", Data: json.RawMessage(`{"flow":"control","timeout":100.0}`)}
	b := evaluation.Value{Type: "json", Data: json.RawMessage(`{"timeout":1e2,"flow":"control"}`)}
	if !evaluation.Equal(a, b) {
		t.Fatal("JSON-safe values changed by numeric serialization")
	}
	for _, raw := range []string{`{"timeout":1e999999}`, `{"timeout":1e-999999}`} {
		if (evaluation.Value{Type: "json", Data: json.RawMessage(raw)}).Validate("json") == nil {
			t.Fatal("unbounded JSON number accepted")
		}
	}
	d := base()
	d.Type = "json"
	d.Rollout = nil
	d.Default = evaluation.Value{Type: "json", Data: json.RawMessage(`{"flow":"treatment"}`)}
	d.Safe = a
	d.Killed = true
	c, err := evaluation.Compile(d)
	if err != nil {
		t.Fatal(err)
	}
	r, err := c.Evaluate("user", nil)
	if err != nil || !evaluation.Equal(r.Value, a) {
		t.Fatalf("JSON kill safety: %+v %v", r, err)
	}
}
