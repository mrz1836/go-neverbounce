package neverbounce

import (
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// readmeSources are the compiled files whose code the README's Go blocks must
// quote: the package's examples and the example programs.
func readmeSources() []string {
	return []string{"example_test.go", "examples/main.go", "examples/quickstart/main.go"}
}

// readReadme returns README.md.
func readReadme(tb testing.TB) string {
	tb.Helper()
	data, err := os.ReadFile("README.md")
	require.NoError(tb, err)
	return string(data)
}

// prose returns md without its fenced code blocks, so that code such as
// errors.AsType[*neverbounce.CallError](err) is never read as a link.
func prose(md string) string {
	return regexp.MustCompile("(?s)```.*?```").ReplaceAllString(md, "")
}

// normalizeSpace collapses every run of whitespace to one space, so that a
// README block matches its source at any indentation.
func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }

// fencedBlocks returns the content of every fenced code block in md whose info
// string is lang.
func fencedBlocks(md, lang string) []string {
	re := regexp.MustCompile("(?s)```" + regexp.QuoteMeta(lang) + "\n(.*?)\n```")
	matches := re.FindAllStringSubmatch(md, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		out = append(out, m[1])
	}
	return out
}

func TestReadmeGoBlocksAreCompiled(t *testing.T) {
	var sources strings.Builder
	for _, name := range readmeSources() {
		data, err := os.ReadFile(filepath.FromSlash(name))
		require.NoError(t, err)
		sources.WriteString(normalizeSpace(string(data)))
		sources.WriteString("\n")
	}
	compiled := sources.String()
	blocks := fencedBlocks(readReadme(t), "go")
	require.NotEmpty(t, blocks)
	for i, block := range blocks {
		assert.Contains(t, compiled, normalizeSpace(block), "README Go block %d is not quoted verbatim from %v:\n%s",
			i+1, readmeSources(), block)
	}
}

func TestReadmeHasNoPlaceholders(t *testing.T) {
	assert.NotContains(t, readReadme(t), "<!--", "every placeholder must be filled in")
}

func TestReadmeRelativeLinksResolve(t *testing.T) {
	md := prose(readReadme(t))
	links := regexp.MustCompile(`(?:\]\(|href=")([^)"#][^)"]*)`).FindAllStringSubmatch(md, -1)
	require.NotEmpty(t, links)
	for _, m := range links {
		target := m[1]
		if strings.Contains(target, "://") || strings.HasPrefix(target, "mailto:") {
			continue
		}
		target, _, _ = strings.Cut(target, "#")
		_, err := os.Stat(filepath.FromSlash(target))
		assert.NoError(t, err, "README links to a missing file: %s", target)
	}
}

// headingSlug returns GitHub's anchor for a heading: lowercase, keeping
// letters, marks, numbers, underscores, hyphens and spaces, with each space
// turned into a hyphen.
func headingSlug(heading string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(heading) {
		switch {
		case r == ' ':
			b.WriteRune('-')
		case r == '-' || r == '_' || unicode.IsLetter(r) || unicode.IsMark(r) || unicode.IsNumber(r):
			b.WriteRune(r)
		}
	}
	return b.String()
}

func TestReadmeAnchorsMatchHeadings(t *testing.T) {
	md := readReadme(t)
	slugs := map[string]bool{}
	inFence := false
	for line := range strings.SplitSeq(md, "\n") {
		if strings.HasPrefix(line, "```") {
			inFence = !inFence
			continue
		}
		if inFence || !strings.HasPrefix(line, "#") {
			continue
		}
		heading := strings.TrimSpace(strings.TrimLeft(line, "#"))
		heading = strings.ReplaceAll(heading, "&nbsp;", " ")
		slugs[headingSlug(heading)] = true
	}
	anchors := regexp.MustCompile(`(?:\]\(|href=")#([^)"]+)`).FindAllStringSubmatch(prose(md), -1)
	require.NotEmpty(t, anchors)
	for _, m := range anchors {
		anchor, err := url.PathUnescape(m[1])
		require.NoError(t, err)
		assert.True(t, slugs[anchor], "README links to #%s, which no heading produces", m[1])
	}
}

// TestReadmeLimitsMatchCode checks every default and limit the README states
// against the constant that enforces it.
func TestReadmeLimitsMatchCode(t *testing.T) {
	md := readReadme(t)
	tests := []struct {
		claim string
		ok    bool
	}{
		{"60s (`DefaultTimeout`)", DefaultTimeout == 60*time.Second},
		{"at most 10 minutes", maxTimeout == 10*time.Minute},
		{"8 MiB (`DefaultMaxResponseBytes`)", DefaultMaxResponseBytes == 8<<20},
		{"1 byte to 64 MiB", maxResponseBytesLimit == 64<<20},
		{"also capped at 1 MiB", smallResponseCap == 1<<20},
		{"30 minutes (`DefaultDownloadTimeout`)", DefaultDownloadTimeout == 30*time.Minute},
		{"at most 12 hours", maxDownloadTimeout == 12*time.Hour},
		{"1 GiB (`DefaultMaxDownloadBytes`)", DefaultMaxDownloadBytes == 1<<30},
		{"1 byte to 64 GiB", maxDownloadBytesLimit == 64<<30},
		{"1 to 256 bytes of visible ASCII", maxKeyBytes == 256},
		{"1 to 1024 bytes of UTF-8", maxEmailBytes == 1024},
		{"1s to 300s", minCheckTimeout == time.Second && maxCheckTimeout == 300*time.Second},
		{"at least 10 seconds below `WithTimeout`", checkTimeoutHeadroom == 10*time.Second},
		{"25,000,000 bytes (`MaxJobRequestBytes`)", MaxJobRequestBytes == 25_000_000},
		{"| Any other request | 64 KiB |", maxSmallRequestBytes == 64<<10},
		{
			"1 to 128 columns of up to 256 bytes; values up to 4096 bytes",
			maxColumns == 128 && maxColumnBytes == 256 && maxValueBytes == 4096,
		},
		{"8192, 2048 and 255 bytes", maxRemoteURLBytes == 8192 && maxCallbackURLBytes == 2048 && maxFilenameBytes == 255},
		{
			"32; names up to 128 bytes, values up to 1024",
			maxCallbackHeaders == 32 && maxHeaderNameBytes == 128 && maxHeaderValueBytes == 1024,
		},
		{"| Error response body read | 64 KiB, best-effort |", maxErrorBodyBytes == 64<<10},
		{"| Response headers (SDK-built transport) | 64 KiB |", maxResponseHeaderBytes == 64<<10},
		{"100,000 / 1,000,000", maxContainers == 100_000 && maxValues == 1_000_000},
		{"4 KiB / 64 / 64 KiB", maxTextBytes == 4<<10 && maxFlags == 64 && maxDataBytes == 64<<10},
		{"`ItemsPerPage`, or 1000 when not sent", MaxItemsPerPage == 1000},
		{
			"`Interval` 1 second to 10 minutes; `MaxWait` up to 72 hours",
			minWaitInterval == time.Second && maxWaitInterval == 10*time.Minute && maxWaitLimit == 72*time.Hour,
		},
		{"`MaxPages` 1 to 100,000; 100 rows per page by default", maxPagesLimit == 100_000 && DefaultPageSize == 100},
		{"10 seconds by default", DefaultWaitInterval == 10*time.Second},
		{"cut to 1 KiB", maxDiagnosticMessageBytes == 1<<10},
	}
	for _, tt := range tests {
		assert.Contains(t, md, tt.claim)
		assert.True(t, tt.ok, "the code no longer matches the README claim %q", tt.claim)
	}
}
