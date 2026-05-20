DO $$ BEGIN
  RAISE EXCEPTION 'migration 006_enrichment_findings_tag is not reversible (up-only)';
END $$;
