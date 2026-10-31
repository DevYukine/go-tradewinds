-- +goose Up
CREATE TABLE snapshot_routes (
    account_id text NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    route_key text NOT NULL,
    data jsonb NOT NULL,
    PRIMARY KEY (account_id, route_key)
);

INSERT INTO snapshot_routes (account_id, route_key, data)
SELECT snapshots.account_id, route.key, route.value
FROM snapshots
CROSS JOIN LATERAL jsonb_each(
    CASE WHEN jsonb_typeof(data -> 'routes') = 'object' THEN data -> 'routes' ELSE '{}'::jsonb END
) AS route;

UPDATE snapshots SET data = data - 'routes' WHERE data ? 'routes';

-- +goose Down
UPDATE snapshots
SET data = snapshots.data || jsonb_build_object('routes', routes.data)
FROM (
    SELECT account_id, jsonb_object_agg(route_key, data) AS data
    FROM snapshot_routes
    GROUP BY account_id
) AS routes
WHERE snapshots.account_id = routes.account_id;

DROP TABLE snapshot_routes;
