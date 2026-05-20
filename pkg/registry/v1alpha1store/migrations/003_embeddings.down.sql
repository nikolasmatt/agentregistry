DO $$ BEGIN
  RAISE EXCEPTION 'migration 003_embeddings is not reversible (up-only)';
END $$;
