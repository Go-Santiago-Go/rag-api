// Package judge scores a generated answer against the passages it was given.
//
// Recall answers "did the right passage come back". It says nothing about what
// the model then did with it, so a service can retrieve perfectly and still
// contradict its own sources. This package measures that second half.
package judge

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/go-santiago-go/rag-api/eval/internal/golden"
	"github.com/go-santiago-go/rag-api/internal/service"
	"github.com/go-santiago-go/rag-api/internal/store"
)

// MaxScore is the top of the rubric. The scale is deliberately three points
// wide (0, 1, 2) rather than five.
//
// A five point scale collapses: graders cluster on 4 and the metric stops
// discriminating, which is the same failure as a retrieval baseline that scores
// 97% because the task was too easy. A metric that cannot fail cannot inform, so
// the rubric offers only "good", "flawed" and "wrong".
const MaxScore = 2

// Scores is one graded answer.
type Scores struct {
	// Faithfulness asks whether the answer is supported by the sources it was
	// given. This is the hallucination detector.
	Faithfulness int `json:"faithfulness"`
	// Correctness asks whether the answer states the labelled fact. It is scored
	// separately because the two failures are different bugs: an unfaithful
	// answer means the model ignored good retrieval, an incorrect but faithful
	// answer usually means retrieval never handed it the fact.
	Correctness int `json:"correctness"`
	// Reason is the judge's one-line justification, kept for spot-checking. A
	// score with no reason cannot be audited, and an unauditable judge is the
	// same decoration problem the harness exists to avoid.
	Reason string `json:"reason"`
}

// rubric is the grading instruction. It is a package constant rather than a
// per-call string because a prompt that drifts between runs makes two runs
// incomparable, exactly like changing chunk size mid-experiment.
const rubric = `You are grading one answer produced by a retrieval-augmented question answering service.

Grade strictly on two independent axes, each scored 0, 1 or 2.

FAITHFULNESS: is the ANSWER supported by the SOURCES?
  2 = every factual claim in the answer appears in the sources
  1 = mainly supported, but at least one factual detail is not in the sources
  0 = a claim contradicts the sources, or a central claim is absent from them
  A refusal such as "I don't know" asserts nothing, so it scores 2.
  Judge this ONLY against the SOURCES. The REFERENCE FACT is outside knowledge
  and must not be used here: an answer that is true in the world but unsupported
  by these sources is unfaithful.

CORRECTNESS: does the ANSWER convey the REFERENCE FACT?
  2 = states the reference fact correctly
  1 = partially correct, hedged into uselessness, or correct but presented as
      one option among wrong alternatives
  0 = absent, evasive, or contradicting the reference fact
  A refusal scores 0. Declining is honest but it is not an answer.

Reply with JSON only, no prose and no code fence:
{"faithfulness": <0-2>, "correctness": <0-2>, "reason": "<one short sentence>"}`

// Judge grades one answer. It takes a Generator rather than a Bedrock client so
// the caller chooses the judging model, and so tests can drive it with a fake.
func Judge(ctx context.Context, g service.Generator, q golden.Question, answer string, sources []store.Match) (Scores, error) {
	raw, err := g.Generate(ctx, buildPrompt(q, answer, sources))
	if err != nil {
		return Scores{}, fmt.Errorf("judge %s: %w", q.ID, err)
	}
	s, err := ParseScores(raw)
	if err != nil {
		return Scores{}, fmt.Errorf("judge %s: %w", q.ID, err)
	}
	return s, nil
}

func buildPrompt(q golden.Question, answer string, sources []store.Match) string {
	var b strings.Builder
	b.WriteString(rubric)
	fmt.Fprintf(&b, "\n\nQUESTION:\n%s\n", q.Question)
	fmt.Fprintf(&b, "\nREFERENCE FACT (verbatim from the answering passage):\n%s\n", q.ExpectedSubstring)
	if q.Notes != "" {
		fmt.Fprintf(&b, "\nREFERENCE NOTES:\n%s\n", q.Notes)
	}
	b.WriteString("\nSOURCES:\n")
	if len(sources) == 0 {
		b.WriteString("(none)\n")
	}
	for i, m := range sources {
		fmt.Fprintf(&b, "[%d] (%s) %s\n\n", i+1, m.DocumentID, m.Content)
	}
	fmt.Fprintf(&b, "ANSWER:\n%s\n", answer)
	return b.String()
}

// ParseScores extracts the judge's JSON verdict.
//
// It is lenient about surrounding prose (models add "Here is the grading:"
// however firmly you ask them not to) and strict about the values themselves.
// An out-of-range score is an error rather than a clamp: silently clamping a 5
// to a 2 would fabricate a grade the judge never gave, and the resulting number
// would look perfectly reasonable.
func ParseScores(raw string) (Scores, error) {
	start := strings.Index(raw, "{")
	end := strings.LastIndex(raw, "}")
	if start < 0 || end < start {
		return Scores{}, fmt.Errorf("no JSON object in judge reply: %q", truncate(raw))
	}

	// Decode into pointers so a missing field is distinguishable from an
	// explicit 0. A judge that omitted "faithfulness" would otherwise be
	// recorded as having scored it zero, turning a parse bug into a finding.
	var v struct {
		Faithfulness *int   `json:"faithfulness"`
		Correctness  *int   `json:"correctness"`
		Reason       string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw[start:end+1]), &v); err != nil {
		return Scores{}, fmt.Errorf("parse judge reply %q: %w", truncate(raw), err)
	}
	if v.Faithfulness == nil || v.Correctness == nil {
		return Scores{}, fmt.Errorf("judge reply missing a score: %q", truncate(raw))
	}
	for name, got := range map[string]int{"faithfulness": *v.Faithfulness, "correctness": *v.Correctness} {
		if got < 0 || got > MaxScore {
			return Scores{}, fmt.Errorf("%s score %d outside 0..%d", name, got, MaxScore)
		}
	}
	return Scores{Faithfulness: *v.Faithfulness, Correctness: *v.Correctness, Reason: v.Reason}, nil
}

func truncate(s string) string {
	const limit = 120
	s = strings.TrimSpace(s)
	if len(s) > limit {
		return s[:limit] + "..."
	}
	return s
}
