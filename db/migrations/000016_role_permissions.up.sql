CREATE TABLE role_permissions (
  role_id uuid REFERENCES user_roles(id) ON DELETE CASCADE,
  permission text REFERENCES permissions(node) ON DELETE CASCADE,
  PRIMARY KEY (role_id, permission)
);
