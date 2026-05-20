DO $$ BEGIN
  RAISE EXCEPTION 'migration 002_enrichment_findings is not reversible (up-only)';
END $$;
