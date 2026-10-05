WITH counts AS MATERIALIZED (
    SELECT * FROM metric_counts WHERE project_id=$1 AND environment_id=$2 AND run_id=$3
)
SELECT jsonb_build_object(
    'cohorts',COALESCE((SELECT jsonb_agg(row_to_json(c)) FROM (
        SELECT variant_id,(category='finalized') AS finalized,
               COALESCE(sum(value) FILTER(WHERE metric='exposed'),0) AS exposed,
               COALESCE(sum(value) FILTER(WHERE metric='converted'),0) AS converted
        FROM counts WHERE category IN ('provisional','finalized') GROUP BY variant_id,category
    ) c),'[]'::jsonb),
    'requests',COALESCE((SELECT jsonb_agg(row_to_json(r)) FROM (
        SELECT variant_id,COALESCE(sum(value) FILTER(WHERE metric='count'),0) AS count,
               COALESCE(sum(value) FILTER(WHERE metric='errors'),0) AS errors
        FROM counts WHERE category='request' GROUP BY variant_id
    ) r),'[]'::jsonb),
    'buckets',COALESCE((SELECT jsonb_agg(row_to_json(b)) FROM (
        SELECT variant_id,bucket AS upper_bound_ms,value AS count FROM counts WHERE category='latency'
    ) b),'[]'::jsonb),
    'quality',COALESCE((SELECT jsonb_object_agg(metric,value) FROM counts WHERE category='quality'),'{}'::jsonb)
)
