// Package docs owns the generated documentation index embedded in the binary.
//
// The Markdown sources and scripts/build_site.py own the prose, titles,
// descriptions, anchors and section text. docs.json is generated from that
// model; this package only decodes it and answers list, search and read
// requests for the MCP docs tools. It never re-parses Markdown, so a page
// cannot acquire a second title, slug or anchor here.
package docs

import (
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"unicode"
)

//go:embed docs.json
var embedded embed.FS

// Schema is the manifest contract this build understands. A generated file
// with another schema fails closed instead of being half-read.
const Schema = 1

// EmbeddedPath is the one generated artifact read by this package.
const EmbeddedPath = "docs.json"

type Manifest struct {
	Schema      int    `json:"schema"`
	GeneratedBy string `json:"generated_by"`
	BaseURL     string `json:"base_url"`
	Version     string `json:"version"`
	Pages       []Page `json:"pages"`
}

type Page struct {
	Path        string    `json:"path"`
	Slug        string    `json:"slug"`
	Output      string    `json:"output"`
	Markdown    string    `json:"markdown"`
	URL         string    `json:"url"`
	MarkdownURL string    `json:"markdown_url"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	Group       string    `json:"group"`
	Order       int       `json:"order"`
	Bytes       int       `json:"bytes"`
	SHA256      string    `json:"sha256"`
	Sections    []Section `json:"sections"`
	Text        string    `json:"markdown_text"`
}

type Section struct {
	Heading string `json:"heading"`
	ID      string `json:"id"`
	Text    string `json:"text"`
	Context string `json:"context"`
}

type Summary struct {
	Path        string `json:"path"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Group       string `json:"group"`
	URL         string `json:"url"`
	MarkdownURL string `json:"markdown_url"`
	Sections    int    `json:"sections"`
}

type Hit struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	URL     string `json:"url"`
	Anchor  string `json:"anchor,omitempty"`
	Heading string `json:"heading,omitempty"`
	Context string `json:"context,omitempty"`
	Text    string `json:"text"`
	Score   int    `json:"score"`
}

type SearchResult struct {
	Query   string `json:"query"`
	Results []Hit  `json:"results"`
	Total   int    `json:"total"`
}

var (
	loadOnce sync.Once
	loaded   *Manifest
	loadErr  error
)

// Load decodes the embedded index once, rejecting unknown fields and an
// unsupported schema. Callers get the same immutable manifest for the process
// lifetime; no tool can mutate the embedded data.
func Load() (*Manifest, error) {
	loadOnce.Do(func() {
		raw, err := embedded.ReadFile(EmbeddedPath)
		if err != nil {
			loadErr = fmt.Errorf("read embedded docs index: %w", err)
			return
		}
		var manifest Manifest
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&manifest); err != nil {
			loadErr = fmt.Errorf("decode embedded docs index: %w", err)
			return
		}
		if manifest.Schema != Schema {
			loadErr = fmt.Errorf("docs index schema %d, want %d; regenerate with scripts/build_site.py --write-manifest", manifest.Schema, Schema)
			return
		}
		if len(manifest.Pages) == 0 {
			loadErr = errors.New("docs index contains no pages")
			return
		}
		seen := make(map[string]bool, len(manifest.Pages))
		for _, page := range manifest.Pages {
			if page.Path == "" || page.Title == "" || page.Markdown == "" || page.URL == "" {
				loadErr = fmt.Errorf("docs index has an incomplete page record: %q", page.Path)
				return
			}
			if seen[page.Path] {
				loadErr = fmt.Errorf("docs index repeats page %q", page.Path)
				return
			}
			seen[page.Path] = true
		}
		loaded = &manifest
	})
	return loaded, loadErr
}

// List returns the pages in navigation order.
func (m *Manifest) List() []Summary {
	out := make([]Summary, 0, len(m.Pages))
	for _, page := range m.Pages {
		out = append(out, Summary{
			Path:        page.Path,
			Title:       page.Title,
			Description: page.Description,
			Group:       page.Group,
			URL:         page.URL,
			MarkdownURL: page.MarkdownURL,
			Sections:    len(page.Sections),
		})
	}
	return out
}

// Page returns one page by its repository path, for example docs/mcp.md.
func (m *Manifest) Page(path string) (*Page, error) {
	path = strings.TrimSpace(path)
	for i := range m.Pages {
		if m.Pages[i].Path == path || m.Pages[i].Slug == path || m.Pages[i].Markdown == path {
			return &m.Pages[i], nil
		}
	}
	return nil, fmt.Errorf("unknown docs path %q; call docs_list for the available paths", path)
}

// Read returns the whole page, or one section identified by its heading id.
// Section reads are the recommended path for large pages because the full
// Markdown is returned only when section is empty.
func (m *Manifest) Read(path, section string) (*Page, *Section, error) {
	page, err := m.Page(path)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(section) == "" {
		return page, nil, nil
	}
	for i := range page.Sections {
		if page.Sections[i].ID == section {
			return page, &page.Sections[i], nil
		}
	}
	return nil, nil, fmt.Errorf("unknown section %q on %s; call docs_read without a section for the heading ids", section, page.Path)
}

// Search ranks title, heading and body matches. The query is split into
// lower-case words; every word must appear somewhere in a candidate section,
// and title/heading matches weigh more than body matches. Results are stable
// for equal scores by navigation order.
func (m *Manifest) Search(query, path string, limit int) (SearchResult, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return SearchResult{}, errors.New("query is required")
	}
	words := searchWords(query)
	if len(words) == 0 {
		return SearchResult{}, errors.New("query contains no searchable words")
	}
	if limit <= 0 {
		limit = 8
	}
	if limit > 20 {
		limit = 20
	}
	var scope *Page
	if strings.TrimSpace(path) != "" {
		var err error
		scope, err = m.Page(path)
		if err != nil {
			return SearchResult{}, err
		}
	}
	result := SearchResult{Query: query}
	for _, page := range m.Pages {
		if scope != nil && page.Path != scope.Path {
			continue
		}
		if score, ok := scoreText(words, page.Title, page.Description); ok {
			result.Results = append(result.Results, Hit{
				Path: page.Path, Title: page.Title, URL: page.URL,
				Text: snippet(page.Description, words), Score: score + 20,
			})
		}
		for _, section := range page.Sections {
			body := strings.Join([]string{section.Heading, section.Context, section.Text}, " ")
			score, ok := scoreText(words, section.Heading, body)
			if !ok {
				continue
			}
			result.Results = append(result.Results, Hit{
				Path: page.Path, Title: page.Title, URL: page.URL,
				Anchor: section.ID, Heading: section.Heading, Context: section.Context,
				Text: snippet(section.Text, words), Score: score,
			})
		}
	}
	sort.SliceStable(result.Results, func(i, j int) bool {
		if result.Results[i].Score != result.Results[j].Score {
			return result.Results[i].Score > result.Results[j].Score
		}
		if result.Results[i].Path != result.Results[j].Path {
			return result.Results[i].Path < result.Results[j].Path
		}
		return result.Results[i].Anchor < result.Results[j].Anchor
	})
	result.Total = len(result.Results)
	if len(result.Results) > limit {
		result.Results = result.Results[:limit]
	}
	return result, nil
}

func searchWords(query string) []string {
	fields := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	out := fields[:0]
	for _, word := range fields {
		if len([]rune(word)) >= 2 {
			out = append(out, word)
		}
	}
	return out
}

func scoreText(words []string, title, body string) (int, bool) {
	title = strings.ToLower(title)
	body = strings.ToLower(body)
	score := 0
	for _, word := range words {
		inTitle := strings.Contains(title, word)
		inBody := strings.Contains(body, word)
		if !inTitle && !inBody {
			return 0, false
		}
		if inTitle {
			score += 8
		}
		if inBody {
			score += 1 + strings.Count(body, word)
		}
	}
	return score, true
}

func snippet(text string, words []string) string {
	compact := strings.Join(strings.Fields(text), " ")
	if len(compact) <= 220 {
		return compact
	}
	lower := strings.ToLower(compact)
	start := 0
	for _, word := range words {
		if at := strings.Index(lower, word); at >= 0 {
			start = at
			break
		}
	}
	start -= 60
	if start < 0 {
		start = 0
	}
	end := start + 220
	if end > len(compact) {
		end = len(compact)
	}
	if start > 0 {
		start = strings.Index(compact[start:], " ") + start
	}
	if end < len(compact) {
		if space := strings.LastIndex(compact[:end], " "); space > start {
			end = space
		}
	}
	value := strings.TrimSpace(compact[start:end])
	if start > 0 {
		value = "..." + value
	}
	if end < len(compact) {
		value += "..."
	}
	return value
}
