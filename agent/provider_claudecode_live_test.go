package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Autumn-27/norma/agentcore"
	acperm "github.com/Autumn-27/norma/permission"
	actool "github.com/Autumn-27/norma/tool"
)

// TestClaudeCodeLiveToolLoop drives the real agent harness through the signed-in
// Claude Code CLI: the model must call the Read tool and use its result. It uses
// subscription quota, so it runs only with SCOPEWEAVER_CLAUDE_CODE_LIVE=1.
func TestClaudeCodeLiveToolLoop(t *testing.T) {
	if os.Getenv("SCOPEWEAVER_CLAUDE_CODE_LIVE") != "1" {
		t.Skip("set SCOPEWEAVER_CLAUDE_CODE_LIVE=1 to run against the signed-in claude CLI")
	}
	secret := filepath.Join(t.TempDir(), "secret.txt")
	if err := os.WriteFile(secret, []byte("The codeword is PERIWINKLE-42.\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	prov, err := ConfigFrom("claude-code", os.Getenv("SCOPEWEAVER_CLAUDE_CODE_LIVE_MODEL"), "", "", "").NewProvider()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	reply, err := agentcore.Run(ctx, agentcore.Options{
		Provider:       prov,
		SystemPrompt:   []string{"You are a careful assistant. Use tools to read files; never guess file contents."},
		Tools:          []actool.CoreTool{actool.NewRead()},
		PermissionMode: acperm.ModeBypass,
		MaxTurns:       4,
		NonStreaming:   true,
	}, "Read the file "+secret+" and answer with only the codeword it contains.")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(reply, "PERIWINKLE-42") {
		t.Fatalf("reply=%q", reply)
	}
	t.Logf("reply=%q", reply)
}
