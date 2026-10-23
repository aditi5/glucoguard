# GlucoGuard — diabetes monitoring and safety-guidance backend

A Go backend for an adult, India-first diabetes app. Patients log glucose readings, and a deterministic, clinician-reviewable safety engine returns the next step: treat a low, recheck, follow your plan, contact your care team, or call 112.

> **Status: engineering skeleton with DRAFT clinical rules.** Every threshold and patient-facing message is marked for clinician review. The server **refuses to start with `APP_ENV=production`** until a qualified diabetes clinician has signed off on the rule set and message catalog, and real authentication is wired in.

**Scope of v1** (decided up front):

- Adults only. Children, adolescents, pregnancy, gestational diabetes and flagged special circumstances never get the generic adult pathway. They're routed to "follow your personal plan / contact your care team", and universal emergency rules still apply to them.
- India first. Emergency number 112, mg/dL default (mmol/L supported), DPDP-style consent recorded at sign-up.
- Education and safety guidance only. No insulin dosing, no medication changes, no AI-generated treatment.

---

## Quick start

```bash
go test -race ./...                 # engine + API tests (in-memory store)
go run ./cmd/server                 # dev server on :8080, in-memory store, dev auth

# with PostgreSQL
docker compose up -d db
DATABASE_URL=postgres://glucoguard:glucoguard@localhost:5432/glucoguard?sslmode=disable go run ./cmd/server

# run the API test suite against Postgres too
TEST_DATABASE_URL=postgres://... go test ./internal/api/
```

Try it:

```bash
# Register (consent is mandatory)
curl -s -XPOST localhost:8080/v1/patients -d '{
  "display_name":"Ravi","age_group":"adult","diabetes_type":"type2",
  "consent_given":true,"consent_version":"privacy-notice-2026-10"}'

# Submit a reading (dev auth: "Dev patient:<id>")
curl -s -XPOST localhost:8080/v1/glucose-readings -H "Authorization: Dev patient:$ID" -d '{
  "client_reading_id":"phone-uuid-1","value":62,"unit":"mg/dL",
  "measured_at":"2026-10-24T09:00:00+05:30","symptoms":["shaky"]}'
```

---

## Layout

```
cmd/server/            entrypoint, production safety gate, graceful shutdown
internal/clinical/     THE SAFETY ENGINE: pure Go, no I/O, no LLM
  types.go             inputs, Decision, care plan validation, rule-set version
  rules.go             the rules table (one row per clinical rule)
  engine.go            evaluation + resolution (highest urgency wins; escalations only add)
  messages.go          patient-facing message catalog (versioned IDs, review status)
  units.go             mg/dL ↔ mmol/L, structural validation, meter limits
  engine_test.go       table-driven clinical scenarios, coverage check, 50k-case invariant fuzz
internal/app/          services: submit → evaluate → persist → remind → audit; reports; reminder worker
internal/store/        Store interface, PostgreSQL (pgx) and in-memory implementations
internal/api/          REST handlers, dev auth, rate limiting, error→fail-safe mapping, API tests
migrations/            SQL schema (embedded, auto-applied with an advisory lock)
```

---

## How the safety engine decides

`clinical.Evaluate(input) → Decision` is pure and deterministic.

1. Every rule in scope is evaluated. `ScopeAdult` rules are skipped for out-of-scope patients.
2. The **primary** rule is the match with the **highest urgency**, with ties broken by priority. It sets `action_code` and `approved_message_id`.
3. Reason codes and escalations from **all** matches are merged. Escalation flags only ever switch on, so a lower-priority rule can add safety but can never remove it.
4. If no rule matches (a gap in the table), evaluation fails. The API then returns HTTP 503 with a `fail_safe` instruction. The patient never gets silence.

### Rules table (draft — needs clinician sign-off)

| ID | Scope | When | Urgency | Action |
|---|---|---|---|---|
| U01 | all | unconscious / seizure / unable to swallow | emergency | No oral intake, call 112 |
| U02 | all | glucose ≥ high **and** vomiting / abdominal pain / breathing difficulty / confusion | emergency | Emergency assessment, call 112 |
| U03 | all | any red-flag symptom (not "confusion alone with a current adult low") | urgent | Seek urgent care, don't wait for a reading |
| U10 | all | measured_at > 5 min in the future | action | Check device clock |
| U11 | all | stale reading whose last value was low | urgent | Check now; treat as low if symptomatic |
| U12 | all | stale reading | action | Check now; never shown as current |
| U20 | out-of-scope | current reading < low or ≥ high | urgent | Follow personal plan, contact care team |
| U21 | out-of-scope | always | info | Saved; route to personal protocol |
| A29 | adult | low + confusion | urgent | Treat now, tell someone, notify caregiver |
| A30 | adult | still low after `max_low_treatment_cycles` within 60 min | urgent | Treat again + contact care team |
| A31 | adult | < severe low (54) | urgent | Treat now (15 g), recheck 15 min, notify caregiver |
| A32 | adult | < low (70) | action | 15 g fast carbs, recheck 15 min |
| U40 | all | meter LO/HI or value outside 20–600 mg/dL | urgent | Verify. LO is processed as severe low and never dismissed |
| U41 | all | Δ ≥ 100 mg/dL within 30 min of prior | action | Verify by recheck |
| A42 | adult | hypo symptoms with non-low reading | action | Recheck now; treat as low if unable to check |
| A50 | adult | high, **no approved plan** | action | Contact care team |
| A51 | adult | ≥ very high, approved plan | action | Follow sick-day plan (incl. ketones if prescribed) |
| A52 | adult | ≥ high, approved plan | monitor | Follow high-glucose plan |
| U60 | all | no approved plan | info | Ask clinician to set up plan |
| A61 | adult | in range | info | Keep following plan |

With no approved plan, low thresholds fall back to the ADA level 1 (<70) and level 2 (<54) hypoglycemia definitions. High readings never say "follow your plan"; they route to the care team instead.

### Tests that guard it

- **Table-driven scenarios** cover boundaries (69/70, 53/54), units, meter LO/HI, a likely unit mix-up (5.5 entered as mg/dL), stale and future data, cycles, special populations and no-plan behaviour.
- **`TestEveryRuleIsCovered`** fails if a rule is added without a scenario.
- **`TestRulesTableIntegrity`** checks for unique IDs and priorities, catalog references, and that emergencies always call emergency services.
- **`TestSafetyInvariants`** runs 50,000 random inputs. It checks that no-oral symptoms always give an emergency, that oral treatment never appears alongside no-oral symptoms, that a current low is never below "action", that stale data is never "in range", that out-of-scope patients never get the adult pathway, and that emergencies never ask for a recheck.

---

## API

All responses are JSON. The backend's `decision` is authoritative and the client renders it.

| Method | Path | Notes |
|---|---|---|
| GET | `/v1/meta` | rule-set version, approval status, emergency number |
| GET | `/v1/messages` | message catalog (client renders by `approved_message_id`) |
| POST | `/v1/patients` | register; requires `consent_given` + current `consent_version` |
| GET/PUT/DELETE | `/v1/patients/me` | profile; DELETE needs `?confirm=DELETE`, erases all personal data |
| POST | `/v1/glucose-readings` | submit + evaluate in one call; idempotent on `client_reading_id` (or `Idempotency-Key`) |
| GET | `/v1/glucose-readings` | `?from&to&limit` (RFC3339) |
| POST | `/v1/readings/{id}/evaluate` | re-evaluate now (optionally with extra `symptoms`) |
| GET | `/v1/action-plans/current` | current plan, or `using_defaults` |
| POST | `/v1/follow-ups` | `acknowledged` / `recheck` (with reading) / `symptom_update` / `escalated` / `resolved` |
| POST | `/v1/devices/{provider}/sync` | 501 until the first device adapter lands |
| GET | `/v1/reports/glucose` | `?from&to&format=csv`; stats, per-reading decisions, follow-ups |
| GET | `/v1/audit-events` | patient's own audit trail |
| POST | `/v1/patients/{id}/care-plans` | **clinician only**; `approve: true` records the approver |
| GET | `/v1/patients/{id}/reports/glucose` | clinician report |

Evaluation response (abridged):

```json
{
  "evaluation": { "id": "…", "decision": {
    "urgency": "urgent", "action_code": "TREAT_SEVERE_LOW", "approved_message_id": "SEVERE_LOW_001",
    "reason_codes": ["GLUCOSE_BELOW_SEVERE_LOW","GLUCOSE_BELOW_LOW"],
    "matched_rules": ["A31_SEVERE_LOW","A32_LOW"],
    "care_plan_version": "v3", "care_plan_approved": true, "rule_set_version": "in-adult-2026.10.0-draft",
    "follow_up_policy": {"required": true, "recheck_after_minutes": 15},
    "escalation": {"call_emergency_services": false, "contact_care_team": false, "notify_caregiver": true},
    "value_mg_dl": 48, "data_status": "current", "low_treatment_cycle": 1 }},
  "message": { "id": "SEVERE_LOW_001", "title": "Very low glucose — treat now", "body": "…", "review_status": "draft_pending_clinician_review" }
}
```

---

## Safety and data properties built in

- `measured_at` is stored separately from `received_at`, so a delayed sync can't masquerade as a current reading.
- Readings are idempotent per patient and client ID, which makes retries and re-syncs safe. A duplicate returns the decision the patient already saw.
- Reminders are **durable** (a Postgres row), claimed with leases and `SKIP LOCKED`, retried with exponential backoff, and audited when exhausted. The client also schedules a local notification from `follow_up_policy`, so push is never the only mechanism.
- The audit trail is append-only and enforced by a DB trigger. Every rule execution records the rule-set version, care-plan version, matched rules and message ID. On account deletion, audit rows are kept with the patient reference removed.
- Care plans are append-only and versioned. The DB checks enforce `severe_low < low < high ≤ very_high` and require an approver on approved plans.
- Logs never contain request bodies (health data). Responses send `Cache-Control: no-store`. JSON decoding rejects unknown fields. Bodies are capped at 64 KB. Requests are rate-limited.
- CSV exports neutralise spreadsheet formula injection.

---

## Before any patient uses this

**Clinical**
- [ ] A diabetes clinician reviews every row in `rules.go` and every message in `messages.go`. Mark messages `approved`, set `RuleSetClinicallyApproved = true`, and bump `RuleSetVersion`.
- [ ] Clinician decides values tagged `REVIEW`: default high/very-high, rapid-change Δ, max low cycles, high recheck interval.
- [ ] Hindi (and other) translations are reviewed as clinical content, not just copy.
- [ ] Separate protocols exist for any population you later bring into scope.

**Engineering**
- [ ] Replace `DevHeaderAuth` with real auth (phone OTP / OIDC) and enforce care-team membership on clinician routes.
- [ ] Real notifier (FCM/APNs + SMS fallback) and caregiver alerting via a consented channel.
- [ ] Encryption at rest (managed Postgres/KMS), TLS everywhere, backups, and India data residency.
- [ ] Mobile app: local reminder scheduling, offline queue with `client_reading_id`, and a "stale" banner driven by `data_status`.
- [ ] First device adapter behind `/v1/devices/{provider}/sync`.

**Regulatory**
- [ ] Assess classification under India's Medical Devices Rules 2017 (CDSCO software-as-medical-device guidance) and the DPDP Act 2023 before making any product claims.

An optional AI layer can later explain decisions in plain language or summarise a week of readings. It must only paraphrase approved messages, show the original alongside, and never change `urgency`, `action_code` or escalation.
