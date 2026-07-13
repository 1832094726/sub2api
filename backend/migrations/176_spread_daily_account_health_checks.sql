-- Migration 175 used account_id directly as the minute slot. Sequential IDs
-- therefore clustered near midnight. Multiplying by 53 (coprime with 1440)
-- keeps slots unique while distributing adjacent IDs across the full day.

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

    slot_minutes := MOD(NEW.id * 53, 1440);
    slot_hour := slot_minutes / 60;
    slot_minute := MOD(slot_minutes, 60);
    slot_cron := slot_minute::TEXT || ' ' || slot_hour::TEXT || ' * * *';
    slot_next_run := DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => slot_minutes);
    IF slot_next_run <= NOW() THEN
        slot_next_run := slot_next_run + INTERVAL '1 day';
    END IF;
    should_enable := NEW.status IN ('active', 'error');

    INSERT INTO scheduled_test_plans (
        account_id, model_id, cron_expression, enabled, max_results,
        auto_recover, next_run_at, system_generated, created_at, updated_at
    ) VALUES (
        NEW.id, '', slot_cron, should_enable, 30,
        TRUE, slot_next_run, TRUE, NOW(), NOW()
    )
    ON CONFLICT (account_id) WHERE system_generated = TRUE
    DO UPDATE SET enabled = EXCLUDED.enabled, updated_at = NOW();

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

WITH health_slots AS (
    SELECT
        id,
        MOD(account_id * 53, 1440)::INTEGER AS slot_minutes
    FROM scheduled_test_plans
    WHERE system_generated = TRUE
)
UPDATE scheduled_test_plans p
SET
    cron_expression = MOD(s.slot_minutes, 60)::TEXT || ' ' || (s.slot_minutes / 60)::TEXT || ' * * *',
    next_run_at = CASE
        WHEN DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => s.slot_minutes) <= NOW()
            THEN DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => s.slot_minutes) + INTERVAL '1 day'
        ELSE DATE_TRUNC('day', NOW()) + MAKE_INTERVAL(mins => s.slot_minutes)
    END,
    updated_at = NOW()
FROM health_slots s
WHERE p.id = s.id;
