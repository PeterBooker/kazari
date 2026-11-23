package sqlite

import (
	"context"

	"github.com/PeterBooker/kazari/internal/dbtemplate"
)

// EmptyDB is a database at the latest migration, migrated once for the
// package's tests and copied for each.
var EmptyDB = dbtemplate.New(func(ctx context.Context, path string) error {
	db, err := Open(ctx, path, Options{})
	if err != nil {
		return err
	}
	return db.Close()
})

const UnreferencedBlobsQuery = unreferencedBlobs
