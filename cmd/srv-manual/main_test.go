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

func TestAddLineNumbersToCodeBlocksClassifiesCommandsAndPreservesAttrs(t *testing.T) {
	html := `<pre><code class="language-bash">ssh srv list
</code></pre>`

	got := addLineNumbersToCodeBlocks(html)

	if !strings.Contains(got, `class="code-block code-block-command"`) {
		t.Fatalf("expected command classification on pre block, got %q", got)
	}
	if !strings.Contains(got, `<code class="language-bash">`) {
		t.Fatalf("expected original code attrs to be preserved, got %q", got)
	}
}

func TestAddLineNumbersToCodeBlocksHonorsManualOverride(t *testing.T) {
	html := `<!-- srv-manual:block=diagram -->
<pre><code>┌── srv ──┐
</code></pre>`

	got := addLineNumbersToCodeBlocks(html)

	if !strings.Contains(got, `class="code-block code-block-diagram"`) {
		t.Fatalf("expected manual diagram classification, got %q", got)
	}
	if strings.Contains(got, `srv-manual:block=diagram`) {
		t.Fatalf("expected override comment to be consumed, got %q", got)
	}
}

func TestAddHeadingAnchorsAndNumbersReplacesWholeHeading(t *testing.T) {
	html := `<h2 id="build">Build</h2><h3 id="with-code">Use <code>srv</code></h3>`
	entries := []tocEntry{
		{level: 2, text: "Build", id: "build"},
		{level: 3, text: "Use srv", id: "with-code"},
	}

	got := addHeadingAnchorsAndNumbers(html, 13, entries)

	if strings.Contains(got, `</h2></h2>`) || strings.Contains(got, `</h3></h3>`) {
		t.Fatalf("expected headings to have a single closing tag, got %q", got)
	}
	if !strings.Contains(got, `<h2 id="build"><a class="heading-anchor" href="#build">13.1 Build</a></h2>`) {
		t.Fatalf("expected numbered h2 heading, got %q", got)
	}
	if !strings.Contains(got, `<h3 id="with-code"><a class="heading-anchor" href="#with-code">13.1.1 Use <code>srv</code></a></h3>`) {
		t.Fatalf("expected inline heading markup to be preserved, got %q", got)
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

func TestExtractCommandExamplesMergesMultilineCommandsAndSkipsOutput(t *testing.T) {
	markdown := "```bash\n# Install\nsudo podman run --rm --privileged \\\n  --network host \\\n  docker.io/library/archlinux:latest\ndemo created - state: provisioning\n```\n\n```text\ndemo created - state: provisioning\n```\n"

	got := extractCommandExamples(markdown)

	if len(got) != 1 {
		t.Fatalf("expected one extracted command, got %v", got)
	}
	if got[0] != "sudo podman run --rm --privileged --network host docker.io/library/archlinux:latest" {
		t.Fatalf("unexpected merged command: %q", got[0])
	}
}

func TestExtractCommandExamplesSkipsHeredocBodies(t *testing.T) {
	markdown := "```bash\nsudo tee /etc/sysctl.d/test.conf >/dev/null <<'EOF'\nnet.ipv4.ip_forward = 1\nEOF\nsudo sysctl --system\n```\n"

	got := extractCommandExamples(markdown)
	want := []string{
		"sudo tee /etc/sysctl.d/test.conf >/dev/null <<'EOF'",
		"sudo sysctl --system",
	}

	if len(got) != len(want) {
		t.Fatalf("len(got) = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestExtractEnvVarsFiltersKernelConfigSymbols(t *testing.T) {
	markdown := "`SRV_ZEN_API_KEY` and `OUTPUT_DIR` are supported. `CONFIG_LSM` is not an env var."

	got := extractEnvVars(markdown)

	if strings.Contains(strings.Join(got, ","), "CONFIG_LSM") {
		t.Fatalf("expected kernel config symbols to be excluded, got %v", got)
	}
	if !strings.Contains(strings.Join(got, ","), "SRV_ZEN_API_KEY") || !strings.Contains(strings.Join(got, ","), "OUTPUT_DIR") {
		t.Fatalf("expected documented env vars to be retained, got %v", got)
	}
}

func TestLooksLikeCommandLineRejectsPureEnvAssignmentsAndPaths(t *testing.T) {
	if looksLikeCommandLine("SRV_BASE_KERNEL=/var/lib/srv/images/arch-base/vmlinux") {
		t.Fatalf("expected pure env assignments to stay out of the command index")
	}
	if looksLikeCommandLine("SRV_DATA_DIR/") {
		t.Fatalf("expected bare path examples to stay out of the command index")
	}
	if !looksLikeCommandLine("OUTPUT_DIR=/var/lib/srv/images/arch-base ./images/arch-base/build.sh") {
		t.Fatalf("expected env-prefixed command lines to remain command-like")
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

func TestMobileCSSShowsTOCToggleAndLetsTablesScroll(t *testing.T) {
	baseToggle := strings.Index(css, ".toc-toggle {\n\tdisplay: none;")
	mobile := strings.Index(css, "@media (max-width: 980px) {")
	if baseToggle == -1 || mobile == -1 {
		t.Fatalf("expected both base and mobile CSS blocks")
	}
	if baseToggle > mobile {
		t.Fatalf("expected the default TOC toggle rule before the mobile override")
	}
	mobileCSS := css[mobile:]
	if !strings.Contains(mobileCSS, "\t.toc-toggle { display: block; }") {
		t.Fatalf("expected mobile CSS to reveal the TOC toggle")
	}
	if !strings.Contains(mobileCSS, "\ttable { display: block; overflow-x: auto; max-width: 100%; width: max-content; min-width: 100%; }") {
		t.Fatalf("expected mobile CSS to keep wide tables scrollable inside the content column")
	}
}

func TestManualJSIncludesTOCScrollspy(t *testing.T) {
	if !strings.Contains(js, `document.getElementById('manual-toc')`) {
		t.Fatalf("expected TOC scrollspy script to target the manual TOC")
	}
	if !strings.Contains(js, `link.setAttribute('aria-current', 'location')`) {
		t.Fatalf("expected TOC scrollspy script to expose the active location semantically")
	}
	if !strings.Contains(js, `window.addEventListener('scroll', scheduleUpdate, { passive: true })`) {
		t.Fatalf("expected TOC scrollspy script to update on scroll")
	}
	if !strings.Contains(js, `toc.scrollTop +=`) {
		t.Fatalf("expected TOC scrollspy script to keep the active link visible inside the TOC")
	}
}
