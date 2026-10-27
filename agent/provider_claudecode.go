package agent

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"iter"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/Autumn-27/artex/locale"
	"github.com/Autumn-27/norma/llm"
)

// The claude-code provider runs every model turn through the official Claude Code
// CLI in print mode (`claude -p`), signed in with the operator's own Claude
// subscription. ScopeWeaver keeps its own agent loop: the CLI's built-in tools,
// settings, hooks and skills are disabled, and the host's tools are offered to it
// as a real MCP server that executes nothing (see claudecode_mcp.go). The model's
// tool_use blocks are read back from the CLI's output stream and executed by
// ScopeWeaver, so the guard, intercept rules, traffic recording and finding
// workflow apply exactly as with an API provider.
//
// Boundaries: this provider never reads, copies or forwards Claude credentials,
// does not expose an API endpoint, and strips API-key variables from the child
// environment so usage is billed to the signed-in subscription rather than an API
// key. See docs/llm-providers.md#claude-code-claude-subscription.
const (
	claudeCodeFormat       llm.Format = "claude-code"
	claudeCodeDefaultModel            = "claude-sonnet-5-5"
	// Subscription limits assume ordinary individual use; one CLI process at a
	// time is the default. SCOPEWEAVER_CLAUDE_CODE_MAX_PROCS raises it.
	claudeCodeDefaultProcs = 1
	claudeCodeMaxOutput    = 8 << 20
)

// ClaudeCodeModels are the model ids offered in the profile form. The CLI also
// accepts its aliases (opus, sonnet, haiku).
var ClaudeCodeModels = []string{"claude-sonnet-5-5", "claude-opus-5-5", "claude-haiku-5-5"}

// Environment that would make the CLI bill an API key, a cloud provider or a
// different endpoint instead of the signed-in subscription.
var claudeCodeStrippedEnv = []string{
	"ANTHROPIC_API_KEY",
	"ANTHROPIC_AUTH_TOKEN",
	"ANTHROPIC_BASE_URL",
	"ANTHROPIC_MODEL",
	"ANTHROPIC_CUSTOM_HEADERS",
	"CLAUDE_CODE_USE_BEDROCK",
	"CLAUDE_CODE_USE_VERTEX",
	"CLAUDE_CODE_USE_FOUNDRY",
	"CLAUDE_CODE_OAUTH_TOKEN",
}

// claudeCodeProcs bounds concurrent CLI processes across every provider instance.
var claudeCodeProcs = make(chan struct{}, claudeCodeMaxProcs())

func claudeCodeMaxProcs() int {
	if n, err := strconv.Atoi(strings.TrimSpace(os.Getenv("SCOPEWEAVER_CLAUDE_CODE_MAX_PROCS"))); err == nil && n > 0 {
		return n
	}
	return claudeCodeDefaultProcs
}

// claudeCodeBinary resolves the CLI: SCOPEWEAVER_CLAUDE_BIN, then PATH.
func claudeCodeBinary() (string, error) {
	if p := strings.TrimSpace(os.Getenv("SCOPEWEAVER_CLAUDE_BIN")); p != "" {
		return p, nil
	}
	p, err := exec.LookPath("claude")
	if err != nil {
		return "", locale.Errorf("claude-code: the claude CLI was not found; install Claude Code and sign in with your subscription (claude, then /login)")
	}
	return p, nil
}

type claudeCodeProvider struct {
	model  string
	effort string
}

func newClaudeCodeProvider(c Config) (llm.Provider, error) {
	if c.LocalLLMOnly {
		return nil, locale.Errorf("claude-code: unavailable while the local-only LLM network setting is on")
	}
	if _, err := claudeCodeBinary(); err != nil {
		return nil, err
	}
	model := c.Model
	if model == "" {
		model = claudeCodeDefaultModel
	}
	return claudeCodeProvider{model: model, effort: c.ReasoningEffort}, nil
}

// Stream runs one non-streaming CLI turn and replays it as normalized events, so
// callers that always stream (the agent harness) work unchanged.
func (p claudeCodeProvider) Stream(ctx context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		msg, stop, usage, err := p.Complete(ctx, req)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		events := []llm.StreamEvent{{Type: llm.SEMessageStart}}
		for _, b := range msg.Content {
			switch b.Type {
			case llm.BlockText:
				events = append(events, llm.StreamEvent{Type: llm.SETextDelta, Text: b.Text})
			case llm.BlockToolUse:
				events = append(events,
					llm.StreamEvent{Type: llm.SEToolUseStart, ToolID: b.ID, ToolName: b.Name},
					llm.StreamEvent{Type: llm.SEToolInputJSON, Text: string(b.Input)})
			}
		}
		events = append(events,
			llm.StreamEvent{Type: llm.SEMessageDelta, StopReason: stop, Usage: usage},
			llm.StreamEvent{Type: llm.SEMessageStop})
		for _, ev := range events {
			if !yield(ev, nil) {
				return
			}
		}
	}
}

func (p claudeCodeProvider) Complete(ctx context.Context, req llm.CompletionRequest) (llm.Message, string, llm.Usage, error) {
	bin, err := claudeCodeBinary()
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	select {
	case claudeCodeProcs <- struct{}{}:
		defer func() { <-claudeCodeProcs }()
	case <-ctx.Done():
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}

	// An empty working directory keeps project CLAUDE.md files and settings out
	// of the prompt; the system prompt and MCP config files live there too.
	dir, err := os.MkdirTemp("", "scopeweaver-claude-")
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	defer os.RemoveAll(dir)
	systemFile := filepath.Join(dir, "system.md")
	if err := os.WriteFile(systemFile, []byte(claudeCodeSystemPrompt(req)), 0o600); err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}
	stdin, err := claudeCodeInput(req.Messages)
	if err != nil {
		return llm.Message{}, "", llm.Usage{}, err
	}

	args := []string{
		"-p",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
		"--verbose",
		"--model", p.model,
		"--system-prompt-file", systemFile,
		"--tools", "",
		"--setting-sources", "",
		"--disable-slash-commands",
		"--no-session-persistence",
		"--strict-mcp-config",
	}
	if len(req.Tools) > 0 {
		// Host tools are served over MCP so the model calls them natively. One
		// assistant turn is all the host needs: --max-turns 1 keeps the CLI from
		// spending another turn on the placeholder tool results it would receive.
		srv, err := newClaudeCodeToolServer(req.Tools)
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		defer srv.close()
		cfg, err := srv.config()
		if err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		cfgFile := filepath.Join(dir, "mcp.json")
		if err := os.WriteFile(cfgFile, cfg, 0o600); err != nil {
			return llm.Message{}, "", llm.Usage{}, err
		}
		args = append(args,
			"--mcp-config", cfgFile,
			"--allowedTools", "mcp__"+claudeCodeMCPServer,
			"--max-turns", "1",
		)
	}
	if p.effort != "" {
		args = append(args, "--effort", p.effort)
	}
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	cmd.Env = claudeCodeEnv(os.Environ())
	cmd.Stdin = bytes.NewReader(stdin)
	cmd.WaitDelay = 5 * time.Second
	var stdout, stderr limitedBuffer
	stdout.limit, stderr.limit = claudeCodeMaxOutput, 64<<10
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if ctx.Err() != nil {
		return llm.Message{}, "", llm.Usage{}, ctx.Err()
	}
	run, parseErr := parseClaudeCodeStream(stdout.Bytes())
	if parseErr != nil {
		if runErr != nil {
			return llm.Message{}, "", llm.Usage{}, locale.Errorf("claude-code: CLI failed: %s", clipBody(strings.TrimSpace(runErr.Error()+" "+stderr.String())))
		}
		return llm.Message{}, "", llm.Usage{}, parseErr
	}
	return run.message()
}

// claudeCodeEnv drops variables that would redirect billing away from the
// signed-in subscription.
func claudeCodeEnv(env []string) []string {
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		drop := false
		for _, s := range claudeCodeStrippedEnv {
			if name == s {
				drop = true
				break
			}
		}
		if !drop {
			out = append(out, kv)
		}
	}
	return out
}

// claudeCodeSystemPrompt is the host system prompt plus a short note on how the
// conversation is presented. Tool schemas are NOT described here: the CLI gets
// them over MCP, so the model sees and calls real tools. The text is identical
// for every turn of a session, so the CLI's prompt cache reuses it.
func claudeCodeSystemPrompt(req llm.CompletionRequest) string {
	var b strings.Builder
	b.WriteString(strings.Join(req.System, "\n\n"))
	b.WriteString("\n\n# Conversation format\n\n")
	b.WriteString("You are the model behind a host application that runs its own agent loop. The user message is the conversation so far, one block per turn: <user>, <assistant>, <tool_call> and <tool_result> elements in order. Write only the assistant's next turn.\n")
	if len(req.Tools) > 0 {
		b.WriteString("\nYour tools are provided by the host. Call them as you normally would; the host executes each call and returns the result in the next turn, so do not wait for it or repeat the call. Make all the calls this turn needs in this one reply.\n")
	}
	return b.String()
}

// claudeCodeInput renders the conversation as one stream-json user message with
// one text block per turn. Earlier turns render byte-identically every time, so
// the CLI's prompt cache serves the history and only new turns are processed.
func claudeCodeInput(msgs []llm.Message) ([]byte, error) {
	blocks := make([]map[string]string, 0, len(msgs))
	for _, m := range msgs {
		if s := renderClaudeCodeTurn(m); s != "" {
			blocks = append(blocks, map[string]string{"type": "text", "text": s})
		}
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]string{"type": "text", "text": "<user>\n(empty)\n</user>"})
	}
	line, err := json.Marshal(map[string]any{
		"type":    "user",
		"message": map[string]any{"role": "user", "content": blocks},
	})
	if err != nil {
		return nil, err
	}
	return append(line, '\n'), nil
}

func renderClaudeCodeTurn(m llm.Message) string {
	var b strings.Builder
	role := string(m.Role)
	var text strings.Builder
	flush := func() {
		if s := strings.TrimSpace(text.String()); s != "" {
			fmt.Fprintf(&b, "<%s>\n%s\n</%s>\n", role, s, role)
		}
		text.Reset()
	}
	for _, c := range m.Content {
		switch c.Type {
		case llm.BlockText:
			text.WriteString(c.Text)
			text.WriteString("\n")
		case llm.BlockToolUse:
			flush()
			input := strings.TrimSpace(string(c.Input))
			if input == "" {
				input = "{}"
			}
			fmt.Fprintf(&b, "<tool_call id=%q name=%q>\n%s\n</tool_call>\n", c.ID, c.Name, input)
		case llm.BlockToolResult:
			flush()
			attr := ""
			if c.IsError {
				attr = ` error="true"`
			}
			fmt.Fprintf(&b, "<tool_result id=%q%s>\n%s\n</tool_result>\n", c.ToolUseID, attr, toolResultText(c.Content))
		}
		// Thinking blocks are not replayed: their signatures belong to the API
		// session that produced them and the CLI starts a fresh session each turn.
	}
	flush()
	return strings.TrimSpace(b.String())
}

func toolResultText(blocks []llm.ContentBlock) string {
	var parts []string
	for _, c := range blocks {
		if c.Type == llm.BlockText {
			parts = append(parts, c.Text)
		} else {
			parts = append(parts, "["+string(c.Type)+" content omitted]")
		}
	}
	return strings.Join(parts, "\n")
}

// claudeCodeRun is what one CLI invocation produced: the assistant blocks of the
// turn the host asked for, plus the run's final result event.
type claudeCodeRun struct {
	blocks []llm.ContentBlock
	result claudeCodeResult
}

type claudeCodeResult struct {
	Subtype        string `json:"subtype"`
	IsError        bool   `json:"is_error"`
	Result         string `json:"result"`
	APIErrorStatus any    `json:"api_error_status"`
	Usage          struct {
		InputTokens              int `json:"input_tokens"`
		OutputTokens             int `json:"output_tokens"`
		CacheReadInputTokens     int `json:"cache_read_input_tokens"`
		CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	} `json:"usage"`
}

// parseClaudeCodeStream reads stream-json output: the assistant messages up to
// and including the first one that calls tools (later turns only react to the
// MCP placeholder results and are not part of the host's turn), and the final
// result event.
func parseClaudeCodeStream(out []byte) (claudeCodeRun, error) {
	var run claudeCodeRun
	gotResult, done := false, false
	for _, line := range bytes.Split(out, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				Content []struct {
					Type  string          `json:"type"`
					Text  string          `json:"text"`
					ID    string          `json:"id"`
					Name  string          `json:"name"`
					Input json.RawMessage `json:"input"`
				} `json:"content"`
			} `json:"message"`
		}
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		switch ev.Type {
		case "assistant":
			if done {
				continue
			}
			for _, c := range ev.Message.Content {
				switch c.Type {
				case "text":
					if s := strings.TrimSpace(c.Text); s != "" {
						run.blocks = append(run.blocks, llm.TextBlock(s))
					}
				case "tool_use":
					input := bytes.TrimSpace(c.Input)
					if len(input) == 0 || input[0] != '{' || !json.Valid(input) {
						input = []byte("{}")
					}
					id := c.ID
					if id == "" {
						id = newClaudeCodeToolID()
					}
					run.blocks = append(run.blocks, llm.ContentBlock{
						Type:  llm.BlockToolUse,
						ID:    id,
						Name:  strings.TrimPrefix(c.Name, claudeCodeMCPPrefix),
						Input: json.RawMessage(input),
					})
					done = true
				}
			}
		case "result":
			run.result = claudeCodeResult{}
			if err := json.Unmarshal(line, &run.result); err != nil {
				return run, err
			}
			gotResult = true
		}
	}
	if !gotResult {
		return run, locale.Errorf("claude-code: the CLI returned no result")
	}
	return run, nil
}

func (r claudeCodeRun) message() (llm.Message, string, llm.Usage, error) {
	u := r.result.Usage
	usage := llm.Usage{
		InputTokens:      u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens,
		OutputTokens:     u.OutputTokens,
		CacheReadTokens:  u.CacheReadInputTokens,
		CacheWriteTokens: u.CacheCreationInputTokens,
	}
	msg := llm.Message{Role: llm.RoleAssistant, Content: r.blocks}
	// --max-turns 1 ends a tool-calling run with error_max_turns by design: the
	// turn the host wanted is complete, so only an empty one is a real failure.
	failed := r.result.IsError || (r.result.Subtype != "" && r.result.Subtype != "success")
	if failed && r.result.Subtype == "error_max_turns" && len(r.blocks) > 0 {
		failed = false
	}
	if failed {
		detail := strings.TrimSpace(r.result.Result)
		if detail == "" {
			detail = r.result.Subtype
		}
		if r.result.APIErrorStatus != nil {
			detail = fmt.Sprintf("%v %s", r.result.APIErrorStatus, detail)
		}
		return llm.Message{}, "", usage, locale.Errorf("claude-code: %s", clipBody(detail))
	}
	if len(msg.Content) == 0 {
		// A successful run with no assistant block: keep the plain reply as text.
		if s := strings.TrimSpace(r.result.Result); s != "" {
			msg.Content = append(msg.Content, llm.TextBlock(s))
		}
	}
	if len(msg.ToolUses()) > 0 {
		return msg, "tool_use", usage, nil
	}
	return msg, "end_turn", usage, nil
}

func newClaudeCodeToolID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return "toolu_cc_" + hex.EncodeToString(b[:])
}

// limitedBuffer keeps at most limit bytes and discards the rest.
type limitedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room > 0 {
		if len(p) > room {
			b.Buffer.Write(p[:room])
		} else {
			b.Buffer.Write(p)
		}
	}
	return len(p), nil
}
