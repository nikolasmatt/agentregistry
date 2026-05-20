DO $$ BEGIN
  RAISE EXCEPTION 'migration 007_drop_enrichment_findings is not reversible (up-only)';
END $$;
