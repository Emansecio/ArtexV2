package agent

// pentestCompactPrompt is the ARTEX-specialized AutoCompact summarization prompt.
// It replaces Norma's generic coding summary prompt with a security-testing
// structure so compacted worker/planner/chat runs preserve targets, endpoints,
// credentials, traffic IDs, ruled-out hypotheses, and spilled output paths
// while staying concise.
const pentestCompactPrompt = `CRITICAL: Respond with TEXT ONLY. Do NOT call any tools.

- Do NOT use Bash, Read, WebFetch, or ANY other tool.
- You already have all context in the conversation above.
- Output plain text only: a brief <analysis> block followed by a concise <summary> block.

Summarize this authorized penetration-testing session so execution can continue accurately without repeating work. Preserve exact hostnames, URLs, endpoints, parameter names, credentials/tokens, node IDs, traffic IDs, and local/spill file paths. Omit boilerplate and verbose raw outputs.

Wrap your brief reasoning in <analysis>...</analysis>, then output <summary>...</summary> with these exact sections:

1. Active Intent / Goal & Authorized Scope:
   - Exact intent or operator request, intent_id (if any), and any active operation constraints.
2. Confirmed Targets, Endpoints, Parameters & Tech Stack:
   - Concrete hosts, ports, URLs, HTTP methods, parameters, frameworks, versions, and defenses (WAF/auth/rate-limit).
3. Credentials, Tokens, Cookies, Sessions & Key Traffic IDs:
   - Valid/invalid accounts, JWTs/cookies/API keys (or where stored), and relevant traffic_id / blob hashes.
4. Findings & Facts (Persisted vs. Pending):
   - What has already been written via record_fact / insert_assets / report_finding (with IDs if known), and what was observed but NOT yet persisted.
5. Tested & Ruled-Out Hypotheses:
   - Attack vectors, payloads, and endpoints already tried and their exact outcome so they are not repeated.
6. Local Artifacts & Spilled Output Files:
   - Scripts/files written in the run directory and any <persisted-output> spill file paths worth referencing.
7. Exact Next Step:
   - The single immediate next action to continue the active intent.

REMINDER: Do NOT call any tools. Output only <analysis>...</analysis> followed by <summary>...</summary>.`

// artexCompactableTools lists ARTEX domain read tools whose older tool_result
// payloads are safe for MicroCompact to clear (they can always be re-queried).
var artexCompactableTools = []string{
	"traffic_get",
	"traffic_search",
	"traffic_blob",
	"traffic_messages",
	"list_assets",
	"graph_overview",
	"node_detail",
	"list_findings",
	"list_facts",
	"list_untested_assets",
	"expand_digest",
	"get_worker_trace",
	"get_worker_output",
	"list_worker_traces",
	"search_all_worker_traces",
	"list_task_credentials",
	"list_companies",
	"TaskOutput",
	"shell_read",
	"shell_send",
	// Orchestration read tools (Auto/Planner/Reporter) — re-queryable payloads.
	"get_task_graph",
	"list_task_findings",
	"get_task_worker_trace",
	"list_task_worker_traces",
	"search_task_worker_traces",
	"get_task_node_detail",
	"get_finding_traffic",
	"get_finding_retest_context",
	"list_tasks",
}
