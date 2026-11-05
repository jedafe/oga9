-- Ref: oga9-engineering/sections/29-wordpress-connector.md + ADR-028 —
-- recorded consent decisions for untrusted OAuth clients (trusted
-- first-party clients skip consent per the ADR).
CREATE TABLE oauth_consents (
  user_id uuid NOT NULL REFERENCES users(id),
  client_id text NOT NULL REFERENCES oauth_clients(client_id),
  granted_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (user_id, client_id)
);
