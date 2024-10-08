// Package evaluation is a pure local evaluator: it performs no I/O and owns no goroutines.
package evaluation

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"math"
	"reflect"
	"sort"
	"strings"

	"github.com/flagforge/flagforge/pkg/types"
)

// Bucket uses length-delimited inputs to avoid ambiguous concatenations.
func Bucket(flag, environment, subject string) int {
	h := sha256.New()
	var size [8]byte
	for _, part := range []string{flag, environment, subject} {
		binary.BigEndian.PutUint64(size[:], uint64(len(part)))
		_, _ = h.Write(size[:])
		_, _ = h.Write([]byte(part))
	}
	var digest [32]byte
	h.Sum(digest[:0])
	return int(binary.BigEndian.Uint64(digest[:8]) % 100000)
}

func Number(v any) (float64, bool) {
	var n float64
	switch x := v.(type) {
	case float64:
		n = x
	case float32:
		n = float64(x)
	case int:
		n = float64(x)
	case int64:
		n = float64(x)
	case int32:
		n = float64(x)
	case json.Number:
		var err error
		n, err = x.Float64()
		if err != nil {
			return 0, false
		}
	default:
		return 0, false
	}
	return n, !math.IsNaN(n) && !math.IsInf(n, 0)
}

func equal(a, b any) bool {
	if n, ok := Number(a); ok {
		m, ok := Number(b)
		return ok && n == m
	}
	return reflect.DeepEqual(a, b)
}

func Match(c types.Condition, s types.Subject) bool {
	v, exists := s.Attributes[c.Attribute]
	if c.Attribute == "key" {
		v, exists = s.Key, s.Key != ""
	}
	if c.Operator == "exists" {
		return exists
	}
	if c.Operator == "not_exists" {
		return !exists
	}
	if !exists || len(c.Values) == 0 {
		return false
	}
	x := c.Values[0]
	switch c.Operator {
	case "equals":
		return equal(v, x)
	case "not_equals":
		return !equal(v, x)
	case "in", "not_in":
		found := false
		for _, x := range c.Values {
			if equal(v, x) {
				found = true
				break
			}
		}
		if c.Operator == "not_in" {
			return !found
		}
		return found
	case "contains", "not_contains", "starts_with", "ends_with":
		a, ok := v.(string)
		b, ok2 := x.(string)
		if !ok || !ok2 {
			return false
		}
		switch c.Operator {
		case "contains":
			return strings.Contains(a, b)
		case "not_contains":
			return !strings.Contains(a, b)
		case "starts_with":
			return strings.HasPrefix(a, b)
		default:
			return strings.HasSuffix(a, b)
		}
	case "greater_than", "greater_or_equal", "less_than", "less_or_equal":
		a, ok := Number(v)
		b, ok2 := Number(x)
		if !ok || !ok2 {
			return false
		}
		switch c.Operator {
		case "greater_than":
			return a > b
		case "greater_or_equal":
			return a >= b
		case "less_than":
			return a < b
		default:
			return a <= b
		}
	}
	return false
}

func contains(list []string, key string) bool {
	for _, k := range list {
		if k == key {
			return true
		}
	}
	return false
}

func ruleMatches(r types.Rule, s types.Subject, snapshot *types.Snapshot, depth int) bool {
	if depth > 64 {
		return false
	}
	for _, c := range r.Conditions {
		if !Match(c, s) {
			return false
		}
	}
	for _, key := range r.Segments {
		seg, ok := snapshot.Segments[key]
		if !ok || contains(seg.Exclude, s.Key) {
			return false
		}
		matched := contains(seg.Include, s.Key)
		if !matched {
			for _, sr := range seg.Rules {
				if ruleMatches(sr, s, snapshot, depth+1) {
					matched = true
					break
				}
			}
		}
		if !matched {
			return false
		}
	}
	return len(r.Conditions) > 0 || len(r.Segments) > 0
}

// Prepare sorts rules once before publishing a snapshot. Lower priority wins;
// equal priorities use rule ID as a deterministic tie-breaker.
func Prepare(s *types.Snapshot) {
	for key, f := range s.Flags {
		sort.Slice(f.Rules, func(i, j int) bool {
			if f.Rules[i].Priority == f.Rules[j].Priority {
				return f.Rules[i].ID < f.Rules[j].ID
			}
			return f.Rules[i].Priority < f.Rules[j].Priority
		})
		s.Flags[key] = f
	}
}

func Evaluate(ctx context.Context, snapshot *types.Snapshot, key string, s types.Subject, fallback any) types.EvaluationResult {
	if ctx.Err() != nil || snapshot == nil {
		return types.EvaluationResult{Value: fallback, Reason: "ERROR"}
	}
	return evaluate(snapshot, key, s, fallback, 0)
}

func evaluate(snapshot *types.Snapshot, key string, s types.Subject, fallback any, depth int) types.EvaluationResult {
	f, ok := snapshot.Flags[key]
	if !ok || depth > 64 {
		return types.EvaluationResult{Value: fallback, Reason: "ERROR"}
	}
	result := func(v, reason, rule string) types.EvaluationResult {
		value, ok := f.Variations[v]
		if !ok {
			return types.EvaluationResult{Value: fallback, Reason: "ERROR", Version: f.Version}
		}
		return types.EvaluationResult{Value: value, Variation: v, Reason: reason, RuleID: rule, Version: f.Version}
	}
	if !f.Enabled {
		return result(f.OffVariation, "FLAG_DISABLED", "")
	}
	for _, p := range f.Prerequisites {
		r := evaluate(snapshot, p.FlagKey, s, nil, depth+1)
		if r.Reason == "ERROR" || r.Reason == "PREREQUISITE_FAILED" || r.Variation != p.Variation {
			return result(f.OffVariation, "PREREQUISITE_FAILED", "")
		}
	}
	if contains(f.Exclude, s.Key) {
		return result(f.OffVariation, "TARGET_MATCH", "")
	}
	if v, ok := f.Targets[s.Key]; ok {
		return result(v, "TARGET_MATCH", "")
	}
	for _, r := range f.Rules {
		if ruleMatches(r, s, snapshot, 0) {
			return result(r.Variation, "RULE_MATCH", r.ID)
		}
	}
	if len(f.Rollout) > 0 && s.Key != "" {
		bucket, bound := Bucket(f.Key, snapshot.Project+"/"+snapshot.Environment, s.Key), 0
		for _, v := range f.Rollout {
			bound += v.Weight
			if bucket < bound {
				return result(v.VariationKey, "ROLLOUT", "")
			}
		}
	}
	return result(f.DefaultVariation, "DEFAULT", "")
}
