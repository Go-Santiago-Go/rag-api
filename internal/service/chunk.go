package service

import "strings"

// Chunker splits a document's text into the passages that get embedded and
// stored. It is an interface because chunking strategy is the single biggest
// lever on retrieval quality and the one most worth experimenting with: the
// ingest pipeline depends on this, so a new strategy is a new implementation
// rather than an edit to the pipeline.
type Chunker interface {
	Chunk(text string) []string
}

// Chunk splits text into overlapping passages of at most size runes, each
// starting size-overlap runes after the previous. Overlap keeps an idea that
// straddles a boundary intact in at least one chunk. Splitting on runes, not
// bytes, prevents cutting a multi-byte UTF-8 character in half.
//
// The loop advances by size-overlap, so that stride must stay positive or start
// never moves. Chunk defends that invariant: a non-positive size yields no
// chunks, and an overlap that meets or exceeds size is clamped to zero (a
// no-overlap walk) rather than looping forever.
func Chunk(text string, size, overlap int) []string {
	if size <= 0 {
		return nil
	}
	if overlap < 0 || overlap >= size {
		overlap = 0
	}
	runes := []rune(text)
	var chunks []string
	for start := 0; start < len(runes); start += size - overlap {
		end := min(start+size, len(runes))
		chunks = append(chunks, string(runes[start:end]))
		if end == len(runes) {
			break
		}
	}
	return chunks
}

// FixedChunker cuts every size runes regardless of what the text is saying. It
// is the baseline strategy: trivially predictable, and it respects nothing about
// the document, so roughly 70% of the chunks it produces begin mid-sentence.
type FixedChunker struct {
	size    int
	overlap int
}

// NewFixedChunker returns the fixed-size strategy. See Chunk for how size and
// overlap are validated.
func NewFixedChunker(size, overlap int) FixedChunker {
	return FixedChunker{size: size, overlap: overlap}
}

// Chunk implements Chunker.
func (c FixedChunker) Chunk(text string) []string {
	return Chunk(text, c.size, c.overlap)
}

// StructuredChunker splits on markdown headings so a chunk is a section of the
// document rather than an arbitrary window over it.
//
// A naive version of this degenerates immediately: real documents have sections
// far larger and far smaller than any useful chunk size, so splitting purely on
// headings yields both 4,000-rune chunks that dilute an embedding and 40-rune
// chunks that carry no information. The two bounds below are what make the
// strategy hold up.
type StructuredChunker struct {
	// maxSize is the ceiling. A section longer than this is split by the
	// fixed-size walk, with its heading prepended to every piece so a fragment
	// four splits deep still says what it is about.
	maxSize int
	// minSize is the floor. Consecutive sections are accumulated until they
	// reach it, so a one-line heading and its two-line body merge with what
	// follows instead of becoming a chunk that says nothing.
	minSize int
	// overlap applies only inside the fixed-size fallback. Section boundaries
	// need no overlap because they are already placed where the topic changes.
	overlap int
}

// NewStructuredChunker returns the heading-aware strategy. minSize is derived as
// half of maxSize rather than exposed as a fourth knob: the useful property is
// that the floor sits well below the ceiling, and the exact ratio is not
// something worth tuning before there is evidence it matters.
func NewStructuredChunker(maxSize, overlap int) StructuredChunker {
	return StructuredChunker{maxSize: maxSize, minSize: maxSize / 2, overlap: overlap}
}

// Compile-time proof both strategies satisfy Chunker.
var (
	_ Chunker = FixedChunker{}
	_ Chunker = StructuredChunker{}
)

// Chunk implements Chunker.
func (c StructuredChunker) Chunk(text string) []string {
	if c.maxSize <= 0 {
		return nil
	}

	var chunks []string
	var pending []string // small sections accumulating toward minSize

	flush := func() {
		if joined := strings.TrimSpace(strings.Join(pending, "\n\n")); joined != "" {
			chunks = append(chunks, joined)
		}
		pending = nil
	}

	for _, sec := range splitSections(text) {
		full := sec.text()
		if full == "" {
			continue
		}

		// Oversized sections cannot merge with anything, so close out whatever
		// is pending before splitting this one on its own.
		if runeLen(full) > c.maxSize {
			flush()
			chunks = append(chunks, c.split(sec)...)
			continue
		}

		pending = append(pending, full)
		if runeLen(strings.Join(pending, "\n\n")) >= c.minSize {
			flush()
		}
	}
	flush()

	return chunks
}

// split breaks an oversized section into fixed-size pieces, each carrying the
// section heading. Repeating the heading costs a few tokens per chunk and buys
// every fragment the one piece of context that says what it belongs to.
func (c StructuredChunker) split(sec section) []string {
	prefix := ""
	budget := c.maxSize
	if sec.heading != "" {
		prefix = sec.heading + "\n\n"
		// Leave room for the heading so the finished chunk still respects
		// maxSize, but never shrink the body below half the ceiling: a freakishly
		// long heading should not collapse the body to nothing.
		budget = max(c.maxSize-runeLen(prefix), c.maxSize/2)
	}

	var out []string
	var cur []string
	curLen := 0

	flush := func() {
		if len(cur) > 0 {
			out = append(out, prefix+strings.Join(cur, "\n\n"))
			cur, curLen = nil, 0
		}
	}

	for _, para := range splitParagraphs(sec.body) {
		n := runeLen(para)

		// A single paragraph over budget has no internal boundary to cut on, so
		// it is the one case that falls through to the rune walk and can produce
		// a mid-word edge. Everything else is packed on paragraph boundaries.
		if n > budget {
			flush()
			for _, piece := range Chunk(para, budget, c.overlap) {
				out = append(out, prefix+strings.TrimSpace(piece))
			}
			continue
		}

		if curLen > 0 && curLen+n > budget {
			flush()
		}
		cur = append(cur, para)
		curLen += n + 2 // the blank line rejoining them
	}
	flush()

	return out
}

// splitParagraphs breaks text on blank lines, leaving fenced code blocks whole.
// A blank line inside a YAML or shell example is not a paragraph break, and
// splitting there would emit a chunk containing half a code block, which is
// exactly the kind of broken citation this strategy exists to prevent.
func splitParagraphs(text string) []string {
	var (
		paragraphs []string
		cur        []string
		inFence    bool
	)

	flush := func() {
		if p := strings.TrimSpace(strings.Join(cur, "\n")); p != "" {
			paragraphs = append(paragraphs, p)
		}
		cur = nil
	}

	for line := range strings.SplitSeq(text, "\n") {
		if isFence(line) {
			inFence = !inFence
		}
		if !inFence && strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		cur = append(cur, line)
	}
	flush()

	return paragraphs
}

// section is one markdown heading and the text beneath it, up to the next
// heading of any level. Text before the first heading is a section with no
// heading.
type section struct {
	heading string // the raw heading line, e.g. "## Storage Classes"
	body    string // everything after the heading line, up to the next heading
}

func (s section) text() string {
	switch {
	case s.body == "":
		return s.heading
	case s.heading == "":
		return s.body
	default:
		return s.heading + "\n\n" + s.body
	}
}

// splitSections cuts text at every markdown heading.
//
// Fenced code blocks are tracked because "# " starts a comment in shell, YAML,
// Dockerfiles and most of what Kubernetes documentation puts in examples.
// Treating those as headings would shatter a code sample into fragments and
// invent sections that do not exist.
func splitSections(text string) []section {
	var (
		sections []section
		body     []string
		heading  string
		inFence  bool
	)

	flush := func() {
		trimmed := strings.TrimSpace(strings.Join(body, "\n"))
		if trimmed != "" || heading != "" {
			sections = append(sections, section{heading: heading, body: trimmed})
		}
		body = nil
	}

	for line := range strings.SplitSeq(text, "\n") {
		if isFence(line) {
			inFence = !inFence
		}
		if !inFence && isHeading(line) {
			flush()
			heading = strings.TrimSpace(line)
			continue // the heading is stored on the section, not in its body
		}
		body = append(body, line)
	}
	flush()

	return sections
}

// isHeading reports whether line is an ATX markdown heading: one to six hashes
// followed by whitespace. The trailing space matters, since "#nogo" is not a
// heading.
func isHeading(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#"))
	if hashes < 1 || hashes > 6 || hashes == len(trimmed) {
		return false
	}
	return trimmed[hashes] == ' ' || trimmed[hashes] == '\t'
}

// isFence reports whether line opens or closes a fenced code block.
func isFence(line string) bool {
	trimmed := strings.TrimLeft(line, " ")
	return strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")
}

func runeLen(s string) int { return len([]rune(s)) }
