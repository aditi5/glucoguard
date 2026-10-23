-- GlucoGuard initial schema (PostgreSQL 15+)
-- Applied automatically at startup by the server (see internal/store/postgres.go).

CREATE EXTENSION IF NOT EXISTS pgcrypto;

CREATE TABLE IF NOT EXISTS patients (
    id                    UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    display_name          TEXT        NOT NULL,
    age_group             TEXT        NOT NULL CHECK (age_group IN ('adult','older_adult','adolescent','child')),
    diabetes_type         TEXT        NOT NULL CHECK (diabetes_type IN ('type1','type2','gestational','other')),
    pregnant              BOOLEAN     NOT NULL DEFAULT FALSE,
    special_circumstances BOOLEAN     NOT NULL DEFAULT FALSE,
    preferred_unit        TEXT        NOT NULL DEFAULT 'mg/dL' CHECK (preferred_unit IN ('mg/dL','mmol/L')),
    language              TEXT        NOT NULL DEFAULT 'en',
    caregiver_name        TEXT,
    caregiver_phone       TEXT,
    -- DPDP Act: record when and to what version of the notice consent was given.
    consent_version       TEXT        NOT NULL,
    consent_given_at      TIMESTAMPTZ NOT NULL,
    created_at            TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Care plans are append-only. A new version supersedes the old one, and history
-- is kept so every past evaluation can be explained.
CREATE TABLE IF NOT EXISTS care_plans (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id                UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    version                   TEXT        NOT NULL,
    low_mg_dl                 NUMERIC(5,1) NOT NULL,
    severe_low_mg_dl          NUMERIC(5,1) NOT NULL,
    high_mg_dl                NUMERIC(5,1) NOT NULL,
    very_high_mg_dl           NUMERIC(5,1) NOT NULL,
    stale_after_seconds       INTEGER     NOT NULL,
    low_recheck_seconds       INTEGER     NOT NULL,
    high_recheck_seconds      INTEGER     NOT NULL,
    max_low_treatment_cycles  INTEGER     NOT NULL,
    clinician_approved        BOOLEAN     NOT NULL DEFAULT FALSE,
    approved_by               TEXT,
    approved_at               TIMESTAMPTZ,
    effective_from            TIMESTAMPTZ NOT NULL DEFAULT now(),
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (patient_id, version),
    CHECK (severe_low_mg_dl < low_mg_dl AND low_mg_dl < high_mg_dl AND high_mg_dl <= very_high_mg_dl),
    CHECK (NOT clinician_approved OR (approved_by IS NOT NULL AND approved_at IS NOT NULL))
);
CREATE INDEX IF NOT EXISTS care_plans_current ON care_plans (patient_id, effective_from DESC);

CREATE TABLE IF NOT EXISTS glucose_readings (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id         UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    -- Client-generated ID makes submission idempotent across retries and device re-syncs.
    client_reading_id  TEXT        NOT NULL,
    value              NUMERIC(6,2),                -- original value as entered (NULL for LO/HI)
    unit               TEXT        NOT NULL CHECK (unit IN ('mg/dL','mmol/L')),
    meter_flag         TEXT        NOT NULL DEFAULT '' CHECK (meter_flag IN ('','LO','HI')),
    value_mg_dl        NUMERIC(5,1) NOT NULL,       -- normalised for queries and reports
    source             TEXT        NOT NULL CHECK (source IN ('manual_meter','cgm')),
    device_id          TEXT,
    meal_context       TEXT        NOT NULL DEFAULT 'unspecified'
                       CHECK (meal_context IN ('unspecified','fasting','before_meal','after_meal','bedtime','overnight')),
    symptoms           TEXT[]      NOT NULL DEFAULT '{}',
    notes              TEXT,
    measured_at        TIMESTAMPTZ NOT NULL,        -- when the blood was measured
    received_at        TIMESTAMPTZ NOT NULL,        -- when the server received it (never confuse the two)
    UNIQUE (patient_id, client_reading_id)
);
CREATE INDEX IF NOT EXISTS glucose_readings_timeline ON glucose_readings (patient_id, measured_at DESC);

CREATE TABLE IF NOT EXISTS evaluations (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id           UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    reading_id           UUID        NOT NULL REFERENCES glucose_readings(id) ON DELETE CASCADE,
    urgency              TEXT        NOT NULL,
    action_code          TEXT        NOT NULL,
    approved_message_id  TEXT        NOT NULL,
    reason_codes         TEXT[]      NOT NULL,
    matched_rules        TEXT[]      NOT NULL,
    rule_set_version     TEXT        NOT NULL,
    care_plan_version    TEXT        NOT NULL,
    low_treatment_cycle  INTEGER     NOT NULL DEFAULT 0,
    decision             JSONB       NOT NULL,      -- full decision as returned to the client
    acknowledged_at      TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS evaluations_by_patient ON evaluations (patient_id, created_at DESC);

CREATE TABLE IF NOT EXISTS follow_up_events (
    id                   UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id           UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    evaluation_id        UUID        NOT NULL REFERENCES evaluations(id) ON DELETE CASCADE,
    kind                 TEXT        NOT NULL CHECK (kind IN ('acknowledged','recheck','symptom_update','escalated','resolved')),
    recheck_reading_id   UUID        REFERENCES glucose_readings(id) ON DELETE SET NULL,
    symptoms             TEXT[]      NOT NULL DEFAULT '{}',
    note                 TEXT,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Reminders are persisted (durable), so a crash or failed push never loses a recheck.
CREATE TABLE IF NOT EXISTS reminders (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id      UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    evaluation_id   UUID        NOT NULL REFERENCES evaluations(id) ON DELETE CASCADE,
    due_at          TIMESTAMPTZ NOT NULL,
    status          TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','sent','failed','cancelled','completed')),
    attempts        INTEGER     NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (evaluation_id)
);
CREATE INDEX IF NOT EXISTS reminders_due ON reminders (status, due_at) WHERE status IN ('pending','failed');

CREATE TABLE IF NOT EXISTS device_connections (
    id                 UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    patient_id         UUID        NOT NULL REFERENCES patients(id) ON DELETE CASCADE,
    provider           TEXT        NOT NULL,
    status             TEXT        NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','revoked','error')),
    scopes             TEXT[]      NOT NULL DEFAULT '{}',
    last_successful_sync TIMESTAMPTZ,
    last_error         TEXT,
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (patient_id, provider)
);

-- Append-only audit trail. patient_id is nulled (not deleted) on account
-- deletion so the trail of rule executions survives without personal data.
CREATE TABLE IF NOT EXISTS audit_events (
    id           BIGSERIAL PRIMARY KEY,
    patient_id   UUID        REFERENCES patients(id) ON DELETE SET NULL,
    actor        TEXT        NOT NULL,
    action       TEXT        NOT NULL,
    entity_type  TEXT        NOT NULL,
    entity_id    TEXT,
    detail       JSONB       NOT NULL DEFAULT '{}',
    at           TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS audit_events_by_patient ON audit_events (patient_id, at DESC);

-- Enforce append-only at the database level.
CREATE OR REPLACE FUNCTION audit_events_immutable() RETURNS trigger AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND NEW.patient_id IS NULL AND OLD.patient_id IS NOT NULL
       AND NEW.action = OLD.action AND NEW.detail = OLD.detail THEN
        RETURN NEW; -- allowed: ON DELETE SET NULL during account deletion
    END IF;
    RAISE EXCEPTION 'audit_events is append-only';
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS audit_events_no_mutation ON audit_events;
CREATE TRIGGER audit_events_no_mutation
    BEFORE UPDATE OR DELETE ON audit_events
    FOR EACH ROW EXECUTE FUNCTION audit_events_immutable();
