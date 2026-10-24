-- Ref: oga9-engineering/sections/03-rbac-positions.md (group 5)
CREATE TABLE position_permissions (
  position_id uuid REFERENCES org_positions(id) ON DELETE CASCADE,
  permission text REFERENCES permissions(node) ON DELETE CASCADE,
  PRIMARY KEY (position_id, permission)
);
