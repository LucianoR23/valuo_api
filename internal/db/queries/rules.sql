-- Rules that fill a new expense (plan 3.8).

-- name: ListRules :many
SELECT * FROM rules
WHERE household_id = $1
ORDER BY lower(match_text), created_at;

-- name: GetRule :one
SELECT * FROM rules
WHERE id = $1 AND household_id = $2;

-- name: CountRules :one
SELECT count(*) FROM rules WHERE household_id = $1;

-- name: CreateRule :one
INSERT INTO rules (household_id, created_by, match_text, match_key, payment_method_id,
                   min_amount, max_amount, set_category_id, set_payment_method_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: UpdateRule :one
UPDATE rules
SET match_text = $3, match_key = $4, payment_method_id = $5, min_amount = $6,
    max_amount = $7, set_category_id = $8, set_payment_method_id = $9, updated_at = NOW()
WHERE id = $1 AND household_id = $2
RETURNING *;

-- name: DeleteRule :execrows
DELETE FROM rules WHERE id = $1 AND household_id = $2;

-- name: ListRulesForMatch :many
-- The space's rules with what the matcher needs: the space's currency, and
-- the owner and state of the payment method a rule fills.
SELECT r.id, r.match_key, r.payment_method_id, r.min_amount, r.max_amount,
       r.set_category_id, r.set_payment_method_id, r.created_at,
       h.base_currency::text AS base_currency,
       pm.owner_user_id AS set_owner_id,
       COALESCE(pm.is_active, false) AS set_active
FROM rules r
JOIN households h ON h.id = r.household_id
LEFT JOIN payment_methods pm ON pm.id = r.set_payment_method_id
WHERE r.household_id = $1;

-- name: ListRecentDescriptions :many
-- Descriptions of the space's confirmed expenses since a date, most used
-- first: what Cargar gasto offers while typing (plan 3.8).
SELECT description, count(*)::int AS uses, max(spent_at)::date AS last_used
FROM expenses
WHERE household_id = $1 AND status = 'confirmed' AND spent_at >= $2 AND description <> ''
GROUP BY description
ORDER BY uses DESC, last_used DESC
LIMIT 500;
