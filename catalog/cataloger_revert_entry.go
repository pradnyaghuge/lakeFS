package catalog

import (
	"context"

	"github.com/treeverse/lakefs/db"
)

func (c *cataloger) RevertEntry(ctx context.Context, repository string, branch string, path string) error {
	if err := Validate(ValidateFields{
		"repository": ValidateRepoName(repository),
		"branch":     ValidateBranchName(branch),
		"path":       ValidatePath(path),
	}); err != nil {
		return err
	}
	_, err := c.db.Transact(func(tx db.Tx) (interface{}, error) {
		branchID, err := getBranchID(tx, repository, branch, LockTypeShare)
		if err != nil {
			return nil, err
		}
		res, err := tx.Exec(`DELETE FROM entries WHERE branch_id = $1 AND path = $2 AND min_commit = 0`, branchID, path)
		if err != nil {
			return nil, err
		}
		if affected, err := res.RowsAffected(); err != nil {
			return nil, err
		} else if affected != 1 {
			return nil, ErrEntryNotFound
		}
		return nil, nil
	}, c.txOpts(ctx)...)
	return err
}