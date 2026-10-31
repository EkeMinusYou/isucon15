-- These lines are delivered with the final log snapshot, not as live telemetry.
-- A reported day population does not explain the load controller's decisions.
create or replace view isureal_daily_population as
select run_id, line_number, source_artifact,
    try_cast(regexp_extract(line, '^time=([^ ]+)', 1) as timestamptz) as occurred_at,
    regexp_extract(line, 'msg="(asia|europe|america) ', 1) as region,
    regexp_extract(line, 'msg="(?:asia|europe|america) (.+): [0-9]+ 人"', 1) as day_label,
    try_cast(regexp_extract(line, ': ([0-9]+) 人"', 1) as double) as users
from isureal_bench_lines
where regexp_matches(line,
    '^time=[^ ]+ level=INFO msg="(asia|europe|america) (?:[0-9]+ 日目|最終日（グランプリ）): [0-9]+ 人"$');

-- The final grand prix line reports three region counts, including valid zeros.
create or replace view isureal_grand_prix_population as
with parsed as (
    select run_id, line_number, source_artifact,
        regexp_extract(line,
            '^time=([^ ]+) level=INFO msg="グランプリ: アジア ([0-9]+) 人, ヨーロッパ ([0-9]+) 人, アメリカ ([0-9]+) 人"$',
            ['at', 'asia', 'europe', 'america']) as captured
    from isureal_bench_lines
    where regexp_matches(line,
        '^time=[^ ]+ level=INFO msg="グランプリ: アジア [0-9]+ 人, ヨーロッパ [0-9]+ 人, アメリカ [0-9]+ 人"$')
)
select run_id, line_number, source_artifact,
    try_cast(captured.at as timestamptz) as occurred_at,
    region, try_cast(users as double) as users
from parsed, lateral (values
    ('asia', captured.asia), ('europe', captured.europe), ('america', captured.america)
) regions(region, users);

create or replace view bench_progress as
with events as (
    select run_id, line_number, source_artifact, occurred_at,
        'daily_users.' || region || '.' || day_label as metric, users as value
    from isureal_daily_population
    union all
    select run_id, line_number, source_artifact, occurred_at,
        'grand_prix_users.' || region as metric, users as value
    from isureal_grand_prix_population
)
select p.run_id, p.line_number,
    case when w.status = 'ok' then epoch(p.occurred_at - w.started_at) end::double as elapsed_s,
    p.metric, p.value, 'reported_user_count' as semantics, p.source_artifact
from events p left join load_windows w using (run_id);
