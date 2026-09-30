-- name: UpsertAgentQuotaGroupBlock :exec
INSERT INTO agent_quota_group_block (owner_id, group_key, reason, failure_task_id)
VALUES (@owner_id, @group_key, @reason, @failure_task_id)
ON CONFLICT (owner_id, group_key) DO UPDATE SET
    blocked_at = now(),
    reason = EXCLUDED.reason,
    failure_task_id = EXCLUDED.failure_task_id;

-- name: GetAgentQuotaGroupBlock :one
SELECT * FROM agent_quota_group_block
WHERE owner_id = @owner_id AND group_key = @group_key;

-- name: ClearAgentQuotaGroupBlock :execrows
DELETE FROM agent_quota_group_block
WHERE owner_id = @owner_id AND group_key = @group_key;

-- name: ListAgentQuotaGroupRuntimes :many
SELECT DISTINCT runtime_id FROM agent
WHERE owner_id = @owner_id
  AND runtime_config->>'quota_group' = @group_key::text
  AND runtime_id IS NOT NULL;
