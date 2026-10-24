CREATE TABLE plan_limits (
  plan_slug text REFERENCES plans(slug), limit_key text NOT NULL,
  limit_value bigint NOT NULL,
  PRIMARY KEY (plan_slug, limit_key)
);
