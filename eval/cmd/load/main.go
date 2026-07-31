// Command load ingests the evaluation corpus into a running go-rag-api instance.
//
// It posts each document through the real POST /ingest endpoint rather than
// calling the service directly. Ingestion is the path under test: chunking
// decides what can be retrieved at all, so there is no dead weight to route
// around, and it runs once per configuration rather than once per question.
// Retrieval is measured lower down (see eval/cmd/run), where generation would
// otherwise sit inside a metric it cannot influence.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/go-santiago-go/go-rag-api/internal/store"
)

type ingestRequest struct {
	DocumentID string `json:"document_id"`
	Text       string `json:"text"`
}

func main() {
	corpus := flag.String("corpus", "eval/corpus", "directory holding the corpus, one subdirectory per section")
	baseURL := flag.String("url", "http://localhost:8080", "base URL of a running go-rag-api")
	reset := flag.Bool("reset", true, "truncate stored chunks before loading")
	flag.Parse()

	if err := run(context.Background(), *corpus, *baseURL, *reset); err != nil {
		slog.Error("load failed", "err", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, corpus, baseURL string, reset bool) error {
	docs, err := collect(corpus)
	if err != nil {
		return err
	}
	if len(docs) == 0 {
		return fmt.Errorf("no documents found under %s", corpus)
	}

	if reset {
		if err := resetStore(ctx); err != nil {
			return fmt.Errorf("reset store: %w", err)
		}
	}

	// Ingest is synchronous and embeds every chunk, so this is the slow part.
	// Timing it is worth doing: the projected duration from a per-chunk embed
	// rate is the first thing a real run confirms or corrects.
	start := time.Now()
	for i, d := range docs {
		if err := ingest(ctx, baseURL, d); err != nil {
			// Fail fast. A partially loaded corpus still answers queries, it just
			// answers them against a corpus nobody can describe, which is the exact
			// failure this harness exists to prevent.
			return fmt.Errorf("ingest %s (%d of %d): %w", d.DocumentID, i+1, len(docs), err)
		}
		slog.Info("ingested", "document_id", d.DocumentID, "progress", fmt.Sprintf("%d/%d", i+1, len(docs)))
	}

	elapsed := time.Since(start)
	slog.Info("corpus loaded",
		"documents", len(docs),
		"elapsed", elapsed.Round(time.Second),
		"per_document", (elapsed / time.Duration(len(docs))).Round(time.Millisecond),
	)
	return nil
}

// collect walks the corpus and pairs each markdown file with the document ID it
// will be stored and cited under.
func collect(corpus string) ([]ingestRequest, error) {
	var docs []ingestRequest
	err := filepath.WalkDir(corpus, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".md") {
			return nil
		}

		rel, err := filepath.Rel(corpus, path)
		if err != nil {
			return err
		}
		// Corpus documents live in section subdirectories. Markdown at the top
		// level is repository documentation (PROVENANCE.md), not corpus content.
		if !strings.Contains(filepath.ToSlash(rel), "/") {
			return nil
		}

		text, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		docs = append(docs, ingestRequest{
			// The document ID is the corpus-relative path without its extension,
			// so architecture/cgroups.md becomes "architecture/cgroups". This is
			// the string the golden set labels against. It was chosen because it
			// survives re-chunking: chunk IDs are a function of the chunking
			// strategy, so labelling at that level would invalidate the golden set
			// at exactly the experiment it exists to support.
			DocumentID: strings.TrimSuffix(filepath.ToSlash(rel), ".md"),
			Text:       string(text),
		})
		return nil
	})
	return docs, err
}

func ingest(ctx context.Context, baseURL string, doc ingestRequest) error {
	body, err := json.Marshal(doc)
	if err != nil {
		return err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, baseURL+"/ingest", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")

	// Embedding every chunk of a long document takes well past the default
	// client timeout of none-but-the-server's, so give it room explicitly.
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusCreated {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("status %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// resetStore empties the chunks table so a load starts from a known corpus.
// Without it a second run silently doubles every document, and recall would then
// be measured against a corpus that matches nothing on disk.
func resetStore(ctx context.Context) error {
	// The eval harness only ever runs against local docker-compose, so it reads
	// DATABASE_URL with the same default as the server rather than reimplementing
	// the server's cloud PG* resolution.
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://postgres:localdev@localhost:5432/go_rag_api?sslmode=disable"
	}

	pg, err := store.NewPostgres(ctx, dsn)
	if err != nil {
		return err
	}
	if err := pg.Reset(ctx); err != nil {
		return err
	}
	slog.Info("store reset")
	return nil
}
