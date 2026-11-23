package harness

import (
	"context"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// On hitting MaxTurns with a Settlement configured, the loop injects the wrap-up
// prompt, keeps tool schemas identical during wrap-up to preserve prompt cache,
// blocks DisabledTools at execution time, and finishes with the ORIGINAL budget reason.
func TestSettlementOnBudgetHit(t *testing.T) {
	var toolsSeen [][]string
	var effortsSeen []string
	turns := [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`), // turn 1 → hits MaxTurns=1
		textTurn("已写回事实并总结"),                          // settlement turn → natural stop
	}
	call := func(_ context.Context, req llm.CompletionRequest) iter.Seq2[llm.StreamEvent, error] {
		names := make([]string, 0, len(req.Tools))
		for _, ts := range req.Tools {
			names = append(names, ts.Name)
		}
		i := len(toolsSeen)
		toolsSeen = append(toolsSeen, names)
		effortsSeen = append(effortsSeen, req.ReasoningEffort)
		evs := turns[i]
		return func(yield func(llm.StreamEvent, error) bool) {
			for _, ev := range evs {
				if !yield(ev, nil) {
					return
				}
			}
		}
	}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW 请写回事实", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: call})

	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns（收尾后仍报原预算 reason）", term.Reason)
	}
	found := false
	for _, m := range term.Messages {
		for _, c := range m.Content {
			if strings.Contains(c.Text, "SETTLE_NOW") {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("收尾提示未被注入到对话")
	}
	if len(toolsSeen) < 2 {
		t.Fatalf("应至少 2 次模型调用(主+收尾)，得 %d", len(toolsSeen))
	}
	if !contains(toolsSeen[0], "Bash") {
		t.Fatalf("主阶段应能看到 Bash: %v", toolsSeen[0])
	}
	if !slices.Equal(toolsSeen[0], toolsSeen[1]) {
		t.Fatalf("收尾阶段应保持 tool schemas 不变以命中 prompt cache: main=%v settle=%v", toolsSeen[0], toolsSeen[1])
	}
	if effortsSeen[0] != "" || effortsSeen[1] != "low" {
		t.Fatalf("expected ReasoningEffort ['', 'low'], got %v", effortsSeen)
	}
}

func TestSettlementBlocksDisabledToolExecution(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo main"}`),
		toolTurn("b", "Bash", `{"command":"echo should-not-run"}`),
		textTurn("wrapped up"),
	}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
		Settlement:     &Settlement{Prompt: "SETTLE_NOW", DisabledTools: []string{"Bash"}},
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v, want ReasonMaxTurns", term.Reason)
	}
	var blocked bool
	for _, msg := range term.Messages {
		for _, c := range msg.Content {
			if c.Type == llm.BlockToolResult && c.ToolUseID == "b" {
				if !c.IsError || len(c.Content) == 0 || !strings.Contains(c.Content[0].Text, "disabled during wrap-up") {
					t.Fatalf("expected wrap-up block error for call b, got %+v", c)
				}
				if strings.Contains(c.Content[0].Text, "should-not-run") {
					t.Fatalf("disabled tool should not have executed: %+v", c)
				}
				blocked = true
			}
		}
	}
	if !blocked {
		t.Fatal("expected blocked tool_result for settlement call b")
	}
}

// With no Settlement, hitting the budget finishes immediately (unchanged behavior).
func TestNoSettlementFinishesImmediately(t *testing.T) {
	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("a", "Bash", `{"command":"echo 1"}`),
		toolTurn("b", "Bash", `{"command":"echo 2"}`),
	}}
	in := QueryInput{
		Tools:          tool.NewRegistry(tool.NewBash()),
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		MaxTurns:       1,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonMaxTurns {
		t.Fatalf("reason=%v", term.Reason)
	}
	if m.calls != 1 {
		t.Fatalf("无收尾应只调用 1 次模型，得 %d", m.calls)
	}
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
