package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-santiago-go/rag-api/internal/service"
)

// ingestRequest is the JSON body POST /ingest accepts: the raw document text
// plus an ID the caller chooses. The ID groups this document's chunks so they
// can be cited back later.
type ingestRequest struct {
	DocumentID string `json:"document_id"`
	Text       string `json:"text"`
}

// Ingest returns a handler that makes a document searchable. It parses the
// request, hands the text to the ingest service (chunk -> embed -> store), and
// maps the outcome to a status code. All the work lives in the service; the
// handler only translates HTTP to and from that one call.
func Ingest(svc *service.IngestService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req ingestRequest
		// DisallowUnknownFields turns a mistyped field into a 400 instead of a
		// silent no-op, so a malformed client hears about it rather than
		// appearing to succeed while storing nothing.
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.DocumentID == "" || req.Text == "" {
			http.Error(w, "document_id and text are required", http.StatusBadRequest)
			return
		}

		if err := svc.Ingest(r.Context(), req.DocumentID, req.Text); err != nil {
			// Log the real cause server-side; return a generic message so we do
			// not leak Bedrock or database internals to the caller.
			slog.Error("ingest failed", "document_id", req.DocumentID, "err", err)
			http.Error(w, "ingest failed", http.StatusInternalServerError)
			return
		}

		// 201 Created: Ingest ran synchronously, so by the time we reach here the
		// chunks are embedded and written. 201 states that resources were created;
		// 202 Accepted would imply the work is still pending, which is not true
		// here. (If ingestion ever moves to a background worker, 202 becomes the
		// honest code.) No body to return, so none is written.
		w.WriteHeader(http.StatusCreated)
	}
}
