-- Ref: oga9-engineering/sections/18-assets.md — one credential row per
-- (asset, kind); re-saving a kind updates the ref in place.
CREATE UNIQUE INDEX digital_credentials_asset_kind ON digital_credentials (asset_id, kind);
