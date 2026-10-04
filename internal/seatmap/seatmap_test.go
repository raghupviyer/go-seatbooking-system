package seatmap

import (
	"reflect"
	"testing"
)

func TestRuns(t *testing.T) {
	got := Runs([]string{"A10", "B1", "A9", "A11", "A13", "LOUNGE", "B2"})
	want := [][]string{{"A9", "A10", "A11"}, {"A13"}, {"B1", "B2"}, {"LOUNGE"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Runs = %v, want %v", got, want)
	}
}

func TestSuggest(t *testing.T) {
	tests := []struct {
		name      string
		available []string
		n         int
		want      []string
	}{
		{
			name:      "exact block preferred over a longer one",
			available: []string{"A1", "A2", "A3", "A4", "A5", "B1", "B2"},
			n:         2,
			want:      []string{"B1", "B2"},
		},
		{
			name:      "prefix of the longest block",
			available: []string{"A1", "A2", "A3", "A4", "A5", "B1"},
			n:         3,
			want:      []string{"A1", "A2", "A3"},
		},
		{
			name:      "longest blocks concatenated",
			available: []string{"A1", "A2", "A3", "B1", "B2", "C1"},
			n:         5,
			want:      []string{"A1", "A2", "A3", "B1", "B2"},
		},
		{
			name:      "remainder prefers an exact block",
			available: []string{"A1", "A2", "A3", "B1", "B2", "C1"},
			n:         4,
			want:      []string{"A1", "A2", "A3", "C1"},
		},
		{
			name:      "fewer available than requested",
			available: []string{"A1", "B5"},
			n:         3,
			want:      []string{"A1", "B5"},
		},
		{
			name:      "nothing available",
			available: nil,
			n:         2,
			want:      []string{},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Suggest(tt.available, tt.n); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Suggest = %v, want %v", got, tt.want)
			}
		})
	}
}
