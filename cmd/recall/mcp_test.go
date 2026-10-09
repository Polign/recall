package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// runMCP feeds the requests to a server and returns its responses by id.
func runMCP(t *testing.T, s *mcpServer, requests ...string) map[float64]rpcResponse {
	t.Helper()
	var buf bytes.Buffer
	s.enc = json.NewEncoder(&buf)
	if err := s.serve(context.Background(), strings.NewReader(strings.Join(requests, "\n"))); err != nil {
		t.Fatalf("serve: %v", err)
	}
	out := map[float64]rpcResponse{}
	dec := json.NewDecoder(&buf)
	for {
		var resp rpcResponse
		if err := dec.Decode(&resp); err != nil {
			break
		}
		var id float64
		if err := json.Unmarshal(resp.ID, &id); err != nil {
			t.Fatalf("response id %s: %v", resp.ID, err)
		}
		out[id] = resp
	}
	return out
}

// resultOf re-marshals a response result into v, which is how the test reads
// the map[string]any the handlers build.
func resultOf(t *testing.T, resp rpcResponse, v any) {
	t.Helper()
	if resp.Error != nil {
		t.Fatalf("unexpected error response: %+v", resp.Error)
	}
	b, err := json.Marshal(resp.Result)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatal(err)
	}
}

// toolText returns the text payload of a tools/call result and whether the
// call was reported as a failure.
func toolText(t *testing.T, resp rpcResponse) (string, bool) {
	t.Helper()
	var r struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	resultOf(t, resp, &r)
	if len(r.Content) != 1 || r.Content[0].Type != "text" {
		t.Fatalf("want one text content block, got %+v", r.Content)
	}
	return r.Content[0].Text, r.IsError
}

func TestMCPInitialize(t *testing.T) {
	s := newMemoryMCP(t)
	s.write = false
	resps := runMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`)

	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
		Capabilities    struct {
			Tools *struct{} `json:"tools"`
		} `json:"capabilities"`
		ServerInfo struct {
			Name string `json:"name"`
		} `json:"serverInfo"`
		Instructions string `json:"instructions"`
	}
	resultOf(t, resps[1], &r)

	// A version we speak is echoed back rather than overridden.
	if r.ProtocolVersion != "2024-11-05" {
		t.Errorf("protocolVersion = %q, want the client's 2024-11-05", r.ProtocolVersion)
	}
	if r.Capabilities.Tools == nil {
		t.Error("tools capability not advertised")
	}
	if r.ServerInfo.Name != "recall" {
		t.Errorf("serverInfo.name = %q, want recall", r.ServerInfo.Name)
	}
	if !strings.Contains(r.Instructions, "memories") {
		t.Errorf("instructions do not name the collection: %q", r.Instructions)
	}
	if !strings.Contains(r.Instructions, "read-only") {
		t.Errorf("a read-only server should say so: %q", r.Instructions)
	}
}

func TestMCPInitializeUnknownVersion(t *testing.T) {
	s := &mcpServer{api: &api{base: "http://unused"}}
	resps := runMCP(t, s, `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"1999-01-01"}}`)

	var r struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	resultOf(t, resps[1], &r)
	if r.ProtocolVersion != mcpVersions[0] {
		t.Errorf("protocolVersion = %q, want our newest %q", r.ProtocolVersion, mcpVersions[0])
	}
}

func TestMCPUnknownToolAndMethod(t *testing.T) {
	s := &mcpServer{api: &api{base: "http://unused"}, collection: "memory"}
	resps := runMCP(t, s,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"polign_teleport","arguments":{}}}`,
		`{"jsonrpc":"2.0","id":2,"method":"resources/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"ping"}`)

	if text, isErr := toolText(t, resps[1]); !isErr || !strings.Contains(text, "unknown memory tool") {
		t.Errorf("want an unknown-tool failure, got isError=%v %q", isErr, text)
	}
	// An unsupported method is a protocol error, unlike a failing tool call.
	if resps[2].Error == nil || resps[2].Error.Code != codeMethodNotFound {
		t.Errorf("resources/list error = %+v, want code %d", resps[2].Error, codeMethodNotFound)
	}
	if resps[3].Error != nil {
		t.Errorf("ping failed: %+v", resps[3].Error)
	}
}

// A notification carries no id and must draw no response, or the client sees
// a reply it never asked for.
func TestMCPNotificationIsSilent(t *testing.T) {
	s := &mcpServer{api: &api{base: "http://unused"}, collection: "memory"}
	resps := runMCP(t, s,
		`{"jsonrpc":"2.0","method":"notifications/initialized"}`,
		`{"jsonrpc":"2.0","id":1,"method":"ping"}`)

	if len(resps) != 1 {
		t.Errorf("got %d responses, want only the ping's", len(resps))
	}
}

func TestMCPMalformedInputEndsTheSession(t *testing.T) {
	s := &mcpServer{api: &api{base: "http://unused"}}
	var buf bytes.Buffer
	s.enc = json.NewEncoder(&buf)
	err := s.serve(context.Background(), strings.NewReader("{not json}\n"))
	if err == nil {
		t.Fatal("want an error: the decoder cannot resynchronise mid-value")
	}
	if !strings.Contains(buf.String(), `"code":-32700`) {
		t.Errorf("want a parse error reported to the client, got %q", buf.String())
	}
}
