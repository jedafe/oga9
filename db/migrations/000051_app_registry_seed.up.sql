-- Ref: oga9-engineering/sections/06-app-framework-registry.md
-- MVP registry seeds — manifests carry the dependency graph (requires),
-- owned permission nodes, published/consumed events, and the entitlement
-- feature key the activation check resolves (Rule 25).
INSERT INTO app_registry (app_id, version, label, mvp_required, manifest) VALUES
('organization_core','1.0.0','Organization Core',true,'{
  "id":"organization_core","version":"1.0.0","requires":[],
  "permissions":["org.members.view","org.members.manage","org.admin.settings","org.admin.branding","org.directory.manage"],
  "events":{"publishes":["OrganizationCreated","OrgUnitCreated","OrgUnitUpdated","OrganizationSettingChanged"],"consumes":[]},
  "entitlements":{"feature":"app.organization_core"}
}'),
('members','1.0.0','Members',true,'{
  "id":"members","version":"1.0.0","requires":["organization_core"],
  "permissions":["org.members.view","org.members.invite","org.members.approve","org.members.manage"],
  "events":{"publishes":["MemberApproved","MemberInvited","InvitationAccepted"],"consumes":["OrganizationCreated"]},
  "entitlements":{"feature":"app.members"}
}'),
('finance_lite','0.1.0','Finance Lite',false,'{
  "id":"finance_lite","version":"0.1.0","requires":["organization_core","members"],
  "permissions":["org.finance.dues.manage","org.finance.payments.record","org.finance.payments.approve","org.finance.reports.read"],
  "events":{"publishes":["InvoiceIssued","PaymentReceived"],"consumes":["MemberApproved"]},
  "entitlements":{"feature":"app.finance_lite","limits":{"financial_years":"plan"}}
}'),
('events_meetings','1.0.0','Events & Meetings',false,'{
  "id":"events_meetings","version":"1.0.0","requires":["organization_core","members"],
  "permissions":["org.activities.view","org.events.manage","org.events.register_own","org.minutes.approve"],
  "events":{"publishes":["MeetingScheduled","MinutesApproved"],"consumes":[]},
  "entitlements":{"feature":"app.events_meetings"}
}'),
('comms_documents','1.0.0','Comms & Documents',true,'{
  "id":"comms_documents","version":"1.0.0","requires":["organization_core"],
  "permissions":["org.comms.view","org.comms.broadcast","org.documents.view_any","org.documents.manage"],
  "events":{"publishes":["AnnouncementPublished"],"consumes":[]},
  "entitlements":{"feature":"app.comms_documents"}
}'),
('subscription_admin','1.0.0','Subscription Admin',true,'{
  "id":"subscription_admin","version":"1.0.0","requires":["organization_core"],
  "permissions":["org.admin.billing"],
  "events":{"publishes":["SubscriptionChanged"],"consumes":[]},
  "entitlements":{"feature":"app.organization_core"}
}');
