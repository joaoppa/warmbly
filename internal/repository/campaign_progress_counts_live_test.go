package repository

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// GetCampaignProgress joined leads, steps and progress rows side by side on
// campaign_id, which multiplied every progress count by leads x steps. One sent
// email on a campaign with 3 leads and 2 steps read as 6 sent. The counts stay
// proportional, so the dashboard merely looked wrong, but the bounce breaker
// reads the same numbers and gates on a minimum sample: an inflated count walks
// straight past it, and a campaign pauses itself on its first bounce.
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_test?sslmode=disable \
//	  go test ./internal/repository/ -run LiveCampaignProgressCounts -v
func TestLiveCampaignProgressCounts(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	ctx := context.Background()
	repo := &campaignProgressRepository{db: handle.Pool}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	// Two email steps on the campaign.
	steps := []uuid.UUID{uuid.New(), uuid.New()}
	for i, id := range steps {
		exec(`INSERT INTO sequences (id, campaign_id, organization_id, name, subject, body_plain, body_html, kind, position, created_at, updated_at)
		      VALUES ($1, $2, $3, 'Step', 'Subject', 'plain', '<p>html</p>', 'email', $4, NOW(), NOW())`,
			id, f.campaign, f.org, i)
	}

	// Three leads, so a cartesian product would multiply by six.
	contacts := []uuid.UUID{uuid.New(), uuid.New(), uuid.New()}
	for i, id := range contacts {
		exec(`INSERT INTO contacts (id, user_id, organization_id, email, first_name, last_name, company, phone, custom_fields, updated_at, created_at)
		      VALUES ($1, $2, $3, $4, 'Lead', 'Live', '', '', '{}'::jsonb, NOW(), NOW())`,
			id, f.owner, f.org, "prog-"+id.String()[:8]+"@test.local")
		exec(`INSERT INTO campaign_leads (campaign_id, contact_id) VALUES ($1, $2)`, f.campaign, id)
		_ = i
	}

	// Exactly one sent email, which bounced.
	exec(`INSERT INTO campaign_contact_progress (campaign_id, contact_id, sequence_id, sent_at, bounced_at)
	      VALUES ($1, $2, $3, NOW(), NOW())`, f.campaign, contacts[0], steps[0])

	got, err := repo.GetCampaignProgress(ctx, f.campaign)
	if err != nil {
		t.Fatalf("GetCampaignProgress: %v", err)
	}

	if got.EmailsSent != 1 {
		t.Errorf("emails_sent = %d, want 1 (leads x steps would give 6)", got.EmailsSent)
	}
	if got.EmailsBounced != 1 {
		t.Errorf("emails_bounced = %d, want 1", got.EmailsBounced)
	}
	if got.TotalContacts != 3 {
		t.Errorf("total_contacts = %d, want 3", got.TotalContacts)
	}
	if got.TotalSequences != 2 {
		t.Errorf("total_sequences = %d, want 2", got.TotalSequences)
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM campaign_contact_progress WHERE campaign_id = $1`, f.campaign)
		_, _ = pool.Exec(ctx, `DELETE FROM campaign_leads WHERE campaign_id = $1`, f.campaign)
		_, _ = pool.Exec(ctx, `DELETE FROM sequences WHERE campaign_id = $1`, f.campaign)
		for _, id := range contacts {
			_, _ = pool.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, id)
		}
	})
}
