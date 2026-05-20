DO $$ BEGIN
  RAISE EXCEPTION 'migration 001_v1alpha1_schema is not reversible (up-only)';
END $$;
