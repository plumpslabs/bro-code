package plan

import "encoding/json"

// PlanStep represents a single task unit within a structured plan.
type PlanStep struct {
	ID          string   `json:"id"`
	Description string   `json:"description"`
	Status      string   `json:"status"` // "pending" | "in_progress" | "done" | "blocked" | "skipped"
	Files       []string `json:"files,omitempty"`
}

// Plan represents an explicit execution plan for complex multi-file tasks.
type Plan struct {
	Goal  string     `json:"goal"`
	Steps []PlanStep `json:"steps"`
}

func (p *Plan) ToJSON() string {
	b, _ := json.MarshalIndent(p, "", "  ")
	return string(b)
}

func FromJSON(data string) (*Plan, error) {
	var p Plan
	err := json.Unmarshal([]byte(data), &p)
	return &p, err
}
