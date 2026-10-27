package agent

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"time"

	"github.com/Autumn-27/norma/llm"
)

// The claude-code provider exposes ScopeWeaver's host tools to the CLI as a real
// MCP server, so the model invokes them the way it natively would. The server
// never executes anything: it answers tools/list with the host schemas and
// returns a short placeholder for tools/call. The authoritative record of what
// the model wanted is the tool_use block in the CLI's stream-json output, which
// the provider turns into ordinary llm.BlockToolUse for ScopeWeaver to execute.
//
// Before this, host tools were described in the system prompt and requested back
// as structured output. Real agents whose prompts say "call add_intent" made the
// model attempt a native call instead, which failed with "No such tool available"
// and dispatched no work.
const (
	// claudeCodeMCPServer is the MCP server name; the CLI prefixes tool names with it.
	claudeCodeMCPServer = "scopeweaver"
	claudeCodeMCPPrefix = "mcp__" + claudeCodeMCPServer + "__"
	// claudeCodeMCPProtocol is used only when the client sends no version.
	claudeCodeMCPProtocol = "2025-06-18"
)

// claudeCodeToolServer serves the host tool schemas over MCP Streamable HTTP on
// loopback. The URL carries a random path token so only the CLI invocation that
// received it can reach the endpoint.
type claudeCodeToolServer struct {
	url   string
	ln    net.Listener
	srv   *http.Server
	tools []llm.ToolSchema
}

func newClaudeCodeToolServer(tools []llm.ToolSchema) (*claudeCodeToolServer, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	var tok [16]byte
	if _, err := rand.Read(tok[:]); err != nil {
		ln.Close()
		return nil, err
	}
	path := "/mcp/" + hex.EncodeToString(tok[:])
	s := &claudeCodeToolServer{
		url:   "http://" + ln.Addr().String() + path,
		ln:    ln,
		tools: tools,
	}
	mux := http.NewServeMux()
	mux.HandleFunc(path, s.handle)
	s.srv = &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go s.srv.Serve(ln)
	return s, nil
}

func (s *claudeCodeToolServer) close() {
	if s == nil {
		return
	}
	s.srv.Close()
}

type mcpRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type mcpResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Result  any       `json:"result,omitempty"`
	Error   *mcpError `json:"error,omitempty"`
}

type mcpError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *claudeCodeToolServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		// Streamable HTTP allows a server with no server-initiated stream to refuse GET.
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	var req mcpRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	// A notification carries no id, or an explicit null id; it takes no response.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	var id any
	json.Unmarshal(req.ID, &id)
	resp := mcpResponse{JSONRPC: "2.0", ID: id}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		json.Unmarshal(req.Params, &p)
		if p.ProtocolVersion == "" {
			p.ProtocolVersion = claudeCodeMCPProtocol
		}
		resp.Result = map[string]any{
			"protocolVersion": p.ProtocolVersion,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": claudeCodeMCPServer, "version": "1"},
		}
	case "tools/list":
		list := make([]any, 0, len(s.tools))
		for _, t := range s.tools {
			schema := t.InputSchema
			if schema == nil {
				schema = map[string]any{"type": "object"}
			}
			list = append(list, map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": schema,
			})
		}
		resp.Result = map[string]any{"tools": list}
	case "tools/call":
		// The host executes the call; this placeholder only ends the CLI's turn.
		// The provider reads the model's tool_use block from the output stream.
		resp.Result = map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "Handed to the host. Stop now; the host returns the result in the next turn."}},
		}
	case "ping":
		resp.Result = map[string]any{}
	default:
		// Unknown methods, including the CLI's server/discover probe, are optional.
		resp.Error = &mcpError{Code: -32601, Message: "method not found: " + req.Method}
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(resp)
}

// claudeCodeMCPConfig is the --mcp-config document pointing at this server.
func (s *claudeCodeToolServer) config() ([]byte, error) {
	return json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			claudeCodeMCPServer: map[string]any{"type": "http", "url": s.url},
		},
	})
}
