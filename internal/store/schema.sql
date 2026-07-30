-- schema.sql: canonical schema for go-rag-api, embedded into the binary and
-- applied idempotently on startup (see schema.go). This is what creates the
-- schema in cloud RDS, where there is no docker-entrypoint-initdb.d hook.
--
-- Every statement is guarded with IF NOT EXISTS so running it on every boot is
-- safe: first boot creates, later boots are no-ops.

-- Enable pgvector. It ships in the image / is allowlisted on RDS but stays off
-- until enabled per database; this makes the vector type and distance operators
-- (e.g. <=> cosine distance) available.
CREATE EXTENSION IF NOT EXISTS vector;

-- One row per chunk: a passage of a source document plus the vector that
-- encodes its meaning. Retrieval reads vector + citation metadata from one row.
CREATE TABLE IF NOT EXISTS chunks (
    id          BIGSERIAL PRIMARY KEY,   -- auto-incrementing surrogate key
    document_id TEXT NOT NULL,           -- which source document this chunk came from
    content     TEXT NOT NULL,           -- the chunk text itself, returned as a citation
    embedding   vector(1024) NOT NULL    -- Titan v2 embedding; 1024 MUST match the embedding model's output
);
