package judge

import "testing"

// The parser is the one part of the judge that runs on every graded answer and
// never touches the network, so it is also the one part that can be tested. A
// parser that quietly returned zeroes would report the service as hallucinating
// on every question, which is a finding-shaped bug.
func TestParseScores(t *testing.T) {
	tests := []struct {
		name    string
		raw     string
		want    Scores
		wantErr bool
	}{
		{
			name: "bare json",
			raw:  `{"faithfulness": 2, "correctness": 1, "reason": "hedged"}`,
			want: Scores{Faithfulness: 2, Correctness: 1, Reason: "hedged"},
		},
		{
			name: "wrapped in prose and a code fence",
			raw:  "Here is my grading:\n```json\n{\"faithfulness\": 0, \"correctness\": 0, \"reason\": \"unsupported\"}\n```\nHope that helps.",
			want: Scores{Faithfulness: 0, Correctness: 0, Reason: "unsupported"},
		},
		{
			name: "zero scores are preserved, not treated as missing",
			raw:  `{"faithfulness": 0, "correctness": 0, "reason": "fabricated"}`,
			want: Scores{Faithfulness: 0, Correctness: 0, Reason: "fabricated"},
		},
		{
			name:    "missing a score is an error, not a zero",
			raw:     `{"correctness": 2, "reason": "no faithfulness field"}`,
			wantErr: true,
		},
		{
			name:    "score above the scale is rejected rather than clamped",
			raw:     `{"faithfulness": 5, "correctness": 2, "reason": "five point habit"}`,
			wantErr: true,
		},
		{
			name:    "negative score rejected",
			raw:     `{"faithfulness": -1, "correctness": 2, "reason": "nonsense"}`,
			wantErr: true,
		},
		{
			name:    "no json at all",
			raw:     "I am unable to grade this answer.",
			wantErr: true,
		},
		{
			name:    "malformed json",
			raw:     `{"faithfulness": 2, "correctness":}`,
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseScores(tt.raw)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("ParseScores(%q) = %+v, want error", tt.raw, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseScores(%q) returned error: %v", tt.raw, err)
			}
			if got != tt.want {
				t.Errorf("ParseScores(%q) = %+v, want %+v", tt.raw, got, tt.want)
			}
		})
	}
}
