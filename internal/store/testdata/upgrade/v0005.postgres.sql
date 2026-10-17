--
-- PostgreSQL database dump
--

\restrict Lqxvch9M9jmhd948fSggkUfKrhhKdHfQls8g527yeZZ73zm4qvdlBrDE0Tb1YN9

-- Dumped from database version 16.15
-- Dumped by pg_dump version 16.15

SET statement_timeout = 0;
SET lock_timeout = 0;
SET idle_in_transaction_session_timeout = 0;
SET client_encoding = 'UTF8';
SET standard_conforming_strings = on;
SELECT pg_catalog.set_config('search_path', '', false);
SET check_function_bodies = false;
SET xmloption = content;
SET client_min_messages = warning;
SET row_security = off;

SET default_tablespace = '';

SET default_table_access_method = heap;

--
-- Name: api_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.api_keys (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    mode text NOT NULL,
    prefix text NOT NULL,
    sha256_hash text NOT NULL,
    last_used_at bigint,
    revoked_at bigint,
    created_at bigint NOT NULL,
    CONSTRAINT api_keys_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: app_domains; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_domains (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    host text NOT NULL,
    is_primary integer DEFAULT 0 NOT NULL,
    status text NOT NULL,
    token text NOT NULL,
    last_check_at bigint,
    last_error text,
    created_at bigint NOT NULL,
    CONSTRAINT app_domains_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'active'::text, 'failed'::text])))
);


--
-- Name: app_provider_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_provider_accounts (
    app_id text NOT NULL,
    provider_account_id text NOT NULL
);


--
-- Name: app_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.app_settings (
    app_id text NOT NULL,
    key text NOT NULL,
    value text NOT NULL,
    updated_at bigint NOT NULL
);


--
-- Name: apps; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.apps (
    id text NOT NULL,
    org_id text NOT NULL,
    name text NOT NULL,
    slug text NOT NULL,
    default_return_url text,
    branding text DEFAULT '{}'::text NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);


--
-- Name: audit_log; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.audit_log (
    id text NOT NULL,
    org_id text NOT NULL,
    actor_type text NOT NULL,
    actor_id text,
    action text NOT NULL,
    target text,
    before text,
    after text,
    ip text,
    at bigint NOT NULL,
    CONSTRAINT audit_log_actor_type_check CHECK ((actor_type = ANY (ARRAY['user'::text, 'api_key'::text, 'system'::text])))
);


--
-- Name: blobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.blobs (
    id text NOT NULL,
    org_id text NOT NULL,
    kind text NOT NULL,
    content_type text NOT NULL,
    size bigint NOT NULL,
    sha256 text NOT NULL,
    storage text DEFAULT 'db'::text NOT NULL,
    data bytea,
    created_at bigint NOT NULL
);


--
-- Name: connector_kv; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.connector_kv (
    provider_account_id text NOT NULL,
    key text NOT NULL,
    value_enc bytea NOT NULL,
    expires_at bigint
);


--
-- Name: customers; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.customers (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    mode text NOT NULL,
    external_ref text,
    name text,
    email text,
    phone text,
    billing_legal_name text,
    billing_gstin text,
    billing_line1 text,
    billing_line2 text,
    billing_city text,
    billing_state_code text,
    billing_postal_code text,
    billing_country text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT customers_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.events (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    mode text NOT NULL,
    type text NOT NULL,
    object_type text NOT NULL,
    object_id text NOT NULL,
    object_version bigint NOT NULL,
    data text NOT NULL,
    created_at bigint NOT NULL
);


--
-- Name: idempotency_keys; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.idempotency_keys (
    app_id text NOT NULL,
    mode text NOT NULL,
    key text NOT NULL,
    request_hash text NOT NULL,
    response_status integer DEFAULT 0 NOT NULL,
    response_enc bytea,
    created_at bigint NOT NULL,
    CONSTRAINT idempotency_keys_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: jobs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.jobs (
    id text NOT NULL,
    kind text NOT NULL,
    payload text NOT NULL,
    state text NOT NULL,
    run_at bigint NOT NULL,
    attempts integer DEFAULT 0 NOT NULL,
    max_attempts integer NOT NULL,
    locked_until bigint,
    last_error text,
    dedupe_key text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT jobs_state_check CHECK ((state = ANY (ARRAY['pending'::text, 'running'::text, 'done'::text, 'dead'::text])))
);


--
-- Name: ledger_entries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.ledger_entries (
    id text NOT NULL,
    org_id text NOT NULL,
    mode text NOT NULL,
    kind text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    payment_id text,
    refund_id text,
    dispute_id text,
    settlement_id text,
    occurred_at bigint NOT NULL,
    CONSTRAINT ledger_entries_kind_check CHECK ((kind = ANY (ARRAY['charge'::text, 'refund'::text, 'dispute_hold'::text, 'dispute_release'::text, 'dispute_loss'::text, 'fee'::text, 'fee_tax'::text, 'payout'::text]))),
    CONSTRAINT ledger_entries_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: org_settings; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.org_settings (
    org_id text NOT NULL,
    key text NOT NULL,
    value text NOT NULL,
    updated_at bigint NOT NULL
);


--
-- Name: orgs; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.orgs (
    id text NOT NULL,
    name text NOT NULL,
    created_at bigint NOT NULL
);


--
-- Name: payment_attempts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.payment_attempts (
    id text NOT NULL,
    org_id text NOT NULL,
    mode text NOT NULL,
    payment_id text NOT NULL,
    provider_account_id text NOT NULL,
    provider_ref text NOT NULL,
    provider_order_id text,
    provider_payment_id text,
    status text NOT NULL,
    method text,
    failure_code text,
    failure_message text,
    raw_last text,
    amount_received bigint DEFAULT 0 NOT NULL,
    currency_received text,
    duplicate integer DEFAULT 0 NOT NULL,
    fee bigint,
    fee_tax bigint,
    net bigint,
    mandate_id text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    poll_count integer DEFAULT 0 NOT NULL,
    next_poll_at bigint,
    CONSTRAINT payment_attempts_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: payments; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.payments (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    mode text NOT NULL,
    kind text NOT NULL,
    subscription_id text,
    amount bigint NOT NULL,
    currency text NOT NULL,
    amount_received bigint DEFAULT 0 NOT NULL,
    amount_refunded bigint DEFAULT 0 NOT NULL,
    status text NOT NULL,
    order_ref text,
    description text,
    customer_id text,
    metadata text DEFAULT '{}'::text NOT NULL,
    return_url text,
    client_secret_hash text NOT NULL,
    expires_at bigint NOT NULL,
    version bigint DEFAULT 1 NOT NULL,
    flags text DEFAULT '[]'::text NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT payments_kind_check CHECK ((kind = ANY (ARRAY['one_time'::text, 'renewal'::text, 'mandate_setup'::text]))),
    CONSTRAINT payments_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: provider_accounts; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.provider_accounts (
    id text NOT NULL,
    org_id text NOT NULL,
    mode text NOT NULL,
    connector text NOT NULL,
    name text NOT NULL,
    config_enc bytea,
    status text DEFAULT 'active'::text NOT NULL,
    statement_descriptor text,
    provider_gstin text,
    accepted_currencies text DEFAULT '[]'::text NOT NULL,
    enabled_purposes text DEFAULT '["one_time"]'::text NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT provider_accounts_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: provider_events; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.provider_events (
    id text NOT NULL,
    provider_account_id text NOT NULL,
    provider_event_id text NOT NULL,
    headers text NOT NULL,
    body bytea NOT NULL,
    verified integer NOT NULL,
    received_at bigint NOT NULL,
    processed_at bigint,
    error text
);


--
-- Name: refunds; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.refunds (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    mode text NOT NULL,
    payment_id text NOT NULL,
    attempt_id text NOT NULL,
    amount bigint NOT NULL,
    currency text NOT NULL,
    status text NOT NULL,
    reason text,
    provider_ref text NOT NULL,
    provider_refund_id text,
    initiated_by text NOT NULL,
    actor text,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    poll_count integer DEFAULT 0 NOT NULL,
    next_poll_at bigint,
    CONSTRAINT refunds_initiated_by_check CHECK ((initiated_by = ANY (ARRAY['api'::text, 'dashboard'::text, 'provider'::text, 'system'::text]))),
    CONSTRAINT refunds_mode_check CHECK ((mode = ANY (ARRAY['test'::text, 'live'::text])))
);


--
-- Name: schedules; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schedules (
    name text NOT NULL,
    next_run_at bigint NOT NULL,
    locked_until bigint,
    updated_at bigint NOT NULL
);


--
-- Name: schema_migrations; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.schema_migrations (
    version bigint NOT NULL,
    name text NOT NULL,
    applied_at bigint NOT NULL
);


--
-- Name: sessions; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.sessions (
    id text NOT NULL,
    user_id text NOT NULL,
    token_hash text NOT NULL,
    csrf_key bytea NOT NULL,
    created_at bigint NOT NULL,
    last_seen_at bigint NOT NULL,
    expires_at bigint NOT NULL,
    ip text NOT NULL,
    user_agent text NOT NULL
);


--
-- Name: unknown_provider_objects; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.unknown_provider_objects (
    id text NOT NULL,
    provider_account_id text NOT NULL,
    provider_event_id text NOT NULL,
    kind text NOT NULL,
    provider_id text NOT NULL,
    detail text,
    created_at bigint NOT NULL
);


--
-- Name: users; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.users (
    id text NOT NULL,
    email text NOT NULL,
    password_hash text NOT NULL,
    totp_secret_enc bytea,
    totp_last_step bigint NOT NULL,
    role text NOT NULL,
    disabled integer NOT NULL,
    created_at bigint NOT NULL,
    CONSTRAINT users_role_check CHECK ((role = ANY (ARRAY['admin'::text, 'operator'::text, 'viewer'::text])))
);


--
-- Name: webhook_deliveries; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.webhook_deliveries (
    id text NOT NULL,
    event_id text NOT NULL,
    endpoint_id text NOT NULL,
    status text NOT NULL,
    attempt_count integer NOT NULL,
    next_attempt_at bigint,
    last_status integer,
    last_error text,
    delivered_at bigint,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL,
    CONSTRAINT webhook_deliveries_status_check CHECK ((status = ANY (ARRAY['pending'::text, 'delivered'::text, 'failed'::text])))
);


--
-- Name: webhook_endpoints; Type: TABLE; Schema: public; Owner: -
--

CREATE TABLE public.webhook_endpoints (
    id text NOT NULL,
    org_id text NOT NULL,
    app_id text NOT NULL,
    url text NOT NULL,
    signing_secret_enc bytea NOT NULL,
    event_types text NOT NULL,
    enabled integer NOT NULL,
    created_at bigint NOT NULL,
    updated_at bigint NOT NULL
);


--
-- Data for Name: api_keys; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.api_keys (id, org_id, app_id, mode, prefix, sha256_hash, last_used_at, revoked_at, created_at) VALUES ('key_01up', 'org_default', 'app_01up', 'test', 'pmx_test_sk_up01', '2c26b46b68ffc68ff99b453c1d30413413422d706483bfa0f98a5e886266e7ae', 1760000009000, NULL, 1760000002000);
INSERT INTO public.api_keys (id, org_id, app_id, mode, prefix, sha256_hash, last_used_at, revoked_at, created_at) VALUES ('key_02up', 'org_default', 'app_01up', 'live', 'pmx_live_sk_up02', 'fcde2b2edba56bf408601fb721fe9b5c338d10ee429ea04fae5511b68fbf8fb9', NULL, 1760000008000, 1760000002000);


--
-- Data for Name: app_domains; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.app_domains (id, org_id, app_id, host, is_primary, status, token, last_check_at, last_error, created_at) VALUES ('dom_01up', 'org_default', 'app_01up', 'pay.voicestudio.example.com', 1, 'active', 'tok01up', 1760000006000, NULL, 1760000005500);


--
-- Data for Name: app_provider_accounts; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.app_provider_accounts (app_id, provider_account_id) VALUES ('app_01up', 'pa_01up');


--
-- Data for Name: app_settings; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.app_settings (app_id, key, value, updated_at) VALUES ('app_01up', 'payment_ttl', '45', 1760000004000);


--
-- Data for Name: apps; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.apps (id, org_id, name, slug, default_return_url, branding, created_at, updated_at) VALUES ('app_01up', 'org_default', 'Voice Studio', 'voicestudio', 'https://shop.example.com/thanks', '{"accent":"#0a7"}', 1760000001000, 1760000001000);


--
-- Data for Name: audit_log; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.audit_log (id, org_id, actor_type, actor_id, action, target, before, after, ip, at) VALUES ('aud_01up1', 'org_default', 'system', NULL, 'setting.update', 'org:org_default/payment_ttl', '60', '90', NULL, 1760000004000);


--
-- Data for Name: blobs; Type: TABLE DATA; Schema: public; Owner: -
--



--
-- Data for Name: connector_kv; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.connector_kv (provider_account_id, key, value_enc, expires_at) VALUES ('pa_01up', 'plan_cache', '\xaabbcc00ddeeff', NULL);


--
-- Data for Name: customers; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.customers (id, org_id, app_id, mode, external_ref, name, email, phone, billing_legal_name, billing_gstin, billing_line1, billing_line2, billing_city, billing_state_code, billing_postal_code, billing_country, created_at, updated_at) VALUES ('cus_01up', 'org_default', 'app_01up', 'test', 'u1', 'Asha Rao', 'asha@example.com', '+919999999999', 'Asha Rao', NULL, '1 MG Road', NULL, 'Bengaluru', '29', '560001', 'IN', 1760000003000, 1760000003000);


--
-- Data for Name: events; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.events (id, org_id, app_id, mode, type, object_type, object_id, object_version, data, created_at) VALUES ('evt_01up1', 'org_default', 'app_01up', 'test', 'payment.succeeded', 'payment', 'pay_01up1', 3, '{"id":"pay_01up1","status":"succeeded"}', 1760000050000);


--
-- Data for Name: idempotency_keys; Type: TABLE DATA; Schema: public; Owner: -
--



--
-- Data for Name: jobs; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.jobs (id, kind, payload, state, run_at, attempts, max_attempts, locked_until, last_error, dedupe_key, created_at, updated_at) VALUES ('job_01up1', 'upgrade.fixture', '{"n":1}', 'pending', 1760099999000, 0, 10, NULL, NULL, 'dedupe-up1', 1760000007000, 1760000007000);


--
-- Data for Name: ledger_entries; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, refund_id, dispute_id, settlement_id, occurred_at) VALUES ('led_01up1', 'org_default', 'test', 'charge', 49900, 'INR', 'pay_01up1', NULL, NULL, NULL, 1760000050000);
INSERT INTO public.ledger_entries (id, org_id, mode, kind, amount, currency, payment_id, refund_id, dispute_id, settlement_id, occurred_at) VALUES ('led_01up2', 'org_default', 'test', 'refund', -10000, 'INR', 'pay_01up1', 'ref_01up1', NULL, NULL, 1760000050001);


--
-- Data for Name: org_settings; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.org_settings (org_id, key, value, updated_at) VALUES ('org_default', 'payment_ttl', '90', 1760000004000);
INSERT INTO public.org_settings (org_id, key, value, updated_at) VALUES ('org_default', 'routing', '{"rules":[],"catch_all":{"name":"all","when":{},"action":{"kind":"route","targets":[{"connector":"razorpay"}]}}}', 1760000004000);


--
-- Data for Name: orgs; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.orgs (id, name, created_at) VALUES ('org_default', 'Acme Payments', 1760000000000);


--
-- Data for Name: payment_attempts; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.payment_attempts (id, org_id, mode, payment_id, provider_account_id, provider_ref, provider_order_id, provider_payment_id, status, method, failure_code, failure_message, raw_last, amount_received, currency_received, duplicate, fee, fee_tax, net, mandate_id, created_at, updated_at, poll_count, next_poll_at) VALUES ('att_01up1', 'org_default', 'test', 'pay_01up1', 'pa_01up', 'UPREF00000000000001', 'order_up1', 'pay_up1', 'captured', 'upi', NULL, NULL, NULL, 49900, 'INR', 0, 1178, 180, 48542, NULL, 1760000011000, 1760000050000, 2, NULL);
INSERT INTO public.payment_attempts (id, org_id, mode, payment_id, provider_account_id, provider_ref, provider_order_id, provider_payment_id, status, method, failure_code, failure_message, raw_last, amount_received, currency_received, duplicate, fee, fee_tax, net, mandate_id, created_at, updated_at, poll_count, next_poll_at) VALUES ('att_01up0', 'org_default', 'test', 'pay_01up1', 'pa_01up', 'UPREF00000000000000', 'order_up0', NULL, 'failed', 'card', 'card_declined', 'The card was declined.', NULL, 0, NULL, 0, NULL, NULL, NULL, NULL, 1760000010500, 1760000010900, 0, NULL);


--
-- Data for Name: payments; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.payments (id, org_id, app_id, mode, kind, subscription_id, amount, currency, amount_received, amount_refunded, status, order_ref, description, customer_id, metadata, return_url, client_secret_hash, expires_at, version, flags, created_at, updated_at) VALUES ('pay_01up1', 'org_default', 'app_01up', 'test', 'one_time', NULL, 49900, 'INR', 49900, 10000, 'succeeded', 'ord-1', 'Pro plan', 'cus_01up', '{"plan":"pro","note":"café ₹499"}', 'https://shop.example.com/thanks', 'a3f1c0de', 1760000100000, 4, '["refunded_partial"]', 1760000010000, 1760000050000);
INSERT INTO public.payments (id, org_id, app_id, mode, kind, subscription_id, amount, currency, amount_received, amount_refunded, status, order_ref, description, customer_id, metadata, return_url, client_secret_hash, expires_at, version, flags, created_at, updated_at) VALUES ('pay_01up2', 'org_default', 'app_01up', 'test', 'one_time', NULL, 1000, 'USD', 0, 0, 'created', NULL, NULL, NULL, '{}', NULL, 'b4e2d1ef', 1760009999000, 1, '[]', 1760000060000, 1760000060000);


--
-- Data for Name: provider_accounts; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.provider_accounts (id, org_id, mode, connector, name, config_enc, status, statement_descriptor, provider_gstin, accepted_currencies, enabled_purposes, created_at, updated_at) VALUES ('pa_01up', 'org_default', 'test', 'razorpay', 'main', '\x00deadbeef01ff7f', 'active', 'VOICESTUDIO', '29AAAAA0000A1Z5', '["INR"]', '["one_time"]', 1760000001500, 1760000001500);


--
-- Data for Name: provider_events; Type: TABLE DATA; Schema: public; Owner: -
--



--
-- Data for Name: refunds; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.refunds (id, org_id, app_id, mode, payment_id, attempt_id, amount, currency, status, reason, provider_ref, provider_refund_id, initiated_by, actor, created_at, updated_at, poll_count, next_poll_at) VALUES ('ref_01up1', 'org_default', 'app_01up', 'test', 'pay_01up1', 'att_01up1', 10000, 'INR', 'succeeded', 'customer request', 'UPREFUND00000000001', 'rfnd_up1', 'api', NULL, 1760000040000, 1760000050000, 1, NULL);


--
-- Data for Name: schedules; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.schedules (name, next_run_at, locked_until, updated_at) VALUES ('system.heartbeat', 1760000060000, NULL, 1760000000000);


--
-- Data for Name: schema_migrations; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.schema_migrations (version, name, applied_at) VALUES (1, '0001_init.sql', 1792244411501);
INSERT INTO public.schema_migrations (version, name, applied_at) VALUES (2, '0002_core.sql', 1792244411690);
INSERT INTO public.schema_migrations (version, name, applied_at) VALUES (3, '0003_webhooks.sql', 1792244411742);
INSERT INTO public.schema_migrations (version, name, applied_at) VALUES (4, '0004_auth.sql', 1792244411774);
INSERT INTO public.schema_migrations (version, name, applied_at) VALUES (5, '0005_connectors.sql', 1792244411821);


--
-- Data for Name: sessions; Type: TABLE DATA; Schema: public; Owner: -
--



--
-- Data for Name: unknown_provider_objects; Type: TABLE DATA; Schema: public; Owner: -
--



--
-- Data for Name: users; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.users (id, email, password_hash, totp_secret_enc, totp_last_step, role, disabled, created_at) VALUES ('usr_01up', 'admin@example.com', '$argon2id$v=19$m=65536,t=3,p=4$c2FsdHNhbHQ$aGFzaGhhc2hoYXNoaGFzaA', NULL, 0, 'admin', 0, 1760000005000);


--
-- Data for Name: webhook_deliveries; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.webhook_deliveries (id, event_id, endpoint_id, status, attempt_count, next_attempt_at, last_status, last_error, delivered_at, created_at, updated_at) VALUES ('whd_01up1', 'evt_01up1', 'whe_01up', 'delivered', 1, NULL, 200, NULL, 1760000051000, 1760000050000, 1760000051000);


--
-- Data for Name: webhook_endpoints; Type: TABLE DATA; Schema: public; Owner: -
--

INSERT INTO public.webhook_endpoints (id, org_id, app_id, url, signing_secret_enc, event_types, enabled, created_at, updated_at) VALUES ('whe_01up', 'org_default', 'app_01up', 'https://shop.example.com/hooks', '\x0102030405060708', '["*"]', 1, 1760000005000, 1760000005000);


--
-- Name: api_keys api_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_pkey PRIMARY KEY (id);


--
-- Name: api_keys api_keys_prefix_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.api_keys
    ADD CONSTRAINT api_keys_prefix_key UNIQUE (prefix);


--
-- Name: app_domains app_domains_host_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_domains
    ADD CONSTRAINT app_domains_host_key UNIQUE (host);


--
-- Name: app_domains app_domains_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_domains
    ADD CONSTRAINT app_domains_pkey PRIMARY KEY (id);


--
-- Name: app_provider_accounts app_provider_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_provider_accounts
    ADD CONSTRAINT app_provider_accounts_pkey PRIMARY KEY (app_id, provider_account_id);


--
-- Name: app_settings app_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.app_settings
    ADD CONSTRAINT app_settings_pkey PRIMARY KEY (app_id, key);


--
-- Name: apps apps_org_id_slug_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_org_id_slug_key UNIQUE (org_id, slug);


--
-- Name: apps apps_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.apps
    ADD CONSTRAINT apps_pkey PRIMARY KEY (id);


--
-- Name: audit_log audit_log_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.audit_log
    ADD CONSTRAINT audit_log_pkey PRIMARY KEY (id);


--
-- Name: blobs blobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.blobs
    ADD CONSTRAINT blobs_pkey PRIMARY KEY (id);


--
-- Name: connector_kv connector_kv_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.connector_kv
    ADD CONSTRAINT connector_kv_pkey PRIMARY KEY (provider_account_id, key);


--
-- Name: customers customers_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.customers
    ADD CONSTRAINT customers_pkey PRIMARY KEY (id);


--
-- Name: events events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.events
    ADD CONSTRAINT events_pkey PRIMARY KEY (id);


--
-- Name: idempotency_keys idempotency_keys_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.idempotency_keys
    ADD CONSTRAINT idempotency_keys_pkey PRIMARY KEY (app_id, mode, key);


--
-- Name: jobs jobs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.jobs
    ADD CONSTRAINT jobs_pkey PRIMARY KEY (id);


--
-- Name: ledger_entries ledger_entries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.ledger_entries
    ADD CONSTRAINT ledger_entries_pkey PRIMARY KEY (id);


--
-- Name: org_settings org_settings_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.org_settings
    ADD CONSTRAINT org_settings_pkey PRIMARY KEY (org_id, key);


--
-- Name: orgs orgs_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.orgs
    ADD CONSTRAINT orgs_pkey PRIMARY KEY (id);


--
-- Name: payment_attempts payment_attempts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_attempts
    ADD CONSTRAINT payment_attempts_pkey PRIMARY KEY (id);


--
-- Name: payment_attempts payment_attempts_provider_ref_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payment_attempts
    ADD CONSTRAINT payment_attempts_provider_ref_key UNIQUE (provider_ref);


--
-- Name: payments payments_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.payments
    ADD CONSTRAINT payments_pkey PRIMARY KEY (id);


--
-- Name: provider_accounts provider_accounts_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_accounts
    ADD CONSTRAINT provider_accounts_pkey PRIMARY KEY (id);


--
-- Name: provider_events provider_events_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_events
    ADD CONSTRAINT provider_events_pkey PRIMARY KEY (id);


--
-- Name: provider_events provider_events_provider_account_id_provider_event_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.provider_events
    ADD CONSTRAINT provider_events_provider_account_id_provider_event_id_key UNIQUE (provider_account_id, provider_event_id);


--
-- Name: refunds refunds_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refunds
    ADD CONSTRAINT refunds_pkey PRIMARY KEY (id);


--
-- Name: refunds refunds_provider_ref_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.refunds
    ADD CONSTRAINT refunds_provider_ref_key UNIQUE (provider_ref);


--
-- Name: schedules schedules_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schedules
    ADD CONSTRAINT schedules_pkey PRIMARY KEY (name);


--
-- Name: schema_migrations schema_migrations_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.schema_migrations
    ADD CONSTRAINT schema_migrations_pkey PRIMARY KEY (version);


--
-- Name: sessions sessions_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_pkey PRIMARY KEY (id);


--
-- Name: sessions sessions_token_hash_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.sessions
    ADD CONSTRAINT sessions_token_hash_key UNIQUE (token_hash);


--
-- Name: unknown_provider_objects unknown_provider_objects_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.unknown_provider_objects
    ADD CONSTRAINT unknown_provider_objects_pkey PRIMARY KEY (id);


--
-- Name: unknown_provider_objects unknown_provider_objects_provider_account_id_provider_event_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.unknown_provider_objects
    ADD CONSTRAINT unknown_provider_objects_provider_account_id_provider_event_key UNIQUE (provider_account_id, provider_event_id, kind, provider_id);


--
-- Name: users users_email_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_email_key UNIQUE (email);


--
-- Name: users users_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.users
    ADD CONSTRAINT users_pkey PRIMARY KEY (id);


--
-- Name: webhook_deliveries webhook_deliveries_event_id_endpoint_id_key; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_event_id_endpoint_id_key UNIQUE (event_id, endpoint_id);


--
-- Name: webhook_deliveries webhook_deliveries_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.webhook_deliveries
    ADD CONSTRAINT webhook_deliveries_pkey PRIMARY KEY (id);


--
-- Name: webhook_endpoints webhook_endpoints_pkey; Type: CONSTRAINT; Schema: public; Owner: -
--

ALTER TABLE ONLY public.webhook_endpoints
    ADD CONSTRAINT webhook_endpoints_pkey PRIMARY KEY (id);


--
-- Name: api_keys_app; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX api_keys_app ON public.api_keys USING btree (app_id);


--
-- Name: app_domains_app; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX app_domains_app ON public.app_domains USING btree (app_id);


--
-- Name: audit_log_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX audit_log_at ON public.audit_log USING btree (org_id, at);


--
-- Name: customers_external_ref; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX customers_external_ref ON public.customers USING btree (app_id, mode, external_ref) WHERE (external_ref IS NOT NULL);


--
-- Name: events_app_created; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX events_app_created ON public.events USING btree (app_id, created_at);


--
-- Name: jobs_dedupe; Type: INDEX; Schema: public; Owner: -
--

CREATE UNIQUE INDEX jobs_dedupe ON public.jobs USING btree (dedupe_key) WHERE ((dedupe_key IS NOT NULL) AND (state = ANY (ARRAY['pending'::text, 'running'::text])));


--
-- Name: jobs_state_run_at; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX jobs_state_run_at ON public.jobs USING btree (state, run_at);


--
-- Name: ledger_entries_payment; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX ledger_entries_payment ON public.ledger_entries USING btree (payment_id);


--
-- Name: payment_attempts_payment; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX payment_attempts_payment ON public.payment_attempts USING btree (payment_id);


--
-- Name: payment_attempts_provider_order; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX payment_attempts_provider_order ON public.payment_attempts USING btree (provider_account_id, provider_order_id);


--
-- Name: payment_attempts_provider_payment; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX payment_attempts_provider_payment ON public.payment_attempts USING btree (provider_account_id, provider_payment_id);


--
-- Name: payments_app_list; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX payments_app_list ON public.payments USING btree (app_id, mode, id);


--
-- Name: payments_order_ref; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX payments_order_ref ON public.payments USING btree (app_id, mode, order_ref);


--
-- Name: refunds_attempt; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX refunds_attempt ON public.refunds USING btree (attempt_id);


--
-- Name: refunds_payment; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX refunds_payment ON public.refunds USING btree (payment_id);


--
-- Name: refunds_provider_refund; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX refunds_provider_refund ON public.refunds USING btree (attempt_id, provider_refund_id);


--
-- Name: sessions_expires; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_expires ON public.sessions USING btree (expires_at);


--
-- Name: sessions_user; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX sessions_user ON public.sessions USING btree (user_id);


--
-- Name: webhook_deliveries_endpoint; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX webhook_deliveries_endpoint ON public.webhook_deliveries USING btree (endpoint_id, status, updated_at);


--
-- Name: webhook_endpoints_app; Type: INDEX; Schema: public; Owner: -
--

CREATE INDEX webhook_endpoints_app ON public.webhook_endpoints USING btree (app_id);


--
-- PostgreSQL database dump complete
--

\unrestrict Lqxvch9M9jmhd948fSggkUfKrhhKdHfQls8g527yeZZ73zm4qvdlBrDE0Tb1YN9

