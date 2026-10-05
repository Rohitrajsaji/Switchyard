SELECT jsonb_build_object(
    'cohorts',COALESCE((SELECT jsonb_agg(row_to_json(c)) FROM (
        SELECT variant_id,finalized,count(*) AS exposed,count(*) FILTER(WHERE converted) AS converted
        FROM contributions GROUP BY variant_id,finalized
    ) c),'[]'::jsonb),
    'requests',COALESCE((SELECT jsonb_agg(row_to_json(r)) FROM (
        SELECT variant_id,count(*) AS count,count(*) FILTER(WHERE is_error) AS errors
        FROM request_samples GROUP BY variant_id
    ) r),'[]'::jsonb),
    'buckets',COALESCE((SELECT jsonb_agg(row_to_json(b)) FROM (
        SELECT variant_id,upper_bound_ms,count(*) AS count FROM request_samples GROUP BY variant_id,upper_bound_ms
    ) b),'[]'::jsonb),
    'quality',jsonb_build_object(
        'quarantined_events',(SELECT count(*) FROM facts WHERE status='quarantined'),
        'future_events',(SELECT count(*) FROM facts WHERE status='accepted' AND occurred_at>$4),
        'pending_outcomes',(SELECT count(*) FROM linked WHERE reference_status='pending'),
        'invalid_reference_outcomes',(SELECT count(*) FROM linked WHERE reference_status='invalid_reference'),
        'outside_window_completions',(SELECT count(*) FROM linked e JOIN anchors a USING(user_id,variant_id)
            WHERE e.kind='listing_completion' AND e.reference_status='valid'
            AND NOT (e.occurred_at>=a.occurred_at AND e.occurred_at<=a.occurred_at+interval '30 minutes')),
        'duplicate_attributed_completions',(SELECT count(*) FROM attributed_completions)-(SELECT count(*) FROM converted_users)
    )
)
