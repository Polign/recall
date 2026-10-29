package main

import (
	"bufio"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// specTool is one row of a tool table in docs/memory-api-v1.md or
// docs/state-api-v1.md.
type specTool struct {
	required []string
	readOnly bool
	// http is the verb under /v1/agent/, when the table has that column.
	http string
}

// readSpecTables parses the tool tables under the named headings of an API
// document. Columns are found by the header row: "Tool", "Required
// arguments" (comma separated; a cell starting with "none" means no top-level
// requirement), "Read-only" ("yes" for a read-only tool) and, in the State
// API document, "HTTP verb".
func readSpecTables(t *testing.T, doc string, headings ...string) map[string]map[string]specTool {
	t.Helper()
	path := filepath.Join("..", "..", "docs", doc)
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("the API document must exist beside the code: %v", err)
	}
	defer f.Close()
	out := map[string]map[string]specTool{}
	var current string
	var columns map[string]int
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
		for i := range cells {
			cells[i] = strings.TrimSpace(cells[i])
		}
		if len(cells) < 3 {
			continue
		}
		if cells[0] == "Tool" {
			columns = map[string]int{}
			for i, c := range cells {
				columns[c] = i
			}
			continue
		}
		if strings.HasPrefix(cells[0], "---") || columns == nil {
			continue
		}
		cell := func(name string) string {
			i, ok := columns[name]
			if !ok || i >= len(cells) {
				return ""
			}
			return cells[i]
		}
		req := cell("Required arguments")
		var required []string
		if !strings.HasPrefix(req, "none") {
			for _, r := range strings.Split(req, ",") {
				required = append(required, strings.TrimSpace(r))
			}
		}
		out[current][cells[0]] = specTool{required: required, readOnly: cell("Read-only") == "yes", http: cell("HTTP verb")}
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
func compareTools(t *testing.T, doc, section string, served []mcpTool, documented map[string]specTool) {
	t.Helper()
	seen := map[string]bool{}
	for _, tool := range served {
		seen[tool.Name] = true
		spec, ok := documented[tool.Name]
		if !ok {
			t.Errorf("%s: tool %q is served but not in docs/%s", section, tool.Name, doc)
			continue
		}
		got := toolRequired(tool)
		want := append([]string(nil), spec.required...)
		sort.Strings(want)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: %s required arguments: served %v, documented %v", section, tool.Name, got, want)
		}
		if ro := tool.Annotations != nil; ro != spec.readOnly {
			t.Errorf("%s: %s read-only: served %v, documented %v", section, tool.Name, ro, spec.readOnly)
		}
	}
	for name := range documented {
		if !seen[name] {
			t.Errorf("%s: tool %q is documented but not served", section, name)
		}
	}
}

// TestMemoryAPIDocumentMatchesServedTools keeps docs/memory-api-v1.md honest:
// the typed and text tool tables must name exactly the tools the MCP server
// serves, with the same required arguments and read-only marks.
func TestMemoryAPIDocumentMatchesServedTools(t *testing.T) {
	const (
		doc   = "memory-api-v1.md"
		typed = "### 2.1 Typed tools"
		text  = "### 2.2 Text tools"
	)
	tables := readSpecTables(t, doc, typed, text, "## 6.")
	for _, h := range []string{typed, text} {
		if len(tables[h]) == 0 {
			t.Fatalf("no tool table found under %q", h)
		}
	}
	if len(tables["## 6."]) != 0 {
		t.Errorf("section 6 of the Memory API document holds a tool table; the State API tools are documented in state-api-v1.md")
	}

	s := newMemoryMCP(t)
	compareTools(t, doc, "typed", s.memoryTools(), tables[typed])

	s.memory.text = true
	compareTools(t, doc, "text", s.textTools(), tables[text])
}

// TestStateAPIDocumentMatchesServedTools keeps docs/state-api-v1.md honest:
// its tool table must name exactly the agent tools the MCP server serves,
// with their required arguments and read-only marks, and the HTTP verb column
// must be exactly the routes under /v1/agent/.
func TestStateAPIDocumentMatchesServedTools(t *testing.T) {
	const (
		doc   = "state-api-v1.md"
		tools = "## 2. Tools"
	)
	table := readSpecTables(t, doc, tools)[tools]
	if len(table) == 0 {
		t.Fatalf("no tool table found under %q", tools)
	}
	s := newMemoryMCP(t)
	s.agents = newAgentRuntime()
	compareTools(t, doc, "state", s.agentTools(), table)

	for name, spec := range table {
		if spec.http == "" {
			t.Errorf("%s has no HTTP verb in the document", name)
			continue
		}
		if got := restAgentVerbs[spec.http]; got != name {
			t.Errorf("documented verb %q serves %q, want %q", spec.http, got, name)
		}
	}
	for verb, tool := range restAgentVerbs {
		if table[tool].http != verb {
			t.Errorf("route /v1/agent/%s serves %s, which the document gives verb %q", verb, tool, table[tool].http)
		}
		if !agentTools[tool] {
			t.Errorf("route /v1/agent/%s names %q, which is not an agent tool", verb, tool)
		}
	}
	for tool := range agentTools {
		found := false
		for _, t2 := range restAgentVerbs {
			if t2 == tool {
				found = true
			}
		}
		if !found {
			t.Errorf("agent tool %s has no route under /v1/agent/", tool)
		}
	}
}
