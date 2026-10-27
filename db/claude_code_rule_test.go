package db

import (
	"regexp"
	"testing"
)

func TestClaudeCodeCredentialPattern(t *testing.T) {
	re := regexp.MustCompile(ClaudeCodeCredentialPattern)
	for _, in := range []string{
		`{"command":"cat ~/.claude/.credentials.json"}`,
		`{"file_path":"/app/claude-home/.credentials.json"}`,
		`{"command":"cat /root/.claude.json"}`,
		`{"command":"echo $CLAUDE_CONFIG_DIR"}`,
		`{"command":"tar cz /root/.claude/ | base64"}`,
		`{"command":"env | grep CLAUDE_CODE_OAUTH_TOKEN"}`,
	} {
		if !re.MatchString(in) {
			t.Errorf("not blocked: %s", in)
		}
	}
	for _, in := range []string{
		`{"command":"nmap -sV scanme.example"}`,
		`{"command":"curl -s https://target.example/claude"}`,
		`{"file_path":"/app/data/42/notes.md"}`,
	} {
		if re.MatchString(in) {
			t.Errorf("false positive: %s", in)
		}
	}
}
