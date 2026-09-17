package calculation

import (
	"math"
	"testing"
)

func TestEvaluate(t *testing.T) {
	tests := []struct {
		name    string
		formula string
		values  map[string]float64
		want    float64
		wantErr bool
	}{
		{
			name:    "simple subtraction",
			formula: "{revenue} - {cogs}",
			values:  map[string]float64{"revenue": 1000, "cogs": 600},
			want:    400,
		},
		{
			name:    "gross margin %",
			formula: "({revenue} - {cogs}) / {revenue} * 100",
			values:  map[string]float64{"revenue": 1000, "cogs": 600},
			want:    40,
		},
		{
			name:    "constant formula",
			formula: "42",
			values:  map[string]float64{},
			want:    42,
		},
		{
			name:    "nested parentheses",
			formula: "({a} + ({b} * {c}))",
			values:  map[string]float64{"a": 2, "b": 3, "c": 4},
			want:    14,
		},
		{
			name:    "unary minus",
			formula: "-{cogs}",
			values:  map[string]float64{"cogs": 500},
			want:    -500,
		},
		{
			name:    "zero denominator",
			formula: "{a} / {b}",
			values:  map[string]float64{"a": 10, "b": 0},
			wantErr: true,
		},
		{
			name:    "missing metric ref",
			formula: "{revenue} - {missing}",
			values:  map[string]float64{"revenue": 1000},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Evaluate(tt.formula, tt.values)
			if tt.wantErr {
				if err == nil {
					t.Errorf("expected error, got nil (result=%v)", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if math.Abs(got-tt.want) > 1e-9 {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}
