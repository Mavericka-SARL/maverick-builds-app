package gateway

import (
	"context"
	"errors"
)

// TenantDeleter lets a background job delete a tenant through the handler's
// one deletion path (deleteTenant), the way DELETE /api/admin/tenants/{id}
// does — internal/dormancy retires unused workspaces with it.
type TenantDeleter struct{ h *handler }

var errNoHandler = errors.New("tenant deleter is not attached to a handler")

// NewTenantDeleter returns a deleter for Deps.TenantDeleter.
func NewTenantDeleter() *TenantDeleter { return &TenantDeleter{} }

// Delete removes the tenant as the system, recording reason with the audit
// events.
func (d *TenantDeleter) Delete(ctx context.Context, customerID, reason string) error {
	if d == nil || d.h == nil {
		return errNoHandler
	}
	_, err := d.h.deleteTenant(ctx, customerID, "", "", "system", reason)
	return err
}
