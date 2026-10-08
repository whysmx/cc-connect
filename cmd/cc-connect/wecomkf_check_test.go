package main

import (
	"strings"
	"testing"

	"github.com/chenhg5/cc-connect/config"
)

func kfProject(agentType string, agentOpts map[string]any, adminFrom string, platforms ...string) config.ProjectConfig {
	if len(platforms) == 0 {
		platforms = []string{"wecom_kf"}
	}
	proj := config.ProjectConfig{Name: "kf", AdminFrom: adminFrom, Agent: config.AgentConfig{Type: agentType, Options: agentOpts}}
	for _, p := range platforms {
		proj.Platforms = append(proj.Platforms, config.PlatformConfig{Type: p})
	}
	return proj
}

func TestWeComKFDeploymentWarnings(t *testing.T) {
	cases := []struct {
		name string
		proj config.ProjectConfig
		want []string // substrings, one per expected warning
	}{
		{"recommended setup", kfProject("codex", map[string]any{"backend": "exec", "mode": "suggest"}, ""), nil},
		{"codex defaults are read-only", kfProject("codex", nil, "admin1"), nil},
		{"other platforms are not checked", kfProject("codex", map[string]any{"mode": "yolo"}, "*", "feishu", "wecom"), nil},
		{"writable mode", kfProject("codex", map[string]any{"mode": "Full-Auto"}, ""), []string{`mode "full-auto"`}},
		{"yolo and app server", kfProject("codex", map[string]any{"mode": "yolo", "backend": "app_server"}, ""), []string{`mode "yolo"`, `backend "app_server"`}},
		{"non-codex agent", kfProject("claudecode", nil, ""), []string{`agent type "claudecode"`}},
		{"everyone is admin", kfProject("codex", nil, " * ", "telegram", "WeCom_KF"), []string{`admin_from = "*"`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := wecomKFDeploymentWarnings(tc.proj)
			if len(got) != len(tc.want) {
				t.Fatalf("warnings = %q, want %d matching %q", got, len(tc.want), tc.want)
			}
			for i, sub := range tc.want {
				if !strings.Contains(got[i], sub) {
					t.Errorf("warning %d = %q, want it to mention %q", i, got[i], sub)
				}
			}
		})
	}
}
