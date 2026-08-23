# Known Bugs — Warmbly Fork

This file temporarily tracks confirmed bugs found during the 2026-08-23 technical audit.

> GitHub Issues is currently disabled in this fork, so the bugs below are being recorded here until Issues is enabled. They should later be converted into individual issues.

Audited commit: `ed9bbde210b60eb339206c46fcdae80a2bea1757`

---

## BUG-001 — Campaign/mailbox execution is scoped by `user_id` instead of `organization_id`

**Severity:** BLOCKER for multi-tenant production

### Problem

The campaign send path resolves mailboxes using the campaign owner's `user_id` instead of the campaign's `organization_id`.

When the same user administers multiple organizations, a campaign from organization A can see/select mailboxes from organization B.

### Evidence

The scheduler calls repository methods with `campaign.UserID`:

- `internal/scheduler/campaign_scheduler.go` — `GetByCampaignSenders`
- `internal/scheduler/campaign_scheduler.go` — `GetByTags`
- `internal/scheduler/campaign_scheduler.go` — `GetAllActiveByUser`

The corresponding queries in `internal/repository/pg_email.go` filter by `ea.user_id` without constraining the mailbox to the campaign organization.

Related logic found with the same tenant-scoping risk:

- `PauseAllByUserID`
- `ValidateCampaignReady`
- `AccountHasActiveCampaign`
- `CountActiveCampaignsForAccount`

There is also an inconsistency where explicit sender assignment can be validated by organization, while the send path later reads by user.

### Reproduction

The audit reproduced this inside a transaction with rollback:

1. Create organization A and organization B under the same user.
2. Place mailbox A in organization A and mailbox B in organization B.
3. Run the exact mailbox query used by the scheduler for a campaign from organization A.
4. The query returns mailbox B as eligible.

No test data remained after rollback.

### Expected behavior

A campaign from organization A must never list, select, count, validate, pause, or use mailboxes/campaigns from organization B, even when the same user belongs to both.

### Recommended fix

- Audit the full campaign/mailbox path for legacy `user_id` tenant scoping.
- Use `organization_id` for tenant boundaries.
- Keep `user_id` only for actor/authorship semantics.
- Cover explicit senders, tag-resolved senders, fallback/all senders, validation, counts and pause logic.

### Required regression test

One user, two organizations, one mailbox in each, campaign only in organization A. Assert that organization B resources are never visible or actionable from A.

---

## BUG-002 — Core tenant entities still allow incomplete organization scoping

**Severity:** GRAVE

### Problem

The schema still contains remnants of the older user-scoped model:

- contact email uniqueness is enforced by `(user_id, lower(email))`, not organization;
- `organization_id` remains nullable in core entities such as contacts/campaigns/sequences/email accounts;
- suppression logic can be skipped when a campaign has `organization_id = NULL`.

### Impact

1. Two organizations administered by the same user cannot independently contain the same contact email.
2. Tenant invariants are not enforced at the database level.
3. A malformed/legacy campaign with no organization can bypass organization-scoped suppression checks.

### Evidence

Contact uniqueness:

```sql
CREATE UNIQUE INDEX idx_contacts_user_email_unique
  ON contacts (user_id, lower(email));
```

The audit also identified nullable `organization_id` columns in core commercial entities and a suppression gate conditional on a non-null campaign organization.

### Expected behavior

- Contact identity must be unique within an organization, not globally per user.
- Core tenant-owned records must always belong to an organization.
- Suppression must fail closed; absence of tenant context must never silently bypass compliance checks.

### Recommended fix

- Replace the contact uniqueness rule with `(organization_id, lower(email))`.
- Backfill and make `organization_id NOT NULL` where appropriate, at minimum for:
  - contacts
  - campaigns
  - sequences
  - email_accounts
- Make suppression logic fail safely if organization context is unexpectedly missing.

### Required regression tests

- Same email can exist once in organization A and once in organization B.
- Duplicate email within the same organization is rejected.
- No core tenant entity can be created without an organization.
- Suppression cannot be bypassed by missing organization context.

---

## BUG-003 — Possible duplicate campaign email after crash between SMTP acceptance and progress persistence

**Severity:** GRAVE / reliability

### Problem

The campaign send flow records `sent_at` only after the provider has accepted the message.

If the process crashes after SMTP acceptance but before `RecordEmailSent` is persisted, the reconciler can later select the same contact/step again and resend it.

### Evidence

In `internal/tasks/campaign_task.go`, message metadata/status is stored and then campaign progress is updated through `RecordEmailSent`.

If `RecordEmailSent` fails, the current behavior logs a warning rather than failing the operation:

```text
Failed to record email sent
```

The audit did not find a pre-send reservation/claim for the `(campaign_id, contact_id, sequence_id)` pair in the campaign path.

### Failure scenario

```text
SMTP accepts message
       ↓
process/container crashes
       ↓
progress not persisted
       ↓
reconciler selects the same pair again
       ↓
duplicate email
```

### Expected behavior

A campaign step must have crash-safe at-most-once semantics from the recipient's perspective, or an explicit durable reservation/outbox mechanism that prevents a completed provider send from being repeated after recovery.

### Recommended direction

Do not patch this casually. This is in the transactional core of sending.

Investigate using a durable pre-send reservation/execution key/outbox state, potentially reusing `task_execution_keys`, with clear handling for:

- reserved
- dispatched/unknown outcome
- confirmed sent
- retryable failure

### Interim operational control

Alert on occurrences of:

```text
Failed to record email sent
```

and avoid unnecessary backend restarts during active campaign delivery until the semantics are corrected.

---

## Non-bugs intentionally NOT tracked here

The following audit findings are product gaps or implementation choices, not defects:

- no first-class Company entity;
- playbook not directly linked/versioned per campaign;
- WhatsApp not native in Unibox;
- organization management API being JWT-only;
- white-label work;
- Chatwoot/Twenty decisions;
- n8n integration work;
- rebranding;
- deployment/backup/DKIM setup.

Those belong in the implementation roadmap, not the bug backlog.
