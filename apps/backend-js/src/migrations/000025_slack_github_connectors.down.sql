-- The reverse of 000025: the four catalogue rows the Slack and GitHub connectors
-- exposed, and the policies that were seeded for the two that write outside Bolu.
--
-- Nothing else has to be repaired. The rows are the product's own list rather than a
-- workspace's data, so no workspace loses anything by their removal; a Bolu that was
-- granted the tool keeps a grant naming a connection, and the tool filter reads the
-- catalogue rather than the grant, so the tool simply stops being offered.

DELETE FROM approval_policies
WHERE action_kind IN ('slack.post_message', 'github.create_issue');

DELETE FROM tool_catalog
WHERE name IN ('slack.post_message', 'slack.list_channels', 'github.list_repos', 'github.create_issue');
