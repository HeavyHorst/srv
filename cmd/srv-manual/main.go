package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/ast"
	east "github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
	"github.com/yuin/goldmark/text"
	"github.com/yuin/goldmark/util"
)

type section struct {
	title      string
	anchor     string
	sourcePath string
	markdown   string
}

type tocEntry struct {
	level int
	text  string
	id    string
}

var firstH1Re = regexp.MustCompile(`(?s)<h1[^>]*>.*?</h1>\s*`)

func stripFirstH1(html string) string {
	return firstH1Re.ReplaceAllString(html, "")
}

var headingRe = regexp.MustCompile(`(?s)<(h[1-6])\s+id="([^"]+)">(.*?)</h[1-6]>`)

var preCodeRe = regexp.MustCompile(`(?s)<pre><code[^>]*>(.*?)</code></pre>`)

func addLineNumbersToCodeBlocks(html string) string {
	return preCodeRe.ReplaceAllStringFunc(html, func(match string) string {
		submatch := preCodeRe.FindStringSubmatch(match)
		content := submatch[1]
		// Unescape HTML entities that goldmark escapes
		content = strings.ReplaceAll(content, "&lt;", "<")
		content = strings.ReplaceAll(content, "&gt;", ">")
		content = strings.ReplaceAll(content, "&amp;", "&")

		lines := strings.Split(content, "\n")
		var wrapped []string
		for _, line := range lines {
			if line == "" && len(lines) > 1 && len(wrapped) == len(lines)-1 {
				// Skip trailing empty line
				continue
			}
			wrapped = append(wrapped, fmt.Sprintf(`<span class="line">%s</span>`, line))
		}
		return fmt.Sprintf("<pre><code>%s</code></pre>", strings.Join(wrapped, ""))
	})
}

func addHeadingAnchorsAndNumbers(html string, secNum int, entries []tocEntry) string {
	counters := make([]int, 5)
	entryIdx := 0
	return headingRe.ReplaceAllStringFunc(html, func(match string) string {
		submatch := headingRe.FindStringSubmatch(match)
		tag := submatch[1]
		id := submatch[2]
		innerHTML := submatch[3]

		// Skip h1 entries in our tracking (they were stripped from content)
		for entryIdx < len(entries) && entries[entryIdx].level <= 1 {
			entryIdx++
		}

		// Find the corresponding entry to get the level
		if entryIdx < len(entries) && entries[entryIdx].id == id {
			level := entries[entryIdx].level
			if level > 1 && level <= 4 {
				counters[level]++
				for j := level + 1; j < 5; j++ {
					counters[j] = 0
				}
				num := fmt.Sprintf("%d", secNum)
				for j := 2; j <= level; j++ {
					num += fmt.Sprintf(".%d", counters[j])
				}
				entryIdx++
				return fmt.Sprintf(`<%s id="%s"><a class="heading-anchor" href="#%s">%s %s</a></%s>`, tag, id, id, num, innerHTML, tag)
			}
			entryIdx++
		}
		return fmt.Sprintf(`<%s id="%s"><a class="heading-anchor" href="#%s">%s</a></%s>`, tag, id, id, innerHTML, tag)
	})
}

var admonitionRe = regexp.MustCompile(`(?m)^!!! (\w+)\n((?:    .+\n)+)`)

func preprocessAdmonitions(content string, md goldmark.Markdown) string {
	return admonitionRe.ReplaceAllStringFunc(content, func(match string) string {
		submatch := admonitionRe.FindStringSubmatch(match)
		kind := submatch[1]

		bodyLines := strings.Split(strings.TrimRight(submatch[2], "\n"), "\n")
		for i, line := range bodyLines {
			bodyLines[i] = strings.TrimPrefix(line, "    ")
		}
		body := strings.Join(bodyLines, "\n")

		var bodyHTML bytes.Buffer
		if err := md.Convert([]byte(body), &bodyHTML); err != nil {
			bodyHTML.WriteString(htmlEscape(body))
		}

		return fmt.Sprintf("\n<div class=\"admonition %s\">\n%s\n</div>\n", kind, bodyHTML.String())
	})
}

func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	return s
}

var mdLinkRe = regexp.MustCompile(`\[([^\]]*)\]\(([^)]+\.md(?:#[^)]*)?)\)`)

func resolveLinks(content string, fromPath string, anchorMap map[string]string) string {
	fromDir := filepath.Dir(fromPath)
	return mdLinkRe.ReplaceAllStringFunc(content, func(match string) string {
		submatch := mdLinkRe.FindStringSubmatch(match)
		text := submatch[1]
		link := submatch[2]

		parts := strings.SplitN(link, "#", 2)
		refPath := parts[0]
		fragment := ""
		if len(parts) == 2 {
			fragment = parts[1]
		}

		absRef := filepath.Join(fromDir, refPath)
		absRef = filepath.ToSlash(absRef)

		if _, ok := anchorMap[absRef]; ok {
			if fragment != "" {
				return fmt.Sprintf("[%s](#%s)", text, fragment)
			}
			return fmt.Sprintf("[%s](#%s)", text, anchorMap[absRef])
		}
		return match
	})
}

type tocCollector struct {
	entries []tocEntry
	counter map[string]int
	source  []byte
}

func (t *tocCollector) Transform(node *ast.Document, reader text.Reader, pc parser.Context) {
	ast.Walk(node, func(n ast.Node, entering bool) (ast.WalkStatus, error) {
		if !entering {
			return ast.WalkContinue, nil
		}
		h, ok := n.(*ast.Heading)
		if !ok {
			return ast.WalkContinue, nil
		}

		headingText := strings.TrimSpace(string(h.Text(t.source)))
		id := nextUniqueID(slugify(headingText), t.counter)
		h.SetAttributeString("id", id)

		t.entries = append(t.entries, tocEntry{
			level: h.Level,
			text:  headingText,
			id:    id,
		})
		return ast.WalkContinue, nil
	})
}

func slugify(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ToLower(s)
	s = strings.ReplaceAll(s, " ", "-")
	reg := regexp.MustCompile(`[^a-z0-9\-]`)
	s = reg.ReplaceAllString(s, "")
	s = regexp.MustCompile(`-+`).ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if s == "" {
		return "section"
	}
	return s
}

func nextUniqueID(base string, counter map[string]int) string {
	if counter[base] == 0 {
		counter[base] = 1
		return base
	}

	for suffix := counter[base]; ; suffix++ {
		candidate := fmt.Sprintf("%s-%d", base, suffix)
		if counter[candidate] == 0 {
			counter[base] = suffix + 1
			counter[candidate] = 1
			return candidate
		}
	}
}

func sectionAnchor(relPath string) string {
	stem := strings.TrimSuffix(filepath.ToSlash(relPath), filepath.Ext(relPath))
	stem = strings.ReplaceAll(stem, "/", "-")
	return "doc-" + slugify(stem)
}

func buildSections(docsDir string) ([]section, error) {
	indexBytes, err := os.ReadFile(filepath.Join(docsDir, "index.md"))
	if err != nil {
		return nil, fmt.Errorf("reading index.md: %w", err)
	}
	indexContent := string(indexBytes)

	linkRe := regexp.MustCompile(`\[([^\]]*)\]\(([^)]+\.md)\)`)

	var sections []section
	seen := map[string]bool{}

	addSection := func(relPath string) {
		relPath = filepath.ToSlash(relPath)
		if seen[relPath] {
			return
		}
		seen[relPath] = true

		b, err := os.ReadFile(filepath.Join(docsDir, relPath))
		if err != nil {
			return
		}

		heading := extractFirstHeading(string(b))
		if heading == "" {
			heading = strings.TrimSuffix(filepath.Base(relPath), ".md")
			heading = strings.ReplaceAll(heading, "-", " ")
		}

		sections = append(sections, section{
			title:      heading,
			anchor:     sectionAnchor(relPath),
			sourcePath: relPath,
			markdown:   string(b),
		})
	}

	// Start with index
	addSection("index.md")

	// Follow links from index
	matches := linkRe.FindAllStringSubmatch(indexContent, -1)
	for _, m := range matches {
		link := m[2]
		if strings.HasPrefix(link, "http") {
			continue
		}
		addSection(link)
	}

	// Then collect from known directories in order
	dirs := []string{"getting-started", "reference", "tasks", "examples", "networking"}
	for _, dir := range dirs {
		entries, err := os.ReadDir(filepath.Join(docsDir, dir))
		if err != nil {
			continue
		}
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
				continue
			}
			addSection(filepath.Join(dir, e.Name()))
		}
	}

	// Add cheatsheet
	addSection("cheatsheet.md")

	return sections, nil
}

func extractFirstHeading(content string) string {
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(line, "# ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "# "))
		}
	}
	return ""
}

func buildAnchorMap(sections []section) map[string]string {
	m := make(map[string]string)
	for _, s := range sections {
		m[s.sourcePath] = s.anchor
	}
	return m
}

func makeGoldmark() goldmark.Markdown {
	return goldmark.New(
		goldmark.WithExtensions(
			east.NewTable(),
			east.Strikethrough,
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(),
		),
	)
}

func renderSection(s section, anchorMap map[string]string, globalCounter map[string]int, secNum int) (string, []tocEntry, error) {
	md := makeGoldmark()

	content := s.markdown
	content = resolveLinks(content, s.sourcePath, anchorMap)
	content = preprocessAdmonitions(content, md)

	collector := &tocCollector{
		counter: globalCounter,
		source:  []byte(content),
	}

	mdWithToc := goldmark.New(
		goldmark.WithExtensions(
			east.NewTable(),
			east.Strikethrough,
		),
		goldmark.WithRendererOptions(
			html.WithUnsafe(),
		),
		goldmark.WithParserOptions(
			parser.WithASTTransformers(
				util.Prioritized(collector, 100),
			),
		),
	)

	var buf bytes.Buffer
	if err := mdWithToc.Convert([]byte(content), &buf); err != nil {
		return "", nil, fmt.Errorf("rendering %s: %w", s.sourcePath, err)
	}

	rendered := addLineNumbersToCodeBlocks(addHeadingAnchorsAndNumbers(stripFirstH1(buf.String()), secNum, collector.entries))

	return rendered, collector.entries, nil
}

const css = `
*, *::before, *::after { box-sizing: border-box; }

html {
	font-size: 14px;
	-webkit-text-size-adjust: 100%;
	background: #f5f5f5;
}

body {
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
	line-height: 1.6;
	color: #000;
	max-width: 55rem;
	margin: 2rem auto;
	padding: 3rem 4rem 4rem;
	background: #fff;
	border: 1px solid #ddd;
	box-shadow: 0 1px 3px rgba(0,0,0,0.1);
	min-height: 80vh;
}

h1, h2, h3, h4, h5, h6 {
	line-height: 1.25;
	margin: 1rem 0 0.25rem;
	scroll-margin-top: 1rem;
	font-weight: 700;
}

h1 { font-size: 1.5rem; margin-top: 1.5rem; }
h2 { font-size: 1.25rem; }
h3 { font-size: 1.1rem; }
h4 { font-size: 1rem; }

h1:first-child { margin-top: 0; }

.heading-anchor {
	color: inherit;
	text-decoration: none;
}

.heading-anchor:hover { text-decoration: underline; }

.section-meta {
	font-size: 0.75rem;
	color: #666;
	margin-bottom: 1rem;
}

.section-meta code {
	background: transparent;
	color: #666;
	padding: 0;
}

.section-divider {
	border: none;
	border-top: 2px solid #000;
	margin: 4rem 0 3rem;
}

.section-header {
	font-size: 1.5rem;
	font-weight: 700;
	margin: 0 0 0.5rem;
	color: #000;
}

.section-header a {
	color: #000;
	text-decoration: none;
}

.section-header a:hover { text-decoration: underline; }

a { color: #000; text-decoration: underline; }
a:hover { color: #333; }

code {
	font-family: "Berkeley Mono", "JetBrains Mono", "SF Mono", Menlo, Consolas, monospace;
	font-size: 0.85em;
	background: transparent;
	padding: 0;
	border-radius: 0;
}

pre {
	background: transparent;
	color: #000;
	padding: 1rem 0.5rem;
	border-radius: 0;
	border: 1px solid #ccc;
	counter-reset: line;
	overflow-x: auto;
	white-space: pre;
}

pre code {
	background: none;
	color: inherit;
	padding: 0;
	font-size: 0.85rem;
	border-radius: 0;
}

pre code .line {
	display: block;
	white-space: pre;
	line-height: inherit;
	margin: 0;
	padding: 0;
}

pre code .line::before {
	counter-increment: line;
	content: counter(line);
	display: inline-block;
	width: 1.5rem;
	margin-right: 0.5rem;
	text-align: right;
	color: #999;
	user-select: none;
}

table {
	border-collapse: collapse;
	width: 100%;
	margin: 0.75rem 0;
	font-size: 0.85rem;
}

th, td {
	border: 1px solid #000;
	padding: 0.25rem 0.5rem;
	text-align: left;
}

th {
	font-weight: 700;
}

blockquote {
	border-left: 4px solid #000;
	margin: 0.75rem 0;
	padding: 0.25rem 0.75rem;
	color: #000;
}

.admonition {
	border-left: 4px solid #000;
	padding: 0.5rem 0.75rem;
	margin: 0.75rem 0;
	background: #fff;
	border-radius: 0;
}

.admonition.note { border-left-color: #004080; }
.admonition.note::before {
	content: "[NOTE]";
	display: block;
	font-weight: 700;
	color: #004080;
	font-size: 0.85rem;
	margin-bottom: 0.25rem;
}
.admonition.note strong { color: #004080; }

.admonition.warning { border-left-color: #804000; }
.admonition.warning::before {
	content: "[WARNING]";
	display: block;
	font-weight: 700;
	color: #804000;
	font-size: 0.85rem;
	margin-bottom: 0.25rem;
}
.admonition.warning strong { color: #804000; }

.admonition strong { display: block; margin-bottom: 0.25rem; text-transform: uppercase; letter-spacing: 0.05em; font-size: 0.85rem; }

hr { border: none; border-top: 1px solid #000; margin: 1rem 0; }

footer {
	border-top: 2px solid #000;
	margin-top: 2rem;
	padding-top: 1rem;
	font-size: 0.75rem;
	color: #666;
}

footer code {
	background: transparent;
	color: #666;
	padding: 0;
}

ul, ol { padding-left: 2rem; }
li { margin-bottom: 0.15rem; }

nav.toc {
	padding: 0.75rem 1rem;
	margin-bottom: 1rem;
}

nav.toc h2 { margin-top: 0; font-size: 1.1rem; border-bottom: none; padding-bottom: 0; margin-bottom: 0.5rem; }

.toc-sep { border: none; border-top: 1px solid #000; margin: 0.5rem 0; }

.toc-section { margin-bottom: 0.5rem; }

.toc-section-title {
	font-weight: 700;
	text-decoration: none;
}

.toc-path {
	font-size: 0.75rem;
	color: #666;
	background: transparent;
	padding: 0;
	margin-left: 0.5rem;
}

.toc-entries {
	list-style: none;
	padding-left: 0;
	margin: 0.25rem 0;
}

.toc-entries li {
	margin-bottom: 0.1rem;
	display: flex;
	align-items: baseline;
	gap: 0.5rem;
}

.toc-entries ul {
	list-style: none;
	padding-left: 1.25rem;
	margin: 0;
}

.toc-num {
	font-weight: 400;
	color: #666;
	min-width: 2rem;
	flex-shrink: 0;
}

.toc-level {
	font-size: 0.75rem;
	color: #666;
	background: #f0f0f0;
	padding: 0 0.25rem;
	flex-shrink: 0;
}

nav.toc a { color: #000; text-decoration: underline; }
nav.toc a:hover { color: #333; }

.toc-section-num {
	font-weight: 700;
	margin-right: 0.25rem;
}

@media print {
	body { max-width: none; padding: 0; border: none; box-shadow: none; }
	html { background: #fff; }
	nav.toc { display: none; }
	.manual-section { page-break-before: always; }
	.manual-section:first-child { page-break-before: auto; }
}

@media (max-width: 768px) {
	body { padding: 1rem; margin: 0.5rem; min-height: auto; }
	.section-header { position: sticky; top: 0; background: #fff; padding: 0.5rem 0; border-bottom: 1px solid #ddd; z-index: 100; }
	.toc-toggle { display: block; }
	nav.toc { display: none; margin-top: 0.5rem; }
	nav.toc.expanded { display: block; }
}

.toc-toggle {
	display: none;
	width: 100%;
	padding: 0.5rem;
	background: #f5f5f5;
	border: 1px solid #ddd;
	font-family: inherit;
	font-size: 1rem;
	cursor: pointer;
}

.toc-toggle:hover { background: #eee; }
`

func buildTOC(sections []section, tocsBySection [][]tocEntry) string {
	var buf strings.Builder

	buf.WriteString("<nav class=\"toc\">\n<h2>Table of Contents</h2>\n")

	for i, s := range sections {
		secNum := i + 1
		if i > 0 {
			buf.WriteString("<hr class=\"toc-sep\">\n")
		}
		buf.WriteString(fmt.Sprintf("<div class=\"toc-section\">\n<span class=\"toc-section-num\">%d.</span><a href=\"#%s\" class=\"toc-section-title\">%s</a><code class=\"toc-path\">%s</code>\n", secNum, s.anchor, s.title, s.sourcePath))

		var entries []tocEntry
		if i < len(tocsBySection) {
			entries = tocsBySection[i]
		}

		if len(entries) > 0 {
			buf.WriteString("<ul class=\"toc-entries\">\n")
			counters := make([]int, 5)
			currentLevel := 2
			for _, e := range entries {
				if e.level <= 1 || e.level > 4 {
					continue
				}
				for e.level > currentLevel {
					buf.WriteString("<ul>\n")
					currentLevel++
				}
				for e.level < currentLevel {
					buf.WriteString("</ul>\n")
					currentLevel--
				}
				counters[e.level]++
				for j := e.level + 1; j < 5; j++ {
					counters[j] = 0
				}
				num := fmt.Sprintf("%d", secNum)
				for j := 2; j <= e.level; j++ {
					num += fmt.Sprintf(".%d", counters[j])
				}
				buf.WriteString(fmt.Sprintf("<li><span class=\"toc-num\">%s</span><a href=\"#%s\">%s</a></li>\n", num, e.id, e.text))
			}
			for currentLevel > 2 {
				buf.WriteString("</ul>\n")
				currentLevel--
			}
			buf.WriteString("</ul>\n")
		}
		buf.WriteString("</div>\n")
	}

	buf.WriteString("</nav>\n")
	return buf.String()
}

func main() {
	docsDir := "docs"
	if len(os.Args) > 1 {
		docsDir = os.Args[1]
	}

	outPath := "manual.html"
	if len(os.Args) > 2 {
		outPath = os.Args[2]
	}

	sections, err := buildSections(docsDir)
	if err != nil {
		fmt.Fprintf(os.Stderr, "error building sections: %v\n", err)
		os.Exit(1)
	}

	anchorMap := buildAnchorMap(sections)

	globalCounter := make(map[string]int)

	var body strings.Builder
	var tocsBySection [][]tocEntry
	headingCount := 0

	for i, s := range sections {
		if i > 0 {
			body.WriteString("<hr class=\"section-divider\">\n")
		}
		secNum := i + 1
		body.WriteString(fmt.Sprintf("<section id=\"%s\" class=\"manual-section\">\n<h1 class=\"section-header\"><a href=\"#%s\">%d. %s</a></h1>\n<div class=\"section-meta\"><code>%s</code> · %d words</div>\n", s.anchor, s.anchor, secNum, s.title, s.sourcePath, len(strings.Fields(s.markdown))))
		html, toc, err := renderSection(s, anchorMap, globalCounter, secNum)
		if err != nil {
			fmt.Fprintf(os.Stderr, "error rendering %s: %v\n", s.sourcePath, err)
			os.Exit(1)
		}
		body.WriteString(html)
		body.WriteString("\n</section>\n")
		tocsBySection = append(tocsBySection, toc)
		headingCount += len(toc)
	}

	tocHTML := buildTOC(sections, tocsBySection)

	// Calculate total word count
	totalWords := 0
	for _, s := range sections {
		totalWords += len(strings.Fields(s.markdown))
	}

	footerHTML := fmt.Sprintf(`<footer>
<div>Sections: %d · Headings: %d · Words: %d</div>
<div>MIT License · Copyright (c) 2026 Rene Michaelis</div>
</footer>
`, len(sections), headingCount, totalWords)

	var out strings.Builder
	out.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	out.WriteString("<meta charset=\"utf-8\">\n")
	out.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	out.WriteString("<title>srv manual</title>\n")
	out.WriteString("<style>\n")
	out.WriteString(css)
	out.WriteString("\n</style>\n")
	out.WriteString("</head>\n<body>\n")
	out.WriteString("<button class=\"toc-toggle\" onclick=\"document.querySelector('nav.toc').classList.toggle('expanded')\">Table of Contents</button>\n")
	out.WriteString(tocHTML)
	out.WriteString("\n")
	out.WriteString(body.String())
	out.WriteString(footerHTML)
	out.WriteString("</body>\n</html>\n")

	if err := os.WriteFile(outPath, []byte(out.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "wrote %s (%d sections, %d headings)\n", outPath, len(sections), headingCount)
}
