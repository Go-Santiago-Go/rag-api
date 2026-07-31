package handler

import (
	"embed"
	"net/http"
)

// demoPage is the single-page demo UI, compiled into the binary.
//
// Embedding rather than serving from disk is what makes this work in the
// deployed image: the final stage is distroless, so there is no filesystem to
// copy a loose asset into and no shell to arrange one. The page ships inside the
// same binary the Dockerfile already builds, which keeps the demo part of the
// deployed artifact instead of a second thing to host and keep in sync.
//
//go:embed index.html
var demoPage embed.FS

// Demo serves the browser demo at the site root.
//
// The API is the product and this page is a client of it: it calls POST /query
// like any other caller and renders whatever comes back. Nothing about the
// response shape is special-cased here, so the page cannot drift into showing
// something the API does not actually return.
func Demo() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		// ServeFileFS handles Content-Type, conditional requests and range
		// requests, which hand-writing the bytes would not.
		http.ServeFileFS(w, r, demoPage, "index.html")
	}
}
