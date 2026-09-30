package handler

import (
	"context"
	"testing"

	"github.com/multica-ai/multica/server/internal/testutil"
)

func TestAssignedSquadHandoffRequiresVerifiedDelivery(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	leaderRuntime := dbfx.Runtime(t, "handoff leader runtime")
	leader := dbfx.Agent(t, "handoff leader", leaderRuntime)
	workerRuntime := dbfx.Runtime(t, "handoff worker runtime")
	worker := dbfx.Agent(t, "handoff worker", workerRuntime)
	squad := dbfx.Squad(t, "handoff squad", leader)
	issueID := dbfx.Issue(t, "handoff gate", testutil.Cols{
		"status": "in_progress", "assignee_type": "squad", "assignee_id": squad,
	})
	issue, err := testHandler.Queries.GetIssue(ctx, parseUUID(issueID))
	if err != nil {
		t.Fatal(err)
	}
	leaderTask := dbfx.Task(t, leader, testutil.Cols{
		"runtime_id": leaderRuntime, "issue_id": issueID, "status": "completed",
		"is_leader_task": true, "squad_id": squad,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	delegated := dbfx.Task(t, worker, testutil.Cols{
		"runtime_id": workerRuntime, "issue_id": issueID, "status": "running",
		"squad_id": squad, "delegated_from_task_id": leaderTask,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	unrelated := dbfx.Task(t, worker, testutil.Cols{
		"runtime_id": workerRuntime, "issue_id": issueID, "status": "running",
		"squad_id":           squad,
		"originator_user_id": testUserID, "accountable_user_id": testUserID,
	})
	check := func(name, actorType, taskID, commentType string, want int) {
		t.Helper()
		opts := commentTriggerComputeOptions{OriginatorUserID: testUserID, CommentType: commentType}
		if taskID != "" {
			opts.AuthoringTaskID = parseUUID(taskID)
		}
		got, _ := testHandler.computeCommentAgentTriggers(ctx, issue, "Work status", nil, actorType, worker, opts)
		if len(got) != want {
			t.Errorf("%s: got %d leader triggers, want %d", name, len(got), want)
		}
	}
	check("verified delivery", "agent", delegated, "comment", 1)
	check("progress update", "agent", delegated, "progress_update", 0)
	check("unrelated run", "agent", unrelated, "comment", 0)
	check("missing source task", "agent", "", "comment", 0)
	check("system failure relay", "system", delegated, "comment", 0)
}
