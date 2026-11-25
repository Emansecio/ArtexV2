-- Audit performance indexes (idempotent).
CREATE INDEX IF NOT EXISTS idx_skill_usage_exp
    ON skill_usage(exploration_id) WHERE exploration_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_tool_usage_exp
    ON tool_usage(exploration_id) WHERE exploration_id IS NOT NULL;

CREATE INDEX IF NOT EXISTS idx_activity_tool_use_desc
    ON activity(id DESC) WHERE kind = 'tool_use';

CREATE INDEX IF NOT EXISTS idx_notification_deliveries_claimable
    ON notification_deliveries(channel_id, next_attempt_at, id)
    WHERE state IN ('pending', 'sending');

CREATE INDEX IF NOT EXISTS idx_expnodes_met_goals
    ON exploration_nodes(id) WHERE kind = 'goal' AND state = 'met';
