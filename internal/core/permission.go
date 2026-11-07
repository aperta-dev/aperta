package core

import (
	"context"
	"encoding/json"
	"fmt"
)

type PermissionRequest struct {
	Tool     string
	Input    json.RawMessage
	ReadOnly bool
}

type Permissions interface {
	Check(ctx context.Context, req PermissionRequest) error
}

type Policy struct {
	AllowWrites bool
}

func (p Policy) Check(_ context.Context, req PermissionRequest) error {
	if req.ReadOnly || p.AllowWrites {
		return nil
	}
	return fmt.Errorf("permission denied: %s can change files or run commands, and this session only allows read-only tools", req.Tool)
}
