CREATE TABLE IF NOT EXISTS mp_entities (
  kind text NOT NULL,
  id text NOT NULL,
  payload jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(),
  PRIMARY KEY (kind,id)
);
CREATE UNIQUE INDEX IF NOT EXISTS mp_server_port ON mp_entities ((payload->>'node_id'), (payload->>'port')) WHERE kind='server';
CREATE UNIQUE INDEX IF NOT EXISTS mp_node_address ON mp_entities ((payload->>'address'), (payload->>'ssh_port')) WHERE kind='node';
CREATE UNIQUE INDEX IF NOT EXISTS mp_route_domain ON mp_entities (lower(payload->>'domain')) WHERE kind='route';
CREATE TABLE IF NOT EXISTS mp_users (
  id text PRIMARY KEY, username text NOT NULL UNIQUE, password_hash text NOT NULL,
  role text NOT NULL CHECK(role IN ('owner','admin','operator','viewer')),
  totp_secret bytea, totp_pending bytea, totp_last bigint NOT NULL DEFAULT -1,
  recovery_codes jsonb NOT NULL DEFAULT '[]', created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mp_sessions (
  token_hash text PRIMARY KEY, user_id text NOT NULL REFERENCES mp_users(id) ON DELETE CASCADE,
  csrf_token text NOT NULL, expires_at timestamptz NOT NULL
);
CREATE TABLE IF NOT EXISTS mp_server_operators (
  server_id text NOT NULL, user_id text NOT NULL REFERENCES mp_users(id) ON DELETE CASCADE,
  PRIMARY KEY(server_id,user_id)
);
CREATE INDEX IF NOT EXISTS mp_server_operators_user ON mp_server_operators(user_id,server_id);
CREATE TABLE IF NOT EXISTS mp_commands (
  id text PRIMARY KEY, node_id text NOT NULL, server_id text NOT NULL DEFAULT '', job_id text NOT NULL DEFAULT '',
  payload jsonb NOT NULL, state text NOT NULL DEFAULT 'pending', mutating boolean NOT NULL DEFAULT false,
  result jsonb, created_at timestamptz NOT NULL DEFAULT now(), delivered_at timestamptz, deadline timestamptz NOT NULL
);
CREATE INDEX IF NOT EXISTS mp_command_dispatch ON mp_commands(node_id,state,created_at);
ALTER TABLE mp_commands ADD COLUMN IF NOT EXISTS late_result jsonb;
CREATE UNIQUE INDEX IF NOT EXISTS mp_one_server_mutation ON mp_commands(server_id) WHERE mutating AND state IN ('pending','delivered') AND server_id<>'';
CREATE TABLE IF NOT EXISTS mp_work (
  id text PRIMARY KEY, kind text NOT NULL, node_id text NOT NULL, secret bytea,
  state text NOT NULL DEFAULT 'queued', claimed_at timestamptz, expires_at timestamptz NOT NULL DEFAULT now()+interval '15 minutes',
  created_at timestamptz NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS mp_metrics (
  id bigserial PRIMARY KEY, node_id text NOT NULL, server_id text NOT NULL DEFAULT '',
  measured_at timestamptz NOT NULL DEFAULT now(), payload jsonb NOT NULL
);
CREATE INDEX IF NOT EXISTS mp_metrics_lookup ON mp_metrics(server_id,node_id,measured_at DESC);
CREATE TABLE IF NOT EXISTS mp_audit (
  id text PRIMARY KEY, actor_id text NOT NULL, actor text NOT NULL, action text NOT NULL, object_id text NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), detail text NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS mp_idempotency (
  actor_id text NOT NULL, key text NOT NULL, request_hash text NOT NULL, response jsonb NOT NULL,
  created_at timestamptz NOT NULL DEFAULT now(), PRIMARY KEY(actor_id,key)
);
