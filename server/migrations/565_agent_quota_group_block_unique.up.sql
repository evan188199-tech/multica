DROP INDEX CONCURRENTLY IF EXISTS agent_quota_group_block_owner_group_idx;
CREATE UNIQUE INDEX CONCURRENTLY agent_quota_group_block_owner_group_idx
ON agent_quota_group_block (owner_id, group_key);
