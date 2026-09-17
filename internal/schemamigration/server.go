package schemamigration

import (
	"context"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/timestamppb"

	schemamigrationv1 "github.com/mavericks-engine/mavericks/gen/go/schemamigration/v1"
)

type Server struct {
	schemamigrationv1.UnimplementedSchemaMigrationServiceServer
	log       zerolog.Logger
	store     *Store
	generator *Generator
}

func NewServer(log zerolog.Logger, store *Store, generator *Generator) *Server {
	return &Server{log: log, store: store, generator: generator}
}

func (s *Server) GenerateMigration(ctx context.Context, req *schemamigrationv1.GenerateMigrationRequest) (*schemamigrationv1.GenerateMigrationResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}

	versionNum, err := s.store.NextVersion(ctx, req.ModelId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	files, err := s.generator.Generate(ctx, req.ModelId, versionNum)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	rec, err := s.store.Create(ctx, req.ModelId, req.RevisionId, versionNum, files)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	s.log.Info().
		Str("model_id", req.ModelId).
		Int32("version", versionNum).
		Str("migration_id", rec.ID).
		Msg("migration generated")

	return &schemamigrationv1.GenerateMigrationResponse{
		Migration: recordToProto(rec),
	}, nil
}

func (s *Server) PreviewMigration(ctx context.Context, req *schemamigrationv1.PreviewMigrationRequest) (*schemamigrationv1.PreviewMigrationResponse, error) {
	if req.MigrationId == "" {
		return nil, status.Error(codes.InvalidArgument, "migration_id is required")
	}

	rec, err := s.store.Get(ctx, req.MigrationId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	protoFiles, warnings, isDestructive := analyzeFiles(rec.Files)

	return &schemamigrationv1.PreviewMigrationResponse{
		Files:         protoFiles,
		Warnings:      warnings,
		IsDestructive: isDestructive,
	}, nil
}

func (s *Server) ApplyMigration(ctx context.Context, req *schemamigrationv1.ApplyMigrationRequest) (*schemamigrationv1.ApplyMigrationResponse, error) {
	if req.MigrationId == "" {
		return nil, status.Error(codes.InvalidArgument, "migration_id is required")
	}

	// Guard against applying destructive migrations without explicit confirmation
	rec, err := s.store.Get(ctx, req.MigrationId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	_, _, isDestructive := analyzeFiles(rec.Files)
	if isDestructive && !req.ConfirmedDestructive {
		return nil, status.Error(codes.FailedPrecondition,
			"migration contains destructive operations; set confirmed_destructive=true to proceed")
	}

	if err := s.store.Apply(ctx, req.MigrationId); err != nil {
		return &schemamigrationv1.ApplyMigrationResponse{Success: false, Error: err.Error()}, nil
	}

	s.log.Info().Str("migration_id", req.MigrationId).Msg("migration applied")
	return &schemamigrationv1.ApplyMigrationResponse{Success: true}, nil
}

func (s *Server) RollbackMigration(ctx context.Context, req *schemamigrationv1.RollbackMigrationRequest) (*schemamigrationv1.RollbackMigrationResponse, error) {
	if req.MigrationId == "" {
		return nil, status.Error(codes.InvalidArgument, "migration_id is required")
	}

	if err := s.store.Rollback(ctx, req.MigrationId); err != nil {
		return &schemamigrationv1.RollbackMigrationResponse{Success: false, Error: err.Error()}, nil
	}

	s.log.Info().Str("migration_id", req.MigrationId).Msg("migration rolled back")
	return &schemamigrationv1.RollbackMigrationResponse{Success: true}, nil
}

func (s *Server) ListMigrations(ctx context.Context, req *schemamigrationv1.ListMigrationsRequest) (*schemamigrationv1.ListMigrationsResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}

	records, err := s.store.List(ctx, req.ModelId)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	migrations := make([]*schemamigrationv1.Migration, len(records))
	for i, r := range records {
		migrations[i] = recordToProto(r)
	}
	return &schemamigrationv1.ListMigrationsResponse{Migrations: migrations}, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func analyzeFiles(files []migrationFile) ([]*schemamigrationv1.MigrationFile, []string, bool) {
	var protoFiles []*schemamigrationv1.MigrationFile
	var warnings []string
	isDestructive := false

	for _, f := range files {
		upper := strings.ToUpper(f.SQL)

		if strings.Contains(upper, "DROP TABLE") {
			warnings = append(warnings, f.Filename+": contains DROP TABLE")
			isDestructive = true
		}
		if strings.Contains(upper, "TRUNCATE") {
			warnings = append(warnings, f.Filename+": contains TRUNCATE")
			isDestructive = true
		}
		if strings.Contains(upper, "ALTER TABLE") && strings.Contains(upper, "DROP COLUMN") {
			warnings = append(warnings, f.Filename+": drops a column")
			isDestructive = true
		}

		protoFiles = append(protoFiles, &schemamigrationv1.MigrationFile{
			Filename: f.Filename,
			Sql:      f.SQL,
			Checksum: f.Checksum,
		})
	}
	return protoFiles, warnings, isDestructive
}

func recordToProto(r *migrationRecord) *schemamigrationv1.Migration {
	m := &schemamigrationv1.Migration{
		Id:            r.ID,
		ModelId:       r.ModelID,
		RevisionId:    r.RevisionID,
		VersionNumber: r.VersionNumber,
		Status:        statusToProto(r.Status),
		CreatedAt:     timestamppb.New(r.CreatedAt),
	}
	if r.AppliedAt != nil {
		m.AppliedAt = timestamppb.New(*r.AppliedAt)
	}
	for _, f := range r.Files {
		m.Files = append(m.Files, &schemamigrationv1.MigrationFile{
			Filename: f.Filename,
			Sql:      f.SQL,
			Checksum: f.Checksum,
		})
	}
	return m
}

func statusToProto(s string) schemamigrationv1.MigrationStatus {
	switch s {
	case "pending":
		return schemamigrationv1.MigrationStatus_MIGRATION_STATUS_PENDING
	case "applied":
		return schemamigrationv1.MigrationStatus_MIGRATION_STATUS_APPLIED
	case "rolled_back":
		return schemamigrationv1.MigrationStatus_MIGRATION_STATUS_ROLLED_BACK
	case "failed":
		return schemamigrationv1.MigrationStatus_MIGRATION_STATUS_FAILED
	default:
		return schemamigrationv1.MigrationStatus_MIGRATION_STATUS_UNSPECIFIED
	}
}
