-- Onda 0 performance indexes to eliminate sequential scans in:
--   - CancelIntent (DELETE FROM findings WHERE node_id=$1)
--   - TaskListMetricsAll (finding_metrics index-only scan on severity)
--   - OverviewFingerprint (MAX(updated_at) / COUNT(*) per exploration)
--   - Task delete/archive cleanup and filtering on llm_records (WHERE task_id=$1)

CREATE INDEX IF NOT EXISTS idx_findings_node_id
    ON findings(node_id)
    WHERE node_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_findings_task_sev
    ON findings(task_id, created_at DESC)
    INCLUDE (severity);

CREATE INDEX IF NOT EXISTS idx_exp_nodes_updated
    ON exploration_nodes(exploration_id, updated_at DESC);

CREATE INDEX IF NOT EXISTS idx_llm_records_task_id
    ON llm_records(task_id)
    WHERE task_id IS NOT NULL AND task_id <> '';
