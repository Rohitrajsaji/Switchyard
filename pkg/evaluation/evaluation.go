// Package evaluation is a deterministic, side-effect-free flag evaluator shared by server and SDK.
package evaluation

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"reflect"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

var ErrInvalidDefinition = errors.New("invalid flag definition")
var ErrInvalidContext = errors.New("invalid evaluation context")
var attributePattern = regexp.MustCompile(`^[a-zA-Z][a-zA-Z0-9_.-]{0,63}$`)
var keyPattern = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)

type Value struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}
type Rule struct {
	Attribute string            `json:"attribute"`
	Operator  string            `json:"operator"`
	Values    []json.RawMessage `json:"values"`
	Value     Value             `json:"value"`
}
type Rollout struct {
	TrafficBP int    `json:"traffic_bp"`
	Salt      string `json:"salt"`
	Value     Value  `json:"value"`
}
type Variant struct {
	ID       string `json:"id"`
	Ordinal  int    `json:"ordinal"`
	WeightBP int    `json:"weight_bp"`
	Value    Value  `json:"value"`
}
type Experiment struct {
	RunID           string    `json:"run_id"`
	EligibilitySalt string    `json:"eligibility_salt"`
	VariantSalt     string    `json:"variant_salt"`
	TrafficBP       int       `json:"traffic_bp"`
	Variants        []Variant `json:"variants"`
}
type Definition struct {
	ProjectID     string      `json:"project_id"`
	EnvironmentID string      `json:"environment_id"`
	FlagID        string      `json:"flag_id"`
	Key           string      `json:"key"`
	Revision      int64       `json:"revision"`
	Type          string      `json:"type"`
	Killed        bool        `json:"killed"`
	Default       Value       `json:"default"`
	Safe          Value       `json:"safe"`
	Rules         []Rule      `json:"rules"`
	Rollout       *Rollout    `json:"rollout,omitempty"`
	Experiment    *Experiment `json:"experiment,omitempty"`
}
type Result struct {
	Value     Value  `json:"value"`
	Reason    string `json:"reason"`
	Revision  int64  `json:"revision"`
	RunID     string `json:"run_id,omitempty"`
	VariantID string `json:"variant_id,omitempty"`
}
type compiledRule struct {
	rule    Rule
	targets []any
}
type Compiled struct {
	definition Definition
	rules      []compiledRule
	canonical  []byte
}

func (v Value) Validate(kind string) error {
	if v.Type != kind || len(v.Data) == 0 || len(v.Data) > 16384 || !json.Valid(v.Data) {
		return ErrInvalidDefinition
	}
	if kind == "boolean" {
		var value bool
		if bytes.Equal(bytes.TrimSpace(v.Data), []byte("null")) || json.Unmarshal(v.Data, &value) != nil {
			return ErrInvalidDefinition
		}
	} else if kind != "json" {
		return ErrInvalidDefinition
	}
	if kind == "json" {
		var data any
		decoder := json.NewDecoder(bytes.NewReader(v.Data))
		decoder.UseNumber()
		if decoder.Decode(&data) != nil || !boundedJSON(data) {
			return ErrInvalidDefinition
		}
	}
	return nil
}

// Equal compares JSON structure and exact bounded decimal numbers, including exponent spelling.
func Equal(a, b Value) bool {
	if a.Type != b.Type || a.Validate(a.Type) != nil || b.Validate(b.Type) != nil {
		return false
	}
	var av, bv any
	ad := json.NewDecoder(bytes.NewReader(a.Data))
	ad.UseNumber()
	bd := json.NewDecoder(bytes.NewReader(b.Data))
	bd.UseNumber()
	if ad.Decode(&av) != nil || bd.Decode(&bv) != nil {
		return false
	}
	return equalJSON(av, bv)
}

func boundedJSON(v any) bool {
	switch x := v.(type) {
	case json.Number:
		if len(x) > 512 {
			return false
		}
		text := string(x)
		coefficient := text
		if i := strings.IndexAny(text, "eE"); i >= 0 {
			exponent, err := strconv.Atoi(text[i+1:])
			if err != nil || exponent < -308 || exponent > 308 {
				return false
			}
			coefficient = text[:i]
		}
		n, err := strconv.ParseFloat(text, 64)
		if err != nil || math.IsInf(n, 0) || math.Abs(n) > 1e15 || (n != 0 && math.Abs(n) < 1e-308) {
			return false
		}
		if n == 0 && strings.Trim(strings.ReplaceAll(coefficient, ".", ""), "-0") != "" {
			return false
		}
		return true
	case []any:
		for _, item := range x {
			if !boundedJSON(item) {
				return false
			}
		}
	case map[string]any:
		for _, item := range x {
			if !boundedJSON(item) {
				return false
			}
		}
	}
	return true
}
func equalJSON(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		ar, ok := new(big.Rat).SetString(string(x))
		if !ok {
			return false
		}
		br, ok := new(big.Rat).SetString(string(y))
		return ok && ar.Cmp(br) == 0
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i, item := range x {
			if !equalJSON(item, y[i]) {
				return false
			}
		}
		return true
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for key, item := range x {
			other, exists := y[key]
			if !exists || !equalJSON(item, other) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(a, b)
	}
}
func SensitiveAttribute(attribute string) bool {
	for _, part := range strings.Split(strings.ToLower(attribute), ".") {
		switch strings.ReplaceAll(part, "-", "_") {
		case "email", "email_address", "phone", "phone_number", "name", "full_name", "address", "payment_id", "payment_identifier", "card_number":
			return true
		}
	}
	return false
}
func Compile(d Definition) (*Compiled, error) {
	if d.Rules == nil {
		d.Rules = make([]Rule, 0)
	}
	if d.ProjectID == "" || d.EnvironmentID == "" || d.FlagID == "" || d.Revision < 1 || !keyPattern.MatchString(d.Key) {
		return nil, fmt.Errorf("%w: identity and positive revision required", ErrInvalidDefinition)
	}
	if err := d.Default.Validate(d.Type); err != nil {
		return nil, fmt.Errorf("%w: default type", err)
	}
	if err := d.Safe.Validate(d.Type); err != nil {
		return nil, fmt.Errorf("%w: safe type", err)
	}
	if len(d.Rules) > 50 {
		return nil, fmt.Errorf("%w: too many rules", ErrInvalidDefinition)
	}
	for i, rule := range d.Rules {
		if !attributePattern.MatchString(rule.Attribute) || SensitiveAttribute(rule.Attribute) {
			return nil, fmt.Errorf("%w: rule %d attribute prohibited", ErrInvalidDefinition, i)
		}
		if err := rule.Value.Validate(d.Type); err != nil {
			return nil, fmt.Errorf("%w: rule %d value", err, i)
		}
		if len(rule.Values) < 1 || len(rule.Values) > 100 || (rule.Operator != "in" && len(rule.Values) != 1) {
			return nil, fmt.Errorf("%w: rule %d target count", ErrInvalidDefinition, i)
		}
		if rule.Operator != "eq" && rule.Operator != "in" && rule.Operator != "gt" && rule.Operator != "gte" && rule.Operator != "lt" && rule.Operator != "lte" {
			return nil, fmt.Errorf("%w: rule %d operator", ErrInvalidDefinition, i)
		}
		for _, value := range rule.Values {
			p, err := primitive(value)
			if err != nil {
				return nil, fmt.Errorf("%w: rule %d target", ErrInvalidDefinition, i)
			}
			if rule.Operator != "eq" && rule.Operator != "in" {
				if _, ok := p.(float64); !ok {
					return nil, fmt.Errorf("%w: numeric comparison requires number", ErrInvalidDefinition)
				}
			}
		}
	}
	if d.Rollout != nil {
		if d.Rollout.TrafficBP < 0 || d.Rollout.TrafficBP > 10000 || d.Rollout.Salt == "" || len(d.Rollout.Salt) > 128 {
			return nil, fmt.Errorf("%w: rollout allocation or salt", ErrInvalidDefinition)
		}
		if err := d.Rollout.Value.Validate(d.Type); err != nil {
			return nil, err
		}
	}
	if e := d.Experiment; e != nil {
		if e.RunID == "" || e.EligibilitySalt == "" || e.VariantSalt == "" || len(e.RunID) > 128 || len(e.EligibilitySalt) > 128 || len(e.VariantSalt) > 128 || e.TrafficBP < 0 || e.TrafficBP > 10000 || len(e.Variants) < 2 || len(e.Variants) > 10 {
			return nil, fmt.Errorf("%w: experiment identity/allocation", ErrInvalidDefinition)
		}
		total := 0
		ids := map[string]bool{}
		ordinals := map[int]bool{}
		for _, v := range e.Variants {
			if v.ID == "" || len(v.ID) > 128 || ids[v.ID] || v.Ordinal < 0 || ordinals[v.Ordinal] || v.WeightBP < 1 || v.WeightBP > 10000 {
				return nil, fmt.Errorf("%w: duplicate/invalid variant", ErrInvalidDefinition)
			}
			if err := v.Value.Validate(d.Type); err != nil {
				return nil, err
			}
			ids[v.ID] = true
			ordinals[v.Ordinal] = true
			total += v.WeightBP
		}
		if total != 10000 {
			return nil, fmt.Errorf("%w: variant weights must total 10000", ErrInvalidDefinition)
		}
	}
	// Copy once at compilation so callers cannot mutate an evaluator through shared slices/pointers.
	body, err := json.Marshal(d)
	if err != nil || len(body) > 65536 {
		return nil, ErrInvalidDefinition
	}
	var copy Definition
	if err := json.Unmarshal(body, &copy); err != nil {
		return nil, ErrInvalidDefinition
	}
	if copy.Experiment != nil {
		slices.SortFunc(copy.Experiment.Variants, func(a, b Variant) int { return a.Ordinal - b.Ordinal })
	}
	c := &Compiled{definition: copy, canonical: body, rules: make([]compiledRule, 0, len(copy.Rules))}
	for _, rule := range copy.Rules {
		targets := make([]any, 0, len(rule.Values))
		for _, value := range rule.Values {
			p, err := primitive(value)
			if err != nil {
				return nil, ErrInvalidDefinition
			}
			targets = append(targets, p)
		}
		c.rules = append(c.rules, compiledRule{rule: rule, targets: targets})
	}
	return c, nil
}
func (c *Compiled) MarshalJSON() ([]byte, error) { return bytes.Clone(c.canonical), nil }
func (c *Compiled) Evaluate(userID string, attributes map[string]json.RawMessage) (Result, error) {
	if err := ValidateContext(userID, attributes); err != nil {
		return Result{}, err
	}
	d := &c.definition
	result := func(v Value, reason, runID, variantID string) Result {
		v.Data = bytes.Clone(v.Data)
		return Result{Value: v, Reason: reason, Revision: d.Revision, RunID: runID, VariantID: variantID}
	}
	if d.Killed {
		return result(d.Safe, "kill_switch", "", ""), nil
	}
	for _, rule := range c.rules {
		raw, ok := attributes[rule.rule.Attribute]
		if !ok {
			continue
		}
		input, err := primitive(raw)
		if err != nil {
			continue
		}
		if matches(rule, input) {
			return result(rule.rule.Value, "targeting", "", ""), nil
		}
	}
	if e := d.Experiment; e != nil {
		if Bucket(d.ProjectID, d.EnvironmentID, d.FlagID, e.RunID, "eligibility", e.EligibilitySalt, userID) >= e.TrafficBP {
			return result(d.Default, "default", "", ""), nil
		}
		bucket := Bucket(d.ProjectID, d.EnvironmentID, d.FlagID, e.RunID, "variant", e.VariantSalt, userID)
		cumulative := 0
		for _, v := range e.Variants {
			cumulative += v.WeightBP
			if bucket < cumulative {
				return result(v.Value, "experiment", e.RunID, v.ID), nil
			}
		}
	}
	if r := d.Rollout; r != nil && Bucket(d.ProjectID, d.EnvironmentID, d.FlagID, "rollout:"+r.Salt, "eligibility", r.Salt, userID) < r.TrafficBP {
		return result(r.Value, "rollout", "", ""), nil
	}
	return result(d.Default, "default", "", ""), nil
}
func ValidateContext(userID string, attributes map[string]json.RawMessage) error {
	if userID == "" || len(userID) > 256 || len(attributes) > 100 {
		return ErrInvalidContext
	}
	for key, value := range attributes {
		if !attributePattern.MatchString(key) || len(value) > 1024 || !json.Valid(value) {
			return ErrInvalidContext
		}
	}
	return nil
}

// Bucket is the v1 cross-language contract: u32 byte lengths, UTF-8, SHA-256,
// first u64 big-endian, modulo 10000. Revision and traffic never enter the hash.
func Bucket(projectID, environmentID, flagID, runID, purpose, salt, userID string) int {
	h := sha256.New()
	var length [4]byte
	for _, field := range []string{"v1", projectID, environmentID, flagID, runID, purpose, salt, userID} {
		binary.BigEndian.PutUint32(length[:], uint32(len(field)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(field))
	}
	return int(binary.BigEndian.Uint64(h.Sum(nil)[:8]) % 10000)
}
func primitive(raw json.RawMessage) (any, error) {
	if len(raw) > 1024 {
		return nil, ErrInvalidDefinition
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, err
	}
	switch v := value.(type) {
	case bool, string:
		return v, nil
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) || math.Abs(v) > 1e15 {
			return nil, ErrInvalidDefinition
		}
		return v, nil
	default:
		return nil, ErrInvalidDefinition
	}
}
func matches(rule compiledRule, input any) bool {
	if rule.rule.Operator == "eq" || rule.rule.Operator == "in" {
		for _, target := range rule.targets {
			if input == target {
				return true
			}
		}
		return false
	}
	n, ok := input.(float64)
	if !ok {
		return false
	}
	target := rule.targets[0].(float64)
	switch rule.rule.Operator {
	case "gt":
		return n > target
	case "gte":
		return n >= target
	case "lt":
		return n < target
	case "lte":
		return n <= target
	}
	return false
}
