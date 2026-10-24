CREATE TABLE plan_capabilities (
  plan_slug text REFERENCES plans(slug), capability text NOT NULL,
  PRIMARY KEY (plan_slug, capability)
);
