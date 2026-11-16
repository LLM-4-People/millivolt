package mcp

import (
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/LLM-4-People/millivolt/internal/docs"
)

// Docs tools read the generated documentation index embedded in the binary.
// They are local-only: no proxy route, no storage, no credential. The tools are
// the MCP-facing adapters for internal/docs; the index generator is
// scripts/build_site.py, so page titles and anchors have one owner.

const maxDocsReadBytes = 256 << 10

type DocsListInput struct{}

type DocsListOutput struct {
	Pages []docs.Summary `json:"pages" jsonschema:"every published document in navigation order"`
	Count int            `json:"count" jsonschema:"number of published documents"`
	URL   string         `json:"url" jsonschema:"the public documentation origin"`
	Note  string         `json:"note" jsonschema:"what this index covers and how to read a page"`
}

type DocsSearchInput struct {
	Query string `json:"query" jsonschema:"words to find in titles, headings and body text; required"`
	Path  string `json:"path,omitempty" jsonschema:"optional single document path from docs_list, for example docs/mcp.md"`
	Limit int    `json:"limit,omitempty" jsonschema:"optional result cap from 1 to 20; the default is 8"`
}

type DocsSearchOutput struct {
	Query     string     `json:"query"`
	Results   []docs.Hit `json:"results" jsonschema:"ranked sections; title and heading matches rank above body matches"`
	Returned  int        `json:"returned"`
	Total     int        `json:"total" jsonschema:"matches before the result cap"`
	Truncated bool       `json:"truncated" jsonschema:"true when the total is above the returned count"`
	Note      string     `json:"note"`
}

type DocsReadInput struct {
	Path     string `json:"path" jsonschema:"document path from docs_list, for example docs/mcp.md"`
	Section  string `json:"section,omitempty" jsonschema:"optional heading id from docs_read's sections list; omit for the whole page"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"optional byte cap; the result is truncated with an explicit marker"`
}

type DocsReadOutput struct {
	Path        string         `json:"path"`
	Title       string         `json:"title"`
	Description string         `json:"description"`
	URL         string         `json:"url"`
	MarkdownURL string         `json:"markdown_url"`
	Section     *docs.Section  `json:"section,omitempty"`
	Sections    []docs.Section `json:"sections"`
	Markdown    string         `json:"markdown,omitempty" jsonschema:"the full Markdown source; omitted for a section read"`
	Text        string         `json:"text,omitempty" jsonschema:"the section text for a section read"`
	Bytes       int            `json:"bytes"`
	Truncated   bool           `json:"truncated"`
	Note        string         `json:"note"`
}

func (s *Service) docsList(_ context.Context, _ DocsListInput) (*DocsListOutput, error) {
	manifest, err := docs.Load()
	if err != nil {
		return nil, err
	}
	return &DocsListOutput{
		Pages: manifest.List(),
		Count: len(manifest.Pages),
		URL:   manifest.BaseURL,
		Note:  "Start here. Read a page with docs_read and its section id for a bounded answer; search terms with docs_search. The index is generated from the same Markdown as the published site.",
	}, nil
}

func (s *Service) docsSearch(_ context.Context, in DocsSearchInput) (*DocsSearchOutput, error) {
	manifest, err := docs.Load()
	if err != nil {
		return nil, err
	}
	result, err := manifest.Search(in.Query, in.Path, in.Limit)
	if err != nil {
		return nil, err
	}
	return &DocsSearchOutput{
		Query:     result.Query,
		Results:   result.Results,
		Returned:  len(result.Results),
		Total:     result.Total,
		Truncated: result.Total > len(result.Results),
		Note:      "A hit names a path and an anchor. Read that section with docs_read by passing its path and the anchor as section.",
	}, nil
}

func (s *Service) docsRead(_ context.Context, in DocsReadInput) (*DocsReadOutput, error) {
	manifest, err := docs.Load()
	if err != nil {
		return nil, err
	}
	page, section, err := manifest.Read(in.Path, in.Section)
	if err != nil {
		return nil, err
	}
	out := &DocsReadOutput{
		Path:        page.Path,
		Title:       page.Title,
		Description: page.Description,
		URL:         page.URL,
		MarkdownURL: page.MarkdownURL,
		Sections:    page.Sections,
	}
	if section != nil {
		out.Section = section
		out.Text = strings.TrimSpace(strings.Join([]string{section.Heading, section.Text}, "\n\n"))
		out.Bytes = len(out.Text)
		out.Note = "This is one section. Omit section to read the whole page, or pass another heading id."
		return out, nil
	}
	limit := in.MaxBytes
	if limit <= 0 || limit > maxDocsReadBytes {
		limit = maxDocsReadBytes
	}
	text := page.Text
	if len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
		out.Truncated = true
	}
	out.Markdown = text
	out.Bytes = len(text)
	out.Note = fmt.Sprintf("The full Markdown source is %d bytes. Use a section id for a smaller, anchored answer.", page.Bytes)
	if out.Truncated {
		out.Note = fmt.Sprintf("Truncated at the max_bytes cap; the full page is %d bytes. Read a section for the rest.", page.Bytes)
	}
	return out, nil
}
