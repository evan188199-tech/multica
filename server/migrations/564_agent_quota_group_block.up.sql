-- A quota group is scoped to the human owner, so another workspace/member
-- cannot block an unrelated account by choosing the same public group label.
CREATE TABLE agent_quota_group_block (
    owner_id uuid NOT NULL,
    group_key text NOT NULL,
    blocked_at timestamptz NOT NULL DEFAULT now(),
    reason text NOT NULL,
    failure_task_id uuid NOT NULL
);
