DO $$ BEGIN
  RAISE EXCEPTION 'migration 004_notify_payload_discrete is not reversible (up-only)';
END $$;
