-- 001_init.sql — initial schema for go-rag-api.
-- Loaded automatically by the pgvector image on first boot via
-- /docker-entrypoint-initdb.d (see docker-compose.yml).

-- Enable pgvector. It ships in the image but stays off until enabled per
-- database; this is what makes the vector column type and distance operators
-- (e.g. <=> cosine distance) available.
CREATE EXTENSION IF NOT EXISTS vector;

-- One row per chunk: a passage of a source document plus the vector that
-- encodes its meaning. Retrieval reads vector + citation metadata from one row.
CREATE TABLE chunks (
    id          BIGSERIAL PRIMARY KEY,   -- auto-incrementing surrogate key
    document_id TEXT NOT NULL,           -- which source document this chunk came from
    content     TEXT NOT NULL,           -- the chunk text itself, returned as a citation
    embedding   vector(1024) NOT NULL    -- Titan v2 embedding; 1024 MUST match the embedding model's output
);
