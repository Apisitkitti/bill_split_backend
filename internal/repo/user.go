package repo

import (
	"context"

	"github.com/OatApisit/billsplit-api/internal/model"
)

// UpsertUser records the identity LINE returned, refreshing the display name
// and picture, which change whenever the user edits their LINE profile.
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
