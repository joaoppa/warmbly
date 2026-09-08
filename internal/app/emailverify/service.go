// Package emailverify (app layer) orchestrates pre-send email verification:
// it loads contacts due for a check, picks the verifier for their organization
// (the paid provider connected on the workspace, else the in-house probe),
// persists each verdict, and offers the member-facing actions (re-verify,
// mark deliverable). The SMTP probe inside the in-house verifier dials remote
// MX hosts on :25 and must never run from a worker (a sending IP).
package emailverify

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/rs/zerolog/log"

	"github.com/warmbly/warmbly/internal/config"
	"github.com/warmbly/warmbly/internal/errx"
	"github.com/warmbly/warmbly/internal/models"
	"github.com/warmbly/warmbly/internal/pkg/emailverify"
	"github.com/warmbly/warmbly/internal/repository"
)

// Provider is a paid verification backend bound to one organization.
type Provider struct {
	Name string
	// ConnectionID is the integration connection carrying the key; nil for
	// the instance-wide key an operator configured.
	ConnectionID *uuid.UUID
	Client       *emailverify.MillionVerifier
}

// ProviderSource resolves the paid provider an organization connected.
// Implemented by the integration service.
type ProviderSource interface {
	// VerificationProviderFor returns nil when the org has no usable provider.
	VerificationProviderFor(ctx context.Context, orgID uuid.UUID) (*Provider, error)
	// ReportVerificationProviderError flips the connection's health when the
	// provider rejected the key or ran out of credits.
	ReportVerificationProviderError(ctx context.Context, connectionID uuid.UUID, err error)
	// ClearVerificationProviderError withdraws that error once the provider works.
	ClearVerificationProviderError(ctx context.Context, connectionID uuid.UUID)
}

// Service verifies contact email addresses before they are ever sent to.
type Service interface {
	// VerifyAddress verifies an arbitrary address for an organization without
	// touching the DB, through whichever verifier the org uses.
	VerifyAddress(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result

	// VerifyPending verifies up to `limit` contacts due for a check. Returns
	// the number processed. Driven by the scheduler.
	VerifyPending(ctx context.Context, limit int) (int, *errx.Error)

	// Request applies a member action: queue a re-check, or record a manual
	// verdict.
	Request(ctx context.Context, orgID uuid.UUID, req models.ContactVerificationRequest) (*models.ContactVerificationResponse, *errx.Error)

	// Overview reports which verifier the org uses, its credits, and the
	// org's contacts by verdict.
	Overview(ctx context.Context, orgID uuid.UUID) (*models.VerificationOverview, *errx.Error)

	// Kick wakes the scheduler for an immediate pass (after a re-verify
	// request or an import), instead of waiting for the next interval.
	Kick()
	// Wake is the channel the scheduler selects on.
	Wake() <-chan struct{}

	// SetVerdictHook registers a callback run once per organization after a
	// pass that changed its contacts, e.g. to resume a campaign that was
	// parked waiting for verification.
	SetVerdictHook(fn func(ctx context.Context, orgID uuid.UUID))

	// SetEvidence attaches the evidence ledger, so a check never overrides
	// what real mail has shown.
	SetEvidence(e *Evidence)
	// Explain builds the contact's verification detail for the drawer.
	Explain(ctx context.Context, contactID uuid.UUID) *models.ContactVerificationDetail
}

type service struct {
	repo      repository.ContactRepository
	builtin   emailverify.Verifier
	providers ProviderSource
	// platform is the instance-wide paid client an operator configured for
	// workspaces that bring no key of their own; nil when unset.
	platform     *emailverify.MillionVerifier
	builtinReady bool

	wake chan struct{}
	hook func(ctx context.Context, orgID uuid.UUID)

	evidence *Evidence

	// breakers are per organization: one tenant's list must not decide
	// whether another tenant's rejections are trusted.
	breakersMu sync.Mutex
	breakers   map[uuid.UUID]*breaker

	creditsMu sync.Mutex
	credits   map[string]creditsEntry
}

type creditsEntry struct {
	n   int
	err error
	at  time.Time
}

// Options configures the service.
type Options struct {
	// Builtin is the in-house verifier. Required.
	Builtin emailverify.Verifier
	// BuiltinReady reports whether Builtin can run its SMTP probe (a public
	// HELO host is configured). Shown to members so a self-hosted instance
	// without one knows why every verdict is "unknown".
	BuiltinReady bool
	// Providers resolves per-org paid providers. Optional.
	Providers ProviderSource
	// PlatformMillionVerifierKey is the operator's own key, used for every
	// workspace without a key of its own. Optional.
	PlatformMillionVerifierKey string
}

// NewService wires the verification service.
func NewService(repo repository.ContactRepository, opts Options) Service {
	s := &service{
		repo:         repo,
		builtin:      opts.Builtin,
		builtinReady: opts.BuiltinReady,
		providers:    opts.Providers,
		wake:         make(chan struct{}, 1),
		breakers:     map[uuid.UUID]*breaker{},
		credits:      map[string]creditsEntry{},
	}
	if k := strings.TrimSpace(opts.PlatformMillionVerifierKey); k != "" {
		s.platform = emailverify.NewMillionVerifier(k, "")
	}
	return s
}

func (s *service) Kick() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

func (s *service) Wake() <-chan struct{} { return s.wake }

func (s *service) SetVerdictHook(fn func(ctx context.Context, orgID uuid.UUID)) { s.hook = fn }

func (s *service) SetEvidence(e *Evidence) { s.evidence = e }

func (s *service) Explain(ctx context.Context, contactID uuid.UUID) *models.ContactVerificationDetail {
	return s.evidence.Explain(ctx, contactID)
}

func (s *service) breakerFor(orgID uuid.UUID) *breaker {
	s.breakersMu.Lock()
	defer s.breakersMu.Unlock()
	b, ok := s.breakers[orgID]
	if !ok {
		b = newBreaker(config.VerificationBreakerWindow, config.VerificationBreakerInvalidPct, time.Duration(config.VerificationBreakerCooldownMinutes)*time.Minute)
		s.breakers[orgID] = b
	}
	return b
}

// providerFor resolves the org's paid provider: its own connection first,
// then the operator's instance-wide key.
func (s *service) providerFor(ctx context.Context, orgID uuid.UUID) *Provider {
	if s.providers != nil {
		p, err := s.providers.VerificationProviderFor(ctx, orgID)
		if err != nil {
			log.Warn().Err(err).Str("organization_id", orgID.String()).Msg("verification: could not resolve provider; using built-in")
		} else if p != nil {
			return p
		}
	}
	if s.platform != nil {
		return &Provider{Name: emailverify.ProviderMillionVerifier, Client: s.platform}
	}
	return nil
}

func (s *service) VerifyAddress(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result {
	if p := s.providerFor(ctx, orgID); p != nil {
		res, err := p.Client.Check(ctx, email)
		if err == nil {
			return res
		}
		s.noteProviderError(ctx, p, err)
	}
	return s.verifyBuiltin(ctx, orgID, email)
}

// verifyBuiltin runs the in-house verifier under the org's self-check breaker.
func (s *service) verifyBuiltin(ctx context.Context, orgID uuid.UUID, email string) emailverify.Result {
	res := s.builtin.Verify(ctx, email)
	// Syntax, no-MX and disposable verdicts do not come from a probe, so they
	// neither feed nor fall under the breaker.
	probeVerdict := res.SubStatus == emailverify.SubStatusNone || res.SubStatus == emailverify.SubStatusCatchAll || res.SubStatus == emailverify.SubStatusRole
	if !probeVerdict {
		return res
	}
	if s.breakerFor(orgID).observe(res.Status == emailverify.StatusInvalid) && res.Status == emailverify.StatusInvalid {
		res.Status = emailverify.StatusUnknown
		res.Reason = "probe rejection not trusted: the in-house check is rejecting an unusual share of addresses and is cooling down (" + res.Reason + ")"
	}
	return res
}

func (s *service) noteProviderError(ctx context.Context, p *Provider, err error) {
	if p == nil || err == nil {
		return
	}
	if errors.Is(err, emailverify.ErrMillionVerifierKey) || errors.Is(err, emailverify.ErrMillionVerifierCredits) {
		if p.ConnectionID != nil && s.providers != nil {
			s.providers.ReportVerificationProviderError(ctx, *p.ConnectionID, err)
		}
		s.creditsMu.Lock()
		s.credits[cacheKey(p)] = creditsEntry{err: err, at: time.Now()}
		s.creditsMu.Unlock()
	}
}

func cacheKey(p *Provider) string {
	if p.ConnectionID != nil {
		return p.ConnectionID.String()
	}
	return "platform"
}

// providerUsable checks (cached for a minute) that the provider's key works
// and has credits, so a pass never burns a whole batch on a dead key.
func (s *service) providerUsable(ctx context.Context, p *Provider) (int, error) {
	key := cacheKey(p)
	s.creditsMu.Lock()
	e, ok := s.credits[key]
	s.creditsMu.Unlock()
	if ok && time.Since(e.at) < time.Minute {
		return e.n, e.err
	}
	n, err := p.Client.Credits(ctx)
	if err == nil && n <= 0 {
		err = emailverify.ErrMillionVerifierCredits
	}
	s.creditsMu.Lock()
	s.credits[key] = creditsEntry{n: n, err: err, at: time.Now()}
	s.creditsMu.Unlock()
	if err != nil {
		s.noteProviderError(ctx, p, err)
	} else {
		s.noteProviderOK(ctx, p)
	}
	return n, err
}

// noteProviderOK withdraws a recorded provider error once the key answers with
// a balance again. The write is guarded in the repository, so a pass that finds
// the connection already healthy costs nothing.
func (s *service) noteProviderOK(ctx context.Context, p *Provider) {
	if p == nil || p.ConnectionID == nil || s.providers == nil {
		return
	}
	s.providers.ClearVerificationProviderError(ctx, *p.ConnectionID)
}

func (s *service) VerifyPending(ctx context.Context, limit int) (int, *errx.Error) {
	cands, xerr := s.repo.ListVerificationCandidates(ctx, limit)
	if xerr != nil {
		return 0, xerr
	}
	if len(cands) == 0 {
		return 0, nil
	}

	// Group by organization: the verifier is chosen once per org.
	byOrg := map[uuid.UUID][]repository.VerificationCandidate{}
	var order []uuid.UUID
	for _, c := range cands {
		if _, seen := byOrg[c.OrganizationID]; !seen {
			order = append(order, c.OrganizationID)
		}
		byOrg[c.OrganizationID] = append(byOrg[c.OrganizationID], c)
	}

	processed := 0
	for _, orgID := range order {
		if err := ctx.Err(); err != nil {
			break
		}
		n := s.verifyOrgBatch(ctx, orgID, byOrg[orgID])
		processed += n
		if n > 0 && s.hook != nil {
			s.hook(ctx, orgID)
		}
	}
	return processed, nil
}

// verifyOrgBatch checks one org's candidates with its verifier, in parallel
// up to the verifier's concurrency, and persists every verdict.
func (s *service) verifyOrgBatch(ctx context.Context, orgID uuid.UUID, cands []repository.VerificationCandidate) int {
	verify := func(ctx context.Context, email string) emailverify.Result { return s.verifyBuiltin(ctx, orgID, email) }
	workers := config.VerificationProbeConcurrency
	if p := s.providerFor(ctx, orgID); p != nil {
		if _, err := s.providerUsable(ctx, p); err != nil {
			log.Warn().Err(err).Str("organization_id", orgID.String()).Msg("verification: paid provider unusable; using built-in check")
		} else {
			workers = config.VerificationProviderConcurrency
			verify = func(ctx context.Context, email string) emailverify.Result {
				res, err := p.Client.Check(ctx, email)
				if err != nil {
					s.noteProviderError(ctx, p, err)
					// Fall back for this address so the pass still makes progress.
					return s.verifyBuiltin(ctx, orgID, email)
				}
				return res
			}
		}
	}

	var (
		wg        sync.WaitGroup
		mu        sync.Mutex
		processed int
		sem       = make(chan struct{}, workers)
	)
	for _, c := range cands {
		if err := ctx.Err(); err != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(c repository.VerificationCandidate) {
			defer wg.Done()
			defer func() { <-sem }()
			// What real mail showed outranks what the check says.
			res := s.evidence.Apply(ctx, c.ID, verify(ctx, c.Email))
			if xerr := s.repo.UpdateContactVerification(ctx, c.ID, res); xerr != nil {
				// Skip this one; a transient DB error shouldn't abort the whole pass.
				return
			}
			mu.Lock()
			processed++
			mu.Unlock()
		}(c)
	}
	wg.Wait()
	return processed
}

func (s *service) Request(ctx context.Context, orgID uuid.UUID, req models.ContactVerificationRequest) (*models.ContactVerificationResponse, *errx.Error) {
	var ids []uuid.UUID
	for _, raw := range req.Contacts {
		id, err := uuid.Parse(strings.TrimSpace(raw))
		if err != nil {
			return nil, errx.ErrUuid
		}
		ids = append(ids, id)
	}
	if req.CampaignID != "" {
		cid, err := uuid.Parse(strings.TrimSpace(req.CampaignID))
		if err != nil {
			return nil, errx.ErrUuid
		}
		more, xerr := s.repo.UndeliverableLeadIDs(ctx, orgID, cid)
		if xerr != nil {
			return nil, xerr
		}
		ids = append(ids, more...)
	}
	if len(ids) == 0 {
		return nil, errx.NewWithIdentifier(errx.BadRequest, "no_contacts", "no contacts selected")
	}

	resp := &models.ContactVerificationResponse{Action: req.Action}
	switch req.Action {
	case models.ContactVerificationActionVerify:
		n, xerr := s.repo.ResetContactsVerification(ctx, orgID, ids)
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected, resp.Queued = n, true
		s.Kick()
	case models.ContactVerificationActionMarkDeliverable:
		n, xerr := s.repo.SetContactsVerification(ctx, orgID, ids, models.ContactVerificationWrite{
			Status:   string(emailverify.StatusValid),
			Reason:   "marked deliverable by a member",
			Provider: "manual",
			Source:   models.VerificationSourceManual,
		})
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected = n
		if s.hook != nil {
			s.hook(ctx, orgID)
		}
	case models.ContactVerificationActionMarkUndeliverable:
		n, xerr := s.repo.SetContactsVerification(ctx, orgID, ids, models.ContactVerificationWrite{
			Status:   string(emailverify.StatusInvalid),
			Reason:   "marked undeliverable by a member",
			Provider: "manual",
			Source:   models.VerificationSourceManual,
		})
		if xerr != nil {
			return nil, xerr
		}
		resp.Affected = n
	default:
		return nil, errx.NewWithIdentifier(errx.BadRequest, "invalid_action", "action must be verify, mark_deliverable or mark_undeliverable")
	}
	return resp, nil
}

func (s *service) Overview(ctx context.Context, orgID uuid.UUID) (*models.VerificationOverview, *errx.Error) {
	counts, xerr := s.repo.VerificationCounts(ctx, orgID)
	if xerr != nil {
		return nil, xerr
	}
	out := &models.VerificationOverview{
		Provider:     emailverify.ProviderBuiltin,
		BuiltinReady: s.builtinReady,
		Counts:       counts,
	}
	if p := s.providerFor(ctx, orgID); p != nil {
		if p.ConnectionID != nil {
			id := p.ConnectionID.String()
			out.ConnectionID = &id
		}
		n, err := s.providerUsable(ctx, p)
		if err != nil {
			out.ProviderError = providerErrorText(err)
		} else {
			out.Provider = p.Name
			out.Credits = &n
		}
	}
	return out, nil
}

func providerErrorText(err error) string {
	switch {
	case errors.Is(err, emailverify.ErrMillionVerifierKey):
		return "MillionVerifier rejected the API key. Reconnect it with a current key."
	case errors.Is(err, emailverify.ErrMillionVerifierCredits):
		return "The MillionVerifier account has no credits left. Top it up to keep using it; the built-in check is used meanwhile."
	default:
		return "MillionVerifier could not be reached; the built-in check is used meanwhile."
	}
}
