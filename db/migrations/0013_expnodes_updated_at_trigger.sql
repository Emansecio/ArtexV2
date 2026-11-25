-- exploration_nodes gets a dedicated updated_at trigger (schema.sql keeps the
-- same definition): bookkeeping-only writes — cold_since_round stamp
-- maintenance from the planner compactor and claim-lease heartbeat/token
-- updates — must NOT bump updated_at. OverviewFingerprint and the
-- graph-overview caches key on MAX(updated_at), so heartbeats and stamp
-- rounds otherwise invalidated them every few seconds without changing
-- anything those views render. Only real content changes bump it.
CREATE OR REPLACE FUNCTION set_expnodes_updated_at() RETURNS trigger AS $$
BEGIN
    IF NEW.payload          IS NOT DISTINCT FROM OLD.payload
       AND NEW.state        IS NOT DISTINCT FROM OLD.state
       AND NEW.kind         IS NOT DISTINCT FROM OLD.kind
       AND NEW.priority     IS NOT DISTINCT FROM OLD.priority
       AND NEW.origin       IS NOT DISTINCT FROM OLD.origin
       AND NEW.owner        IS NOT DISTINCT FROM OLD.owner
       AND NEW.blocked_reason IS NOT DISTINCT FROM OLD.blocked_reason
       AND NEW.delete_reason   IS NOT DISTINCT FROM OLD.delete_reason
       AND NEW.completed_at    IS NOT DISTINCT FROM OLD.completed_at
       AND NEW.content_version IS NOT DISTINCT FROM OLD.content_version THEN
        NEW.updated_at = OLD.updated_at;
    ELSE
        NEW.updated_at = now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
DROP TRIGGER IF EXISTS trg_expnodes_upd ON exploration_nodes;
CREATE TRIGGER trg_expnodes_upd BEFORE UPDATE ON exploration_nodes
    FOR EACH ROW EXECUTE FUNCTION set_expnodes_updated_at();
