-- LLM provider configuration: which providers a workspace may call, in which
-- order, and the encrypted key for each.
--
-- Two rules shape the table:
--   - the API key is stored encrypted (AES-256-GCM, key from the environment),
--     never in clear text, so a database dump does not leak credentials;
--   - the fallback order is data, not code: `priority` orders the chain and
--     `is_default` marks the one a Bolu uses when it has no override.

CREATE TABLE llm_providers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    workspace_id UUID NOT NULL REFERENCES workspaces (id) ON DELETE CASCADE,
    -- What the user sees in the settings screen, e.g. "OpenRouter (utama)".
    name TEXT NOT NULL,
    -- The wire format, which selects the adapter.
    adapter TEXT NOT NULL CHECK (adapter IN ('openai', 'anthropic')),
    -- The endpoint root; empty means the adapter's public default.
    base_url TEXT NOT NULL DEFAULT '',
    model TEXT NOT NULL,
    -- AES-256-GCM ciphertext. NULL means no key stored yet, which the settings
    -- screen reports as "belum ada kunci".
    api_key_encrypted BYTEA,
    -- Lower goes first in the fallback chain.
    priority INT NOT NULL DEFAULT 100,
    -- Zero means the adapter default.
    max_tokens INT NOT NULL DEFAULT 0,
    -- The model's context window. Zero means the adapter default; the value is
    -- what the pre-flight check compares the prompt against.
    context_tokens INT NOT NULL DEFAULT 0,
    is_default BOOLEAN NOT NULL DEFAULT false,
    enabled BOOLEAN NOT NULL DEFAULT true,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- A workspace cannot have two providers with the same label; the label is what
-- the user picks from, so an ambiguous one would be a bug.
CREATE UNIQUE INDEX llm_providers_workspace_name_key
    ON llm_providers (workspace_id, lower(name));

-- At most one default per workspace. The fallback chain falls back to the
-- workspace default, not to an arbitrary row.
CREATE UNIQUE INDEX llm_providers_one_default_key
    ON llm_providers (workspace_id)
    WHERE is_default;

-- The chain read: the default first, then priority order, which is exactly the
-- sort the resolver applies.
CREATE INDEX llm_providers_chain_idx
    ON llm_providers (workspace_id, is_default DESC, priority, created_at);

-- Row Level Security, same shape as every other tenant table in 000001.
ALTER TABLE llm_providers ENABLE ROW LEVEL SECURITY;
ALTER TABLE llm_providers FORCE ROW LEVEL SECURITY;
CREATE POLICY llm_providers_tenant_isolation ON llm_providers
    USING (workspace_id = app_current_workspace_id())
    WITH CHECK (workspace_id = app_current_workspace_id());

-- ------------------------------------------------------------ usage reporting

-- Daily token and cost totals per workspace, provider, model, and purpose. The
-- dashboard and the weekly summary read this instead of scanning the ledger,
-- which grows without bound.
--
-- security_invoker keeps the ledger's Row Level Security in force for whoever
-- queries the view, so the aggregation cannot be used to read another tenant.
CREATE VIEW usage_daily
    WITH (security_invoker = true) AS
SELECT workspace_id,
       date_trunc('day', created_at AT TIME ZONE 'Asia/Jakarta')::date AS day,
       provider,
       model,
       purpose,
       count(*)::bigint                       AS calls,
       sum(input_tokens)::bigint              AS input_tokens,
       sum(output_tokens)::bigint             AS output_tokens,
       sum(cache_read_tokens)::bigint         AS cache_read_tokens,
       sum(cache_write_tokens)::bigint        AS cache_write_tokens,
       sum(cost_micros)::bigint               AS cost_micros
FROM usage_ledger
GROUP BY 1, 2, 3, 4, 5;

COMMENT ON VIEW usage_daily IS
    'Daily usage totals per workspace, provider, model, and purpose. Days follow Asia/Jakarta, which is where the customers are.';