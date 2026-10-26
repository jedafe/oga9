-- Ref: oga9-engineering/sections/14-comms-documents.md — org documents
-- with version history + soft delete (Rule 11); also lands the deferred
-- payment_claims.receipt_media_id FK (MIGRATION-ORDER: stub → ALTER).
CREATE TABLE documents (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  organization_id uuid NOT NULL REFERENCES organizations(id),
  kind text NOT NULL,                              -- minutes|constitution|bylaw|policy|report|resolution|photo|archive|certificate
  title text NOT NULL, storage_key text NOT NULL,  -- object storage ref
  visibility text NOT NULL DEFAULT 'members',      -- public|members|exco|committee|private
  unit_id uuid REFERENCES org_units(id),
  version int NOT NULL DEFAULT 1,
  uploaded_by uuid NOT NULL REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  deleted_at timestamptz                           -- soft delete, Rule 11
);
CREATE INDEX documents_org ON documents (organization_id, kind) WHERE deleted_at IS NULL;

CREATE TABLE document_versions (
  id uuid PRIMARY KEY DEFAULT gen_random_uuid(),
  document_id uuid NOT NULL REFERENCES documents(id),
  version int NOT NULL, storage_key text NOT NULL,
  created_by uuid NOT NULL REFERENCES organization_memberships(id),
  created_at timestamptz NOT NULL DEFAULT now(),
  UNIQUE(document_id, version)
);

ALTER TABLE payment_claims
  ADD CONSTRAINT payment_claims_receipt_media_fk
  FOREIGN KEY (receipt_media_id) REFERENCES documents(id) NOT VALID;
