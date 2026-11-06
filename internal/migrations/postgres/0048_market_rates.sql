-- Indicative exchange rates from a public provider, shown on the dashboard only.
-- Kept apart from fx_rates, which invoices and GST read. Not per org: they are
-- public market data.
CREATE TABLE market_rates (
    currency    TEXT NOT NULL,
    rate_date   TEXT NOT NULL,
    rate_to_inr TEXT NOT NULL,
    source      TEXT NOT NULL,
    fetched_at  BIGINT NOT NULL,
    PRIMARY KEY (currency, rate_date)
);
