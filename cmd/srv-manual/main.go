package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
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

type appendixRef struct {
	anchor string
	title  string
}

type appendixEntry struct {
	name string
	refs []appendixRef
}

var firstH1Re = regexp.MustCompile(`(?s)<h1[^>]*>.*?</h1>\s*`)

func stripFirstH1(html string) string {
	return firstH1Re.ReplaceAllString(html, "")
}

var headingRe = regexp.MustCompile(`(?s)<(h[1-6])\s+id="([^"]+)">(.*?)</h[1-6]>`)

var preCodeRe = regexp.MustCompile(`(?s)(?:<!--\s*srv-manual:block=(command|output|diagram|example)\s*-->\s*)?<pre><code([^>]*)>(.*?)</code></pre>`)
var fencedCodeBlockRe = regexp.MustCompile("(?ms)```[^\n]*\n(.*?)```")
var envVarRe = regexp.MustCompile(`\b[A-Z][A-Z0-9_]*\b`)
var envAssignmentPrefixRe = regexp.MustCompile(`^(?:[A-Z_][A-Z0-9_]*=(?:"[^"]*"|'[^']*'|\S+)\s+)+`)
var pureEnvAssignmentRe = regexp.MustCompile(`^[A-Z_][A-Z0-9_]*=(?:"[^"]*"|'[^']*'|\S+)$`)
var heredocStartRe = regexp.MustCompile(`<<-?\s*['\"]?([A-Za-z0-9_]+)['\"]?`)

func addLineNumbersToCodeBlocks(html string) string {
	return preCodeRe.ReplaceAllStringFunc(html, func(match string) string {
		submatch := preCodeRe.FindStringSubmatch(match)
		blockKind := submatch[1]
		codeAttrs := submatch[2]
		content := submatch[3]
		// Unescape HTML entities that goldmark escapes
		content = strings.ReplaceAll(content, "&lt;", "<")
		content = strings.ReplaceAll(content, "&gt;", ">")
		content = strings.ReplaceAll(content, "&amp;", "&")
		if blockKind == "" {
			blockKind = classifyCodeBlock(content)
		}

		lines := strings.Split(content, "\n")
		var wrapped []string
		for _, line := range lines {
			if line == "" && len(lines) > 1 && len(wrapped) == len(lines)-1 {
				// Skip trailing empty line
				continue
			}
			wrapped = append(wrapped, fmt.Sprintf(`<span class="line">%s</span>`, line))
		}
		if codeAttrs == "" {
			return fmt.Sprintf(`<pre class="code-block code-block-%s"><code>%s</code></pre>`, blockKind, strings.Join(wrapped, ""))
		}
		return fmt.Sprintf(`<pre class="code-block code-block-%s"><code%s>%s</code></pre>`, blockKind, codeAttrs, strings.Join(wrapped, ""))
	})
}

func classifyCodeBlock(content string) string {
	commandish := 0
	for _, rawLine := range strings.Split(content, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			continue
		}
		if isCommandishLine(line) {
			commandish++
		}
	}
	if commandish == 0 {
		return "example"
	}
	return "command"
}

func isCommandishLine(line string) bool {
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	if strings.HasPrefix(line, "#") || strings.HasPrefix(line, "$ ") {
		return true
	}
	if marker := heredocDelimiter(line); marker != "" {
		return true
	}
	if line == "EOF" {
		return true
	}
	return looksLikeCommandLine(line)
}

func looksLikeCommandLine(line string) bool {
	line = strings.TrimSpace(line)
	line = strings.TrimPrefix(line, "$ ")
	if pureEnvAssignmentRe.MatchString(line) {
		return false
	}
	line = envAssignmentPrefixRe.ReplaceAllString(line, "")
	line = strings.TrimSpace(line)
	if line == "" {
		return false
	}
	fields := strings.Fields(line)
	if len(fields) == 0 {
		return false
	}
	cmd := fields[0]
	if strings.HasPrefix(cmd, "./") || strings.HasPrefix(cmd, "../") || strings.HasPrefix(cmd, "/") {
		return true
	}
	known := map[string]struct{}{
		"bash": {}, "cat": {}, "chmod": {}, "chown": {}, "cp": {}, "curl": {}, "depmod": {}, "df": {},
		"docker": {}, "echo": {}, "env": {}, "export": {}, "find": {}, "go": {}, "grep": {},
		"install": {}, "ip": {}, "iptables": {}, "journalctl": {}, "ln": {}, "losetup": {},
		"make": {}, "mkdir": {}, "mkfs.ext4": {}, "mount": {}, "nft": {}, "nvim": {},
		"pacman": {}, "podman": {}, "rm": {}, "rsync": {}, "sed": {}, "sh": {}, "ssh": {},
		"sudo": {}, "sudoedit": {}, "srv": {}, "sysctl": {}, "systemctl": {}, "tailscale": {}, "tar": {},
		"tee": {}, "truncate": {}, "umount": {},
	}
	_, ok := known[cmd]
	return ok
}

func heredocDelimiter(line string) string {
	match := heredocStartRe.FindStringSubmatch(line)
	if len(match) != 2 {
		return ""
	}
	return match[1]
}

func collectAppendixEntries(sections []section) ([]appendixEntry, []appendixEntry) {
	commandRefs := map[string]map[string]appendixRef{}
	envRefs := map[string]map[string]appendixRef{}

	for _, s := range sections {
		ref := appendixRef{anchor: s.anchor, title: s.title}
		for _, cmd := range extractCommandExamples(s.markdown) {
			if _, ok := commandRefs[cmd]; !ok {
				commandRefs[cmd] = map[string]appendixRef{}
			}
			commandRefs[cmd][s.anchor] = ref
		}
		for _, env := range extractEnvVars(s.markdown) {
			if _, ok := envRefs[env]; !ok {
				envRefs[env] = map[string]appendixRef{}
			}
			envRefs[env][s.anchor] = ref
		}
	}

	return flattenAppendixMap(commandRefs), flattenAppendixMap(envRefs)
}

func flattenAppendixMap(index map[string]map[string]appendixRef) []appendixEntry {
	entries := make([]appendixEntry, 0, len(index))
	for name, refsByAnchor := range index {
		refs := make([]appendixRef, 0, len(refsByAnchor))
		for _, ref := range refsByAnchor {
			refs = append(refs, ref)
		}
		sort.Slice(refs, func(i, j int) bool {
			return strings.ToLower(refs[i].title) < strings.ToLower(refs[j].title)
		})
		entries = append(entries, appendixEntry{name: name, refs: refs})
	}
	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].name) < strings.ToLower(entries[j].name)
	})
	return entries
}

func extractCommandExamples(markdown string) []string {
	var commands []string
	seen := map[string]bool{}
	for _, match := range fencedCodeBlockRe.FindAllStringSubmatch(markdown, -1) {
		block := match[1]
		if classifyCodeBlock(block) != "command" {
			continue
		}
		for _, cmd := range extractCommandsFromBlock(block) {
			if seen[cmd] {
				continue
			}
			seen[cmd] = true
			commands = append(commands, cmd)
		}
	}
	return commands
}

func extractCommandsFromBlock(block string) []string {
	var commands []string
	var current []string
	heredocEnd := ""

	flush := func() {
		if len(current) == 0 {
			return
		}
		commands = append(commands, strings.Join(current, " "))
		current = nil
	}

	for _, rawLine := range strings.Split(block, "\n") {
		line := strings.TrimSpace(rawLine)
		if line == "" {
			if heredocEnd == "" {
				flush()
			}
			continue
		}
		if heredocEnd != "" {
			if line == heredocEnd {
				heredocEnd = ""
			}
			continue
		}
		if strings.HasPrefix(line, "#") {
			continue
		}
		if len(current) == 0 && !looksLikeCommandLine(line) && heredocDelimiter(line) == "" {
			continue
		}

		line = strings.TrimPrefix(line, "$ ")
		trimmedLine := strings.TrimSpace(strings.TrimSuffix(line, "\\"))
		if trimmedLine != "" {
			current = append(current, trimmedLine)
		}
		if marker := heredocDelimiter(line); marker != "" {
			heredocEnd = marker
			flush()
			continue
		}
		if strings.HasSuffix(line, "\\") {
			continue
		}
		flush()
	}
	flush()
	return commands
}

func extractEnvVars(markdown string) []string {
	seen := map[string]bool{}
	var vars []string
	for _, token := range envVarRe.FindAllString(markdown, -1) {
		if !looksLikeEnvVar(token) || seen[token] {
			continue
		}
		seen[token] = true
		vars = append(vars, token)
	}
	sort.Strings(vars)
	return vars
}

func looksLikeEnvVar(token string) bool {
	if !strings.Contains(token, "_") {
		return false
	}
	if strings.HasPrefix(token, "CONFIG_") {
		return false
	}
	return true
}

func buildAppendixSection(secNum int, commands []appendixEntry, envVars []appendixEntry) (section, string, []tocEntry) {
	appendix := section{
		title:      "Appendices",
		anchor:     "doc-appendices",
		sourcePath: "generated",
	}
	entries := []tocEntry{
		{level: 2, text: "Command index", id: "appendix-command-index"},
		{level: 2, text: "Environment variables", id: "appendix-env-vars"},
	}

	var body strings.Builder
	body.WriteString(fmt.Sprintf("<section id=\"%s\" class=\"manual-section generated-section\">\n", appendix.anchor))
	body.WriteString(fmt.Sprintf("<h1 class=\"section-header\"><a href=\"#%s\">%d. %s</a></h1>\n", appendix.anchor, secNum, appendix.title))
	body.WriteString(fmt.Sprintf("<div class=\"section-meta\"><span><code>%s</code> · %d command examples · %d environment variables</span></div>\n", appendix.sourcePath, len(commands), len(envVars)))
	body.WriteString(fmt.Sprintf("<h2 id=\"appendix-command-index\"><a class=\"heading-anchor\" href=\"#appendix-command-index\">%d.1 Command index</a></h2>\n", secNum))
	body.WriteString("<p class=\"appendix-intro\">Command examples extracted from the docs. Each entry links back to the sections where it appears.</p>\n")
	body.WriteString(renderAppendixList(commands))
	body.WriteString(fmt.Sprintf("<h2 id=\"appendix-env-vars\"><a class=\"heading-anchor\" href=\"#appendix-env-vars\">%d.2 Environment variables</a></h2>\n", secNum))
	body.WriteString("<p class=\"appendix-intro\">Environment variables and build-time overrides mentioned across the handbook, indexed back to the sections that reference them.</p>\n")
	body.WriteString(renderAppendixList(envVars))
	body.WriteString("</section>\n")

	return appendix, body.String(), entries
}

func renderAppendixList(entries []appendixEntry) string {
	if len(entries) == 0 {
		return `<p class="appendix-empty">No generated entries for this appendix.</p>`
	}
	var buf strings.Builder
	buf.WriteString("<ul class=\"appendix-list\">\n")
	for _, entry := range entries {
		buf.WriteString("<li>")
		buf.WriteString("<code>")
		buf.WriteString(htmlEscape(entry.name))
		buf.WriteString("</code>")
		buf.WriteString(renderAppendixRefs(entry.refs))
		buf.WriteString("</li>\n")
	}
	buf.WriteString("</ul>\n")
	return buf.String()
}

func renderAppendixRefs(refs []appendixRef) string {
	var buf strings.Builder
	buf.WriteString(`<span class="appendix-links">Seen in `)
	for i, ref := range refs {
		if i > 0 {
			buf.WriteString(", ")
		}
		buf.WriteString(fmt.Sprintf(`<a href="#%s">%s</a>`, ref.anchor, htmlEscape(ref.title)))
	}
	buf.WriteString("</span>")
	return buf.String()
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
:root {
	--page-bg: #f1f3f5;
	--paper: #fafafa;
	--panel: #f4f6f8;
	--panel-muted: #f7f8fa;
	--ink: #161514;
	--muted: #5f6670;
	--rule: #d7dde5;
	--rule-strong: #aab3be;
	--accent: #4b5563;
	--shadow: rgba(0, 0, 0, 0.08);
}

*, *::before, *::after { box-sizing: border-box; }

html {
	font-size: 15px;
	-webkit-text-size-adjust: 100%;
	background: linear-gradient(180deg, #eceff3 0%, var(--page-bg) 18rem, #f5f7f9 100%);
}

body {
	font-family: "Iowan Old Style", "Palatino Linotype", Palatino, "Book Antiqua", Georgia, serif;
	line-height: 1.7;
	color: var(--ink);
	max-width: 92rem;
	margin: 0 auto 1.5rem;
	padding: 2rem 2.25rem 3rem;
	background: var(--paper);
	border: 1px solid var(--rule);
	box-shadow: 0 12px 28px var(--shadow);
	min-height: 80vh;
}

.manual-masthead {
	border-bottom: 2px solid var(--rule-strong);
	padding-bottom: 1.25rem;
	margin-bottom: 1.5rem;
}

.manual-kicker,
.section-meta,
nav.toc,
footer,
th,
.toc-toggle {
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}

.manual-kicker {
	font-size: 0.78rem;
	font-weight: 700;
	letter-spacing: 0.08em;
	text-transform: uppercase;
	color: var(--accent);
	margin-bottom: 0.35rem;
}

.manual-masthead h1 {
	font-size: clamp(2.4rem, 5vw, 3.2rem);
	line-height: 1;
	margin: 0;
	letter-spacing: -0.04em;
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}

.manual-lede {
	max-width: 48rem;
	margin: 0.75rem 0 0;
	color: var(--muted);
	font-size: 1rem;
}

.manual-layout {
	display: grid;
	grid-template-columns: minmax(16rem, 20rem) minmax(0, 1fr);
	gap: 2rem;
	align-items: start;
}

.manual-main {
	min-width: 0;
	max-width: 56rem;
}

h1, h2, h3, h4, h5, h6 {
	line-height: 1.2;
	margin: 1.25rem 0 0.4rem;
	scroll-margin-top: 1.5rem;
	font-weight: 700;
	letter-spacing: -0.02em;
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
}

h1 { font-size: 1.6rem; margin-top: 1.5rem; }
h2 { font-size: 1.3rem; margin-top: 1.85rem; }
h3 { font-size: 1.08rem; }
h4 { font-size: 1rem; }

h1:first-child { margin-top: 0; }

p { margin: 0.7rem 0; }

.heading-anchor {
	color: inherit;
	text-decoration: none;
}

.heading-anchor:hover { text-decoration: underline; }

.section-meta {
	display: flex;
	align-items: center;
	justify-content: space-between;
	gap: 0.75rem;
	flex-wrap: wrap;
	font-size: 0.78rem;
	color: var(--muted);
	margin-bottom: 1rem;
	padding-bottom: 0.4rem;
	border-bottom: 1px solid var(--rule);
}

.section-meta code {
	background: transparent;
	color: inherit;
	padding: 0;
}


.section-divider {
	border: none;
	border-top: 1px solid var(--rule-strong);
	margin: 4rem 0 3rem;
}

.section-header {
	font-size: 1.8rem;
	font-weight: 700;
	margin: 0 0 0.5rem;
	color: var(--ink);
}

.section-header a {
	color: inherit;
	text-decoration: none;
}

.section-header a:hover { text-decoration: underline; }

a {
	color: inherit;
	text-decoration: underline;
	text-decoration-thickness: 1px;
	text-underline-offset: 0.12em;
}

a:hover { color: var(--accent); }

code {
	font-family: "Berkeley Mono", "JetBrains Mono", "SF Mono", Menlo, Consolas, monospace;
	font-size: 0.85em;
	background: var(--panel-muted);
	padding: 0.08rem 0.25rem;
	border-radius: 2px;
}

pre {
	background: var(--panel-muted);
	color: var(--ink);
	padding: 0.9rem 0.75rem;
	border-radius: 0;
	border: 1px solid var(--rule);
	counter-reset: line;
	overflow-x: auto;
	white-space: pre;
	box-shadow: inset 0 1px 0 rgba(255, 255, 255, 0.7);
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
	color: #9ca3af;
	user-select: none;
}

table {
	border-collapse: collapse;
	width: 100%;
	margin: 1rem 0;
	font-size: 0.87rem;
}

th, td {
	border: 1px solid var(--rule-strong);
	padding: 0.35rem 0.55rem;
	text-align: left;
}

th {
	font-weight: 700;
	font-size: 0.78rem;
	letter-spacing: 0.05em;
	text-transform: uppercase;
	background: var(--panel);
}

blockquote {
	border-left: 3px solid var(--rule-strong);
	margin: 1rem 0;
	padding: 0.45rem 0.85rem;
	color: var(--muted);
	background: var(--panel-muted);
}

.admonition {
	border-left: 4px solid #000;
	padding: 0.65rem 0.85rem;
	margin: 1rem 0;
	background: var(--panel-muted);
	border: 1px solid var(--rule);
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

.admonition.warning::before {
	content: "[WARNING]";
	display: block;
	font-weight: 700;
	color: #475569;
	font-size: 0.85rem;
	margin-bottom: 0.25rem;
}
.admonition.warning { border-left-color: #475569; }
.admonition.warning strong { color: #475569; }

.admonition strong { display: block; margin-bottom: 0.25rem; text-transform: uppercase; letter-spacing: 0.05em; font-size: 0.85rem; }

hr { border: none; border-top: 1px solid var(--rule-strong); margin: 1rem 0; }

footer {
	border-top: 2px solid var(--rule-strong);
	margin-top: 3rem;
	padding-top: 1rem;
	display: flex;
	flex-wrap: wrap;
	justify-content: space-between;
	gap: 0.5rem 1rem;
	font-size: 0.78rem;
	color: var(--muted);
}

footer code {
	background: transparent;
	color: inherit;
	padding: 0;
}

ul, ol { padding-left: 1.6rem; }
li { margin-bottom: 0.2rem; }

nav.toc {
	position: sticky;
	top: 1.5rem;
	max-height: calc(100vh - 3rem);
	overflow: auto;
	padding: 1rem 1rem 1.1rem;
	border: 1px solid var(--rule);
	background: var(--paper);
}

nav.toc h2 {
	margin-top: 0;
	font-size: 1.05rem;
	border-bottom: 1px solid var(--rule);
	padding-bottom: 0.45rem;
	margin-bottom: 0.5rem;
}

.toc-summary {
	margin: 0 0 0.85rem;
	font-size: 0.78rem;
	color: var(--muted);
}

.toc-sep { border: none; border-top: 1px solid var(--rule); margin: 0.6rem 0; }

.toc-section { margin-bottom: 0.65rem; }

.toc-section-title {
	font-weight: 700;
	text-decoration: none;
}

.toc-path {
	display: block;
	font-size: 0.75rem;
	color: var(--muted);
	background: transparent;
	padding: 0;
	margin-top: 0.15rem;
}

.toc-entries {
	list-style: none;
	padding-left: 0;
	margin: 0.35rem 0 0;
}

.toc-entries li {
	margin-bottom: 0.15rem;
	display: flex;
	align-items: baseline;
	gap: 0.45rem;
}

.toc-entries ul {
	list-style: none;
	padding-left: 0.9rem;
	margin: 0;
	border-left: 1px solid var(--rule);
}

.toc-num {
	font-weight: 400;
	color: var(--muted);
	min-width: 2.55rem;
	flex-shrink: 0;
}

nav.toc a { color: var(--ink); text-decoration: none; }
nav.toc a:hover { color: var(--accent); text-decoration: underline; }
nav.toc a.active,
nav.toc a[aria-current="location"] {
	color: var(--accent);
	text-decoration: underline;
	font-weight: 700;
}

.toc-section-num {
	font-weight: 700;
	margin-right: 0.25rem;
	color: var(--accent);
}

.toc-toggle {
	display: none;
	width: 100%;
	padding: 0.6rem 0.75rem;
	margin-bottom: 1rem;
	background: var(--panel);
	border: 1px solid var(--rule);
	font-family: inherit;
	font-size: 0.95rem;
	font-weight: 700;
	letter-spacing: 0.02em;
	cursor: pointer;
}

.toc-toggle:hover { background: #eef1f4; }

@media print {
	body { max-width: none; padding: 0; border: none; box-shadow: none; background: #fff; }
	html { background: #fff; }
	.manual-layout { display: block; }
	nav.toc { display: none; }
	.toc-toggle { display: none; }
	.manual-section { page-break-before: always; }
	.manual-section:first-child { page-break-before: auto; }
	footer { display: block; }
}

@media (max-width: 980px) {
	body { padding: 1.25rem; margin: 0.5rem; min-height: auto; }
	.manual-layout { display: block; }
	.manual-main { max-width: none; }
	.section-header { position: sticky; top: 0; background: var(--paper); padding: 0.5rem 0; border-bottom: 1px solid var(--rule); z-index: 100; }
	.toc-toggle { display: block; }
	nav.toc { display: none; margin-top: 0.5rem; }
	nav.toc { position: static; max-height: none; }
	nav.toc.expanded { display: block; }
	table { display: block; overflow-x: auto; max-width: 100%; width: max-content; min-width: 100%; }
}

.code-block::before {
	display: block;
	margin-bottom: 0.55rem;
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
	font-size: 0.72rem;
	font-weight: 700;
	letter-spacing: 0.08em;
	text-transform: uppercase;
	white-space: normal;
}

.code-block-command {
	border-left: 3px solid var(--accent);
	background: #f5f6f8;
}

.code-block-command::before {
	content: "Command";
	color: var(--accent);
}

.code-block-example {
	border-left: 3px solid var(--rule-strong);
	background: #f3f4f6;
}

.code-block-example::before {
	content: "Example";
	color: var(--muted);
}

.code-block-output {
	border-left: 3px solid #5d7488;
	background: #f2f5f7;
}

.code-block-output::before {
	content: "Output";
	color: #5d7488;
}

.code-block-diagram {
	border-left: 3px solid #6b7280;
	background: #f3f4f6;
}

.code-block-diagram::before {
	content: "Diagram";
	color: #6b7280;
}

.appendix-intro {
	color: var(--muted);
	margin: 0.6rem 0 1rem;
	font-size: 0.98rem;
}

.appendix-list {
	list-style: none;
	padding-left: 0;
	margin: 1rem 0 0;
	border-top: 1px solid var(--rule);
}

.appendix-list li {
	padding: 0.55rem 0;
	border-bottom: 1px solid var(--rule);
}

.appendix-list code {
	display: inline-block;
	max-width: 100%;
	overflow-x: auto;
	vertical-align: top;
}

.appendix-links {
	display: block;
	margin-top: 0.25rem;
	font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, "Helvetica Neue", Arial, sans-serif;
	font-size: 0.78rem;
	color: var(--muted);
}

.appendix-links a {
	text-decoration: none;
	border-bottom: 1px dotted var(--rule-strong);
}

.appendix-links a:hover {
	border-bottom-color: var(--accent);
	text-decoration: none;
}

.appendix-empty {
	color: var(--muted);
	font-style: italic;
}
`

const js = `(function () {
  const toc = document.getElementById('manual-toc');
  if (!toc) {
    return;
  }

  const links = Array.from(toc.querySelectorAll('a[href^="#"]'));
  const linksByID = new Map();
  const targets = [];

  for (const link of links) {
    const href = link.getAttribute('href');
    if (!href || href === '#') {
      continue;
    }
    const id = decodeURIComponent(href.slice(1));
    const target = document.getElementById(id);
    if (!target) {
      continue;
    }
    if (!linksByID.has(id)) {
      linksByID.set(id, []);
      targets.push(target);
    }
    linksByID.get(id).push(link);
  }

  if (targets.length === 0) {
    return;
  }

  let activeID = '';

	function activationOffset() {
	  if (!window.matchMedia('(max-width: 980px)').matches) {
	    return 32;
    }
    const stickyHeader = document.querySelector('.section-header');
    if (!stickyHeader) {
      return 96;
    }
    return stickyHeader.getBoundingClientRect().height + 24;
  }

  function setActive(id) {
    if (id === activeID) {
      return;
    }
    activeID = id;
    for (const link of links) {
      link.classList.remove('active');
      link.removeAttribute('aria-current');
    }
    if (!id) {
      return;
    }
    const activeLinks = linksByID.get(id) || [];
    for (const link of activeLinks) {
      link.classList.add('active');
      link.setAttribute('aria-current', 'location');
    }
    if (activeLinks.length > 0) {
      keepLinkVisible(activeLinks[0]);
    }
  }

  function keepLinkVisible(link) {
    if (window.getComputedStyle(toc).display === 'none') {
      return;
    }
    const tocRect = toc.getBoundingClientRect();
    const linkRect = link.getBoundingClientRect();
    const padding = 16;

    if (linkRect.top < tocRect.top + padding) {
      toc.scrollTop += linkRect.top - tocRect.top - padding;
      return;
    }
    if (linkRect.bottom > tocRect.bottom - padding) {
      toc.scrollTop += linkRect.bottom - tocRect.bottom + padding;
    }
  }

  function pickActiveID() {
    const offset = activationOffset();
    let current = '';
    for (const target of targets) {
      if (target.getBoundingClientRect().top <= offset) {
        current = target.id;
        continue;
      }
      break;
    }
    if (current) {
      return current;
    }
    if (window.location.hash.length > 1) {
      return decodeURIComponent(window.location.hash.slice(1));
    }
    return targets[0].id;
  }

  function updateActiveLink() {
    setActive(pickActiveID());
	}

	function scheduleUpdate() {
	  updateActiveLink();
	}

  window.addEventListener('scroll', scheduleUpdate, { passive: true });
  window.addEventListener('resize', scheduleUpdate);
  window.addEventListener('hashchange', scheduleUpdate);
  updateActiveLink();
})();`

func buildTOC(sections []section, tocsBySection [][]tocEntry) string {
	var buf strings.Builder

	buf.WriteString("<nav id=\"manual-toc\" class=\"toc\">\n<h2>Table of Contents</h2>\n<p class=\"toc-summary\">Single-page handbook for quick in-page search, long-form reading, and printable offline use.</p>\n")

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
	commandEntries, envVarEntries := collectAppendixEntries(sections)

	globalCounter := make(map[string]int)

	var body strings.Builder
	var tocsBySection [][]tocEntry
	headingCount := 0

	for i, s := range sections {
		if i > 0 {
			body.WriteString("<hr class=\"section-divider\">\n")
		}
		secNum := i + 1
		body.WriteString(fmt.Sprintf("<section id=\"%s\" class=\"manual-section\">\n<h1 class=\"section-header\"><a href=\"#%s\">%d. %s</a></h1>\n<div class=\"section-meta\"><span><code>%s</code> · %d words</span></div>\n", s.anchor, s.anchor, secNum, s.title, s.sourcePath, len(strings.Fields(s.markdown))))
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
	appendixSection, appendixHTML, appendixTOC := buildAppendixSection(len(sections)+1, commandEntries, envVarEntries)
	body.WriteString("<hr class=\"section-divider\">\n")
	body.WriteString(appendixHTML)
	tocSections := append(append([]section{}, sections...), appendixSection)
	tocsBySection = append(tocsBySection, appendixTOC)
	headingCount += len(appendixTOC)
	sectionCount := len(tocSections)

	footerHTML := fmt.Sprintf(`<footer>
<div>Sections: %d · Headings: %d · Words: %d</div>
<div><a href="#manual-top">Back to top</a></div>
<div>MIT License · Copyright (c) 2026 Rene Michaelis</div>
</footer>
`, sectionCount, headingCount, totalWords)

	var out strings.Builder
	out.WriteString("<!DOCTYPE html>\n<html lang=\"en\">\n<head>\n")
	out.WriteString("<meta charset=\"utf-8\">\n")
	out.WriteString("<meta name=\"viewport\" content=\"width=device-width, initial-scale=1\">\n")
	out.WriteString("<title>srv manual</title>\n")
	out.WriteString("<style>\n")
	out.WriteString(css)
	out.WriteString("\n</style>\n")
	out.WriteString("</head>\n<body>\n")
	out.WriteString("<header id=\"manual-top\" class=\"manual-masthead\">\n<div class=\"manual-kicker\">Reference Manual</div>\n<h1>srv</h1>\n<p class=\"manual-lede\">Single-page operator and developer handbook generated from the repository documentation. Built for quick in-page search, long-form reading, and printable offline use.</p>\n</header>\n")
	out.WriteString("<button type=\"button\" class=\"toc-toggle\" aria-controls=\"manual-toc\" aria-expanded=\"false\" onclick=\"var toc=document.getElementById('manual-toc');var expanded=toc.classList.toggle('expanded');this.setAttribute('aria-expanded', expanded);\">Table of Contents</button>\n")
	out.WriteString("<div class=\"manual-layout\">\n")
	tocHTML = buildTOC(tocSections, tocsBySection)
	out.WriteString(tocHTML)
	out.WriteString("<main class=\"manual-main\">\n")
	out.WriteString(body.String())
	out.WriteString(footerHTML)
	out.WriteString("</main>\n</div>\n")
	out.WriteString("<script>\n")
	out.WriteString(js)
	out.WriteString("\n</script>\n")
	out.WriteString("</body>\n</html>\n")

	if err := os.WriteFile(outPath, []byte(out.String()), 0644); err != nil {
		fmt.Fprintf(os.Stderr, "error writing %s: %v\n", outPath, err)
		os.Exit(1)
	}

	fmt.Fprintf(os.Stderr, "wrote %s (%d sections, %d headings)\n", outPath, len(sections), headingCount)
}
