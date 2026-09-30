package handler

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/multica-ai/multica/server/internal/testutil"
	db "github.com/multica-ai/multica/server/pkg/db/generated"
)

func TestQuotaGroupBlocksSharedAgentsUntilOwnerClears(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	t.Cleanup(func() {
		testPool.Exec(context.Background(), `DELETE FROM agent_quota_group_block WHERE owner_id=$1 AND group_key='kimi-shared'`, testUserID)
	})
	runtime := dbfx.Runtime(t, "quota-group-test")
	configured := testutil.Raw(`'{"quota_group":" kimi-shared "}'::jsonb`)
	otherConfigured := testutil.Raw(`'{"quota_group":"unrelated"}'::jsonb`)
	first := dbfx.Agent(t, "quota-first", runtime, testutil.Cols{"runtime_config": configured})
	second := dbfx.Agent(t, "quota-second", runtime, testutil.Cols{"runtime_config": configured})
	other := dbfx.Agent(t, "quota-other", runtime, testutil.Cols{"runtime_config": otherConfigured})
	issue := dbfx.Issue(t, "quota group test")
	failed := dbfx.Task(t, first, testutil.Cols{
		"runtime_id": runtime, "issue_id": issue, "status": "running",
		"attempt": 1, "max_attempts": 1,
	})
	queued := dbfx.Task(t, second, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "priority": 10})
	unrelated := dbfx.Task(t, other, testutil.Cols{"runtime_id": runtime, "issue_id": issue, "priority": 5})

	if _, err := testHandler.TaskService.FailTask(ctx, parseUUID(failed),
		"API Error: 403 You've reached your 5-hour usage limit", "", "", "",
		"agent_error.provider_auth_or_access", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var group string
	if err := testPool.QueryRow(ctx, `SELECT group_key FROM agent_quota_group_block WHERE owner_id=$1`, testUserID).Scan(&group); err != nil || group != "kimi-shared" {
		t.Fatalf("quota group block = %q, err %v", group, err)
	}
	claimed, err := testHandler.TaskService.ClaimTasksForRuntimes(ctx, []pgtype.UUID{parseUUID(runtime)}, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(claimed) != 1 || claimed[0].ID != parseUUID(unrelated) {
		t.Fatalf("blocked group should not claim, unrelated group should: %+v", claimed)
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, queued).Scan(&status); err != nil || status != "queued" {
		t.Fatalf("blocked task status = %q, err %v", status, err)
	}

	get := withURLParam(newRequest(http.MethodGet, "/api/agents/"+second+"/quota", nil), "id", second)
	w := httptest.NewRecorder()
	testHandler.GetAgentQuotaGroup(w, get)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"blocked":true`) {
		t.Fatalf("quota status = %d %s", w.Code, w.Body.String())
	}
	adminID := dbfx.User(t, "quota-workspace-admin", "quota-admin-"+uuid.NewString()+"@multica.test")
	dbfx.Member(t, testWorkspaceID, adminID, "admin")
	adminClear := withURLParam(newRequestAs(adminID, http.MethodPost, "/api/agents/"+second+"/quota/clear", nil), "id", second)
	w = httptest.NewRecorder()
	testHandler.ClearAgentQuotaGroup(w, adminClear)
	if w.Code != http.StatusForbidden {
		t.Fatalf("workspace admin cleared someone else's provider gate: %d %s", w.Code, w.Body.String())
	}
	stillBlocked, err := testHandler.Queries.GetAgentQuotaGroupBlock(ctx, db.GetAgentQuotaGroupBlockParams{OwnerID: parseUUID(testUserID), GroupKey: "kimi-shared"})
	if err != nil || stillBlocked.GroupKey != "kimi-shared" {
		t.Fatalf("quota gate lost after unauthorized clear: %+v, err %v", stillBlocked, err)
	}
	clear := withURLParam(newRequest(http.MethodPost, "/api/agents/"+second+"/quota/clear", nil), "id", second)
	w = httptest.NewRecorder()
	testHandler.ClearAgentQuotaGroup(w, clear)
	if w.Code != http.StatusOK {
		t.Fatalf("clear quota = %d %s", w.Code, w.Body.String())
	}
	next, err := testHandler.TaskService.ClaimTaskForRuntime(ctx, parseUUID(runtime))
	if err != nil || next == nil || next.ID != parseUUID(queued) {
		t.Fatalf("claim after human clear = %+v, err %v", next, err)
	}
}

// Legacy ownerless agents must still record a terminal failure even if their
// runtime_config contains a quota label; no owner-scoped gate can be written.
func TestQuotaGroupOwnerlessAgentFailureDoesNotRollback(t *testing.T) {
	if testHandler == nil || testPool == nil {
		t.Skip("database not available")
	}
	ctx := context.Background()
	runtime := dbfx.Runtime(t, "quota-ownerless-runtime")
	agent := dbfx.Agent(t, "quota-ownerless", runtime, testutil.Cols{
		"runtime_config": testutil.Raw(`'{"quota_group":"legacy-ownerless-test"}'::jsonb`),
	})
	if _, err := testPool.Exec(ctx, `UPDATE agent SET owner_id=NULL WHERE id=$1`, agent); err != nil {
		t.Fatal(err)
	}
	issue := dbfx.Issue(t, "quota ownerless failure")
	task := dbfx.Task(t, agent, testutil.Cols{
		"runtime_id": runtime, "issue_id": issue, "status": "running",
		"attempt": 1, "max_attempts": 1,
	})
	if _, err := testHandler.TaskService.FailTask(ctx, parseUUID(task),
		"API Error: 403 You've reached your 5-hour usage limit", "", "", "",
		"agent_error.provider_auth_or_access", false, "", ""); err != nil {
		t.Fatal(err)
	}
	var status string
	if err := testPool.QueryRow(ctx, `SELECT status FROM agent_task_queue WHERE id=$1`, task).Scan(&status); err != nil || status != "failed" {
		t.Fatalf("ownerless task status = %q, err %v", status, err)
	}
	var blocks int
	if err := testPool.QueryRow(ctx, `SELECT count(*) FROM agent_quota_group_block WHERE group_key='legacy-ownerless-test'`).Scan(&blocks); err != nil || blocks != 0 {
		t.Fatalf("ownerless quota blocks = %d, err %v", blocks, err)
	}
}
