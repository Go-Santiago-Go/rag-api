package service

import (
	"reflect"
	"strings"
	"testing"
)

func TestChunk(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		size    int
		overlap int
		want    []string
	}{
		{"clean multi-chunk walk", "abcdefghij", 4, 1, []string{"abcd", "defg", "ghij"}},
		{"exact fit, no remainder", "abcdef", 3, 0, []string{"abc", "def"}},
		{"text shorter than size", "abc", 10, 2, []string{"abc"}},
		{"empty text", "", 4, 1, nil},
		{"multi-byte runes stay intact", "héllo", 2, 0, []string{"hé", "ll", "o"}},
		{"overlap >= size degrades safely", "abcd", 2, 5, []string{"ab", "cd"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Chunk(tt.text, tt.size, tt.overlap)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Chunk(%q, %d, %d) = %#v, want %#v",
					tt.text, tt.size, tt.overlap, got, tt.want)
			}
		})
	}
}

func TestStructuredChunker(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		maxSize int
		want    []string
	}{
		{
			name:    "sections large enough to stand alone are kept whole",
			text:    "## Alpha\n\n" + strings.Repeat("a", 30) + "\n\n## Beta\n\n" + strings.Repeat("b", 30),
			maxSize: 40,
			want: []string{
				"## Alpha\n\n" + strings.Repeat("a", 30),
				"## Beta\n\n" + strings.Repeat("b", 30),
			},
		},
		{
			name:    "sections below the floor merge forward",
			text:    "## Alpha\n\nsmall\n\n## Beta\n\nalso small",
			maxSize: 200,
			want:    []string{"## Alpha\n\nsmall\n\n## Beta\n\nalso small"},
		},
		{
			name:    "oversized section falls back to fixed-size, heading on every piece",
			text:    "## Alpha\n\n" + strings.Repeat("a", 60),
			maxSize: 40,
			want: []string{
				"## Alpha\n\n" + strings.Repeat("a", 30),
				"## Alpha\n\n" + strings.Repeat("a", 30),
			},
		},
		{
			name:    "text before the first heading becomes its own section",
			text:    "preamble text here\n\n## Alpha\n\nbody",
			maxSize: 200,
			want:    []string{"preamble text here\n\n## Alpha\n\nbody"},
		},
		{
			// The failure this guards against is specific to technical docs: "# "
			// opens a comment in shell and YAML, so a naive splitter shatters every
			// code sample and invents sections that are not in the document.
			name:    "hash inside a fenced code block is not a heading",
			text:    "## Alpha\n\n```bash\n# not a heading\nkubectl get pods\n```\n\nmore text",
			maxSize: 200,
			want:    []string{"## Alpha\n\n```bash\n# not a heading\nkubectl get pods\n```\n\nmore text"},
		},
		{
			name:    "no headings at all still yields the text",
			text:    "just a paragraph with no structure",
			maxSize: 200,
			want:    []string{"just a paragraph with no structure"},
		},
		{
			name:    "empty text yields nothing",
			text:    "",
			maxSize: 200,
			want:    nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Zero overlap keeps the expected fixed-size fallback readable; overlap
			// itself is already covered by TestChunk.
			got := NewStructuredChunker(tt.maxSize, 0).Chunk(tt.text)
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Chunk() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestStructuredChunkerRespectsCeiling is a property check rather than a fixed
// expectation: whatever the input, no chunk may exceed maxSize, or the strategy
// has silently reintroduced the oversized chunks it exists to prevent.
func TestStructuredChunkerRespectsCeiling(t *testing.T) {
	const maxSize = 120
	text := "# Title\n\nintro\n\n## Long\n\n" + strings.Repeat("word ", 200) +
		"\n\n### Short\n\nx\n\n## Another\n\n" + strings.Repeat("more ", 50)

	for _, chunk := range NewStructuredChunker(maxSize, 10).Chunk(text) {
		if n := len([]rune(chunk)); n > maxSize {
			t.Errorf("chunk of %d runes exceeds maxSize %d: %q", n, maxSize, chunk)
		}
		if strings.TrimSpace(chunk) == "" {
			t.Error("produced an empty chunk")
		}
	}
}

func TestIsHeading(t *testing.T) {
	tests := map[string]bool{
		"# Title":        true,
		"###### Six":     true,
		"  ## Indented":  true,
		"####### Seven":  false, // seven hashes is not a heading in markdown
		"#NoSpace":       false,
		"#":              false,
		"Not a heading":  false,
		"":               false,
		"a # mid-line #": false,
	}
	for line, want := range tests {
		if got := isHeading(line); got != want {
			t.Errorf("isHeading(%q) = %v, want %v", line, got, want)
		}
	}
}

// TestStructuredChunkerSplitsOnParagraphs covers the oversized-section fallback.
// Cutting a long section at a raw rune offset leaves chunks beginning mid-word,
// and those chunks are returned to callers verbatim as citations, so the
// fallback packs whole paragraphs instead.
func TestStructuredChunkerSplitsOnParagraphs(t *testing.T) {
	para := func(n int) string { return strings.Repeat("word ", n) + "end." }
	text := "## Alpha\n\n" + para(20) + "\n\n" + para(20) + "\n\n" + para(20)

	chunks := NewStructuredChunker(200, 0).Chunk(text)
	if len(chunks) < 2 {
		t.Fatalf("expected the section to be split, got %d chunk(s)", len(chunks))
	}

	for i, c := range chunks {
		body := strings.TrimPrefix(c, "## Alpha\n\n")
		if !strings.HasPrefix(body, "word ") {
			t.Errorf("chunk %d body starts mid-paragraph: %q", i, body[:min(30, len(body))])
		}
		if !strings.HasSuffix(strings.TrimSpace(body), "end.") {
			t.Errorf("chunk %d body ends mid-paragraph: %q", i, body)
		}
	}
}

// TestStructuredChunkerKeepsCodeFencesWhole guards the paragraph splitter: a
// blank line inside a YAML or shell example is not a paragraph break, and
// splitting there would emit half a code block as a citation.
func TestStructuredChunkerKeepsCodeFencesWhole(t *testing.T) {
	text := "## Alpha\n\n" + strings.Repeat("filler words here. ", 20) +
		"\n\n```yaml\nkey: value\n\nother: value\n```\n\n" + strings.Repeat("more filler text. ", 20)

	for _, c := range NewStructuredChunker(300, 0).Chunk(text) {
		if opens := strings.Count(c, "```"); opens%2 != 0 {
			t.Errorf("chunk has an unbalanced code fence:\n%s", c)
		}
	}
}
