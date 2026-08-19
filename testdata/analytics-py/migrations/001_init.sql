-- analytics events storage
CREATE TABLE IF NOT EXISTS analytics_events (
    id BIGSERIAL PRIMARY KEY,
    user_id TEXT NOT NULL,
    amount NUMERIC(10, 2),
    created_at TIMESTAMP DEFAULT now()
);

CREATE INDEX idx_analytics_events_user ON analytics_events (user_id);
