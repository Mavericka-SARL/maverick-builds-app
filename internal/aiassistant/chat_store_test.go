package aiassistant_test

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mavericks-engine/mavericks/internal/aiassistant"
)

// seedAppAndUser creates the customer -> workspace -> application chain plus
// an identity.user row, using the real migrations (via setupWriteExecutorDB)
// rather than the older hand-curated testdata fixture used by store_test.go —
// that fixture predates ai_assistant.message/llm_settings and identity.user's
// keycloak_sub column, so it can't seed what ChatStore needs.
func seedAppAndUser(t *testing.T, pool *pgxpool.Pool, email string) (appID, userID string) {
	t.Helper()
	ctx := context.Background()

	var customerID, workspaceID string
	if err := pool.QueryRow(ctx, `INSERT INTO core.customer (name) VALUES ('Test Customer') RETURNING id::text`).Scan(&customerID); err != nil {
		t.Fatalf("seed customer: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO core.workspace (customer_id, name) VALUES ($1::uuid, 'Test Workspace') RETURNING id::text`, customerID).Scan(&workspaceID); err != nil {
		t.Fatalf("seed workspace: %v", err)
	}
	if err := pool.QueryRow(ctx, `INSERT INTO core.application (workspace_id, name, mode) VALUES ($1::uuid, 'Test App', 'planning') RETURNING id::text`, workspaceID).Scan(&appID); err != nil {
		t.Fatalf("seed application: %v", err)
	}
	if err := pool.QueryRow(ctx, `
		INSERT INTO identity.user (keycloak_sub, email) VALUES ($1, $2) RETURNING id::text
	`, "sub-"+email, email).Scan(&userID); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	return appID, userID
}

// TestCountLLMCallsInSession_OnlyCountsAssistantMessages is a regression test
// for the rate-limiting design: every Chat() call saves exactly one
// assistant-role message, so counting those (and only those) must give an
// exact LLM-call count, unaffected by user/tool-role messages in between.
func TestCountLLMCallsInSession_OnlyCountsAssistantMessages(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "dev@test.com")
	chatStore := aiassistant.NewChatStore(pool)

	sess, err := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("CreateSession: %v", err)
	}

	saveOrFail := func(role, content string) {
		if _, err := chatStore.SaveMessage(ctx, sess.ID, role, content, nil, "", ""); err != nil {
			t.Fatalf("SaveMessage(%s): %v", role, err)
		}
	}
	saveOrFail("user", "create a revenue metric")
	saveOrFail("assistant", "") // tool-call turn
	saveOrFail("tool", "Metric 'revenue' created")
	saveOrFail("assistant", "Done — I've created the revenue metric.")

	n, err := chatStore.CountLLMCallsInSession(ctx, sess.ID)
	if err != nil {
		t.Fatalf("CountLLMCallsInSession: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 assistant-role messages counted, got %d", n)
	}
}

func TestCountLLMCallsInSession_ScopedToSession(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "dev@test.com")
	chatStore := aiassistant.NewChatStore(pool)

	sessA, _ := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	sessB, _ := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")

	if _, err := chatStore.SaveMessage(ctx, sessA.ID, "assistant", "reply A1", nil, "", ""); err != nil {
		t.Fatalf("SaveMessage A1: %v", err)
	}
	if _, err := chatStore.SaveMessage(ctx, sessA.ID, "assistant", "reply A2", nil, "", ""); err != nil {
		t.Fatalf("SaveMessage A2: %v", err)
	}
	if _, err := chatStore.SaveMessage(ctx, sessB.ID, "assistant", "reply B1", nil, "", ""); err != nil {
		t.Fatalf("SaveMessage B1: %v", err)
	}

	nA, err := chatStore.CountLLMCallsInSession(ctx, sessA.ID)
	if err != nil {
		t.Fatalf("CountLLMCallsInSession A: %v", err)
	}
	if nA != 2 {
		t.Fatalf("expected session A to have 2 calls, got %d", nA)
	}
	nB, err := chatStore.CountLLMCallsInSession(ctx, sessB.ID)
	if err != nil {
		t.Fatalf("CountLLMCallsInSession B: %v", err)
	}
	if nB != 1 {
		t.Fatalf("expected session B to have 1 call, got %d", nB)
	}
}

func TestCountLLMCallsToday_ScopedToUserAcrossSessions(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "dev@test.com")
	_, otherUserID := seedAppAndUser(t, pool, "other@test.com")
	chatStore := aiassistant.NewChatStore(pool)

	sess1, _ := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	sess2, _ := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	otherSess, _ := chatStore.CreateSession(ctx, appID, otherUserID, "openai", "gpt-4o-mini")

	for range 2 {
		if _, err := chatStore.SaveMessage(ctx, sess1.ID, "assistant", "r", nil, "", ""); err != nil {
			t.Fatalf("SaveMessage sess1: %v", err)
		}
	}
	if _, err := chatStore.SaveMessage(ctx, sess2.ID, "assistant", "r", nil, "", ""); err != nil {
		t.Fatalf("SaveMessage sess2: %v", err)
	}
	// Belongs to a different user — must not count toward userID's daily total.
	if _, err := chatStore.SaveMessage(ctx, otherSess.ID, "assistant", "r", nil, "", ""); err != nil {
		t.Fatalf("SaveMessage otherSess: %v", err)
	}

	n, err := chatStore.CountLLMCallsToday(ctx, userID)
	if err != nil {
		t.Fatalf("CountLLMCallsToday: %v", err)
	}
	if n != 3 {
		t.Fatalf("expected 3 calls today across userID's 2 sessions, got %d", n)
	}
}

// TestCountLLMCallsToday_ExcludesPriorDays is a regression test for the day-
// boundary logic: a message from 2 days ago must not count toward today's cap.
func TestCountLLMCallsToday_ExcludesPriorDays(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	ctx := context.Background()
	appID, userID := seedAppAndUser(t, pool, "dev@test.com")
	chatStore := aiassistant.NewChatStore(pool)

	sess, _ := chatStore.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	msg, err := chatStore.SaveMessage(ctx, sess.ID, "assistant", "old reply", nil, "", "")
	if err != nil {
		t.Fatalf("SaveMessage: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE ai_assistant.message SET created_at = now() - interval '2 days' WHERE id=$1::uuid`, msg.ID); err != nil {
		t.Fatalf("backdate message: %v", err)
	}

	n, err := chatStore.CountLLMCallsToday(ctx, userID)
	if err != nil {
		t.Fatalf("CountLLMCallsToday: %v", err)
	}
	if n != 0 {
		t.Fatalf("expected a 2-day-old message to be excluded from today's count, got %d", n)
	}
}

// TestSessionTitleLifecycle: the background auto-namer only fills EMPTY
// titles (SetTitleIfEmpty), while an explicit rename always wins and is
// owner-scoped — a generated title must never clobber a name the user chose.
func TestSessionTitleLifecycle(t *testing.T) {
	pool := setupWriteExecutorDB(t)
	appID, userID := seedAppAndUser(t, pool, "title@test.dev")
	_, otherUserID := func() (string, string) {
		var id string
		if err := pool.QueryRow(context.Background(),
			`INSERT INTO identity.user (keycloak_sub, email) VALUES ('sub-other','other@test.dev') RETURNING id::text`).Scan(&id); err != nil {
			t.Fatalf("seed other user: %v", err)
		}
		return "", id
	}()
	ctx := context.Background()
	store := aiassistant.NewChatStore(pool)

	sess, err := store.CreateSession(ctx, appID, userID, "openai", "gpt-4o-mini")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	if sess.Title != "" {
		t.Fatalf("new session title = %q, want empty (unnamed)", sess.Title)
	}

	// Auto-name fills the empty title...
	if err := store.SetTitleIfEmpty(ctx, sess.ID, "Build revenue model"); err != nil {
		t.Fatalf("SetTitleIfEmpty: %v", err)
	}
	got, _ := store.GetSession(ctx, sess.ID)
	if got.Title != "Build revenue model" {
		t.Errorf("title after auto-name = %q, want 'Build revenue model'", got.Title)
	}
	// ...but never overwrites a non-empty one.
	if err := store.SetTitleIfEmpty(ctx, sess.ID, "Late duplicate generation"); err != nil {
		t.Fatalf("SetTitleIfEmpty(2): %v", err)
	}
	got, _ = store.GetSession(ctx, sess.ID)
	if got.Title != "Build revenue model" {
		t.Errorf("auto-name overwrote an existing title: %q", got.Title)
	}

	// Explicit rename wins over everything.
	if err := store.RenameSession(ctx, sess.ID, userID, "Q3 pricing work"); err != nil {
		t.Fatalf("RenameSession: %v", err)
	}
	got, _ = store.GetSession(ctx, sess.ID)
	if got.Title != "Q3 pricing work" {
		t.Errorf("title after rename = %q, want 'Q3 pricing work'", got.Title)
	}

	// Rename is owner-scoped: another user must not rename this session.
	if err := store.RenameSession(ctx, sess.ID, otherUserID, "hijacked"); err == nil {
		t.Error("RenameSession by a non-owner succeeded, want error")
	}
	// And the list surface serves the title.
	list, err := store.ListSessions(ctx, appID, userID)
	if err != nil || len(list) == 0 {
		t.Fatalf("list sessions: %v (%d)", err, len(list))
	}
	if list[0].Title != "Q3 pricing work" {
		t.Errorf("listed title = %q, want 'Q3 pricing work'", list[0].Title)
	}
}
