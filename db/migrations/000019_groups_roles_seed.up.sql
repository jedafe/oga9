-- Ref: oga9-engineering/sections/03-rbac-positions.md
INSERT INTO user_groups (tenant_id, name, slug, is_system) VALUES
  (NULL, 'Super Admin', 'super_admin', true),
  (NULL, 'Members', 'members', true),
  (NULL, 'Guest', 'guest', true);
INSERT INTO user_roles (tenant_id, name, slug, is_system) VALUES
  (NULL, 'Org Admin', 'org_admin', true),
  (NULL, 'Secretary', 'secretary', true),
  (NULL, 'Treasurer', 'treasurer', true),
  (NULL, 'Financial Secretary', 'financial_secretary', true),
  (NULL, 'President', 'president', true),
  (NULL, 'PRO', 'pro', true),
  (NULL, 'Member', 'member', true);
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'member' AND p.node IN (
  'org.members.view_own','org.finance.view_own','org.finance.statement_view',
  'org.finance.claims.submit_own','org.events.register_own','org.activities.view',
  'org.comms.view','org.documents.view_any','org.chat.use','org.chat.post','org.members.view',
  'org.voice.join','org.welfare.request_own','api.keys.self_manage',
  'org.elections.vote_own');
INSERT INTO role_permissions (role_id, permission)
SELECT r.id, p.node FROM user_roles r, permissions p
WHERE r.slug = 'org_admin' AND p.node LIKE 'org.%';
