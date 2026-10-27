package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
)

// fakeClaude installs a stand-in CLI that records its arguments, environment,
// stdin and the MCP config it was handed, then prints the given stream-json lines.
func fakeClaude(t *testing.T, output string) string {
	t.Helper()
	dir := t.TempDir()
	out := filepath.Join(dir, "out.jsonl")
	if err := os.WriteFile(out, []byte(output), 0o600); err != nil {
		t.Fatal(err)
	}
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$@\" > " + filepath.Join(dir, "args") + "\n" +
		"env > " + filepath.Join(dir, "env") + "\n" +
		"cat mcp.json > " + filepath.Join(dir, "mcp.json") + " 2>/dev/null\n" +
		"cat > " + filepath.Join(dir, "stdin") + "\n" +
		"cat " + out + "\n"
	bin := filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SCOPEWEAVER_CLAUDE_BIN", bin)
	return dir
}

// The model calls the host tool through the MCP server, so the name arrives
// prefixed; --max-turns 1 then ends the run with error_max_turns by design. A
// later assistant turn only reacts to the MCP placeholder and must be dropped.
const fakeToolResult = `{"type":"system","subtype":"init","tools":["mcp__scopeweaver__bash"]}
{"type":"assistant","message":{"content":[{"type":"text","text":"Checking the host."},{"type":"tool_use","id":"toolu_01REAL","name":"mcp__scopeweaver__bash","input":{"command":"id"}}]}}
{"type":"user","message":{"content":[{"type":"tool_result","content":"Handed to the host."}]}}
{"type":"assistant","message":{"content":[{"type":"text","text":"I handed it to the host."}]}}
{"type":"result","subtype":"error_max_turns","is_error":true,"result":"","usage":{"input_tokens":3,"output_tokens":40,"cache_read_input_tokens":900,"cache_creation_input_tokens":100}}
`

func claudeCodeTestRequest() llm.CompletionRequest {
	return llm.CompletionRequest{
		System: []string{"You are the worker."},
		Tools:  []llm.ToolSchema{{Name: "bash", Description: "Run a command", InputSchema: map[string]any{"type": "object"}}},
		Messages: []llm.Message{
			llm.UserText("Map the target."),
		},
	}
}

func TestClaudeCodeCompleteReturnsToolCalls(t *testing.T) {
	dir := fakeClaude(t, fakeToolResult)
	t.Setenv("ANTHROPIC_API_KEY", "sk-should-not-reach-cli")
	t.Setenv("ANTHROPIC_BASE_URL", "https://gateway.invalid")
	prov, err := ConfigFrom("claude-code", "claude-opus-5-5", "", "", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	msg, stop, usage, err := prov.Complete(context.Background(), claudeCodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if stop != "tool_use" {
		t.Fatalf("stop=%q", stop)
	}
	if msg.Text() != "Checking the host." {
		t.Fatalf("text=%q (a turn after the tool call must be dropped)", msg.Text())
	}
	uses := msg.ToolUses()
	if len(uses) != 1 || uses[0].Name != "bash" || string(uses[0].Input) != `{"command":"id"}` || uses[0].ID != "toolu_01REAL" {
		t.Fatalf("tool uses=%+v", uses)
	}
	if usage.InputTokens != 1003 || usage.CacheReadTokens != 900 || usage.CacheWriteTokens != 100 || usage.OutputTokens != 40 {
		t.Fatalf("usage=%+v", usage)
	}

	env, _ := os.ReadFile(filepath.Join(dir, "env"))
	for _, name := range []string{"ANTHROPIC_API_KEY=", "ANTHROPIC_BASE_URL="} {
		if bytes.Contains(env, []byte("\n"+name)) || bytes.HasPrefix(env, []byte(name)) {
			t.Fatalf("%s reached the CLI", name)
		}
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	hasArg := func(want string) bool {
		return bytes.Contains(args, []byte("\n"+want+"\n")) || bytes.HasPrefix(args, []byte(want+"\n"))
	}
	for _, want := range []string{"-p", "--tools", "--strict-mcp-config", "--setting-sources", "--disable-slash-commands", "--no-session-persistence", "--mcp-config", "--allowedTools", "mcp__scopeweaver", "--max-turns", "1", "claude-opus-5-5"} {
		if !hasArg(want) {
			t.Fatalf("missing arg %q in %s", want, args)
		}
	}
	for _, unwanted := range []string{"--json-schema", "--bare"} {
		if bytes.Contains(args, []byte(unwanted)) {
			t.Fatalf("%s must not be passed", unwanted)
		}
	}
	// The MCP config must point the CLI at a loopback server.
	cfg, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err != nil {
		t.Fatalf("no MCP config handed to the CLI: %v", err)
	}
	var parsed struct {
		MCPServers map[string]struct {
			Type string `json:"type"`
			URL  string `json:"url"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(cfg, &parsed); err != nil {
		t.Fatal(err)
	}
	sw, ok := parsed.MCPServers["scopeweaver"]
	if !ok || sw.Type != "http" || !strings.HasPrefix(sw.URL, "http://127.0.0.1:") {
		t.Fatalf("mcp config=%s", cfg)
	}
	stdin, _ := os.ReadFile(filepath.Join(dir, "stdin"))
	if !bytes.Contains(stdin, []byte("Map the target.")) {
		t.Fatalf("stdin=%s", stdin)
	}
	// Tool schemas belong to the MCP server, not the prompt.
	sys, _ := os.ReadFile(filepath.Join(dir, "..", "system.md"))
	if bytes.Contains(sys, []byte("input_schema")) {
		t.Fatal("tool schemas must not be dumped into the system prompt")
	}
}

func TestClaudeCodeStreamMatchesComplete(t *testing.T) {
	fakeClaude(t, fakeToolResult)
	prov, err := ConfigFrom("claude-code", "", "", "", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	acc := llm.NewAccumulator()
	for ev, err := range prov.Stream(context.Background(), claudeCodeTestRequest()) {
		if err != nil {
			t.Fatal(err)
		}
		acc.Add(ev)
	}
	msg := acc.Message()
	if acc.StopReason != "tool_use" || msg.Text() != "Checking the host." || len(msg.ToolUses()) != 1 || string(msg.ToolUses()[0].Input) != `{"command":"id"}` {
		t.Fatalf("stop=%q msg=%+v", acc.StopReason, msg)
	}
	if acc.Usage.InputTokens != 1003 {
		t.Fatalf("usage counted twice or lost: %+v", acc.Usage)
	}
}

func TestClaudeCodeTextOnlyTurn(t *testing.T) {
	fakeClaude(t, `{"type":"assistant","message":{"content":[{"type":"text","text":"No further action is needed."}]}}
{"type":"result","subtype":"success","is_error":false,"result":"No further action is needed.","usage":{"input_tokens":10,"output_tokens":5}}
`)
	prov, err := ConfigFrom("claude-code", "", "", "", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	msg, stop, _, err := prov.Complete(context.Background(), claudeCodeTestRequest())
	if err != nil {
		t.Fatal(err)
	}
	if stop != "end_turn" || msg.Text() != "No further action is needed." || len(msg.ToolUses()) != 0 {
		t.Fatalf("stop=%q msg=%+v", stop, msg)
	}
}

func TestClaudeCodeErrors(t *testing.T) {
	for _, tc := range []struct{ name, stream, want string }{
		{
			name:   "not logged in",
			stream: `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"Claude usage limit reached"}`,
			want:   "usage limit",
		},
		{
			// An empty turn that hit the cap is a real failure, unlike one that called a tool.
			name:   "max turns with no content",
			stream: `{"type":"result","subtype":"error_max_turns","is_error":true,"result":""}`,
			want:   "error_max_turns",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeClaude(t, tc.stream+"\n")
			prov, err := ConfigFrom("claude-code", "", "", "", "").NewProvider()
			if err != nil {
				t.Fatal(err)
			}
			_, _, _, err = prov.Complete(context.Background(), claudeCodeTestRequest())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err=%v, want %q", err, tc.want)
			}
		})
	}
}

func TestClaudeCodeInputIsAppendOnly(t *testing.T) {
	msgs := []llm.Message{
		llm.UserText("Map the target."),
		{Role: llm.RoleAssistant, Content: []llm.ContentBlock{
			{Type: llm.BlockThinking, Thinking: "hidden", Signature: "sig"},
			llm.TextBlock("Running id."),
			{Type: llm.BlockToolUse, ID: "t1", Name: "bash", Input: json.RawMessage(`{"command":"id"}`)},
		}},
		{Role: llm.RoleUser, Content: []llm.ContentBlock{llm.ToolResultText("t1", "uid=0(root)", false)}},
	}
	blocks := func(n int) []string {
		raw, err := claudeCodeInput(msgs[:n])
		if err != nil {
			t.Fatal(err)
		}
		var line struct {
			Message struct {
				Content []struct{ Text string } `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(raw, &line); err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(line.Message.Content))
		for _, c := range line.Message.Content {
			out = append(out, c.Text)
		}
		return out
	}
	short, full := blocks(2), blocks(3)
	if len(full) != 3 || len(short) != 2 || short[0] != full[0] || short[1] != full[1] {
		t.Fatalf("history changed between turns:\n%q\n%q", short, full)
	}
	if strings.Contains(full[1], "hidden") {
		t.Fatal("thinking replayed")
	}
	if !strings.Contains(full[1], `<tool_call id="t1" name="bash">`) || !strings.Contains(full[2], `<tool_result id="t1">`) || !strings.Contains(full[2], "uid=0(root)") {
		t.Fatalf("render=%q", full)
	}
}

func TestClaudeCodeConfigNeedsNoKey(t *testing.T) {
	c := ConfigFrom("claude-code", "", "https://api.anthropic.com", "sk-ignored", "http://proxy.invalid")
	if c.Provider() != "claude-code" || !c.Ready() || c.APIKey != "" || c.BaseURL != "" || c.Proxy != "" || c.Model != claudeCodeDefaultModel {
		t.Fatalf("config=%+v", c)
	}
	c.LocalLLMOnly = true
	fakeClaude(t, fakeToolResult)
	if _, err := c.NewProvider(); err == nil {
		t.Fatal("local-only policy must block the cloud CLI")
	}
}

func TestClaudeCodeFromEnvIgnoresAPIKeys(t *testing.T) {
	t.Setenv("SCOPEWEAVER_CONFIG", filepath.Join(t.TempDir(), "missing.json"))
	t.Setenv("SCOPEWEAVER_LLM_PROVIDER", "claude-code")
	t.Setenv("OPENAI_API_KEY", "sk-openai")
	t.Setenv("ANTHROPIC_API_KEY", "sk-anthropic")
	c, ok := FromEnv()
	if !ok || c.Provider() != "claude-code" || c.APIKey != "" {
		t.Fatalf("ok=%v config=%+v", ok, c)
	}
}

// The MCP server must answer the handshake the CLI performs, list the host tools
// under their real names, and execute nothing.
func TestClaudeCodeToolServerProtocol(t *testing.T) {
	srv, err := newClaudeCodeToolServer([]llm.ToolSchema{
		{Name: "add_intent", Description: "Dispatch an intent", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"summary": map[string]any{"type": "string"}}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.close()

	call := func(body string) map[string]any {
		t.Helper()
		resp, err := http.Post(srv.url, "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode == http.StatusAccepted {
			return nil
		}
		var out map[string]any
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	init := call(`{"jsonrpc":"2.0","id":0,"method":"initialize","params":{"protocolVersion":"2025-11-25"}}`)
	result, _ := init["result"].(map[string]any)
	if result["protocolVersion"] != "2025-11-25" {
		t.Fatalf("initialize=%v (must echo the client's protocol version)", init)
	}
	if _, ok := result["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatalf("initialize must advertise tools: %v", init)
	}
	// The CLI sends notifications/initialized with an explicit null id; a
	// notification takes no response.
	if got := call(`{"jsonrpc":"2.0","id":null,"method":"notifications/initialized"}`); got != nil {
		t.Fatalf("notification answered: %v", got)
	}

	list := call(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`)
	tools, _ := list["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 1 {
		t.Fatalf("tools/list=%v", list)
	}
	first, _ := tools[0].(map[string]any)
	if first["name"] != "add_intent" {
		t.Fatalf("tool name must be the host's: %v", first)
	}
	if _, ok := first["inputSchema"].(map[string]any)["properties"]; !ok {
		t.Fatalf("inputSchema not forwarded: %v", first)
	}

	callRes := call(`{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"add_intent","arguments":{"summary":"x"}}}`)
	content, _ := callRes["result"].(map[string]any)["content"].([]any)
	if len(content) == 0 {
		t.Fatalf("tools/call=%v", callRes)
	}

	// An unknown method, such as the CLI's discovery probe, is a clean error.
	probe := call(`{"jsonrpc":"2.0","id":3,"method":"server/discover"}`)
	if _, ok := probe["error"].(map[string]any); !ok {
		t.Fatalf("server/discover=%v", probe)
	}

	// A GET has no server-initiated stream to offer.
	resp, err := http.Get(srv.url)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET status=%d", resp.StatusCode)
	}
}
