package repo

import (
	"context"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// UpsertUser records the identity LINE returned, refreshing the display name
// and picture, which change whenever the user edits their LINE profile.
//
// Like AddMember it stays a bare statement rather than moving into a transaction
// for the sake of poolTx.Commit's detached commit: writing the same profile twice
// is the same profile, so a deadline in the implicit commit costs nothing a retry
// does not fix. It also runs on every authenticated request, where two extra
// round trips would be paid by every caller to protect a write that cannot be
// double-counted.
func (r *Repo) UpsertUser(ctx context.Context, u model.User) error {
	_, err := r.pool.Exec(ctx, `
		INSERT INTO users (line_user_id, display_name, picture_url)
		VALUES ($1, $2, $3)
		ON CONFLICT (line_user_id) DO UPDATE
		SET display_name = EXCLUDED.display_name,
		    picture_url  = EXCLUDED.picture_url,
		    updated_at   = now()`,
		u.ID, u.DisplayName, u.PictureURL)
	return err
}
