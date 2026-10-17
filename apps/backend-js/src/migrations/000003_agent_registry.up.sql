-- Agent registry: soft delete, the tool catalogue, and the indexes the status
-- derivation needs.
--
-- The agent, agent_template, and agent_grant tables already exist from 000001
-- together with the six seeded templates and their Row Level Security policies.
-- This migration adds what the registry needs on top of them.

-- --------------------------------------------------------------- soft delete

-- A Bolu with task history must not be hard deleted: the tasks, drafts, and
-- messages that reference it are the audit trail. Deleting one marks it instead,
-- and every read filters it out.
ALTER TABLE agents ADD COLUMN deleted_at TIMESTAMPTZ;

-- The tools this Bolu may use, copied from its template and editable per
-- workspace. A tool only becomes effective when its integration is granted to
-- the agent (see the allowed-tools query); this column is the agent's wish list.
ALTER TABLE agents ADD COLUMN tools JSONB NOT NULL DEFAULT '[]'::jsonb;

-- Partial index: the registry only ever lists live agents.
CREATE INDEX agents_live_idx ON agents (workspace_id, team_id)
    WHERE deleted_at IS NULL;

-- ------------------------------------------------------------ tool catalogue

-- What each tool does and, crucially, its label. The label is what decides
-- whether calling the tool needs human approval: `read` never does,
-- `write_internal` writes inside Bolu, and `write_external` reaches the outside
-- world and therefore goes through the approval policy.
--
-- Not tenant data: every workspace sees the same catalogue, so no RLS.
CREATE TABLE tool_catalog (
    name TEXT PRIMARY KEY,
    integration_app TEXT NOT NULL,
    label TEXT NOT NULL CHECK (label IN ('read', 'write_internal', 'write_external')),
    description TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX tool_catalog_app_idx ON tool_catalog (integration_app);

INSERT INTO tool_catalog (name, integration_app, label, description) VALUES
    ('gmail.search',           'gmail',     'read',           'Cari email di kotak masuk'),
    ('gmail.read_attachment',  'gmail',     'read',           'Baca lampiran email'),
    ('gmail.create_draft',     'gmail',     'write_internal', 'Buat draf email'),
    ('gmail.send',             'gmail',     'write_external', 'Kirim email ke penerima'),
    ('drive.search',           'drive',     'read',           'Cari file di Drive'),
    ('drive.read_file',        'drive',     'read',           'Baca isi file Drive'),
    ('drive.create_file',      'drive',     'write_internal', 'Simpan file baru ke Drive'),
    ('whatsapp.read_messages', 'whatsapp',  'read',           'Baca chat masuk'),
    ('whatsapp.send_message',  'whatsapp',  'write_external', 'Kirim pesan WhatsApp'),
    ('jira.search',            'jira',      'read',           'Cari issue Jira'),
    ('jira.create_issue',      'jira',      'write_external', 'Buat issue Jira'),
    ('github.list_pull_requests', 'github', 'read',           'Lihat pull request'),
    ('github.create_comment',  'github',    'write_external', 'Tulis komentar di pull request'),
    ('notion.search',          'notion',    'read',           'Cari halaman Notion'),
    ('notion.create_page',     'notion',    'write_internal', 'Buat halaman Notion'),
    ('tokopedia.list_orders',  'tokopedia', 'read',           'Lihat pesanan marketplace'),
    ('tokopedia.update_order', 'tokopedia', 'write_external', 'Ubah status pesanan')
ON CONFLICT (name) DO NOTHING;

-- --------------------------------------------------------------- derivation

-- The display status is derived from running tasks and pending drafts, so both
-- lookups need an index that matches the filter.
CREATE INDEX drafts_agent_status_idx ON drafts (workspace_id, agent_id, status);
CREATE INDEX tasks_agent_live_idx ON tasks (workspace_id, agent_id, status)
    WHERE status IN ('queued', 'running');
