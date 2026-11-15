-- 000050_rules (plan 3.8): rules that fill a new expense.
--
-- A rule belongs to a space (a household: "Mi plata" or the Hogar). When the
-- description has match_key as whole words (textnorm.Key: lower case, no
-- accents), it fills the category, the payment method or both. Optional
-- conditions: a payment method and an amount range in the space's currency.
-- When several rules match, the one with more conditions wins, then the
-- longer text, then the newest (decided with the user on 2026-11-14).
--
-- Payment methods belong to a person: the payment method of a Hogar rule is
-- only used for its owner. Deleting a category or a payment method deletes
-- its rules.
CREATE TABLE rules (
    id                    UUID          PRIMARY KEY DEFAULT gen_random_uuid(),
    household_id          UUID          NOT NULL REFERENCES households(id) ON DELETE CASCADE,
    created_by            UUID          NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    match_text            TEXT          NOT NULL CHECK (char_length(match_text) BETWEEN 2 AND 60),
    match_key             TEXT          NOT NULL CHECK (char_length(match_key) BETWEEN 2 AND 60),
    payment_method_id     UUID          REFERENCES payment_methods(id) ON DELETE CASCADE,
    min_amount            NUMERIC(14,2) CHECK (min_amount > 0),
    max_amount            NUMERIC(14,2) CHECK (max_amount > 0),
    set_category_id       UUID          REFERENCES categories(id) ON DELETE CASCADE,
    set_payment_method_id UUID          REFERENCES payment_methods(id) ON DELETE CASCADE,
    created_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ   NOT NULL DEFAULT NOW(),
    CONSTRAINT rules_sets_something CHECK (set_category_id IS NOT NULL OR set_payment_method_id IS NOT NULL),
    CONSTRAINT rules_amount_range CHECK (min_amount IS NULL OR max_amount IS NULL OR min_amount <= max_amount),
    CONSTRAINT rules_unique UNIQUE NULLS NOT DISTINCT (household_id, match_key, payment_method_id, min_amount, max_amount)
);

CREATE INDEX idx_rules_created_by ON rules (created_by);
CREATE INDEX idx_rules_payment_method ON rules (payment_method_id);
CREATE INDEX idx_rules_set_category ON rules (set_category_id);
CREATE INDEX idx_rules_set_payment_method ON rules (set_payment_method_id);
