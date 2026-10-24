-- Ref: oga9-engineering/conventions/plans-entitlements-catalog.md
INSERT INTO plans (slug, label, price_monthly_minor, price_annual_minor, sort) VALUES
  ('starter','Starter',0,0,1),('basic','Basic',2400,24000,2),
  ('professional','Professional',7900,79000,3),('business','Business',22400,224000,4),
  ('enterprise','Enterprise',NULL,NULL,5);
INSERT INTO plan_capabilities (plan_slug, capability) VALUES
  ('starter','app.organization_core'),('starter','app.members'),('starter','app.events_meetings'),('starter','app.comms_documents'),
  ('basic','app.finance_lite'),('basic','app.welfare'),
  ('professional','app.finance_lite'),('professional','app.welfare'),('professional','app.elections'),('professional','app.governance'),
  ('business','app.finance_lite'),('business','app.welfare'),('business','app.elections'),('business','app.governance'),('business','app.analytics'),('business','capability.consolidation'),
  ('enterprise','app.finance_lite'),('enterprise','app.welfare'),('enterprise','app.elections'),('enterprise','app.governance'),('enterprise','app.analytics'),('enterprise','capability.consolidation'),('enterprise','capability.sso'),('enterprise','capability.dedicated_db');
INSERT INTO plan_limits (plan_slug, limit_key, limit_value) VALUES
  ('starter','limit.members.active',50),('starter','limit.branches',1),('starter','limit.organizations',1),('starter','limit.storage.mb',250),('starter','limit.messaging.monthly',100),('starter','limit.automation.runs',0),
  ('basic','limit.members.active',500),('basic','limit.branches',3),('basic','limit.organizations',1),('basic','limit.storage.mb',5000),('basic','limit.messaging.monthly',1000),('basic','limit.automation.runs',100),
  ('professional','limit.members.active',5000),('professional','limit.branches',10),('professional','limit.organizations',3),('professional','limit.storage.mb',50000),('professional','limit.messaging.monthly',10000),('professional','limit.automation.runs',2000),
  ('business','limit.members.active',50000),('business','limit.branches',50),('business','limit.organizations',20),('business','limit.storage.mb',500000),('business','limit.messaging.monthly',50000),('business','limit.automation.runs',20000),
  ('enterprise','limit.members.active',-1),('enterprise','limit.branches',-1),('enterprise','limit.organizations',-1),('enterprise','limit.storage.mb',-1),('enterprise','limit.messaging.monthly',-1),('enterprise','limit.automation.runs',-1);
