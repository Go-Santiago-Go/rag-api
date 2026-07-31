// Command run measures retrieval quality against the golden set and reports
// recall@k at two granularities.
//
// It calls store.Search directly rather than posting to /query. Recall depends
// on embedding and search; generation cannot change which chunks came back, so
// including it would put a nondeterministic, billed component inside a
// measurement it provably cannot influence. Nothing is faked: this is the real
// Bedrock embedder against the real pgvector index.
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
	"github.com/go-santiago-go/rag-api/internal/service"
	"github.com/go-santiago-go/rag-api/internal/store"
)

// cutoffs are the values of k reported. They are all read from a single search
// at the deepest cutoff: a ranked list of 20 already contains the top 5 as its
// first five entries, so the shorter cutoffs cost nothing extra.
var cutoffs = []int{1, 3, 5, 10, 20}

// Corpus shape, used only to print the floor a retriever that has learned
// nothing would score. Document-level scoring picks 1 of 45; passage-level
// picks 1 of 1,126, which is why the two metrics are not comparable to each
// other and only comparable to themselves across runs.
const (
	corpusDocuments = 45
	corpusChunks    = 1126
)

// tally counts, for one population of questions, how many had a hit inside the
// top k.
type tally struct {
	total int
	hits  map[int]int
}

func newTally() *tally { return &tally{hits: make(map[int]int)} }

func (t *tally) record(firstRank int) {
	t.total++
	for _, k := range cutoffs {
		if firstRank > 0 && firstRank <= k {
			t.hits[k]++
		}
	}
}

// recall returns the fraction of questions that hit inside the top k, or 0 for
// an empty population.
func (t *tally) recall(k int) float64 {
	if t.total == 0 {
		return 0
	}
	return float64(t.hits[k]) / float64(t.total)
}

// tallySet holds the overall population plus one tally per question type.
type tallySet struct {
	overall *tally
	byType  map[string]*tally
}

func newTallySet() *tallySet {
	return &tallySet{overall: newTally(), byType: map[string]*tally{}}
}

func (s *tallySet) record(qType string, firstRank int) {
	s.overall.record(firstRank)
	if _, ok := s.byType[qType]; !ok {
		s.byType[qType] = newTally()
	}
	s.byType[qType].record(firstRank)
}

func main() {
	goldenPath := flag.String("golden", "eval/golden.json", "path to the labelled question set")
	dsn := flag.String("dsn", "", "Postgres DSN; defaults to DATABASE_URL then the docker-compose default")
	verbose := flag.Bool("verbose", false, "print per-question ranks")
	paraphrased := flag.Bool("paraphrase", false, "ask identifier questions in their token-free phrasing")
	rerank := flag.Bool("rerank", false, "reorder the retrieved candidates with the cross-encoder")
	flag.Parse()

	if err := run(context.Background(), *goldenPath, *dsn, *verbose, *paraphrased, *rerank); err != nil {
		fmt.Fprintln(os.Stderr, "eval failed:", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, goldenPath, dsn string, verbose, paraphrased, rerank bool) error {
	set, err := golden.Load(goldenPath, paraphrased)
	if err != nil {
		return err
	}

	embedder, vs, reranker, err := dependencies(ctx, dsn)
	if err != nil {
		return err
	}

	// The deepest cutoff is the retrieval depth: every shorter cutoff is a
	// prefix of the same ranked list.
	depth := cutoffs[len(cutoffs)-1]

	docs, passages := newTallySet(), newTallySet()

	if verbose {
		fmt.Printf("  %-9s %-11s %-44s %-8s %s\n", "id", "type", "expected document", "doc", "passage")
	}

	start := time.Now()
	for _, q := range set.Questions {
		text := q.Text(paraphrased)
		matches, err := search(ctx, embedder, vs, text, depth)
		if err != nil {
			return fmt.Errorf("question %s: %w", q.ID, err)
		}

		// Rerank the whole candidate list rather than trimming it to topK. The
		// set is then unchanged and only its order moves, so recall@20 must come
		// back identical to the dense run and every difference at a shallower
		// cutoff is attributable to reordering alone. A shifted recall@20 means
		// the harness is wrong, not that the cross-encoder is good.
		if rerank {
			matches, err = reranker.Rerank(ctx, text, matches, len(matches))
			if err != nil {
				return fmt.Errorf("question %s: %w", q.ID, err)
			}
		}

		docRank, passageRank := golden.Ranks(matches, q)
		docs.record(q.Type, docRank)
		passages.record(q.Type, passageRank)

		if verbose {
			fmt.Printf("  %-9s %-11s %-44s %-8s %s\n",
				q.ID, q.Type, q.ExpectedDocumentID, rankLabel(docRank), rankLabel(passageRank))
		}
	}

	report(set, docs, passages, time.Since(start), paraphrased, rerank)
	return nil
}

func search(ctx context.Context, embedder service.Embedder, vs store.VectorStore, q string, depth int) ([]store.Match, error) {
	embedding, err := embedder.Embed(ctx, q)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	matches, err := vs.Search(ctx, embedding, depth)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return matches, nil
}

// dependencies builds the same concrete embedder and store that cmd/server
// wires up, which is what lets the harness measure the real retrieval path
// without going through HTTP.
func dependencies(ctx context.Context, dsn string) (service.Embedder, store.VectorStore, service.Reranker, error) {
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
		return nil, nil, nil, fmt.Errorf("connect postgres: %w", err)
	}

	cfg, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load aws config: %w", err)
	}
	client := bedrockruntime.NewFromConfig(cfg)
	return service.NewBedrockEmbedder(client), pg, service.NewBedrockReranker(client), nil
}

func rankLabel(rank int) string {
	if rank == 0 {
		return "miss"
	}
	return fmt.Sprintf("%d", rank)
}

func report(set golden.Set, docs, passages *tallySet, elapsed time.Duration, paraphrased, rerank bool) {
	phrasing := "original"
	if paraphrased {
		phrasing = "paraphrased"
	}
	retrieval := "dense"
	if rerank {
		retrieval = "dense+rerank"
	}
	fmt.Printf("\nquestions: %d   corpus: %s   phrasing: %s   retrieval: %s   elapsed: %s\n",
		docs.overall.total, short(set.CorpusCommit), phrasing, retrieval, elapsed.Round(time.Millisecond))

	w := tabwriter.NewWriter(os.Stdout, 0, 0, 3, ' ', 0)
	header := []string{"metric", "population", "n"}
	for _, k := range cutoffs {
		header = append(header, fmt.Sprintf("recall@%d", k))
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, strings.Join(header, "\t"))
	writeSet(w, "document", docs)
	writeSet(w, "passage", passages)
	w.Flush()

	fmt.Printf("\nrandom floor at k=5:  document ~%.1f%% (5 of %d)   passage ~%.1f%% (5 of %d)\n",
		5.0/corpusDocuments*100, corpusDocuments, 5.0/corpusChunks*100, corpusChunks)
	fmt.Printf("reranker headroom (recall@20 - recall@5):  document %.1f pts   passage %.1f pts\n",
		(docs.overall.recall(20)-docs.overall.recall(5))*100,
		(passages.overall.recall(20)-passages.overall.recall(5))*100)
}

func writeSet(w *tabwriter.Writer, metric string, s *tallySet) {
	writeRow(w, metric, "overall", s.overall)
	// Fixed order so one run's output diffs cleanly against another's.
	for _, name := range []string{"conceptual", "identifier"} {
		if t, ok := s.byType[name]; ok {
			writeRow(w, metric, name, t)
		}
	}
}

func writeRow(w *tabwriter.Writer, metric, name string, t *tally) {
	row := []string{metric, name, fmt.Sprint(t.total)}
	for _, k := range cutoffs {
		row = append(row, fmt.Sprintf("%.1f%%", t.recall(k)*100))
	}
	fmt.Fprintln(w, strings.Join(row, "\t"))
}

func short(commit string) string {
	if len(commit) > 7 {
		return commit[:7]
	}
	return strings.TrimSpace(commit)
}
