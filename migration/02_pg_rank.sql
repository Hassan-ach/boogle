CREATE OR REPLACE FUNCTION update_page_rank(
    max_iterations INTEGER DEFAULT 20,
    damping_factor FLOAT DEFAULT 0.85
) 
RETURNS TABLE (url_id UUID, rank FLOAT) AS $$
DECLARE node_num INT;
BEGIN

    SELECT COUNT(*) INTO node_num FROM urls;

    IF node_num = 0 THEN 
        RETURN;
    END IF;
    
    CREATE TEMP TABLE temp_page_rank (
        url_id UUID PRIMARY KEY,
        score FLOAT NOT NULL
    ) ON COMMIT DROP;

    TRUNCATE temp_page_rank;
    INSERT INTO temp_page_rank (url_id, score)
    SELECT id, 1.0 / node_num FROM urls;

    FOR i IN 1..max_iterations LOOP
    WITH calculated AS (
        SELECT
            u.id,
            ((1.0 - damping_factor) / node_num) +
            (damping_factor * COALESCE(SUM(tpr.score / out_deg.cnt), 0.0)) AS new_score
        FROM urls u
        LEFT JOIN graph_edges ge ON u.id = ge.to_url
        LEFT JOIN temp_page_rank tpr ON ge.from_url = tpr.url_id
        LEFT JOIN (
            SELECT from_url, COUNT(*) AS cnt
            FROM graph_edges
            GROUP BY from_url
        ) out_deg ON ge.from_url = out_deg.from_url
        GROUP BY u.id
    )
    UPDATE temp_page_rank tpr
    SET score = c.new_score
    FROM calculated c
    WHERE tpr.url_id = c.id;
    END LOOP;

    INSERT INTO page_rank (url_id, score, updated_at)
    SELECT url_id, score, NOW() FROM temp_page_rank
    ON CONFLICT (url_id) 
    DO UPDATE SET score = EXCLUDED.score, updated_at = NOW();
    
    RETURN QUERY
    SELECT url_id, score FROM temp_page_rank ORDER BY score DESC;
END;
$$ LANGUAGE plpgsql;

