package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// specTool is one row of a tool table in docs/memory-api-v1.md.
type specTool struct {
	required []string
	readOnly bool
}

// readSpecTables parses the tool tables under the named headings of the
// Memory API document. A row's second column lists the required arguments,
// comma separated; a column starting with "none" means no top-level
// requirement. The third column is "yes" for a read-only tool.
func readSpecTables(t *testing.T, headings ...string) map[string]map[string]specTool {
	t.Helper()
	path := filepath.Join("..", "..", "docs", "memory-api-v1.md")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the Memory API document must exist beside the code: %v", err)
	}
	defer f.Close()
	out := map[string]map[string]specTool{}
	var current string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") {
			current = ""
			for _, h := range headings {
				if strings.HasPrefix(line, h) {
					current = h
					out[h] = map[string]specTool{}
				}
			}
			continue
		}
		if current == "" || !strings.HasPrefix(line, "|") {
			continue
		}
		cells := strings.Split(strings.Trim(line, "|"), "|")
		if len(cells) < 3 {
			continue
		}
		name := strings.TrimSpace(cells[0])
		if name == "Tool" || strings.HasPrefix(name, "---") {
			continue
		}
		req := strings.TrimSpace(cells[1])
		var required []string
		if !strings.HasPrefix(req, "none") {
			for _, r := range strings.Split(req, ",") {
				required = append(required, strings.TrimSpace(r))
			}
		}
		out[current][name] = specTool{required: required, readOnly: strings.TrimSpace(cells[2]) == "yes"}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func toolRequired(tool mcpTool) []string {
	req, _ := tool.InputSchema["required"].([]string)
	out := append([]string(nil), req...)
	sort.Strings(out)
	return out
}

// compareTools fails when the served tools and the documented table differ in
// names, required arguments or the read-only annotation.
func compareTools(t *testing.T, section string, served []mcpTool, documented map[string]specTool) {
	t.Helper()
	seen := map[string]bool{}
	for _, tool := range served {
		seen[tool.Name] = true
		doc, ok := documented[tool.Name]
		if !ok {
			t.Errorf("%s: tool %q is served but not in docs/memory-api-v1.md", section, tool.Name)
			continue
		}
		got := toolRequired(tool)
		want := append([]string(nil), doc.required...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s required arguments: served %v, documented %v", section, tool.Name, got, want)
		}
		if ro := tool.Annotations != nil; ro != doc.readOnly {
			t.Errorf("%s: %s read-only: served %v, documented %v", section, tool.Name, ro, doc.readOnly)
		}
	}
	for name := range documented {
		if !seen[name] {
			t.Errorf("%s: tool %q is documented but not served", section, name)
		}
	}
}

// TestMemoryAPIDocumentMatchesServedTools keeps docs/memory-api-v1.md honest:
// the typed, text and resume tool tables must name exactly the tools the MCP
// server serves, with the same required arguments and read-only marks.
func TestMemoryAPIDocumentMatchesServedTools(t *testing.T) {
	const (
		typed  = "### 2.1 Typed tools"
		text   = "### 2.2 Text tools"
		resume = "## 6. Resume API"
	)
	tables := readSpecTables(t, typed, text, resume)
	for _, h := range []string{typed, text, resume} {
		if len(tables[h]) == 0 {
			t.Fatalf("no tool table found under %q", h)
		}
	}

	s := newMemoryMCP(t)
	compareTools(t, "typed", s.memoryTools(), tables[typed])

	s.memory.text = true
	compareTools(t, "text", s.textTools(), tables[text])

	s.agents = newAgentRuntime()
	compareTools(t, "resume", s.agentTools(), tables[resume])
}
