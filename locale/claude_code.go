package locale

// Claude Code CLI provider errors (agent/provider_claudecode.go) and the built-in
// intercept rule that guards its sign-in files (db/db.go). Keys are the exact
// English text passed to Errorf or stored as the rule name/message.
func init() {
	RegisterAll(map[string]map[Lang]string{
		"claude-code: the claude CLI was not found; install Claude Code and sign in with your subscription (claude, then /login)": {
			En:     "claude-code: the claude CLI was not found; install Claude Code and sign in with your subscription (claude, then /login)",
			Ko:     "claude-code: claude CLI를 찾을 수 없습니다. Claude Code를 설치하고 구독 계정으로 로그인하세요(claude 실행 후 /login)",
			ZhHans: "claude-code：未找到 claude CLI；请安装 Claude Code 并用订阅账号登录（运行 claude，然后 /login）",
		},
		"claude-code: unavailable while the local-only LLM network setting is on": {
			En:     "claude-code: unavailable while the local-only LLM network setting is on",
			Ko:     "claude-code: 로컬 전용 LLM 네트워크 설정이 켜져 있으면 사용할 수 없습니다",
			ZhHans: "claude-code：开启“仅本地 LLM 网络”设置时不可用",
		},
		"claude-code: CLI failed: %s": {
			En:     "claude-code: CLI failed: %s",
			Ko:     "claude-code: CLI 실행 실패: %s",
			ZhHans: "claude-code：CLI 执行失败：%s",
		},
		"claude-code: the CLI returned no result": {
			En:     "claude-code: the CLI returned no result",
			Ko:     "claude-code: CLI가 결과를 반환하지 않았습니다",
			ZhHans: "claude-code：CLI 未返回结果",
		},
		"seed intercept rules v4: %w": {
			En:     "seed intercept rules v4: %w",
			Ko:     "차단 규칙 v4 초기화 실패: %w",
			ZhHans: "播种拦截规则 v4：%w",
		},
		"[Built-in] Claude Code sign-in files": {
			En:     "[Built-in] Claude Code sign-in files",
			Ko:     "[기본 제공] Claude Code 로그인 파일",
			ZhHans: "[内置] Claude Code 登录文件",
		},
		"Access to the Claude Code CLI sign-in files (.credentials.json, .claude.json, the Claude config directory) is prohibited": {
			En:     "Access to the Claude Code CLI sign-in files (.credentials.json, .claude.json, the Claude config directory) is prohibited",
			Ko:     "Claude Code CLI 로그인 파일(.credentials.json, .claude.json, Claude 설정 디렉터리)에 대한 접근은 금지됩니다",
			ZhHans: "禁止访问 Claude Code CLI 登录文件（.credentials.json、.claude.json、Claude 配置目录）",
		},
		"claude-code: %s": {
			En:     "claude-code: %s",
			Ko:     "claude-code: %s",
			ZhHans: "claude-code：%s",
		},
	})
}
