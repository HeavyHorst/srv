package main

import (
	"strings"
	"testing"
)

func TestAddLineNumbersToCodeBlocksDoesNotInsertInterLineWhitespace(t *testing.T) {
	html := `<pre><code>first
second
</code></pre>`

	got := addLineNumbersToCodeBlocks(html)

	if strings.Contains(got, "</span>\n<span") {
		t.Fatalf("expected adjacent line spans without preserved newlines, got %q", got)
	}
	if !strings.Contains(got, `<span class="line">first</span><span class="line">second</span>`) {
		t.Fatalf("expected code lines to stay wrapped in order, got %q", got)
	}
}

func TestPreprocessAdmonitionsTrimsEachIndentedLine(t *testing.T) {
	content := "!!! note\n    First line.\n    Second line.\n"

	got := preprocessAdmonitions(content, makeGoldmark())

	if strings.Contains(got, "<pre><code>") {
		t.Fatalf("expected admonition body to render as markdown, got %q", got)
	}
	if strings.Contains(got, "    Second line.") {
		t.Fatalf("expected admonition indentation to be trimmed from every line, got %q", got)
	}
	if !strings.Contains(got, "First line.") || !strings.Contains(got, "Second line.") {
		t.Fatalf("expected admonition body to contain both lines, got %q", got)
	}
}

func TestResolveLinksRewritesDocLinks(t *testing.T) {
	anchorMap := map[string]string{
		"reference/troubleshooting.md": "doc-reference-troubleshooting",
	}

	got := resolveLinks("[smoke](../reference/troubleshooting.md)", "getting-started/install.md", anchorMap)
	if got != "[smoke](#doc-reference-troubleshooting)" {
		t.Fatalf("unexpected rewritten link: %q", got)
	}

	got = resolveLinks("[section](../reference/troubleshooting.md#smoke-test)", "getting-started/install.md", anchorMap)
	if got != "[section](#smoke-test)" {
		t.Fatalf("unexpected rewritten fragment link: %q", got)
	}
}

func TestNextUniqueIDHandlesGeneratedAndExplicitCollisions(t *testing.T) {
	counter := map[string]int{}
	ids := []string{
		nextUniqueID("bar", counter),
		nextUniqueID("bar", counter),
		nextUniqueID("bar-1", counter),
		nextUniqueID("bar", counter),
	}
	want := []string{"bar", "bar-1", "bar-1-1", "bar-2"}

	for i := range want {
		if ids[i] != want[i] {
			t.Fatalf("id %d = %q, want %q", i, ids[i], want[i])
		}
	}
}

func TestSectionAnchorUsesPath(t *testing.T) {
	first := sectionAnchor("getting-started/setup.md")
	second := sectionAnchor("reference/setup.md")

	if first == second {
		t.Fatalf("expected anchors to differ for different paths, got %q", first)
	}
	if first != "doc-getting-started-setup" {
		t.Fatalf("unexpected first anchor: %q", first)
	}
	if second != "doc-reference-setup" {
		t.Fatalf("unexpected second anchor: %q", second)
	}
}

func TestExtractFirstHeadingTrimsCRLF(t *testing.T) {
	content := "# Hello world\r\n\r\nBody\r\n"

	if got := extractFirstHeading(content); got != "Hello world" {
		t.Fatalf("extractFirstHeading() = %q, want %q", got, "Hello world")
	}
}

func TestBuildTOCUsesPerSectionEntries(t *testing.T) {
	sections := []section{
		{title: "First", anchor: "doc-first"},
		{title: "Second", anchor: "doc-second"},
	}
	tocsBySection := [][]tocEntry{
		{{level: 2, text: "Alpha", id: "alpha"}},
		{{level: 2, text: "Beta", id: "beta"}},
	}

	got := buildTOC(sections, tocsBySection)

	if !strings.Contains(got, "#doc-first") || !strings.Contains(got, "#doc-second") {
		t.Fatalf("expected top-level section anchors in TOC, got %q", got)
	}
	if !strings.Contains(got, "#alpha") || !strings.Contains(got, "#beta") {
		t.Fatalf("expected per-section subentries in TOC, got %q", got)
	}
	if strings.Index(got, "#alpha") > strings.Index(got, "#doc-second") {
		t.Fatalf("expected first section subentry to stay with first section, got %q", got)
	}
}
