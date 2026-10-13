package plan

import "context"

// The Community Edition's 100 MB, and the description and note that quote
// it, are the hosted Playground's allowance (PLAYGROUND=true). Migrations
// seed them on every deployment, because a migration cannot tell which one
// is the Playground. Anywhere else the Community Edition is the one a
// company runs itself, which has no such limit: ReleasePlaygroundAllowance
// takes it off there, but only while the row still holds exactly what was
// seeded, so an allowance a platform administrator set under Platform ›
// Editions stays.
const (
	playgroundStorageMB   = 100
	playgroundDescription = "Use the platform for as long as you like, with up to 100 MB of data."
	playgroundNote        = "A Community workspace holds up to 100 MB. To go further, run the platform on your own infrastructure — free for your company's internal use — or get the Commercial or Enterprise Edition."
	// SelfHostedCommunityDescription is the Community Edition's description
	// once the Playground's allowance is released.
	SelfHostedCommunityDescription = "Use the platform for as long as you like."
)

// ReleasePlaygroundAllowance removes the Playground's allowance from the
// Community Edition of a deployment that is not the Playground; released
// says it did. Run at start-up, so it holds from the first sign-in.
func ReleasePlaygroundAllowance(ctx context.Context, db DB) (released bool, err error) {
	tag, err := db.Exec(ctx, `
		UPDATE platform.plan
		SET limits = limits - 'max_storage_mb', description = $3, limit_note = '', updated_at = now()
		WHERE key = 'community' AND limits = jsonb_build_object('max_storage_mb', $4::int)
		  AND description = $1 AND limit_note = $2`,
		playgroundDescription, playgroundNote, SelfHostedCommunityDescription, playgroundStorageMB)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}
