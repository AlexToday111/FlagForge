package evaluation

import (
	"context"
	"fmt"
	"github.com/flagforge/flagforge/pkg/types"
	"testing"
)

func flag() types.FlagConfig {
	return types.FlagConfig{Key: "checkout", Type: types.Boolean, Enabled: true, Variations: map[string]any{"on": true, "off": false}, DefaultVariation: "off", OffVariation: "off", Version: 1}
}
func snap(f types.FlagConfig) types.Snapshot {
	return types.Snapshot{Project: "shop", Environment: "production", Version: 1, Flags: map[string]types.FlagConfig{f.Key: f}, Segments: map[string]types.Segment{}}
}

func TestEvaluationOrder(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(*types.FlagConfig, *types.Snapshot)
		reason string
		value  bool
	}{
		{"disabled", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Enabled = false
			f.Targets = map[string]string{"u": "on"}
		}, "FLAG_DISABLED", false},
		{"default", func(_ *types.FlagConfig, _ *types.Snapshot) {}, "DEFAULT", false},
		{"target", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Targets = map[string]string{"u": "on"}
			f.Rollout = []types.RolloutVariation{{VariationKey: "off", Weight: 100000}}
		}, "TARGET_MATCH", true},
		{"exclude wins", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Targets = map[string]string{"u": "on"}
			f.Exclude = []string{"u"}
		}, "TARGET_MATCH", false},
		{"rule", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Rules = []types.Rule{{ID: "eu", Variation: "on", Conditions: []types.Condition{{Attribute: "country", Operator: "equals", Values: []any{"DE"}}, {Attribute: "age", Operator: "greater_or_equal", Values: []any{18}}}}}
		}, "RULE_MATCH", true},
		{"AND fails", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Rules = []types.Rule{{ID: "eu", Variation: "on", Conditions: []types.Condition{{Attribute: "country", Operator: "equals", Values: []any{"DE"}}, {Attribute: "age", Operator: "greater_than", Values: []any{40}}}}}
		}, "DEFAULT", false},
		{"segment", func(f *types.FlagConfig, s *types.Snapshot) {
			s.Segments["premium"] = types.Segment{Key: "premium", Rules: []types.Rule{{Conditions: []types.Condition{{Attribute: "plan", Operator: "equals", Values: []any{"premium"}}}}}}
			f.Rules = []types.Rule{{ID: "segment", Segments: []string{"premium"}, Variation: "on"}}
		}, "RULE_MATCH", true},
		{"rollout", func(f *types.FlagConfig, _ *types.Snapshot) {
			f.Rollout = []types.RolloutVariation{{VariationKey: "on", Weight: 100000}}
		}, "ROLLOUT", true},
		{"prerequisite passes", func(f *types.FlagConfig, s *types.Snapshot) {
			dep := flag()
			dep.Key = "cart"
			dep.DefaultVariation = "on"
			s.Flags[dep.Key] = dep
			f.Prerequisites = []types.Prerequisite{{FlagKey: "cart", Variation: "on"}}
			f.DefaultVariation = "on"
		}, "DEFAULT", true},
		{"prerequisite before target", func(f *types.FlagConfig, s *types.Snapshot) {
			dep := flag()
			dep.Key = "cart"
			s.Flags[dep.Key] = dep
			f.Prerequisites = []types.Prerequisite{{FlagKey: "cart", Variation: "on"}}
			f.Targets = map[string]string{"u": "on"}
		}, "PREREQUISITE_FAILED", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			f := flag()
			s := snap(f)
			test.setup(&f, &s)
			s.Flags[f.Key] = f
			if err := ValidateSnapshot(s); err != nil {
				t.Fatal(err)
			}
			Prepare(&s)
			r := Evaluate(context.Background(), &s, f.Key, types.Subject{Key: "u", Attributes: map[string]any{"country": "DE", "age": 28, "plan": "premium"}}, true)
			if r.Reason != test.reason || r.Value != test.value {
				t.Fatalf("got %+v", r)
			}
		})
	}
}

func TestPriority(t *testing.T) {
	f := flag()
	f.Rules = []types.Rule{{ID: "second", Priority: 2, Variation: "off", Conditions: []types.Condition{{Attribute: "plan", Operator: "equals", Values: []any{"premium"}}}}, {ID: "first", Priority: 1, Variation: "on", Conditions: []types.Condition{{Attribute: "country", Operator: "equals", Values: []any{"DE"}}}}}
	s := snap(f)
	Prepare(&s)
	r := Evaluate(context.Background(), &s, f.Key, types.Subject{Key: "u", Attributes: map[string]any{"plan": "premium", "country": "DE"}}, false)
	if r.RuleID != "first" {
		t.Fatal(r)
	}
}

func TestOperators(t *testing.T) {
	s := types.Subject{Key: "user", Attributes: map[string]any{"country": "DE", "age": 28, "email": "alice@company.com", "beta": true}}
	for _, tt := range []struct {
		attr, op string
		values   []any
		want     bool
	}{
		{"country", "equals", []any{"DE"}, true}, {"country", "not_equals", []any{"FR"}, true}, {"country", "in", []any{"FR", "DE"}, true}, {"country", "not_in", []any{"FR"}, true},
		{"email", "contains", []any{"@"}, true}, {"email", "not_contains", []any{"evil"}, true}, {"email", "starts_with", []any{"alice"}, true}, {"email", "ends_with", []any{"@company.com"}, true},
		{"age", "greater_than", []any{18.0}, true}, {"age", "greater_or_equal", []any{28}, true}, {"age", "less_than", []any{30}, true}, {"age", "less_or_equal", []any{28}, true},
		{"beta", "equals", []any{true}, true}, {"absent", "exists", nil, false}, {"absent", "not_exists", nil, true}, {"country", "exists", nil, true}, {"absent", "not_equals", []any{1}, false},
		{"age", "greater_than", []any{"18"}, false}, {"email", "contains", []any{10}, false}, {"key", "equals", []any{"user"}, true},
	} {
		t.Run(tt.attr+tt.op, func(t *testing.T) {
			if got := Match(types.Condition{Attribute: tt.attr, Operator: tt.op, Values: tt.values}, s); got != tt.want {
				t.Fatalf("got %v", got)
			}
		})
	}
}

func TestRolloutPopulation(t *testing.T) {
	enabled := 0
	for i := 0; i < 100000; i++ {
		u := fmt.Sprintf("user-%d", i)
		b := Bucket("checkout", "shop/production", u)
		if b != Bucket("checkout", "shop/production", u) {
			t.Fatal("not deterministic")
		}
		if b < 20000 {
			enabled++
		}
	}
	if enabled < 19500 || enabled > 20500 {
		t.Fatalf("20%% distribution: %d", enabled)
	}
	if Bucket("ab", "c", "d") == Bucket("a", "bc", "d") {
		t.Fatal("ambiguous inputs")
	}
}

func TestValidation(t *testing.T) {
	for _, tt := range []struct {
		name   string
		change func(*types.Snapshot)
	}{
		{"weight", func(s *types.Snapshot) {
			f := s.Flags["checkout"]
			f.Rollout = []types.RolloutVariation{{VariationKey: "on", Weight: 99999}}
			s.Flags[f.Key] = f
		}},
		{"type", func(s *types.Snapshot) { f := s.Flags["checkout"]; f.Variations["on"] = "true"; s.Flags[f.Key] = f }},
		{"missing default", func(s *types.Snapshot) { f := s.Flags["checkout"]; f.DefaultVariation = "missing"; s.Flags[f.Key] = f }},
		{"cycle", func(s *types.Snapshot) {
			f := s.Flags["checkout"]
			f.Prerequisites = []types.Prerequisite{{FlagKey: "checkout", Variation: "on"}}
			s.Flags[f.Key] = f
		}},
		{"segment cycle", func(s *types.Snapshot) {
			s.Segments["a"] = types.Segment{Key: "a", Rules: []types.Rule{{Segments: []string{"a"}}}}
		}},
		{"operator", func(s *types.Snapshot) {
			f := s.Flags["checkout"]
			f.Rules = []types.Rule{{ID: "x", Variation: "on", Conditions: []types.Condition{{Attribute: "a", Operator: "evil", Values: []any{1}}}}}
			s.Flags[f.Key] = f
		}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			s := snap(flag())
			tt.change(&s)
			if err := ValidateSnapshot(s); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestMissingAndCancelled(t *testing.T) {
	s := snap(flag())
	if r := Evaluate(context.Background(), &s, "missing", types.Subject{}, "fallback"); r.Reason != "ERROR" || r.Value != "fallback" {
		t.Fatal(r)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if r := Evaluate(ctx, &s, "checkout", types.Subject{}, true); r.Reason != "ERROR" {
		t.Fatal(r)
	}
}

func BenchmarkEvaluation(b *testing.B) {
	f := flag()
	f.Rollout = []types.RolloutVariation{{VariationKey: "on", Weight: 20000}, {VariationKey: "off", Weight: 80000}}
	s := snap(f)
	subject := types.Subject{Key: "u"}
	ctx := context.Background()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		Evaluate(ctx, &s, "checkout", subject, false)
	}
}
