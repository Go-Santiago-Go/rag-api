#!/usr/bin/env bash
# prepare.sh: turn Kubernetes website Hugo sources into plain markdown suitable
# for ingestion.
#
# The corpus in this directory is committed, not fetched, so the eval baseline
# stays comparable across runs. This script exists so that transformation is
# auditable and repeatable, not because it runs as part of the eval.
#
# Usage: ./prepare.sh <path-to-kubernetes-website-checkout>
set -euo pipefail

SRC="${1:?usage: prepare.sh <path-to-kubernetes-website-checkout>}"
CONCEPTS="$SRC/content/en/docs/concepts"
DEST="$(cd "$(dirname "$0")" && pwd)"

# The subset. Chosen for document count (the recall@k floor needs enough
# documents to be meaningful) and topical coherence.
SECTIONS=(architecture services-networking storage containers)

for section in "${SECTIONS[@]}"; do
  mkdir -p "$DEST/$section"
  find "$CONCEPTS/$section" -name '*.md' -type f | while read -r file; do
    out="$DEST/$section/$(basename "$file")"

    awk '
      # Strip the YAML front matter delimited by leading --- lines, but keep the
      # title: it is the document heading and real retrieval signal.
      NR == 1 && /^---[[:space:]]*$/ { in_fm = 1; next }
      in_fm && /^---[[:space:]]*$/   { in_fm = 0; next }
      in_fm && /^title:/ {
        sub(/^title:[[:space:]]*/, "")
        gsub(/^["'"'"']|["'"'"']$/, "")
        title = $0
        next
      }
      in_fm { next }
      # First line past the front matter: emit the captured title as an H1 so
      # the structure-aware chunker has a top-level heading to split on.
      !emitted && title { print "# " title; print ""; emitted = 1 }
      { print }
    ' "$file" |
      # Unwrap Hugo shortcodes. A glossary tooltip carrying text="Pods" renders
      # as that text, so keep it; otherwise fall back to the term id. Every
      # other shortcode is layout, not prose, and is dropped.
      perl -0777 -pe 's/\{\{[<%]\s*glossary_tooltip[^>%]*?text="([^"]*)"[^>%]*?[>%]\}\}/$1/g' |
      perl -0777 -pe 's/\{\{[<%]\s*glossary_tooltip[^>%]*?term_id="([^"]*)"[^>%]*?[>%]\}\}/$1/g' |
      # glossary_definition pulls its body from a Hugo data file we do not check
      # out, so keeping its prepend= text would leave a truncated sentence like
      # "In Kubernetes, a Service is" with no definition after it. A fragment
      # that reads like content but answers nothing is worse for retrieval than
      # a gap, so the catch-all below drops these whole.
      perl -0777 -pe 's/\{\{[<%].*?[>%]\}\}//gs' |
      # Hugo section markers such as <!-- overview --> carry no meaning here.
      perl -0777 -pe 's/<!--.*?-->//gs' |
      # Collapse the blank runs the substitutions leave behind.
      perl -0777 -pe 's/\n{3,}/\n\n/g' \
        > "$out"

    # Hugo section indexes are sometimes just a heading with no body. Ingesting
    # one produces a chunk with no content to retrieve, which only adds noise to
    # the corpus, so drop anything that came out essentially empty.
    if [ "$(wc -c < "$out")" -lt 500 ]; then
      rm "$out"
    fi
  done
done

echo "wrote $(find "$DEST" -name '*.md' | wc -l) files, $(find "$DEST" -name '*.md' -exec cat {} + | wc -c) bytes"
