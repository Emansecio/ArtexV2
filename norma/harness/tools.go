package harness

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Autumn-27/norma/llm"
	"github.com/Autumn-27/norma/permission"
	"github.com/Autumn-27/norma/tool"
)

// rawInputTool is the optional opt-out from schema validation. Discovered by
// assertion rather than declared on tool.CoreTool, so hosts implementing that
// interface are unaffected.
type rawInputTool interface{ AcceptsRawInput() bool }

func acceptsRawInput(t tool.CoreTool) bool {
	r, ok := t.(rawInputTool)
	return ok && r.AcceptsRawInput()
}

// execOne resolves, permission-checks, and runs a single tool call, returning a
// tool_result block and any extra messages the tool injects after its result
// (Result.Extra — e.g. the Skill tool's instructions message). It never panics;
// failures become error results. It is safe to call from the streaming
// executor's goroutines (read-only access to loop config; permission/hook
// callbacks are the host's responsibility to make safe).
func (l *loop) execOne(use llm.ContentBlock, emitProgress func(tool.ProgressInfo)) (llm.ContentBlock, []llm.Message) {
	return l.execOneWithCtx(l.toolCtx, use, l.settling, emitProgress)
}

func (l *loop) execOneWithCtx(toolCtx context.Context, use llm.ContentBlock, settling bool, emitProgress func(tool.ProgressInfo)) (llm.ContentBlock, []llm.Message) {
	if toolCtx == nil {
		toolCtx = l.ctx
	}
	if toolCtx != nil {
		if err := toolCtx.Err(); err != nil {
			return llm.ToolResultText(use.ID, "Error: "+err.Error(), true), nil
		}
	}
	t, ok := l.in.Tools.Get(use.Name)
	if !ok {
		return llm.ToolResultText(use.ID, fmt.Sprintf("Error: unknown tool %q", use.Name), true), nil
	}
	if l.isDisabledDuringSettlement(settling, use.Name) {
		return llm.ToolResultText(use.ID, fmt.Sprintf("Error: tool %q is disabled during wrap-up; persist findings and summarize without calling it.", use.Name), true), nil
	}
	input := use.Input

	// Schema validation (FR-04.5).
	//
	// A tool may opt out by reporting AcceptsRawInput: it parses its own
	// arguments and would rather repair a malformed call than have it rejected
	// here, where the only possible answer is a generic error. The schema is
	// still advertised to the model either way — see tool.Spec.RawInput.
	if !acceptsRawInput(t) {
		if err := tool.ValidateInput(t.InputSchema(), input); err != nil {
			return llm.ToolResultText(use.ID, "Error: invalid tool input: "+err.Error(), true), nil
		}
	}

	mode := l.in.PermissionMode
	if l.in.PermissionModeFunc != nil {
		mode = l.in.PermissionModeFunc()
	}
	pc := permission.Context{
		Mode:       mode,
		WorkingDir: l.in.WorkingDir,
		Allowed:    l.in.Allowed,
		Disallowed: l.in.Disallowed,
	}

	isReadOnly := t.IsReadOnly(input)
	var (
		innerTool   tool.CoreTool
		innerName   string
		innerParams json.RawMessage
	)
	if use.Name == tool.ExecuteExtraToolName {
		var errMsg string
		innerTool, innerName, innerParams, errMsg = l.resolveExtraTool(settling, input)
		if errMsg != "" {
			return llm.ToolResultText(use.ID, errMsg, true), nil
		}
		isReadOnly = innerTool.IsReadOnly(innerParams)
	}

	dec := permission.Evaluate(toolCtx, permission.Request{
		ToolName:     use.Name,
		Input:        input,
		IsReadOnly:   isReadOnly,
		ToolDecision: t.CheckPermissions(toolCtx, input, pc),
		Ctx:          pc,
		Ask:          l.in.CanUseTool,
	})
	if dec.Behavior != permission.Allow {
		msg := dec.Message
		if msg == "" {
			msg = "denied"
		}
		return llm.ToolResultText(use.ID, "Tool call was not permitted: "+msg, true), nil
	}
	if len(dec.UpdatedInput) > 0 {
		input = dec.UpdatedInput
		if use.Name == tool.ExecuteExtraToolName {
			var errMsg string
			innerTool, innerName, innerParams, errMsg = l.resolveExtraTool(settling, input)
			if errMsg != "" {
				return llm.ToolResultText(use.ID, errMsg, true), nil
			}
		}
	}
	if innerTool != nil {
		innerDec := permission.Evaluate(toolCtx, permission.Request{
			ToolName:     innerName,
			Input:        innerParams,
			IsReadOnly:   innerTool.IsReadOnly(innerParams),
			ToolDecision: innerTool.CheckPermissions(toolCtx, innerParams, pc),
			Ctx:          pc,
			Ask:          l.in.CanUseTool,
		})
		if innerDec.Behavior != permission.Allow {
			msg := innerDec.Message
			if msg == "" {
				msg = "denied"
			}
			return llm.ToolResultText(use.ID, "Tool call was not permitted: "+msg, true), nil
		}
		if len(innerDec.UpdatedInput) > 0 {
			rewritten, err := marshalExtraToolInput(innerName, innerDec.UpdatedInput)
			if err != nil {
				return llm.ToolResultText(use.ID, "Error: invalid rewritten tool input: "+err.Error(), true), nil
			}
			innerParams = normalizeExtraParams(innerDec.UpdatedInput)
			input = rewritten
		}
	}

	// Pre-tool hook.
	if l.in.Hooks != nil {
		if innerTool != nil {
			if block, msg, updated := l.in.Hooks.PreToolUse(toolCtx, innerName, innerParams); block {
				return llm.ToolResultText(use.ID, "Blocked by hook: "+msg, true), nil
			} else if len(updated) > 0 {
				rewritten, err := marshalExtraToolInput(innerName, updated)
				if err != nil {
					return llm.ToolResultText(use.ID, "Error: invalid rewritten tool input: "+err.Error(), true), nil
				}
				innerParams = normalizeExtraParams(updated)
				input = rewritten
			}
		} else {
			if block, msg, updated := l.in.Hooks.PreToolUse(toolCtx, use.Name, input); block {
				return llm.ToolResultText(use.ID, "Blocked by hook: "+msg, true), nil
			} else if len(updated) > 0 {
				input = updated
			}
		}
	}

	tc := &tool.ToolContext{
		WorkingDir:     l.in.WorkingDir,
		AgentID:        l.in.AgentID,
		ToolUseID:      use.ID,
		Emit:           emitProgress,
		OutputDir:      l.in.ToolOutputDir,
		MaxOutputChars: l.in.MaxToolOutputChars,
		Tasks:          l.in.Tasks,
		Env:            l.in.BashEnv,
	}
	// Framing adds bytes after the cap; hold them back from the head budget
	// (tools that Capture themselves see the same reduced cap).
	if l.in.UntrustedOutputBoundary {
		tc.MaxOutputChars = boundaryCap(tool.MaxOutput(tc), boundaryReserve(l.in.ToolOutputDir))
	}
	// toolCtx carries the run's MaxDuration deadline, so a tool that overruns the
	// wall-clock budget is interrupted here (the turn loop then enters wrap-up). During
	// the wrap-up phase toolCtx is the live parent ctx, so settlement tools run freely.
	res, err := t.Call(toolCtx, input, tc)
	if err != nil {
		res = tool.Errorf("Error: " + err.Error())
	}
	// Global output cap at the single post-tool choke point: EVERY tool —
	// built-in, MCP, custom, host — is bounded here regardless of whether it
	// remembered to Capture itself, so no tool can overflow the context window
	// (the failure mode that motivated this net). CaptureOnce is idempotent:
	// output a tool already spilled/truncated carries a marker and is left
	// untouched, so this never double-truncates or double-spills.
	res = capOutput(tc, res, l.in.UntrustedOutputBoundary)

	if l.in.Hooks != nil {
		raw, _ := json.Marshal(res.Flatten())
		if innerTool != nil {
			l.in.Hooks.PostToolUse(toolCtx, innerName, innerParams, raw, res.IsError)
		} else {
			l.in.Hooks.PostToolUse(toolCtx, use.Name, input, raw, res.IsError)
		}
	}

	var content []llm.ContentBlock
	for _, b := range res.Content {
		if b.Type == llm.BlockText && b.Text == "" {
			continue
		}
		content = append(content, b)
	}
	if len(content) == 0 {
		content = []llm.ContentBlock{llm.TextBlock("(no output)")}
	}
	return llm.ContentBlock{Type: llm.BlockToolResult, ToolUseID: use.ID, Content: content, IsError: res.IsError}, res.Extra
}

func (l *loop) resolveExtraTool(settling bool, input json.RawMessage) (tool.CoreTool, string, json.RawMessage, string) {
	var extraArgs struct {
		ToolName string          `json:"tool_name"`
		Params   json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(input, &extraArgs); err != nil {
		return nil, "", nil, "Error: invalid tool input: " + err.Error()
	}
	if extraArgs.ToolName == "" {
		return nil, "", nil, "Error: tool_name is required"
	}
	if extraArgs.ToolName == tool.ExecuteExtraToolName {
		return nil, "", nil, "Error: nested ExecuteExtraTool calls are not permitted"
	}
	innerTool, innerOK := l.in.Tools.Get(extraArgs.ToolName)
	if !innerOK {
		return nil, "", nil, fmt.Sprintf("Error: unknown tool %q", extraArgs.ToolName)
	}
	if l.isDisabledDuringSettlement(settling, extraArgs.ToolName) {
		return nil, "", nil, fmt.Sprintf("Error: tool %q is disabled during wrap-up; persist findings and summarize without calling it.", extraArgs.ToolName)
	}
	return innerTool, extraArgs.ToolName, normalizeExtraParams(extraArgs.Params), ""
}

func normalizeExtraParams(params json.RawMessage) json.RawMessage {
	if len(params) == 0 || string(params) == "null" {
		return json.RawMessage("{}")
	}
	return params
}

func marshalExtraToolInput(toolName string, params json.RawMessage) (json.RawMessage, error) {
	return json.Marshal(struct {
		ToolName string          `json:"tool_name"`
		Params   json.RawMessage `json:"params"`
	}{
		ToolName: toolName,
		Params:   normalizeExtraParams(params),
	})
}

// capOutput runs every text block of a tool result through the idempotent
// output cap (tool.CaptureOnce). Non-text blocks pass through untouched. It is
// the one place that guarantees no tool's output — however constructed — can
// blow past the context budget. When boundary is set, each text block is then
// framed as untrusted data (see wrapUntrusted) under one fresh nonce per call.
func capOutput(tc *tool.ToolContext, res tool.Result, boundary bool) tool.Result {
	for i, b := range res.Content {
		if b.Type == llm.BlockText {
			res.Content[i].Text = tool.CaptureOnce(tc, b.Text)
		}
	}
	if !boundary {
		return res
	}
	nonce, err := newBoundaryNonce()
	if err != nil {
		return tool.Errorf("Error: cannot frame tool output as untrusted: " + err.Error())
	}
	for i, b := range res.Content {
		if b.Type == llm.BlockText {
			res.Content[i].Text = wrapUntrusted(nonce, b.Text)
		}
	}
	return res
}
