package tool

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/Autumn-27/norma/permission"
)

// catastrophicBash matches a small set of irreversibly destructive commands
// that are hard-denied regardless of mode (NFR-10). This is a safety floor, not
// a full sandbox — hosts should still gate Bash via CanUseTool.
var catastrophicBash = []*regexp.Regexp{
	regexp.MustCompile(`\brm\s+(-[a-zA-Z]*\s+)*-[a-zA-Z]*[rf][a-zA-Z]*\s+(-[a-zA-Z]+\s+)*/(\s|$)`), // rm -rf /
	regexp.MustCompile(`:\s*\(\s*\)\s*\{\s*:\s*\|\s*:\s*&\s*\}\s*;`),                               // fork bomb
	regexp.MustCompile(`\bmkfs\.\w+\s+/dev/`),                                                      // format a device
	regexp.MustCompile(`\bdd\b[^\n]*\bof=/dev/(sd|nvme|hd)`),                                       // overwrite a disk
}

// disallowedAutoBackground lists commands that must never be silently moved to
// the background on timeout.
var disallowedAutoBackground = []string{"sleep"}

// NewBash builds the Bash tool: run a shell command with a timeout, optionally in
// the background.
func NewBash() CoreTool {
	props := map[string]any{
		"command":    map[string]any{"type": "string", "description": "The shell command to execute."},
		"timeout_ms": map[string]any{"type": "integer", "description": "Timeout in ms (default 120000, max 600000)."},
	}
	desc := "Executes a shell command in the working directory and returns combined stdout and stderr. Use for running tests, builds, package managers, and git. Prefer the dedicated file tools for file work."
	prompt := "The command runs through the shell, so you are not limited to one command: chain steps with && or ;, use pipes, and pass multi-line scripts or heredocs. For anything beyond a simple one-liner — loops, parsing, string/JSON handling, HTTP logic, computation — prefer writing a short script (Python, Node, shell, etc.) and executing it (e.g. via a heredoc or by writing a file then running it) rather than forcing everything into a single command. A timeout (default 120s, max 600s) applies. Avoid commands needing interactive input."
	if !BackgroundTasksDisabled() {
		props["run_in_background"] = map[string]any{
			"type":        "boolean",
			"description": "Run in the background and return immediately; read its output later with TaskOutput or Read, and you are notified when it completes. A foreground command that exceeds its timeout is also moved to the background automatically.",
		}
		desc += " Set run_in_background to return immediately and read the output later."
	}
	return Build(Spec{
		Name:        "Bash",
		Description: desc,
		Prompt:      prompt,
		Schema: map[string]any{
			"type":       "object",
			"properties": props,
			"required":   []any{"command"},
		},
		Concurrent:  bashConcurrencySafe,
		Permissions: bashPermissions,
		Run:         runBash,
	})
}

func bashPermissions(_ context.Context, input json.RawMessage, _ permission.Context) permission.Decision {
	var in struct {
		Command string `json:"command"`
	}
	_ = json.Unmarshal(input, &in)
	for _, re := range catastrophicBash {
		if re.MatchString(in.Command) {
			return permission.Denied("denied: command appears irreversibly destructive and is blocked by the safety floor")
		}
	}
	return permission.AskUser("run shell command: " + in.Command)
}

func runBash(ctx context.Context, input json.RawMessage, tc *ToolContext) (Result, error) {
	var in struct {
		Command         string `json:"command"`
		TimeoutMS       int    `json:"timeout_ms"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return Result{}, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return Errorf("Error: command is required"), nil
	}
	timeout := 120 * time.Second
	if in.TimeoutMS > 0 {
		timeout = time.Duration(in.TimeoutMS) * time.Millisecond
		if timeout > 600*time.Second {
			timeout = 600 * time.Second
		}
	}

	// Background-capable path: requires a task manager on the context.
	if tc != nil && tc.Tasks != nil && !BackgroundTasksDisabled() {
		return runBashManaged(ctx, tc, in.Command, timeout, in.RunInBackground)
	}
	// Fallback: synchronous execution (unchanged legacy behavior).
	return runBashSync(ctx, tc, in.Command, timeout)
}

// runBashManaged runs the command through the task manager, supporting explicit
// background launch and timeout auto-backgrounding.
func runBashManaged(ctx context.Context, tc *ToolContext, command string, timeout time.Duration, explicitBG bool) (Result, error) {
	spec := SpawnSpec{Command: command, Kind: KindBash, WorkingDir: tc.WorkingDir, Description: short(command), Env: tc.Env}

	if explicitBG {
		t, err := tc.Tasks.Spawn(spec)
		if err != nil {
			return Errorf("Error: failed to start background command: " + err.Error()), nil
		}
		tc.Tasks.Background(t.ID)
		return Text(fmt.Sprintf(
			"Running in background as %s. Output file: %s\nUse TaskOutput{task_id:%q} or Read to read its output; you are notified when it completes.",
			t.ID, t.OutputPath, t.ID)), nil
	}

	t, finished, output, err := tc.Tasks.RunForeground(ctx, spec, timeout)
	if err != nil {
		if t != nil {
			tc.Tasks.Forget(t.ID)
		}
		return Errorf("Error: " + err.Error()), nil
	}
	if finished {
		info, _ := tc.Tasks.Get(t.ID)
		tc.Tasks.Forget(t.ID)
		if info.Status == TaskFailed {
			if strings.TrimSpace(output) == "" {
				output = "(no output)"
			}
			return Errorf(fmt.Sprintf("%s\n\n[exit code %d]", Capture(tc, CollapseRepeatedLines(output)), info.ExitCode)), nil
		}
		if strings.TrimSpace(output) == "" {
			output = "(command produced no output)"
		}
		return Text(Capture(tc, CollapseRepeatedLines(output))), nil
	}

	// Timed out while still running.
	if autoBackgroundAllowed(command) {
		tc.Tasks.Background(t.ID)
		return Text(fmt.Sprintf(
			"Command still running after %s; moved to background as %s. Output file: %s\nUse TaskOutput{task_id:%q} to read its output; you are notified when it completes.",
			timeout, t.ID, t.OutputPath, t.ID)), nil
	}
	out, _ := tc.Tasks.Output(t.ID, 0)
	tc.Tasks.Kill(t.ID)
	tc.Tasks.Forget(t.ID)
	return Errorf(Capture(tc, CollapseRepeatedLines(out)) + fmt.Sprintf("\n\n[command timed out after %s]", timeout)), nil
}

// runBashSync is the original blocking implementation, used when no task manager
// is available (background execution disabled).
func runBashSync(ctx context.Context, tc *ToolContext, command string, timeout time.Duration) (Result, error) {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	shell, flags := shellCmd()
	cmd := exec.CommandContext(cctx, shell, append(flags, command)...)
	if tc != nil {
		cmd.Dir = tc.WorkingDir
		cmd.Env = withEnv(tc.Env)
	} else {
		cmd.Env = withEnv(nil)
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()

	out := Capture(tc, CollapseRepeatedLines(buf.String()))
	if cctx.Err() == context.DeadlineExceeded {
		return Errorf(out + fmt.Sprintf("\n\n[command timed out after %s]", timeout)), nil
	}
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			if out == "" {
				out = "(no output)"
			}
			return Errorf(fmt.Sprintf("%s\n\n[exit code %d]", out, ee.ExitCode())), nil
		}
		return Errorf(fmt.Sprintf("%s\n\n[failed to run: %v]", out, err)), nil
	}
	if strings.TrimSpace(out) == "" {
		out = "(command produced no output)"
	}
	return Text(out), nil
}

// autoBackgroundAllowed reports whether a timed-out command may be moved to the
// background instead of being killed.
func autoBackgroundAllowed(command string) bool {
	fields := strings.Fields(command)
	if len(fields) == 0 {
		return false
	}
	base := fields[0]
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	for _, d := range disallowedAutoBackground {
		if base == d {
			return false
		}
	}
	return true
}

// bashConcurrencySafe reports whether a Bash call is strictly single-line
// read-only (no shell control operators/redirections/subshells and every pipe
// stage is a whitelisted read-only command), safe to execute concurrently.
func bashConcurrencySafe(input json.RawMessage) bool {
	var in struct {
		Command         string `json:"command"`
		RunInBackground bool   `json:"run_in_background"`
	}
	if json.Unmarshal(input, &in) != nil || in.RunInBackground {
		return false
	}
	cmd := strings.TrimSpace(in.Command)
	if cmd == "" || strings.ContainsAny(cmd, "\n\r><;&`") || strings.Contains(cmd, "$(") {
		return false
	}
	stages, ok := splitUnquotedPipes(cmd)
	if !ok || len(stages) == 0 {
		return false
	}
	for _, st := range stages {
		words, ok := splitShellWords(st)
		if !ok || len(words) == 0 || !isReadOnlyStage(words) {
			return false
		}
	}
	return true
}

func splitUnquotedPipes(s string) ([]string, bool) {
	var stages []string
	var quote byte
	start := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				continue
			}
			if c == quote {
				quote = 0
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			continue
		}
		if c == '|' {
			part := strings.TrimSpace(s[start:i])
			if part == "" {
				return nil, false
			}
			stages = append(stages, part)
			start = i + 1
		}
	}
	if quote != 0 {
		return nil, false
	}
	last := strings.TrimSpace(s[start:])
	if last == "" {
		return nil, false
	}
	return append(stages, last), true
}

func splitShellWords(s string) ([]string, bool) {
	var words []string
	var cur strings.Builder
	var quote byte
	inTok := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if quote != 0 {
			if c == '\\' && quote == '"' && i+1 < len(s) {
				i++
				cur.WriteByte(s[i])
				continue
			}
			if c == quote {
				quote = 0
				continue
			}
			cur.WriteByte(c)
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			inTok = true
			continue
		}
		if c == '\\' && i+1 < len(s) {
			i++
			cur.WriteByte(s[i])
			inTok = true
			continue
		}
		if c == ' ' || c == '\t' {
			if inTok {
				words = append(words, cur.String())
				cur.Reset()
				inTok = false
			}
			continue
		}
		cur.WriteByte(c)
		inTok = true
	}
	if quote != 0 {
		return nil, false
	}
	if inTok {
		words = append(words, cur.String())
	}
	return words, true
}

func isReadOnlyStage(words []string) bool {
	base := words[0]
	if strings.HasPrefix(base, "$") || strings.Contains(base, "=") {
		return false
	}
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	switch base {
	case "dig", "nslookup", "host", "whois", "grep", "rg", "head", "tail",
		"cat", "wc", "jq", "gojq", "ls", "stat", "file", "uname", "id", "whoami", "pwd",
		"sort", "uniq", "cut":
		return true
	case "awk":
		for _, a := range words[1:] {
			if a == "-i" || strings.HasPrefix(a, "-i") && strings.Contains(a, "inplace") {
				return false
			}
		}
		return true
	case "sed":
		for _, a := range words[1:] {
			if a == "-i" || strings.HasPrefix(a, "-i") || strings.HasPrefix(a, "--in-place") {
				return false
			}
		}
		return true
	case "openssl":
		return len(words) > 1 && (words[1] == "s_client" || words[1] == "x509")
	case "curl":
		return isReadOnlyCurl(words[1:])
	default:
		return false
	}
}

func isReadOnlyCurl(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch a {
		case "-o", "-O", "--output", "-d", "--data", "--data-raw", "--data-binary",
			"--data-urlencode", "--json", "-F", "--form", "-T", "--upload-file",
			"-D", "--dump-header", "--trace", "--trace-ascii", "-K", "--config":
			return false
		case "-X", "--request":
			if i+1 >= len(args) {
				return false
			}
			i++
			switch strings.ToUpper(args[i]) {
			case "GET", "HEAD", "OPTIONS":
			default:
				return false
			}
		default:
			for _, pfx := range []string{"-o", "--output=", "-d", "--data=", "--data-raw=", "--data-binary=", "--json=", "-F", "--form=", "-T", "--upload-file=", "-D", "--dump-header="} {
				if strings.HasPrefix(a, pfx) {
					return false
				}
			}
			if strings.HasPrefix(a, "-X") && len(a) > 2 {
				switch strings.ToUpper(a[2:]) {
				case "GET", "HEAD", "OPTIONS":
				default:
					return false
				}
			}
			if strings.HasPrefix(a, "--request=") {
				switch strings.ToUpper(strings.TrimPrefix(a, "--request=")) {
				case "GET", "HEAD", "OPTIONS":
				default:
					return false
				}
			}
		}
	}
	return true
}
