package repository

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Connecting a paid verifier has to reopen the addresses the built-in check
// could not resolve. Without that, a workspace that connects one because the
// in-house probe returned `unknown` waits out the 30-day shelf life before a
// single credit is spent, and the connection card keeps saying degraded from
// whatever the provider answered on the first pass.
//
// Run against the dev stack:
//
//	WARMBLY_TEST_DB=postgres://warmbly:warmbly@localhost:15432/warmbly_dev?sslmode=disable \
//	  go test ./internal/repository/ -run LiveVerificationReopen -v
func TestLiveVerificationReopenOnProviderConnect(t *testing.T) {
	handle, pool := liveContactDB(t)
	f := newSharedOrgFixture(t, pool)
	ctx := context.Background()
	repo := &contactRepository{DB: handle}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("fixture: %v", err)
		}
	}

	// Checked yesterday by the built-in probe and left unknown: far inside the
	// 30-day window, so nothing but a connected verifier makes it a candidate.
	stale := uuid.New()
	exec(`INSERT INTO contacts (id, user_id, organization_id, email, first_name, last_name, company, phone,
	          custom_fields, updated_at, created_at,
	          verification_status, verification_provider, verification_source, verification_checked_at)
	      VALUES ($1, $2, $3, $4, 'Ada', 'Ng', '', '', '{}'::jsonb, NOW(), NOW(),
	          'unknown', 'builtin', 'probe', NOW() - INTERVAL '1 day')`,
		stale, f.owner, f.org, "reopen-"+stale.String()[:8]+"@test.local")

	candidates := func() map[uuid.UUID]bool {
		t.Helper()
		got, xerr := repo.ListVerificationCandidates(ctx, 500)
		if xerr != nil {
			t.Fatalf("list candidates: %v", xerr)
		}
		out := map[uuid.UUID]bool{}
		for _, c := range got {
			out[c.ID] = true
		}
		return out
	}

	if candidates()[stale] {
		t.Fatal("a fresh built-in verdict is a candidate with no verifier connected")
	}

	conn := uuid.New()
	exec(`INSERT INTO integration_connections (id, organization_id, provider, status, health, created_at, updated_at)
	      VALUES ($1, $2, 'millionverifier', 'connected', 'healthy', NOW(), NOW())`, conn, f.org)

	if !candidates()[stale] {
		t.Fatal("connecting a verifier did not reopen the built-in unknown verdict")
	}

	// One-shot: a verdict reached after the connection is not reopened again,
	// so a provider that keeps answering unknown cannot loop every pass.
	exec(`UPDATE contacts SET verification_checked_at = NOW() WHERE id = $1`, stale)
	if candidates()[stale] {
		t.Fatal("a verdict reached after the connection was reopened again")
	}

	// A disconnected verifier is not a reason to re-check anything.
	exec(`UPDATE contacts SET verification_checked_at = NOW() - INTERVAL '1 day' WHERE id = $1`, stale)
	exec(`UPDATE integration_connections SET status = 'disconnected' WHERE id = $1`, conn)
	if candidates()[stale] {
		t.Fatal("a disconnected verifier reopened the verdict")
	}

	t.Cleanup(func() {
		_, _ = pool.Exec(ctx, `DELETE FROM contacts WHERE id = $1`, stale)
		_, _ = pool.Exec(ctx, `DELETE FROM integration_connections WHERE id = $1`, conn)
	})
}
