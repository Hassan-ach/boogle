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
        RETURN;
    END IF;

    -- PageRank values
    -- url_id is used because graph_edges.from_url/to_url reference urls.id
    CREATE TEMP TABLE temp_page_rank (
        url_id UUID PRIMARY KEY,
        score FLOAT NOT NULL
    ) ON COMMIT DROP;

    INSERT INTO temp_page_rank (url_id, score)
    SELECT
        p.url_id,
        1.0 / node_num
    FROM pages p
    WHERE p.indexed = TRUE;

    -- Calculate out-degree once
    CREATE TEMP TABLE temp_out_degree (
        from_url UUID PRIMARY KEY,
        cnt FLOAT NOT NULL
    ) ON COMMIT DROP;

    INSERT INTO temp_out_degree (from_url, cnt)
    SELECT
        from_url,
        COUNT(*)::FLOAT
    FROM graph_edges
    GROUP BY from_url;

    -- PageRank iterations
    FOR i IN 1..max_iterations LOOP

        -- Total rank belonging to dangling nodes
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

            LEFT JOIN graph_edges ge
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
