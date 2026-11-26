package intercept

import "testing"

func TestNoTargetToolsExemptSkillAndReadOnlyTools(t *testing.T) {
	exempt := []string{
		"Skill",
		"SearchExtraTools",
		"AskUserQuestion",
		"shell_read",
		"shell_close",
		"shell_list",
	}
	input := []byte(`{"name":"ssrf","args":"http://169.254.169.254/latest/meta-data/","url":"https://internal.corp:8443/mcp"}`)
	for _, tool := range exempt {
		if got := ExtractTargets(tool, input); len(got) != 0 {
			t.Fatalf("expected %q to extract no targets, got %+v", tool, got)
		}
	}
	// Remote-fetching and config-provisioning tools must remain gated.
	for _, gated := range []string{"install_skill", "create_mcp", "update_mcp", "create_custom_tool", "update_custom_tool"} {
		if got := ExtractTargets(gated, input); len(got) == 0 {
			t.Fatalf("expected %q to remain target-gated", gated)
		}
	}
}
