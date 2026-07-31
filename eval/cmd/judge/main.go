// Command judge measures answer quality, the half of the pipeline recall cannot
// see.
//
// cmd/run stops at retrieval: it proves the answering passage came back and says
// nothing about what the model did with it. A service can retrieve perfectly and
// still contradict its own sources, and nothing in the recall table would move.
// This command runs the real /query pipeline in process, then grades the answer
// with a second, stronger model.
//
// Scores are split by whether retrieval actually found the answering passage.
// That split is the point: it separates "the model ignored good context" from
// "the model was never handed the answer", which are different bugs with
// different fixes.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/bedrockruntime"

	"github.com/go-santiago-go/rag-api/eval/internal/golden"
	"github.com/go-santiago-go/rag-api/eval/internal/judge"
	"github.com/go-santiago-go/rag-api/internal/service"
	"github.com/go-santiago-go/rag-api/internal/store"
)

// defaultJudgeModel is deliberately not the model that writes the answers
// (Haiku 4.5). Grading your own output invites self-preference bias, and a
// stronger grader is the cheap half of the pipeline: one judgement per question
// against a whole retrieval and generation run.
const defaultJudgeModel = "us.anthropic.claude-sonnet-4-6"

func main() {
	goldenPath := flag.String("golden", "eval/golden.json", "path to the labelled question set")
	dsn := flag.String("dsn", "", "Postgres DSN; defaults to DATABASE_URL then the docker-compose default")
	model := flag.String("model", defaultJudgeModel, "Bedrock model ID used to grade answers")
	paraphrased := flag.Bool("paraphrase", false, "ask identifier questions in their token-free phrasing")
	limit := flag.Int("n", 0, "grade only the first n questions (0 = all)")
	validate := flag.Bool("validate", false, "grade deliberately broken answers to prove the judge discriminates")
	verbose := flag.Bool("verbose", false, "print per-question scores and the judge's reasoning")
	flag.Parse()

	if err := run(context.Background(), opts{
		goldenPath: *goldenPath, dsn: *dsn, model: *model,
		paraphrased: *paraphrased, limit: *limit, validate: *validate, verbose: *verbose,
	}); err != nil {
		fmt.Fprintln(os.Stderr, "judge failed:", err)
		os.Exit(1)
	}
}

type opts struct {
	goldenPath, dsn, model         string
	paraphrased, validate, verbose bool
	limit                          int
}

func run(ctx context.Context, o opts) error {
	set, err := golden.Load(o.goldenPath, o.paraphrased)
	if err != nil {
		return err
	}
	questions := set.Questions
	if o.limit > 0 && o.limit < len(questions) {
		questions = questions[:o.limit]
	}

	querySvc, judgeGen, err := dependencies(ctx, o.dsn, o.model)
	if err != nil {
		return err
	}

	started := time.Now()
	if o.validate {
		return validateJudge(ctx, querySvc, judgeGen, questions, o, started)
	}
	return gradeAnswers(ctx, querySvc, judgeGen, set, questions, o, started)
}

// gradeAnswers runs the production query path for each question and grades what
// comes out.
func gradeAnswers(ctx context.Context, svc *service.QueryService, judgeGen service.Generator,
	set golden.Set, questions []golden.Question, o opts, started time.Time) error {

	segments := map[string]*aggregate{
		"retrieved":     newAggregate(),
		"not retrieved": newAggregate(),
	}
	overall := newAggregate()

	for _, q := range questions {
		answer, err := svc.Query(ctx, q.Text(o.paraphrased))
		if err != nil {
			return fmt.Errorf("query %s: %w", q.ID, err)
		}
		scores, err := judge.Judge(ctx, judgeGen, q, answer.Text, answer.Sources)
		if err != nil {
			return err
		}

		segment := "not retrieved"
		if golden.Retrieved(answer.Sources, q) {
			segment = "retrieved"
		}
		segments[segment].record(scores)
		overall.record(scores)

		if o.verbose {
			fmt.Printf("%-8s %-14s faith=%d correct=%d  %s\n",
				q.ID, segment, scores.Faithfulness, scores.Correctness, scores.Reason)
		}
	}

	fmt.Printf("\nquestions: %d   corpus: %s   phrasing: %s   judge: %s   elapsed: %s\n",
		overall.n, short(set.CorpusCommit), phrasing(o.paraphrased), o.model,
		time.Since(started).Round(time.Second))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "\nsegment\tn\tfaithfulness\tcorrectness")
	// Fixed order so one run diffs cleanly against another.
	for _, name := range []string{"retrieved", "not retrieved"} {
		segments[name].writeRow(w, name)
	}
	overall.writeRow(w, "overall")
	w.Flush()

	fmt.Printf("\nscale: 0-%d per axis.\n", judge.MaxScore)
	fmt.Printf("faithfulness spread:  %s\n", dist(overall.faithfulDist))
	fmt.Printf("correctness spread:   %s\n", dist(overall.correctDist))
	fmt.Printf("unfaithful answers (faithfulness 0): %d of %d\n", overall.zeroFaithful, overall.n)
	if overall.noReason > 0 {
		fmt.Printf("scores returned without a reason (unauditable): %d of %d\n", overall.noReason, overall.n)
	}
	fmt.Println("\nThe 'not retrieved' row is the one to read: correctness should collapse (the")
	fmt.Println("answer was never in the context) while faithfulness stays high (the model said")
	fmt.Println("so instead of inventing one). Faithfulness falling here means hallucination.")
	return nil
}

// fabrication is a specific, plausible, and entirely invented claim. It has to
// be absent from the corpus for the test to mean anything: a vague addition
// might genuinely be supported somewhere in the sources, which would make a
// "failed to detect" result unreadable.
const fabrication = " This behaviour is controlled by the --enable-legacy-resolver flag, " +
	"which defaults to true on clusters older than v1.21."

const refusal = "I don't know based on the provided context."

// validateJudge grades deliberately broken answers to prove the judge can tell
// them from good ones.
//
// This is the check on the check. A judge that scores everything highly is
// indistinguishable from a service that answers everything well, and the
// resulting table would look like a triumph. Each variant targets one axis and
// leaves the other alone, so a judge that merely dislikes odd input is
// distinguishable from one that is actually reading.
func validateJudge(ctx context.Context, svc *service.QueryService, judgeGen service.Generator,
	questions []golden.Question, o opts, started time.Time) error {

	if len(questions) < 2 {
		return fmt.Errorf("validation needs at least 2 questions, got %d", len(questions))
	}

	variants := []string{"clean", "swapped sources", "fabrication appended", "refusal"}
	results := map[string]*aggregate{}
	for _, v := range variants {
		results[v] = newAggregate()
	}

	// Answers are generated once and reused across variants so that every
	// variant grades the same prose. Regenerating per variant would let normal
	// model nondeterminism masquerade as a variant effect.
	type generated struct {
		q       golden.Question
		answer  string
		sources []store.Match
	}
	var gens []generated
	for _, q := range questions {
		answer, err := svc.Query(ctx, q.Text(o.paraphrased))
		if err != nil {
			return fmt.Errorf("query %s: %w", q.ID, err)
		}
		gens = append(gens, generated{q: q, answer: answer.Text, sources: answer.Sources})
	}

	for i, g := range gens {
		// Sources from the next question, wrapping around. The answer is still
		// the correct answer to its own question, so only faithfulness should
		// move: correctness is measured against the reference fact, which the
		// answer still states.
		other := gens[(i+1)%len(gens)].sources

		cases := []struct {
			variant string
			answer  string
			sources []store.Match
		}{
			{"clean", g.answer, g.sources},
			{"swapped sources", g.answer, other},
			{"fabrication appended", g.answer + fabrication, g.sources},
			{"refusal", refusal, g.sources},
		}

		for _, c := range cases {
			scores, err := judge.Judge(ctx, judgeGen, g.q, c.answer, c.sources)
			if err != nil {
				return err
			}
			results[c.variant].record(scores)
			if o.verbose {
				fmt.Printf("%-8s %-22s faith=%d correct=%d  %s\n",
					g.q.ID, c.variant, scores.Faithfulness, scores.Correctness, scores.Reason)
			}
		}
	}

	fmt.Printf("\nvalidation: %d questions x %d variants   judge: %s   elapsed: %s\n",
		len(gens), len(variants), o.model, time.Since(started).Round(time.Second))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	fmt.Fprintln(w, "\nvariant\tn\tfaithfulness\tcorrectness")
	for _, v := range variants {
		results[v].writeRow(w, v)
	}
	w.Flush()

	// Each assertion names the axis its variant was built to move. Reporting the
	// separation as a pass or fail rather than a table of numbers is what makes
	// this a gate instead of another thing to eyeball.
	clean := results["clean"]
	checks := []struct {
		name string
		ok   bool
		got  string
	}{
		{
			"swapped sources lowers faithfulness",
			results["swapped sources"].meanFaithful() < clean.meanFaithful(),
			fmt.Sprintf("%.2f vs clean %.2f", results["swapped sources"].meanFaithful(), clean.meanFaithful()),
		},
		{
			"fabrication lowers faithfulness",
			results["fabrication appended"].meanFaithful() < clean.meanFaithful(),
			fmt.Sprintf("%.2f vs clean %.2f", results["fabrication appended"].meanFaithful(), clean.meanFaithful()),
		},
		{
			"refusal scores zero correctness",
			results["refusal"].meanCorrect() == 0,
			fmt.Sprintf("%.2f", results["refusal"].meanCorrect()),
		},
		{
			"refusal keeps faithfulness high",
			results["refusal"].meanFaithful() > clean.meanFaithful()-0.5,
			fmt.Sprintf("%.2f vs clean %.2f", results["refusal"].meanFaithful(), clean.meanFaithful()),
		},
	}

	fmt.Println()
	failed := 0
	for _, c := range checks {
		status := "PASS"
		if !c.ok {
			status = "FAIL"
			failed++
		}
		fmt.Printf("%s  %-40s %s\n", status, c.name, c.got)
	}

	if failed > 0 {
		return fmt.Errorf("%d of %d discrimination checks failed; the judge is not reading the sources, "+
			"so its scores in the normal run mean nothing", failed, len(checks))
	}
	fmt.Println("\nThe judge separates good answers from broken ones, so its scores carry information.")
	return nil
}

// aggregate accumulates scores for one population.
type aggregate struct {
	n                 int
	faithful, correct int
	zeroFaithful      int
	// Score distributions, because a mean hides its own shape: 1.94 is one
	// zero among 32 or four ones among 32, and those are different findings.
	faithfulDist, correctDist [judge.MaxScore + 1]int
	// noReason counts verdicts the judge returned without justification. Such a
	// score cannot be spot-checked, and an unauditable score is exactly the kind
	// of number this harness exists to distrust.
	noReason int
}

func newAggregate() *aggregate { return &aggregate{} }

func (a *aggregate) record(s judge.Scores) {
	a.n++
	a.faithful += s.Faithfulness
	a.correct += s.Correctness
	a.faithfulDist[s.Faithfulness]++
	a.correctDist[s.Correctness]++
	if s.Faithfulness == 0 {
		a.zeroFaithful++
	}
	if strings.TrimSpace(s.Reason) == "" {
		a.noReason++
	}
}

// dist renders a score distribution as "0:1 1:2 2:32".
func dist(d [judge.MaxScore + 1]int) string {
	parts := make([]string, 0, len(d))
	for score, count := range d {
		parts = append(parts, fmt.Sprintf("%d:%d", score, count))
	}
	return strings.Join(parts, " ")
}

func (a *aggregate) meanFaithful() float64 { return mean(a.faithful, a.n) }
func (a *aggregate) meanCorrect() float64  { return mean(a.correct, a.n) }

func mean(total, n int) float64 {
	if n == 0 {
		return 0
	}
	return float64(total) / float64(n)
}

func (a *aggregate) writeRow(w *tabwriter.Writer, name string) {
	if a.n == 0 {
		fmt.Fprintf(w, "%s\t0\t-\t-\n", name)
		return
	}
	fmt.Fprintf(w, "%s\t%d\t%.2f\t%.2f\n", name, a.n, a.meanFaithful(), a.meanCorrect())
}

// dependencies builds the same query pipeline cmd/server wires up, plus the
// separate judging generator. Grading the real service rather than a
// reimplementation is the whole point: a harness that rebuilds the pipeline
// measures the harness.
func dependencies(ctx context.Context, dsn, judgeModel string) (*service.QueryService, service.Generator, error) {
	if dsn == "" {
		dsn = os.Getenv("DATABASE_URL")
	}
	if dsn == "" {
		// The harness only runs against local docker-compose, so it does not
		// reimplement the server's cloud PG* resolution.
		dsn = "postgres://postgres:localdev@localhost:5432/go_rag_api?sslmode=disable"
	}

	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		return nil, nil, fmt.Errorf("connect postgres: %w", err)
	}
	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("load aws config: %w", err)
	}
	client := bedrockruntime.NewFromConfig(cfg)

	svc := service.NewQueryService(
		service.NewBedrockEmbedder(client),
		pg,
		service.NewBedrockReranker(client),
		service.NewBedrockGenerator(client),
	)
	return svc, service.NewBedrockGeneratorWithModel(client, judgeModel), nil
}

func phrasing(paraphrased bool) string {
	if paraphrased {
		return "paraphrased"
	}
	return "original"
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return strings.TrimSpace(commit)
}
