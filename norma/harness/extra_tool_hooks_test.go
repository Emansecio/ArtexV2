package harness

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

type extraHookRecorder struct {
	preNames   []string
	preInputs  []string
	postNames  []string
	postInputs []string
	preFn      func(name string, input []byte) (bool, string, []byte)
}

func (r *extraHookRecorder) PreToolUse(_ context.Context, name string, input []byte) (bool, string, []byte) {
	r.preNames = append(r.preNames, name)
	r.preInputs = append(r.preInputs, string(input))
	if r.preFn != nil {
		return r.preFn(name, input)
	}
	return false, "", nil
}

func (r *extraHookRecorder) PostToolUse(_ context.Context, name string, input, _ []byte, _ bool) {
	r.postNames = append(r.postNames, name)
	r.postInputs = append(r.postInputs, string(input))
}

func (r *extraHookRecorder) Stop(context.Context, []llm.Message) (bool, []string, string) {
	return false, nil, ""
}

func newEchoDeferredTool(name string, executed *[]string) tool.CoreTool {
	return tool.Build(tool.Spec{
		Name:        name,
		Description: "echo test tool",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg": map[string]any{"type": "string"},
			},
			"required": []any{"msg"},
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Allowed()
		},
		Run: func(_ context.Context, in json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			var args struct {
				Msg string `json:"msg"`
			}
			if err := json.Unmarshal(in, &args); err != nil {
				return tool.Errorf("bad args: " + err.Error()), nil
			}
			if executed != nil {
				*executed = append(*executed, name+":"+args.Msg)
			}
			return tool.Text("echo:" + args.Msg), nil
		},
	})
}

func TestExecuteExtraToolHooksReceiveInnerToolAndRewriteParams(t *testing.T) {
	var executed []string
	reg := tool.NewRegistry(newEchoDeferredTool("record_coverage", &executed))
	reg.Add(tool.NewExecuteExtraTool(reg, nil))

	rec := &extraHookRecorder{
		preFn: func(name string, input []byte) (bool, string, []byte) {
			if name == "record_coverage" {
				return false, "", []byte(`{"msg":"rewritten"}`)
			}
			return false, "", nil
		},
	}

	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"record_coverage","params":{"msg":"orig"}}`),
		textTurn("done"),
	}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          reg,
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		Hooks:          rec,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if term.Reason != ReasonCompleted {
		t.Fatalf("reason=%v, want completed", term.Reason)
	}
	if len(rec.preNames) != 1 || rec.preNames[0] != "record_coverage" {
		t.Fatalf("PreToolUse names=%v, want [record_coverage]", rec.preNames)
	}
	if len(rec.preInputs) != 1 || rec.preInputs[0] != `{"msg":"orig"}` {
		t.Fatalf("PreToolUse inputs=%v, want [{\"msg\":\"orig\"}]", rec.preInputs)
	}
	if len(executed) != 1 || executed[0] != "record_coverage:rewritten" {
		t.Fatalf("executed=%v, want [record_coverage:rewritten]", executed)
	}
	if len(rec.postNames) != 1 || rec.postNames[0] != "record_coverage" {
		t.Fatalf("PostToolUse names=%v, want [record_coverage]", rec.postNames)
	}
	if len(rec.postInputs) != 1 || rec.postInputs[0] != `{"msg":"rewritten"}` {
		t.Fatalf("PostToolUse inputs=%v, want [{\"msg\":\"rewritten\"}]", rec.postInputs)
	}
}

func TestExecuteExtraToolPreHookBlocksInnerTool(t *testing.T) {
	var executed []string
	reg := tool.NewRegistry(newEchoDeferredTool("mcp__danger", &executed))
	reg.Add(tool.NewExecuteExtraTool(reg, nil))

	rec := &extraHookRecorder{
		preFn: func(name string, _ []byte) (bool, string, []byte) {
			if name == "mcp__danger" {
				return true, "target not approved", nil
			}
			return false, "", nil
		},
	}

	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"mcp__danger","params":{"msg":"boom"}}`),
		textTurn("done"),
	}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          reg,
		PermissionMode: permission.ModeBypass,
		WorkingDir:     t.TempDir(),
		Hooks:          rec,
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if len(executed) != 0 {
		t.Fatalf("blocked inner tool should not execute, got %v", executed)
	}
	if len(rec.postNames) != 0 {
		t.Fatalf("PostToolUse should not run when blocked, got %v", rec.postNames)
	}
	var foundBlock bool
	for _, msg := range term.Messages {
		for _, c := range msg.Content {
			if c.Type == llm.BlockToolResult && c.ToolUseID == "e1" && c.IsError &&
				len(c.Content) > 0 && strings.Contains(c.Content[0].Text, "Blocked by hook: target not approved") {
				foundBlock = true
			}
		}
	}
	if !foundBlock {
		t.Fatal("expected Blocked by hook error tool_result for e1")
	}
}

func TestExecuteExtraToolOuterUpdatedInputRevalidatesInnerTool(t *testing.T) {
	var executed []string
	safeTool := newEchoDeferredTool("safe_tool", &executed)
	deniedTool := tool.Build(tool.Spec{
		Name:        "denied_tool",
		Description: "denied inner tool",
		Schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"msg": map[string]any{"type": "string"},
			},
		},
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Denied("inner policy denied")
		},
		Run: func(_ context.Context, _ json.RawMessage, _ *tool.ToolContext) (tool.Result, error) {
			executed = append(executed, "denied_tool")
			return tool.Text("should not run"), nil
		},
	})
	reg := tool.NewRegistry(safeTool, deniedTool)
	// Wrap ExecuteExtraTool so its own CheckPermissions rewrites tool_name from safe_tool to denied_tool.
	baseExtra := tool.NewExecuteExtraTool(reg, nil)
	rewritingExtra := tool.Build(tool.Spec{
		Name:        tool.ExecuteExtraToolName,
		Description: baseExtra.Description(),
		Schema:      baseExtra.InputSchema(),
		Permissions: func(context.Context, json.RawMessage, permission.Context) permission.Decision {
			return permission.Decision{
				Behavior:     permission.Allow,
				UpdatedInput: json.RawMessage(`{"tool_name":"denied_tool","params":{"msg":"hi"}}`),
			}
		},
		Run: func(ctx context.Context, in json.RawMessage, tc *tool.ToolContext) (tool.Result, error) {
			return baseExtra.Call(ctx, in, tc)
		},
	})
	reg.Add(rewritingExtra)

	m := &scriptedModel{turns: [][]llm.StreamEvent{
		toolTurn("e1", tool.ExecuteExtraToolName, `{"tool_name":"safe_tool","params":{"msg":"hi"}}`),
		textTurn("done"),
	}}
	in := QueryInput{
		Messages:       []llm.Message{llm.UserText("go")},
		Tools:          reg,
		PermissionMode: permission.ModeDefault,
		WorkingDir:     t.TempDir(),
	}
	term := drain(t, in, QueryDeps{CallModel: m.call})
	if len(executed) != 0 {
		t.Fatalf("rewritten denied_tool should not execute, got %v", executed)
	}
	var foundDenied bool
	for _, msg := range term.Messages {
		for _, c := range msg.Content {
			if c.Type == llm.BlockToolResult && c.ToolUseID == "e1" && c.IsError &&
				len(c.Content) > 0 && strings.Contains(c.Content[0].Text, "inner policy denied") {
				foundDenied = true
			}
		}
	}
	if !foundDenied {
		t.Fatal("expected inner permission denial after outer UpdatedInput rewrite")
	}
}
