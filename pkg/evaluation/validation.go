package evaluation

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"

	"github.com/flagforge/flagforge/pkg/types"
)

var keyPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

func ValidKey(key string) bool { return keyPattern.MatchString(key) }

func ValidValue(kind types.FlagType, value any) bool {
	switch kind {
	case types.Boolean:
		_, ok := value.(bool)
		return ok
	case types.String:
		_, ok := value.(string)
		return ok
	case types.Integer:
		n, ok := Number(value)
		return ok && math.Trunc(n) == n && n >= -9007199254740991 && n <= 9007199254740991
	case types.Float:
		_, ok := Number(value)
		return ok
	case types.JSON:
		_, err := json.Marshal(value)
		return err == nil
	default:
		return false
	}
}

func ValidateFlag(f types.FlagConfig) error {
	if !ValidKey(f.Key) {
		return fmt.Errorf("invalid flag key %q", f.Key)
	}
	if len(f.Variations) == 0 {
		return fmt.Errorf("variations required")
	}
	for k, v := range f.Variations {
		if !ValidKey(k) || !ValidValue(f.Type, v) {
			return fmt.Errorf("invalid %s variation %q", f.Type, k)
		}
	}
	check := func(k string) error {
		if _, ok := f.Variations[k]; !ok {
			return fmt.Errorf("unknown variation %q", k)
		}
		return nil
	}
	if err := check(f.DefaultVariation); err != nil {
		return err
	}
	if err := check(f.OffVariation); err != nil {
		return err
	}
	for subject, v := range f.Targets {
		if subject == "" {
			return fmt.Errorf("empty target key")
		}
		if err := check(v); err != nil {
			return err
		}
	}
	ids := map[string]bool{}
	for _, rule := range f.Rules {
		if rule.ID == "" || ids[rule.ID] {
			return fmt.Errorf("missing or duplicate rule ID")
		}
		ids[rule.ID] = true
		if err := validateRule(rule); err != nil {
			return err
		}
		if err := check(rule.Variation); err != nil {
			return err
		}
	}
	weight := 0
	seen := map[string]bool{}
	for _, r := range f.Rollout {
		if r.Weight < 0 || r.Weight > 100000 || seen[r.VariationKey] {
			return fmt.Errorf("invalid rollout weight or duplicate variation")
		}
		seen[r.VariationKey] = true
		if err := check(r.VariationKey); err != nil {
			return err
		}
		weight += r.Weight
	}
	if len(f.Rollout) > 0 && weight != 100000 {
		return fmt.Errorf("rollout weights must total 100000")
	}
	return nil
}

func validateRule(r types.Rule) error {
	if len(r.Conditions) == 0 && len(r.Segments) == 0 {
		return fmt.Errorf("empty targeting rule")
	}
	for _, c := range r.Conditions {
		if c.Attribute == "" {
			return fmt.Errorf("empty condition attribute")
		}
		switch c.Operator {
		case "exists", "not_exists":
			if len(c.Values) != 0 {
				return fmt.Errorf("existence condition takes no values")
			}
		case "in", "not_in":
			if len(c.Values) == 0 {
				return fmt.Errorf("membership condition needs values")
			}
		case "equals", "not_equals", "contains", "not_contains", "starts_with", "ends_with", "greater_than", "greater_or_equal", "less_than", "less_or_equal":
			if len(c.Values) != 1 {
				return fmt.Errorf("condition requires exactly one value")
			}
		default:
			return fmt.Errorf("unknown operator %q", c.Operator)
		}
		switch c.Operator {
		case "contains", "not_contains", "starts_with", "ends_with":
			if _, ok := c.Values[0].(string); !ok {
				return fmt.Errorf("string operator requires string")
			}
		case "greater_than", "greater_or_equal", "less_than", "less_or_equal":
			if _, ok := Number(c.Values[0]); !ok {
				return fmt.Errorf("numeric operator requires number")
			}
		}
	}
	return nil
}

// ValidateSnapshot checks references and cycles over the complete environment.
func ValidateSnapshot(s types.Snapshot) error {
	for key, seg := range s.Segments {
		if key != seg.Key || !ValidKey(key) {
			return fmt.Errorf("invalid segment key")
		}
		for _, r := range seg.Rules {
			if err := validateRule(r); err != nil {
				return err
			}
			for _, reference := range r.Segments {
				if _, ok := s.Segments[reference]; !ok {
					return fmt.Errorf("unknown segment %q", reference)
				}
			}
		}
	}
	for key, f := range s.Flags {
		if key != f.Key {
			return fmt.Errorf("flag key mismatch")
		}
		if err := ValidateFlag(f); err != nil {
			return fmt.Errorf("flag %q: %w", key, err)
		}
		for _, p := range f.Prerequisites {
			dep, ok := s.Flags[p.FlagKey]
			if !ok {
				return fmt.Errorf("unknown prerequisite %q", p.FlagKey)
			}
			if _, ok := dep.Variations[p.Variation]; !ok {
				return fmt.Errorf("unknown prerequisite variation")
			}
		}
		for _, r := range f.Rules {
			for _, key := range r.Segments {
				if _, ok := s.Segments[key]; !ok {
					return fmt.Errorf("unknown segment %q", key)
				}
			}
		}
	}
	checkGraph := func(keys []string, edges func(string) []string) error {
		state := map[string]int{}
		var visit func(string, int) error
		visit = func(k string, depth int) error {
			if depth > 64 {
				return fmt.Errorf("dependency depth exceeds 64")
			}
			if state[k] == 1 {
				return fmt.Errorf("dependency cycle at %q", k)
			}
			if state[k] == 2 {
				return nil
			}
			state[k] = 1
			for _, next := range edges(k) {
				if err := visit(next, depth+1); err != nil {
					return err
				}
			}
			state[k] = 2
			return nil
		}
		for _, k := range keys {
			if err := visit(k, 0); err != nil {
				return err
			}
		}
		return nil
	}
	keys := []string{}
	for k := range s.Flags {
		keys = append(keys, k)
	}
	if err := checkGraph(keys, func(k string) []string {
		out := []string{}
		for _, p := range s.Flags[k].Prerequisites {
			out = append(out, p.FlagKey)
		}
		return out
	}); err != nil {
		return err
	}
	keys = nil
	for k := range s.Segments {
		keys = append(keys, k)
	}
	return checkGraph(keys, func(k string) []string {
		out := []string{}
		for _, r := range s.Segments[k].Rules {
			out = append(out, r.Segments...)
		}
		return out
	})
}
