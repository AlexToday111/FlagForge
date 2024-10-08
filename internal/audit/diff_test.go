package audit

import (
	"reflect"
	"testing"
)

func TestDiff(t *testing.T) {
	got := Diff(map[string]any{"enabled": false, "name": "same"}, map[string]any{"enabled": true, "name": "same", "rollout": 10})
	want := []string{"enabled: false -> true", "rollout: null -> 10"}
	if !reflect.DeepEqual(got, want) {
		t.Fatal(got)
	}
	if got := Diff(nil, map[string]any{}); len(got) != 0 {
		t.Fatal(got)
	}
}
