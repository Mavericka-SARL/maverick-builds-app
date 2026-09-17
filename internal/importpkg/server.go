package importpkg

import (
	"bytes"
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/rs/zerolog"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	importpkgv1 "github.com/mavericks-engine/mavericks/gen/go/importpkg/v1"
	"github.com/mavericks-engine/mavericks/pkg/auth"
)

type Server struct {
	importpkgv1.UnimplementedImportServiceServer
	log       zerolog.Logger
	store     *Store
	publisher *Publisher // nil when NATS unavailable
}

func NewServer(log zerolog.Logger, store *Store, pub *Publisher) *Server {
	return &Server{log: log, store: store, publisher: pub}
}

func (s *Server) CreateImportJob(ctx context.Context, req *importpkgv1.CreateImportJobRequest) (*importpkgv1.CreateImportJobResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	userID := callerUserID(ctx)
	job, err := s.store.CreateImportJob(ctx, req.ModelId, req.RevisionId, req.FileUrl, userID, req.Mappings)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &importpkgv1.CreateImportJobResponse{Job: job}, nil
}

func (s *Server) GetImportJob(ctx context.Context, req *importpkgv1.GetImportJobRequest) (*importpkgv1.GetImportJobResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	job, err := s.store.GetImportJob(ctx, req.JobId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}
	return &importpkgv1.GetImportJobResponse{Job: job}, nil
}

func (s *Server) ListImportJobs(ctx context.Context, req *importpkgv1.ListImportJobsRequest) (*importpkgv1.ListImportJobsResponse, error) {
	if req.ModelId == "" {
		return nil, status.Error(codes.InvalidArgument, "model_id is required")
	}
	limit := req.PageSize
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	jobs, err := s.store.ListImportJobs(ctx, req.ModelId, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &importpkgv1.ListImportJobsResponse{Jobs: jobs}, nil
}

// ValidateImport parses csv_content, validates each row against the model schema,
// writes valid rows to import.import_staging, and transitions the job to staged/failed.
//
// Expected CSV format: first row is a header; required columns are "metric_id" and "value";
// any additional columns are treated as dimension member keys.
func (s *Server) ValidateImport(ctx context.Context, req *importpkgv1.ValidateImportRequest) (*importpkgv1.ValidateImportResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	if len(req.CsvContent) == 0 {
		return nil, status.Error(codes.InvalidArgument, "csv_content is required")
	}

	r := csv.NewReader(bytes.NewReader(req.CsvContent))
	r.TrimLeadingSpace = true

	header, err := r.Read()
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "read csv header: %v", err)
	}

	colIdx := make(map[string]int, len(header))
	for i, h := range header {
		colIdx[strings.ToLower(strings.TrimSpace(h))] = i
	}

	metricCol, hasMetric := colIdx["metric_id"]
	valueCol, hasValue := colIdx["value"]
	if !hasMetric || !hasValue {
		return nil, status.Error(codes.InvalidArgument, "csv must contain 'metric_id' and 'value' columns")
	}

	// Determine dimension columns (all columns that are not metric_id or value)
	dimCols := make(map[string]int)
	for col, idx := range colIdx {
		if col != "metric_id" && col != "value" {
			dimCols[col] = idx
		}
	}

	var staged []StagingRow
	var importErrs []*importpkgv1.ImportError
	rowNum := 0

	for {
		record, err := r.Read()
		if err != nil {
			break
		}
		rowNum++

		metricID := strings.TrimSpace(record[metricCol])
		rawVal := strings.TrimSpace(record[valueCol])

		if metricID == "" {
			importErrs = append(importErrs, &importpkgv1.ImportError{
				RowNumber: int32(rowNum), Column: "metric_id",
				ErrorCode: "MISSING_VALUE", Message: "metric_id is required",
			})
			continue
		}

		val, err := strconv.ParseFloat(rawVal, 64)
		if err != nil {
			importErrs = append(importErrs, &importpkgv1.ImportError{
				RowNumber: int32(rowNum), Column: "value",
				ErrorCode: "INVALID_NUMBER", Message: fmt.Sprintf("cannot parse %q as number", rawVal),
				RawValue: rawVal,
			})
			continue
		}

		dims := make(map[string]string, len(dimCols))
		for col, idx := range dimCols {
			dims[col] = strings.TrimSpace(record[idx])
		}

		staged = append(staged, StagingRow{
			MetricID:   metricID,
			DimMembers: dims,
			Value:      val,
			RowNumber:  rowNum,
		})
	}

	if err := s.store.StageRows(ctx, req.JobId, staged, importErrs); err != nil {
		s.log.Error().Err(err).Str("job_id", req.JobId).Msg("stage rows")
		return nil, status.Error(codes.Internal, err.Error())
	}

	finalStatus := importpkgv1.ImportStatus_IMPORT_STATUS_STAGED
	if len(staged) == 0 {
		finalStatus = importpkgv1.ImportStatus_IMPORT_STATUS_FAILED
	}

	return &importpkgv1.ValidateImportResponse{
		TotalRows: int32(len(staged) + len(importErrs)),
		ValidRows: int32(len(staged)),
		ErrorRows: int32(len(importErrs)),
		Status:    finalStatus,
	}, nil
}

func (s *Server) CommitImport(ctx context.Context, req *importpkgv1.CommitImportRequest) (*importpkgv1.CommitImportResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}

	job, err := s.store.GetImportJob(ctx, req.JobId)
	if err != nil {
		return nil, status.Error(codes.NotFound, err.Error())
	}

	userID := callerUserID(ctx)
	metricIDs, err := s.store.CommitImport(ctx, req.JobId, job.ModelId, job.RevisionId, userID, ModeIncremental)
	if err != nil {
		if errors.Is(err, ErrWriteDenied) {
			return &importpkgv1.CommitImportResponse{Success: false}, status.Error(codes.PermissionDenied, err.Error())
		}
		return &importpkgv1.CommitImportResponse{Success: false}, status.Error(codes.Internal, err.Error())
	}

	if s.publisher != nil && len(metricIDs) > 0 {
		evt := FactsCommittedEvent{
			ModelID:    job.ModelId,
			RevisionID: job.RevisionId,
			MetricIDs:  metricIDs,
			UserID:     userID,
		}
		if err := s.publisher.PublishFactsCommitted(ctx, evt); err != nil {
			s.log.Warn().Err(err).Str("job_id", req.JobId).Msg("failed to publish facts.committed; recalc may be delayed")
		}
	}

	return &importpkgv1.CommitImportResponse{
		Success:     true,
		RecalcJobId: fmt.Sprintf("import-recalc-%s", req.JobId),
	}, nil
}

func (s *Server) AbortImport(ctx context.Context, req *importpkgv1.AbortImportRequest) (*importpkgv1.AbortImportResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	if err := s.store.AbortImport(ctx, req.JobId); err != nil {
		return &importpkgv1.AbortImportResponse{Success: false}, status.Error(codes.Internal, err.Error())
	}
	return &importpkgv1.AbortImportResponse{Success: true}, nil
}

func (s *Server) GetImportErrors(ctx context.Context, req *importpkgv1.GetImportErrorsRequest) (*importpkgv1.GetImportErrorsResponse, error) {
	if req.JobId == "" {
		return nil, status.Error(codes.InvalidArgument, "job_id is required")
	}
	limit := req.PageSize
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	errs, err := s.store.GetImportErrors(ctx, req.JobId, limit)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	return &importpkgv1.GetImportErrorsResponse{Errors: errs}, nil
}

func callerUserID(ctx context.Context) string {
	if actor, err := auth.ActorFromContext(ctx); err == nil {
		return actor.UserId
	}
	return "00000000-0000-0000-0000-000000000000"
}
