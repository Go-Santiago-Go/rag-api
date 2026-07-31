// Package golden holds the labelled question set and the label-matching rules
// shared by the evaluation commands.
//
// It exists so that cmd/run (retrieval recall) and cmd/judge (answer quality)
// cannot disagree about what a label means. Both ask "did the answering passage
// come back", and if two copies of that rule drifted, the two harnesses would be
// reporting numbers that look comparable and are not.
package golden

import (
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"

	"github.com/go-santiago-go/rag-api/internal/store"
)

// Question is one labelled entry in the golden set.
type Question struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	// Paraphrase asks the same thing without naming the rare token the label
	// matches on, and is set only on identifier questions. The first baseline
	// showed identifier questions outscoring conceptual ones, but most of those
	// questions contained their own answer's distinctive token, so the result
	// could have been measuring string overlap rather than retrieval. Running
	// the set both ways separates the two.
	Paraphrase         string `json:"paraphrase"`
	ExpectedDocumentID string `json:"expected_document_id"`
	// ExpectedSubstring is verbatim text from the answering passage. Document
	// labels turned out to saturate: 45 topically disjoint documents make "did
	// the right file come back" too easy to measure anything. A substring
	// identifies the one chunk in ~25 that actually answers the question, and
	// like a document ID it survives re-chunking, which chunk IDs do not.
	ExpectedSubstring string `json:"expected_substring"`
	// Type is "identifier" (the answer turns on a rare exact token) or
	// "conceptual" (ordinary language, nothing distinctive to match on). Recall
	// is reported per type because dense and sparse retrieval are expected to
	// fail in opposite directions, so the split is what turns a delta into an
	// explanation.
	Type  string `json:"type"`
	Notes string `json:"notes"`
}

// Set is a parsed golden file, pinned to the corpus commit it was labelled
// against.
type Set struct {
	CorpusCommit string     `json:"corpus_commit"`
	Questions    []Question `json:"questions"`
}

// Text returns the phrasing to ask with. Questions without a paraphrase are
// unaffected by the flag, so a paraphrased run changes only the identifier
// bucket and the conceptual rows stay directly comparable to the baseline.
func (q Question) Text(paraphrased bool) string {
	if paraphrased && q.Paraphrase != "" {
		return q.Paraphrase
	}
	return q.Question
}

// Load reads and validates the golden set. The paraphrased flag tightens
// validation rather than changing what is loaded.
func Load(path string, paraphrased bool) (Set, error) {
	var set Set
	raw, err := os.ReadFile(path)
	if err != nil {
		return set, fmt.Errorf("read golden set: %w", err)
	}
	if err := json.Unmarshal(raw, &set); err != nil {
		return set, fmt.Errorf("parse golden set: %w", err)
	}
	if len(set.Questions) == 0 {
		return set, fmt.Errorf("golden set %s contains no questions", path)
	}
	for _, q := range set.Questions {
		// An empty substring matches every chunk and would silently report
		// perfect passage recall, which is the most flattering possible bug.
		if q.ExpectedSubstring == "" {
			return set, fmt.Errorf("question %s has no expected_substring", q.ID)
		}
		// A missing paraphrase would silently fall back to the original wording,
		// mixing both phrasings into one number and hiding the very effect the
		// flag exists to isolate.
		if paraphrased && q.Type == "identifier" && q.Paraphrase == "" {
			return set, fmt.Errorf("identifier question %s has no paraphrase", q.ID)
		}
	}
	return set, nil
}

var whitespace = regexp.MustCompile(`\s+`)

// Normalize makes substring matching insensitive to case and to the line breaks
// that chunking and markdown reflowing introduce. Without it a label spanning a
// wrapped line would never match the stored chunk.
func Normalize(s string) string {
	return whitespace.ReplaceAllString(strings.ToLower(strings.TrimSpace(s)), " ")
}

// Ranks returns the rank at which the expected document first appeared and the
// rank at which the answering passage first appeared, or 0 for never.
//
// A passage hit requires both the right document and the expected text. The
// document check is what stops an incidental mention elsewhere in the corpus
// from counting as having found the answer.
func Ranks(matches []store.Match, q Question) (docRank, passageRank int) {
	want := Normalize(q.ExpectedSubstring)
	for i, m := range matches {
		if m.DocumentID != q.ExpectedDocumentID {
			continue
		}
		if docRank == 0 {
			docRank = i + 1 // ranks are 1-indexed for humans
		}
		if passageRank == 0 && strings.Contains(Normalize(m.Content), want) {
			passageRank = i + 1
		}
		if docRank > 0 && passageRank > 0 {
			break
		}
	}
	return docRank, passageRank
}

// Retrieved reports whether the answering passage is present anywhere in
// matches. cmd/judge uses it to split answers by whether the model was given
// what it needed, which is what separates "the model hallucinated" from "the
// model was never handed the answer".
func Retrieved(matches []store.Match, q Question) bool {
	_, passageRank := Ranks(matches, q)
	return passageRank > 0
}
