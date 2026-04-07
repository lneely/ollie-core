package main

import (
	"testing"
)

func TestRangesOverlap(t *testing.T) {
	tests := []struct {
		name   string
		ranges []lineRange
		ws, we int
		want   bool
	}{
		{"empty ranges", nil, 1, 10, false},
		{"exact match", []lineRange{{1, 10}}, 1, 10, true},
		{"query contained in range", []lineRange{{1, 100}}, 20, 30, true},
		{"range contained in query", []lineRange{{20, 30}}, 1, 100, true},
		{"adjacent before, no overlap", []lineRange{{1, 10}}, 11, 20, false},
		{"adjacent after, no overlap", []lineRange{{11, 20}}, 1, 10, false},
		{"overlap at start", []lineRange{{5, 15}}, 1, 10, true},
		{"overlap at end", []lineRange{{5, 15}}, 10, 20, true},
		{"single line match", []lineRange{{5, 5}}, 5, 5, true},
		{"single line no match", []lineRange{{5, 5}}, 6, 6, false},
		{"multiple ranges, one overlaps", []lineRange{{1, 5}, {20, 30}}, 25, 35, true},
		{"multiple ranges, none overlap", []lineRange{{1, 5}, {20, 30}}, 10, 15, false},
		{"boundary: last line of range", []lineRange{{1, 10}}, 10, 10, true},
		{"boundary: first line of range", []lineRange{{10, 20}}, 10, 10, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rangesOverlap(tt.ranges, tt.ws, tt.we)
			if got != tt.want {
				t.Errorf("rangesOverlap(%v, %d, %d) = %v; want %v",
					tt.ranges, tt.ws, tt.we, got, tt.want)
			}
		})
	}
}

func TestRangesCover(t *testing.T) {
	tests := []struct {
		name   string
		ranges []lineRange
		ws, we int
		want   bool
	}{
		{"empty ranges", nil, 1, 10, false},
		{"exact match", []lineRange{{1, 10}}, 1, 10, true},
		{"superset covers", []lineRange{{1, 100}}, 20, 30, true},
		{"single line covered", []lineRange{{5, 5}}, 5, 5, true},
		{"single line not covered", []lineRange{{5, 5}}, 6, 6, false},
		{"contiguous ranges cover", []lineRange{{1, 5}, {6, 10}}, 1, 10, true},
		{"overlapping ranges cover", []lineRange{{1, 7}, {5, 10}}, 1, 10, true},
		{"gap leaves target uncovered", []lineRange{{1, 5}, {7, 10}}, 1, 10, false},
		{"partial coverage from start", []lineRange{{1, 5}}, 1, 10, false},
		{"partial coverage at end only", []lineRange{{5, 10}}, 1, 10, false},
		{"target starts before coverage", []lineRange{{5, 10}}, 3, 10, false},
		{"target starts mid-coverage", []lineRange{{1, 10}}, 5, 10, true},
		{"non-contiguous covers when sorted", []lineRange{{6, 10}, {1, 5}}, 1, 10, true},
		{"three ranges no gap", []lineRange{{1, 3}, {4, 6}, {7, 10}}, 1, 10, true},
		{"range write: only that range read", []lineRange{{50, 100}}, 50, 100, true},
		{"range write: superset read", []lineRange{{1, 200}}, 50, 100, true},
		{"range write: not read", []lineRange{{1, 49}}, 50, 100, false},
		{"range write: partial lower", []lineRange{{50, 75}}, 50, 100, false},
		{"range write: partial upper", []lineRange{{75, 100}}, 50, 100, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := rangesCover(tt.ranges, tt.ws, tt.we)
			if got != tt.want {
				t.Errorf("rangesCover(%v, %d, %d) = %v; want %v",
					tt.ranges, tt.ws, tt.we, got, tt.want)
			}
		})
	}
}
