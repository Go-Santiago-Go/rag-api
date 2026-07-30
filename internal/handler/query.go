package handler

import (
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/go-santiago-go/go-rag-api/internal/service"
)

type queryRequest struct {
	Question string `json:"question"`
}

type source struct {
	Content    string `json:"content"`
	DocumentID string `json:"document_id"`
}

type queryResponse struct {
	Answer  string   `json:"answer"`
	Sources []source `json:"sources"`
}

func Query(svc *service.QueryService) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req queryRequest
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil {
			http.Error(w, "invalid JSON body", http.StatusBadRequest)
			return
		}
		if req.Question == "" {
			http.Error(w, "question is required", http.StatusBadRequest)
			return
		}

		answer, err := svc.Query(r.Context(), req.Question)
		if err != nil {
			slog.Error("query failed", "err", err)
			http.Error(w, "query failed", http.StatusInternalServerError)
			return
		}

		sources := make([]source, 0, len(answer.Sources))
		for _, m := range answer.Sources {
			sources = append(sources, source{
				Content:    m.Content,
				DocumentID: m.DocumentID,
			})
		}
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(queryResponse{
			Answer:  answer.Text,
			Sources: sources,
		}); err != nil {
			slog.Error("encode query response", "err", err)
		}
	}
}
