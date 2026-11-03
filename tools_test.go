package main

import "testing"

func TestDiagnosticContextMatchesAdvertisedBooleanDefault(t *testing.T) {
	for _, test := range []struct {
		input any
		want  int
	}{
		{nil, 0}, {false, 0}, {true, 5}, {float64(2), 2}, {3, 3}, {-1, 0},
	} {
		if got := diagnosticContextLines(test.input); got != test.want {
			t.Fatalf("contextLines(%v) = %d, want %d", test.input, got, test.want)
		}
	}
}
