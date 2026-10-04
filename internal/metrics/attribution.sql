WITH facts AS MATERIALIZED (
    SELECT * FROM raw_events
    WHERE project_id=$1 AND environment_id=$2 AND run_id=$3 AND received_at<=$4
), accepted AS MATERIALIZED (
    SELECT * FROM facts WHERE status='accepted' AND occurred_at<=$4
), anchors AS MATERIALIZED (
    SELECT DISTINCT ON (user_id) user_id,variant_id,event_id,occurred_at
    FROM accepted WHERE kind='exposure'
    ORDER BY user_id,occurred_at,event_id COLLATE "C"
), linked AS MATERIALIZED (
    SELECT e.*,
           CASE WHEN x.event_id IS NULL THEN 'pending'
                WHEN x.kind<>'exposure' OR x.status<>'accepted' OR x.run_id<>e.run_id
                     OR x.user_id<>e.user_id OR x.variant_id<>e.variant_id
                     OR e.occurred_at<x.occurred_at THEN 'invalid_reference'
                ELSE 'valid' END AS reference_status
    FROM accepted e
    LEFT JOIN raw_events x ON x.project_id=e.project_id AND x.environment_id=e.environment_id
         AND x.event_id=e.exposure_id AND x.received_at<=$4
    WHERE e.kind<>'exposure'
), attributed_completions AS MATERIALIZED (
    SELECT e.* FROM linked e JOIN anchors a ON a.user_id=e.user_id AND a.variant_id=e.variant_id
    WHERE e.kind='listing_completion' AND e.reference_status='valid'
          AND e.occurred_at>=a.occurred_at AND e.occurred_at<=a.occurred_at+interval '30 minutes'
), converted_users AS MATERIALIZED (
    SELECT DISTINCT user_id FROM attributed_completions
), contributions AS MATERIALIZED (
    SELECT a.*,
           (a.occurred_at + interval '30 minutes' + interval '24 hours' < $4) AS finalized,
           (c.user_id IS NOT NULL) AS converted
    FROM anchors a LEFT JOIN converted_users c USING(user_id)
), request_samples AS MATERIALIZED (
    SELECT e.*,
           CASE WHEN latency_ms<=50 THEN 50 WHEN latency_ms<=100 THEN 100
                WHEN latency_ms<=250 THEN 250 WHEN latency_ms<=500 THEN 500
                WHEN latency_ms<=1000 THEN 1000 WHEN latency_ms<=2500 THEN 2500
                WHEN latency_ms<=5000 THEN 5000 WHEN latency_ms<=10000 THEN 10000
                ELSE 60000 END AS upper_bound_ms
    FROM linked e WHERE e.kind='request_outcome' AND e.reference_status='valid'
)
