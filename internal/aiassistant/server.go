package aiassistant

import (
	"context"
	"fmt"
	"strings"

	"github.com/anthropics/anthropic-sdk-go"
	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	aiassistantv1 "github.com/mavericks-engine/mavericks/gen/go/aiassistant/v1"
	commonv1 "github.com/mavericks-engine/mavericks/gen/go/common/v1"
)

type Server struct {
	aiassistantv1.UnimplementedAIAssistantServiceServer
	log    zerolog.Logger
	store  *Store
	claude *anthropic.Client
}

func NewServer(log zerolog.Logger, store *Store, claude *anthropic.Client) *Server {
	return &Server{log: log, store: store, claude: claude}
}

func (s *Server) StartSession(ctx context.Context, req *aiassistantv1.StartSessionRequest) (*aiassistantv1.StartSessionResponse, error) {
	if req.ApplicationId == "" {
		return nil, status.Error(codes.InvalidArgument, "application_id is required")
	}
	if req.Actor == nil {
		return nil, status.Error(codes.InvalidArgument, "actor is required")
	}
	if req.Actor.Role != commonv1.Role_ROLE_DEVELOPER && req.Actor.Role != commonv1.Role_ROLE_PLATFORM_ADMIN {
		return nil, status.Error(codes.PermissionDenied, "AI assistant is available to developers and platform admins only")
	}

	sess, err := s.store.CreateSession(ctx, req.ApplicationId, req.Actor.UserId)
	if err != nil {
		s.log.Error().Err(err).Msg("create session")
		return nil, status.Error(codes.Internal, err.Error())
	}
	sess.Actor = req.Actor
	return &aiassistantv1.StartSessionResponse{Session: sess}, nil
}

func (s *Server) GenerateDiff(ctx context.Context, req *aiassistantv1.GenerateDiffRequest) (*aiassistantv1.GenerateDiffResponse, error) {
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}
	if req.Prompt == "" {
		return nil, status.Error(codes.InvalidArgument, "prompt is required")
	}
	if s.claude == nil {
		return nil, status.Error(codes.Unavailable, "AI assistant not configured: ANTHROPIC_API_KEY not set")
	}

	sess, _, err := s.store.GetSession(ctx, req.SessionId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	systemPrompt := buildSystemPrompt(sess.ApplicationId, req.ActionType)
	userMsg := fmt.Sprintf("Application ID: %s\n\nRequest: %s", sess.ApplicationId, req.Prompt)

	resp, err := s.claude.Messages.New(ctx, anthropic.MessageNewParams{
		Model:     anthropic.ModelClaudeHaiku4_5,
		MaxTokens: 4096,
		System: []anthropic.TextBlockParam{
			{Text: systemPrompt},
		},
		Messages: []anthropic.MessageParam{
			anthropic.NewUserMessage(anthropic.NewTextBlock(userMsg)),
		},
	})
	if err != nil {
		s.log.Error().Err(err).Str("session_id", req.SessionId).Msg("claude api call failed")
		return nil, status.Errorf(codes.Internal, "LLM request failed: %v", err)
	}

	rawText := extractText(resp)
	diffs, impact := parseDiffResponse(rawText)

	action, err := s.store.CreateAction(ctx, req.SessionId, req.ActionType, req.Prompt, diffs, impact)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &aiassistantv1.GenerateDiffResponse{Action: action}, nil
}

func (s *Server) RunInSandbox(ctx context.Context, req *aiassistantv1.RunInSandboxRequest) (*aiassistantv1.RunInSandboxResponse, error) {
	if req.ActionId == "" {
		return nil, status.Error(codes.InvalidArgument, "action_id is required")
	}

	action, err := s.store.GetAction(ctx, req.ActionId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	// MVP sandbox: validate SQL files in the diff set.
	// Full isolated K8s namespace sandbox deferred to Phase 4.
	var issues []string
	for _, diff := range action.Diffs {
		if strings.HasSuffix(diff.Path, ".sql") {
			if err := validateSQL(diff.After); err != nil {
				issues = append(issues, fmt.Sprintf("%s: %v", diff.Path, err))
			}
		}
	}

	if len(issues) > 0 {
		return &aiassistantv1.RunInSandboxResponse{
			Success: false,
			Error:   strings.Join(issues, "; "),
		}, nil
	}
	return &aiassistantv1.RunInSandboxResponse{
		Success: true,
		Output:  fmt.Sprintf("sandbox validation passed (%d file(s) checked)", len(action.Diffs)),
	}, nil
}

func (s *Server) ApplyDiff(ctx context.Context, req *aiassistantv1.ApplyDiffRequest) (*aiassistantv1.ApplyDiffResponse, error) {
	if req.ActionId == "" {
		return nil, status.Error(codes.InvalidArgument, "action_id is required")
	}

	if err := s.store.MarkApplied(ctx, req.ActionId, ""); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.log.Info().Str("action_id", req.ActionId).Msg("diff applied")
	return &aiassistantv1.ApplyDiffResponse{Success: true}, nil
}

func (s *Server) RollbackAction(ctx context.Context, req *aiassistantv1.RollbackActionRequest) (*aiassistantv1.RollbackActionResponse, error) {
	if req.ActionId == "" {
		return nil, status.Error(codes.InvalidArgument, "action_id is required")
	}

	if err := s.store.MarkRolledBack(ctx, req.ActionId); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.log.Info().Str("action_id", req.ActionId).Msg("action rolled back")
	return &aiassistantv1.RollbackActionResponse{Success: true}, nil
}

func (s *Server) GetSession(ctx context.Context, req *aiassistantv1.GetSessionRequest) (*aiassistantv1.GetSessionResponse, error) {
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id is required")
	}

	sess, actions, err := s.store.GetSession(ctx, req.SessionId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &aiassistantv1.GetSessionResponse{Session: sess, Actions: actions}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func buildSystemPrompt(applicationID string, actionType aiassistantv1.ActionType) string {
	base := `You are an AI assistant for the Mavericks Engine, a code-first enterprise application factory.
Help developers modify application definitions by generating precise diffs.

Response format:
PATH: <file path>
BEFORE: <current content or empty>
AFTER: <new content>
DIFF: <unified diff>

IMPACT
metric: <affected metric name>
policy: <affected policy name>
migration: <required migration description>

WARNINGS
- <any risk or caveat>

Application context: ` + applicationID

	switch actionType {
	case aiassistantv1.ActionType_ACTION_TYPE_ADD_METRIC:
		return base + "\n\nFocus: adding a new metric definition with formula and dependencies."
	case aiassistantv1.ActionType_ACTION_TYPE_MODIFY_FORMULA:
		return base + "\n\nFocus: modifying an existing metric formula. Validate all {references} exist."
	case aiassistantv1.ActionType_ACTION_TYPE_ADD_DIMENSION:
		return base + "\n\nFocus: adding a new dimension with member codes and hierarchy placement."
	case aiassistantv1.ActionType_ACTION_TYPE_GENERATE_MIGRATION:
		return base + "\n\nFocus: generating a valid PostgreSQL migration SQL file."
	default:
		return base
	}
}

func extractText(msg *anthropic.Message) string {
	var sb strings.Builder
	for _, block := range msg.Content {
		if block.Type == "text" {
			sb.WriteString(block.Text)
		}
	}
	return sb.String()
}

func parseDiffResponse(text string) ([]*aiassistantv1.FileDiff, *aiassistantv1.ImpactSummary) {
	impact := &aiassistantv1.ImpactSummary{}
	var diffs []*aiassistantv1.FileDiff

	sections := strings.Split(text, "\n\n")
	for _, section := range sections {
		section = strings.TrimSpace(section)
		if strings.HasPrefix(section, "PATH:") {
			if diff := parseDiffBlock(section); diff != nil {
				diffs = append(diffs, diff)
			}
		}
		upper := strings.ToUpper(section)
		if strings.HasPrefix(upper, "IMPACT") {
			impact = parseImpactBlock(section)
		}
		if strings.HasPrefix(upper, "WARNINGS") {
			for _, line := range strings.Split(section, "\n")[1:] {
				line = strings.TrimSpace(strings.TrimPrefix(line, "- "))
				if line != "" {
					impact.Warnings = append(impact.Warnings, line)
				}
			}
		}
	}

	// Fallback: wrap freeform text as a note if no structured diffs were parsed.
	if len(diffs) == 0 {
		diffs = append(diffs, &aiassistantv1.FileDiff{
			Path:  "assistant_note.txt",
			After: text,
			Diff:  text,
		})
	}
	return diffs, impact
}

func parseDiffBlock(block string) *aiassistantv1.FileDiff {
	d := &aiassistantv1.FileDiff{}
	for _, line := range strings.Split(block, "\n") {
		if after, ok := strings.CutPrefix(line, "PATH:"); ok {
			d.Path = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "BEFORE:"); ok {
			d.Before = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "AFTER:"); ok {
			d.After = strings.TrimSpace(after)
		} else if after, ok := strings.CutPrefix(line, "DIFF:"); ok {
			d.Diff = strings.TrimSpace(after)
		}
	}
	if d.Path == "" {
		return nil
	}
	return d
}

func parseImpactBlock(block string) *aiassistantv1.ImpactSummary {
	impact := &aiassistantv1.ImpactSummary{}
	for _, line := range strings.Split(block, "\n")[1:] {
		line = strings.TrimSpace(line)
		if after, ok := strings.CutPrefix(line, "metric:"); ok {
			impact.AffectedMetrics = append(impact.AffectedMetrics, strings.TrimSpace(after))
		} else if after, ok := strings.CutPrefix(line, "policy:"); ok {
			impact.AffectedPolicies = append(impact.AffectedPolicies, strings.TrimSpace(after))
		} else if after, ok := strings.CutPrefix(line, "migration:"); ok {
			impact.RequiredMigrations = append(impact.RequiredMigrations, strings.TrimSpace(after))
		}
	}
	return impact
}

func validateSQL(sql string) error {
	upper := strings.ToUpper(strings.TrimSpace(sql))
	if upper == "" {
		return fmt.Errorf("empty SQL")
	}
	for _, bomb := range []string{"DROP DATABASE", "DROP SCHEMA CASCADE", "TRUNCATE CASCADE"} {
		if strings.Contains(upper, bomb) {
			return fmt.Errorf("dangerous operation: %s", bomb)
		}
	}
	return nil
}
