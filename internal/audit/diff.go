// Package audit derives readable field differences without modifying audit history.
package audit

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
)

func Diff(before, after any) []string {
	a, okA := before.(map[string]any)
	b, okB := after.(map[string]any)
	if !okA || !okB {
		return nil
	}
	keys := map[string]struct{}{}
	for k := range a {
		keys[k] = struct{}{}
	}
	for k := range b {
		keys[k] = struct{}{}
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	out := []string{}
	for _, k := range sorted {
		if reflect.DeepEqual(a[k], b[k]) {
			continue
		}
		old, _ := json.Marshal(a[k])
		next, _ := json.Marshal(b[k])
		out = append(out, fmt.Sprintf("%s: %s -> %s", k, old, next))
	}
	return out
}
