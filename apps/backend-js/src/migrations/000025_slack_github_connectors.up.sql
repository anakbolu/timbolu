-- The Slack and GitHub connectors: the four tools their adapters declare.
--
-- An adapter is not the whole of a connector. The hub knows how to run a tool, but
-- three other readers take the catalogue as the product's own list rather than
-- asking an adapter: the grant screen offers what is here, the tool filter hands a
-- Bolu only what is here, and a workspace's approval policies are seeded from the
-- `write_external` rows here. A tool an adapter declares without a row would be one
-- nobody could grant, and — for the two that write outside Bolu — one no policy
-- would ever hold for approval, which is the failure that matters: the label is what
-- asks a person, and a tool with no row has no label to read.
--
-- Both connectors need a connected account and a grant, so `needs_connection` stays
-- its default of true, exactly like Gmail: nothing here is the workspace's own data.
--
-- The scopes the adapters ask for are not rows: a scope is on the OAuth flow, and
-- what a Bolu may do with a connection is the label below.

INSERT INTO tool_catalog (name, integration_app, label, description) VALUES
    ('slack.post_message', 'slack', 'write_external',
     'Kirim pesan ke sebuah kanal Slack. Membutuhkan persetujuan manusia.'),
    ('slack.list_channels', 'slack', 'read',
     'Daftar kanal publik di workspace Slack yang terhubung'),
    ('github.list_repos', 'github', 'read',
     'Daftar repositori GitHub yang dapat diakses akun terhubung'),
    ('github.create_issue', 'github', 'write_external',
     'Buat issue baru di sebuah repositori GitHub. Membutuhkan persetujuan manusia.')
ON CONFLICT (name) DO NOTHING;

-- A workspace that already exists gets the conservative policy for the two new
-- actions, the same way 000007 did for the tools that existed then and 000021 for
-- the order tools. The provisioner does this for a workspace created from here on;
-- this covers the ones that were onboarded before the connectors existed.
--
-- Only the two `write_external` rows: a read tool is never held for approval, and a
-- policy row for one would be a decision with nothing to decide.
INSERT INTO approval_policies (workspace_id, action_kind, mode)
SELECT w.id, tc.name, 'always_draft'
FROM workspaces w
CROSS JOIN tool_catalog tc
WHERE tc.name IN ('slack.post_message', 'github.create_issue')
ON CONFLICT (workspace_id, action_kind) DO NOTHING;
