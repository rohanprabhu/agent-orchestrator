-- +goose Up
-- Settings and the low-volume local-runner mailbox commit together, serialized
-- per organization. OAuth credentials inside state are AEAD-encrypted.
CREATE TABLE ao_linear_state (
 org_id UUID PRIMARY KEY REFERENCES ao_organizations(id) ON DELETE CASCADE,
 state JSONB NOT NULL DEFAULT '{}'::jsonb
);
ALTER TABLE ao_linear_state ENABLE ROW LEVEL SECURITY;
ALTER TABLE ao_linear_state FORCE ROW LEVEL SECURITY;
CREATE POLICY ao_linear_state_tenant ON ao_linear_state
 USING (org_id = ao_current_org_id()) WITH CHECK (org_id = ao_current_org_id());
CREATE TABLE ao_linear_routes (
 kind TEXT NOT NULL CHECK (kind IN ('oauth','workspace','worker')),
 key TEXT NOT NULL,
 org_id UUID NOT NULL REFERENCES ao_organizations(id) ON DELETE CASCADE,
 PRIMARY KEY(kind,key)
);
ALTER TABLE ao_linear_routes ENABLE ROW LEVEL SECURITY;
ALTER TABLE ao_linear_routes FORCE ROW LEVEL SECURITY;
CREATE POLICY ao_linear_routes_tenant ON ao_linear_routes
 USING (org_id = ao_current_org_id()) WITH CHECK (org_id = ao_current_org_id());
-- Only opaque routing metadata is readable across tenants by the dispatcher.
CREATE POLICY ao_linear_routes_lookup ON ao_linear_routes FOR SELECT
 USING (current_setting('ao.service',true) = 'linear-dispatch');
-- +goose Down
DROP TABLE ao_linear_routes;
DROP TABLE ao_linear_state;
