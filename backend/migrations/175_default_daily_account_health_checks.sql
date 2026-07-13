-- Give every account one system-managed daily health check. Account IDs are
-- spread over all 1,440 minutes so imports cannot create a request spike.

ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS system_generated BOOLEAN NOT NULL DEFAULT FALSE;

CREATE UNIQUE INDEX IF NOT EXISTS idx_scheduled_test_plans_system_account
    ON scheduled_test_plans(account_id)
    WHERE system_generated = TRUE;

CREATE OR REPLACE FUNCTION ensure_default_daily_account_health_check()
RETURNS TRIGGER AS $$
DECLARE
    slot_minutes INTEGER;
    slot_hour INTEGER;
    slot_minute INTEGER;
    slot_cron TEXT;
    slot_next_run TIMESTAMPTZ;
    should_enable BOOLEAN;
BEGIN
    IF NEW.deleted_at IS NOT NULL THEN
        UPDATE scheduled_test_plans
        SET enabled = FALSE, updated_at = NOW()
        WHERE account_id = NEW.id AND system_generated = TRUE;
        RETURN NEW;
    END IF;

    slot_minutes := MOD(NEW.id, 1440);
    slot_hour := slot_minutes / 60;
    slot_minute := MOD(slot_minutes, 60);
    slot_cron := slot_minute::TEXT || ' ' || slot_hour::TEXT || ' * * *';
    slot_next_run := DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => slot_minutes);
    IF slot_next_run <= NOW() THEN
        slot_next_run := slot_next_run + INTERVAL '1 day';
    END IF;
    should_enable := NEW.status IN ('active', 'error');

    INSERT INTO scheduled_test_plans (
        account_id,
        model_id,
        cron_expression,
        enabled,
        max_results,
        auto_recover,
        next_run_at,
        system_generated,
        created_at,
        updated_at
    ) VALUES (
        NEW.id,
        '',
        slot_cron,
        should_enable,
        30,
        TRUE,
        slot_next_run,
        TRUE,
        NOW(),
        NOW()
    )
    ON CONFLICT (account_id) WHERE system_generated = TRUE
    DO UPDATE SET
        enabled = EXCLUDED.enabled,
        updated_at = NOW();

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS trg_default_daily_account_health_check ON accounts;
CREATE TRIGGER trg_default_daily_account_health_check
AFTER INSERT OR UPDATE OF status, deleted_at ON accounts
FOR EACH ROW
EXECUTE FUNCTION ensure_default_daily_account_health_check();

INSERT INTO scheduled_test_plans (
    account_id,
    model_id,
    cron_expression,
    enabled,
    max_results,
    auto_recover,
    next_run_at,
    system_generated,
    created_at,
    updated_at
)
SELECT
    a.id,
    '',
    MOD(a.id, 60)::TEXT || ' ' || MOD(a.id / 60, 24)::TEXT || ' * * *',
    a.status IN ('active', 'error') AND a.deleted_at IS NULL,
    30,
    TRUE,
    CASE
        WHEN DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => MOD(a.id, 1440)::INTEGER) <= NOW()
            THEN DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => MOD(a.id, 1440)::INTEGER) + INTERVAL '1 day'
        ELSE DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => MOD(a.id, 1440)::INTEGER)
    END,
    TRUE,
    NOW(),
    NOW()
FROM accounts a
WHERE a.deleted_at IS NULL
ON CONFLICT (account_id) WHERE system_generated = TRUE DO NOTHING;
