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
    --
    -- Both working tables are created with DROP ... IF EXISTS first. They are
    -- ON COMMIT DROP, so within a single transaction they outlive the first
    -- call and the second call used to fail with
    --   relation "temp_page_rank" already exists
    -- which the ranking service treats as a non-retryable error. Anything that
    -- ran the function twice in one transaction -- a backfill, a repair script,
    -- a test -- was simply broken.
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

    -- The out-degree table must be built from exactly the edge set the rank
    -- table is built from, or rank is not conserved.
    --
    -- It used to be `SELECT from_url, COUNT(*) FROM graph_edges GROUP BY
    -- from_url`, i.e. every edge in the table. A page linking to ten targets
    -- where only three are indexed therefore divided its rank by ten, and the
    -- seven tenths pointing at unindexed pages vanished: `dangling_rank` never
    -- saw them either, because the node *did* have a row in the out-degree
    -- table. On the live index the total came to about 0.771, so a quarter of
    -- all ranking signal was being deleted on every run and each recalculation
    -- dragged every page's score further toward zero.
    --
    -- Restricting the edge set to indexed -> indexed fixes both halves at once:
    -- the surviving links get a correct denominator, and links into unindexed
    -- pages leave their source with no *indexed* out-degree, which is exactly
    -- the dangling condition the redistribution term is defined over.
    CREATE TEMP TABLE IF NOT EXISTS temp_out_degree (
        from_url UUID PRIMARY KEY,
        cnt FLOAT NOT NULL
    ) ON COMMIT DROP;

    TRUNCATE temp_out_degree;

    -- The same restricted edge set is reused by the main join below. It has to
    -- be one set, not two look-alikes: if the denominator and the numerator
    -- disagree about which edges exist, the leak comes straight back.
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

        -- Total rank belonging to dangling nodes: indexed pages with no indexed
        -- out-edge. Rank left on the floor is handed back through the teleport
        -- term below, which is what keeps the total at exactly 1.0.
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

    -- A page that was unindexed between the node count and here would leave a
    -- row behind, and one that became indexed would be missing. Dropping the
    -- first kind keeps page_rank from accumulating scores for pages that are no
    -- longer part of the ranking.
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
