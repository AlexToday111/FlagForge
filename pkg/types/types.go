// Package types defines the versioned wire format shared by the control plane and SDK.
package types

import "time"

type FlagType string

const (
	Boolean FlagType = "BOOLEAN"
	String  FlagType = "STRING"
	Integer FlagType = "INTEGER"
	Float   FlagType = "FLOAT"
	JSON    FlagType = "JSON"
)

type Project struct {
	ID        string    `json:"id"`
	Key       string    `json:"key"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Environment struct {
	ID        string `json:"id"`
	ProjectID string `json:"project_id"`
	Key       string `json:"key"`
	Name      string `json:"name"`
	Protected bool   `json:"protected"`
	Version   int64  `json:"version"`
}

type FeatureFlag struct {
	ID          string    `json:"id"`
	ProjectID   string    `json:"project_id"`
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Type        FlagType  `json:"type"`
	Status      string    `json:"status"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Subject struct {
	Key        string         `json:"key"`
	Attributes map[string]any `json:"attributes,omitempty"`
}

type Condition struct {
	Attribute string `json:"attribute"`
	Operator  string `json:"operator"`
	Values    []any  `json:"values,omitempty"`
}

type Rule struct {
	ID         string      `json:"id"`
	Priority   int         `json:"priority"`
	Conditions []Condition `json:"conditions"`
	Segments   []string    `json:"segments,omitempty"`
	Variation  string      `json:"variation,omitempty"`
}

type Segment struct {
	Key     string   `json:"key"`
	Name    string   `json:"name"`
	Include []string `json:"include,omitempty"`
	Exclude []string `json:"exclude,omitempty"`
	Rules   []Rule   `json:"rules"`
}

type RolloutVariation struct {
	VariationKey string `json:"variation_key"`
	Weight       int    `json:"weight"`
}

type Prerequisite struct {
	FlagKey   string `json:"flag_key"`
	Variation string `json:"variation"`
}

type FlagConfig struct {
	Key              string             `json:"key"`
	Type             FlagType           `json:"type"`
	Enabled          bool               `json:"enabled"`
	Variations       map[string]any     `json:"variations"`
	DefaultVariation string             `json:"default_variation"`
	OffVariation     string             `json:"off_variation"`
	Targets          map[string]string  `json:"targets,omitempty"`
	Exclude          []string           `json:"exclude,omitempty"`
	Rules            []Rule             `json:"rules,omitempty"`
	Rollout          []RolloutVariation `json:"rollout,omitempty"`
	Prerequisites    []Prerequisite     `json:"prerequisites,omitempty"`
	Version          int64              `json:"version"`
}

type Snapshot struct {
	Project     string                `json:"project"`
	Environment string                `json:"environment"`
	Version     int64                 `json:"version"`
	UpdatedAt   time.Time             `json:"updated_at"`
	Flags       map[string]FlagConfig `json:"flags"`
	Segments    map[string]Segment    `json:"segments"`
}

type EvaluationResult struct {
	Value     any    `json:"value"`
	Variation string `json:"variation,omitempty"`
	Reason    string `json:"reason"`
	RuleID    string `json:"rule_id,omitempty"`
	Version   int64  `json:"version"`
}

type Event struct {
	EnvironmentID string    `json:"environment_id"`
	Version       int64     `json:"version"`
	CreatedAt     time.Time `json:"created_at"`
}

type EvaluationEvent struct {
	Flag      string    `json:"flag"`
	Variation string    `json:"variation"`
	Reason    string    `json:"reason"`
	Timestamp time.Time `json:"timestamp"`
}

type Audit struct {
	Diff        []string  `json:"diff,omitempty"`
	ID          int64     `json:"id"`
	Actor       string    `json:"actor"`
	Action      string    `json:"action"`
	Project     string    `json:"project"`
	Environment string    `json:"environment,omitempty"`
	Flag        string    `json:"flag,omitempty"`
	Before      any       `json:"before"`
	After       any       `json:"after"`
	Timestamp   time.Time `json:"timestamp"`
}
