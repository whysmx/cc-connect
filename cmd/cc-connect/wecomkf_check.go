package main

import (
	"strings"

	"github.com/chenhg5/cc-connect/config"
)

// wecomKFDeploymentWarnings returns startup warnings for a project that
// exposes its agent to WeChat customers through the wecom_kf platform. The
// customer-service deployment is meant to be read-only Q&A (see
// docs/wecom-kf-codex-development-plan.md §8), which must be enforced by the
// agent's sandbox rather than by prompts alone.
func wecomKFDeploymentWarnings(proj config.ProjectConfig) []string {
	if !projectUsesPlatform(proj, "wecom_kf") {
		return nil
	}
	var warnings []string
	opts := proj.Agent.Options
	str := func(key string) string {
		v, _ := opts[key].(string)
		return strings.ToLower(strings.TrimSpace(v))
	}

	if strings.EqualFold(proj.Agent.Type, "codex") {
		switch str("mode") {
		case "auto-edit", "autoedit", "auto_edit", "edit", "full-auto", "fullauto", "full_auto", "auto",
			"yolo", "bypass", "dangerously-bypass":
			warnings = append(warnings, `agent mode "`+str("mode")+`" lets Codex write files or run commands for WeChat customers; use mode = "suggest" (read-only sandbox)`)
		}
		switch str("backend") {
		case "app-server", "app_server", "appserver", "ws":
			warnings = append(warnings, `backend "app_server" sends tool-permission prompts to the WeChat customer; use backend = "exec"`)
		}
	} else {
		warnings = append(warnings, `agent type "`+proj.Agent.Type+`" is not codex; make sure it runs in a read-only mode, customers must not be able to modify the project`)
	}

	if strings.TrimSpace(proj.AdminFrom) == "*" {
		warnings = append(warnings, `admin_from = "*" lets every WeChat customer run privileged commands (switch sessions, change mode or work_dir); list admin external_userids instead`)
	}
	return warnings
}

func projectUsesPlatform(proj config.ProjectConfig, platformType string) bool {
	for _, p := range proj.Platforms {
		if strings.EqualFold(p.Type, platformType) {
			return true
		}
	}
	return false
}
