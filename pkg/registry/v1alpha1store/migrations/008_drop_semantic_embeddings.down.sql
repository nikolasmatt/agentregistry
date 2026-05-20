DO $$ BEGIN
  RAISE EXCEPTION 'migration 008_drop_semantic_embeddings is not reversible (up-only)';
END $$;
