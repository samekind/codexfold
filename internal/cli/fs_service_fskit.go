package cli

import "context"

type fsKitAppTransaction interface {
	AppGroupPath() string
	Changed() bool
	Rollback(context.Context) error
	Commit() error
}
