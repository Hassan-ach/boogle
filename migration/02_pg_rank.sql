CREATE OR REPLACE FUNCTION update_page_rank(
    max_iterations INTEGER DEFAULT 20,
    damping_factor FLOAT DEFAULT 0.85
)
RETURNS TABLE (
    out_url_id UUID,
    out_rank FLOAT
) AS $$
DECLARE
    node_num INT;
    dangling_rank FLOAT;
BEGIN

    -- Number of indexed pages
    SELECT COUNT(*)
    INTO node_num
    FROM pages
    WHERE indexed = TRUE;

    IF node_num = 0 THEN
        -- The early return skips the final cleanup, so clear stale scores here.
        DELETE FROM page_rank;
        RETURN;
    END IF;

    -- PageRank values
    -- url_id matches graph_edges' url references. IF NOT EXISTS plus TRUNCATE
    -- lets repeated calls in one transaction reuse the temp tables.
    CREATE TEMP TABLE IF NOT EXISTS temp_page_rank (
        url_id UUID PRIMARY KEY,
        score FLOAT NOT NULL
    ) ON COMMIT DROP;

    TRUNCATE temp_page_rank;

    INSERT INTO temp_page_rank (url_id, score)
    SELECT
        p.url_id,
        1.0 / node_num
    FROM pages p
    WHERE p.indexed = TRUE;

    -- Out-degree and the rank join must use the same indexed -> indexed edge
    -- set: unindexed targets would inflate the denominator and drop rank signal.
    CREATE TEMP TABLE IF NOT EXISTS temp_out_degree (
        from_url UUID PRIMARY KEY,
        cnt FLOAT NOT NULL
    ) ON COMMIT DROP;

    TRUNCATE temp_out_degree;

    -- One restricted edge set feeds both the denominator and the main join.
    CREATE TEMP TABLE IF NOT EXISTS temp_graph_edges (
        from_url UUID NOT NULL,
        to_url   UUID NOT NULL
    ) ON COMMIT DROP;

    TRUNCATE temp_graph_edges;

    INSERT INTO temp_graph_edges (from_url, to_url)
    SELECT ge.from_url, ge.to_url
    FROM graph_edges ge
    JOIN pages src ON src.url_id = ge.from_url AND src.indexed = TRUE
    JOIN pages dst ON dst.url_id = ge.to_url   AND dst.indexed = TRUE;

    INSERT INTO temp_out_degree (from_url, cnt)
    SELECT
        from_url,
        COUNT(*)::FLOAT
    FROM temp_graph_edges
    GROUP BY from_url;

    -- PageRank iterations
    FOR i IN 1..max_iterations LOOP

        -- Dangling rank: indexed pages with no indexed out-edge. It is
        -- redistributed below so the total stays at 1.0.
        SELECT COALESCE(SUM(tpr.score), 0.0)
        INTO dangling_rank
        FROM temp_page_rank tpr
        LEFT JOIN temp_out_degree od
            ON tpr.url_id = od.from_url
        WHERE od.from_url IS NULL;

        WITH calculated AS (
            SELECT
                p.url_id,

                -- Teleportation
                ((1.0 - damping_factor) / node_num)

                -- Redistribute dangling-node rank
                + (
                    damping_factor
                    * dangling_rank
                    / node_num
                )

                -- Rank coming from incoming links
                + (
                    damping_factor
                    * COALESCE(
                        SUM(tpr.score / od.cnt),
                        0.0
                    )
                ) AS new_score

            FROM pages p

            LEFT JOIN temp_graph_edges ge
                ON p.url_id = ge.to_url

            LEFT JOIN temp_page_rank tpr
                ON ge.from_url = tpr.url_id

            LEFT JOIN temp_out_degree od
                ON ge.from_url = od.from_url

            WHERE p.indexed = TRUE

            GROUP BY p.url_id
        )

        UPDATE temp_page_rank tpr
        SET score = c.new_score
        FROM calculated c
        WHERE tpr.url_id = c.url_id;

    END LOOP;

    -- Remove ranks for pages that are no longer indexed before saving new ones.
    DELETE FROM page_rank
    WHERE url_id NOT IN (SELECT url_id FROM temp_page_rank);

    -- Save results
    INSERT INTO page_rank (
        url_id,
        score,
        updated_at
    )
    SELECT
        tpr.url_id,
        tpr.score,
        NOW()
    FROM temp_page_rank tpr

    ON CONFLICT (url_id)
    DO UPDATE SET
        score = EXCLUDED.score,
        updated_at = NOW();

    -- Return results
    RETURN QUERY
    SELECT
        tpr.url_id,
        tpr.score
    FROM temp_page_rank tpr
    ORDER BY tpr.score DESC;

END;
$$ LANGUAGE plpgsql;
