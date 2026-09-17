package tenant

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"google.golang.org/protobuf/types/known/timestamppb"

	tenantv1 "github.com/mavericks-engine/mavericks/gen/go/tenant/v1"
)

type Store struct {
	pool *pgxpool.Pool
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

var ErrNotFound = errors.New("not found")

func (s *Store) CreateCustomer(ctx context.Context, name, plan string) (*tenantv1.Customer, error) {
	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO core.customer (name, plan) VALUES ($1, $2) RETURNING id, created_at`,
		name, plan,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create customer: %w", err)
	}
	return &tenantv1.Customer{Id: id, Name: name, Plan: plan, CreatedAt: timestamppb.New(createdAt)}, nil
}

func (s *Store) GetCustomer(ctx context.Context, customerID string) (*tenantv1.Customer, error) {
	var c tenantv1.Customer
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, name, plan, created_at FROM core.customer WHERE id = $1`,
		customerID,
	).Scan(&c.Id, &c.Name, &c.Plan, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	c.CreatedAt = timestamppb.New(createdAt)
	return &c, nil
}

func (s *Store) CreateWorkspace(ctx context.Context, customerID, name string) (*tenantv1.Workspace, error) {
	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO core.workspace (customer_id, name) VALUES ($1, $2) RETURNING id, created_at`,
		customerID, name,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create workspace: %w", err)
	}
	return &tenantv1.Workspace{
		Id: id, CustomerId: customerID, Name: name,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetWorkspace(ctx context.Context, workspaceID string) (*tenantv1.Workspace, error) {
	var w tenantv1.Workspace
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, customer_id, name, created_at FROM core.workspace WHERE id = $1`,
		workspaceID,
	).Scan(&w.Id, &w.CustomerId, &w.Name, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	w.CreatedAt = timestamppb.New(createdAt)
	return &w, nil
}

func (s *Store) ListWorkspaces(ctx context.Context, customerID string, limit, offset int) ([]*tenantv1.Workspace, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, customer_id, name, created_at FROM core.workspace
		 WHERE customer_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		customerID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var workspaces []*tenantv1.Workspace
	for rows.Next() {
		var w tenantv1.Workspace
		var createdAt time.Time
		if err := rows.Scan(&w.Id, &w.CustomerId, &w.Name, &createdAt); err != nil {
			return nil, err
		}
		w.CreatedAt = timestamppb.New(createdAt)
		workspaces = append(workspaces, &w)
	}
	return workspaces, rows.Err()
}

func (s *Store) CreateApplication(ctx context.Context, workspaceID, name, mode string) (*tenantv1.Application, error) {
	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO core.application (workspace_id, name, mode)
		 VALUES ($1, $2, $3::core.application_mode)
		 RETURNING id, created_at`,
		workspaceID, name, mode,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create application: %w", err)
	}
	return &tenantv1.Application{
		Id: id, WorkspaceId: workspaceID, Name: name,
		Mode:      modeFromString(mode),
		Status:    tenantv1.ApplicationStatus_APPLICATION_STATUS_DRAFT,
		CreatedAt: timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetApplication(ctx context.Context, applicationID string) (*tenantv1.Application, error) {
	var a tenantv1.Application
	var createdAt time.Time
	var modeStr, statusStr string
	err := s.pool.QueryRow(ctx,
		`SELECT id, workspace_id, name, mode, status, created_at FROM core.application WHERE id = $1`,
		applicationID,
	).Scan(&a.Id, &a.WorkspaceId, &a.Name, &modeStr, &statusStr, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.Mode = modeFromString(modeStr)
	a.Status = statusFromString(statusStr)
	a.CreatedAt = timestamppb.New(createdAt)
	return &a, nil
}

func (s *Store) ListApplications(ctx context.Context, workspaceID string, limit, offset int) ([]*tenantv1.Application, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, workspace_id, name, mode, status, created_at FROM core.application
		 WHERE workspace_id = $1 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		workspaceID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var apps []*tenantv1.Application
	for rows.Next() {
		var a tenantv1.Application
		var createdAt time.Time
		var modeStr, statusStr string
		if err := rows.Scan(&a.Id, &a.WorkspaceId, &a.Name, &modeStr, &statusStr, &createdAt); err != nil {
			return nil, err
		}
		a.Mode = modeFromString(modeStr)
		a.Status = statusFromString(statusStr)
		a.CreatedAt = timestamppb.New(createdAt)
		apps = append(apps, &a)
	}
	return apps, rows.Err()
}

func (s *Store) ListCustomers(ctx context.Context, limit, offset int) ([]*tenantv1.Customer, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, name, plan, created_at FROM core.customer ORDER BY created_at DESC LIMIT $1 OFFSET $2`,
		limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var customers []*tenantv1.Customer
	for rows.Next() {
		var c tenantv1.Customer
		var createdAt time.Time
		if err := rows.Scan(&c.Id, &c.Name, &c.Plan, &createdAt); err != nil {
			return nil, err
		}
		c.CreatedAt = timestamppb.New(createdAt)
		customers = append(customers, &c)
	}
	return customers, rows.Err()
}

func (s *Store) UpdateCustomer(ctx context.Context, id, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE core.customer SET name = $2 WHERE id = $1`, id, name)
	return err
}

func (s *Store) DeleteCustomer(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM core.customer WHERE id = $1`, id)
	return err
}

func (s *Store) UpdateWorkspace(ctx context.Context, id, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE core.workspace SET name = $2 WHERE id = $1`, id, name)
	return err
}

func (s *Store) DeleteWorkspace(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM core.workspace WHERE id = $1`, id)
	return err
}

func (s *Store) UpdateApplication(ctx context.Context, id, name string) error {
	_, err := s.pool.Exec(ctx, `UPDATE core.application SET name = $2 WHERE id = $1`, id, name)
	return err
}

func (s *Store) DeleteApplication(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM core.application WHERE id = $1`, id)
	return err
}

func (s *Store) DeleteModel(ctx context.Context, id string) error {
	_, err := s.pool.Exec(ctx, `DELETE FROM core.model WHERE id = $1`, id)
	return err
}

func modeFromString(s string) tenantv1.ApplicationMode {
	switch s {
	case "planning":
		return tenantv1.ApplicationMode_APPLICATION_MODE_PLANNING
	case "crud":
		return tenantv1.ApplicationMode_APPLICATION_MODE_CRUD
	case "execution":
		return tenantv1.ApplicationMode_APPLICATION_MODE_EXECUTION
	default:
		return tenantv1.ApplicationMode_APPLICATION_MODE_UNSPECIFIED
	}
}

func modeToString(m tenantv1.ApplicationMode) string {
	switch m {
	case tenantv1.ApplicationMode_APPLICATION_MODE_PLANNING:
		return "planning"
	case tenantv1.ApplicationMode_APPLICATION_MODE_CRUD:
		return "crud"
	case tenantv1.ApplicationMode_APPLICATION_MODE_EXECUTION:
		return "execution"
	default:
		return "planning"
	}
}

func statusFromString(s string) tenantv1.ApplicationStatus {
	switch s {
	case "published":
		return tenantv1.ApplicationStatus_APPLICATION_STATUS_PUBLISHED
	case "archived":
		return tenantv1.ApplicationStatus_APPLICATION_STATUS_ARCHIVED
	default:
		return tenantv1.ApplicationStatus_APPLICATION_STATUS_DRAFT
	}
}

// ── Model ─────────────────────────────────────────────────────────────────────

func (s *Store) CreateModel(ctx context.Context, applicationID, name, storageType string) (*tenantv1.Model, error) {
	if storageType == "" {
		storageType = "oltp"
	}
	var id string
	var createdAt time.Time
	err := s.pool.QueryRow(ctx,
		`INSERT INTO core.model (application_id, name, storage_type)
		 VALUES ($1, $2, $3::core.storage_type)
		 RETURNING id, created_at`,
		applicationID, name, storageType,
	).Scan(&id, &createdAt)
	if err != nil {
		return nil, fmt.Errorf("create model: %w", err)
	}
	return &tenantv1.Model{
		Id: id, ApplicationId: applicationID, Name: name,
		StorageType: storageType,
		CreatedAt:   timestamppb.New(createdAt),
	}, nil
}

func (s *Store) GetModel(ctx context.Context, modelID string) (*tenantv1.Model, error) {
	var m tenantv1.Model
	var createdAt time.Time
	var storageType string
	err := s.pool.QueryRow(ctx,
		`SELECT id, application_id, name, storage_type::text, created_at
		 FROM core.model WHERE id = $1`,
		modelID,
	).Scan(&m.Id, &m.ApplicationId, &m.Name, &storageType, &createdAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	m.StorageType = storageType
	m.CreatedAt = timestamppb.New(createdAt)
	return &m, nil
}

func (s *Store) ListModels(ctx context.Context, applicationID string, limit, offset int) ([]*tenantv1.Model, error) {
	rows, err := s.pool.Query(ctx,
		`SELECT id, application_id, name, storage_type::text, created_at
		 FROM core.model WHERE application_id = $1
		 ORDER BY created_at DESC LIMIT $2 OFFSET $3`,
		applicationID, limit, offset,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var models []*tenantv1.Model
	for rows.Next() {
		var m tenantv1.Model
		var createdAt time.Time
		var storageType string
		if err := rows.Scan(&m.Id, &m.ApplicationId, &m.Name, &storageType, &createdAt); err != nil {
			return nil, err
		}
		m.StorageType = storageType
		m.CreatedAt = timestamppb.New(createdAt)
		models = append(models, &m)
	}
	return models, rows.Err()
}
