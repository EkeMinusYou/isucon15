-- Participant-visible ISUReal output. Summary messages have no occurrence time.
create table if not exists bench_error_logs (
    run_id varchar, content varchar, source_artifact varchar
);

create or replace view isureal_bench_summaries as
select run_id, source_artifact,
    try_cast(regexp_extract(content,
        '(?s)(\{\s*"pass"\s*:\s*(?:true|false),\s*"score".*?\n\})', 1) as json) as result
from bench_error_logs;

create or replace view bench_results as
select run_id,
    try_cast(json_extract_string(result, '$.score') as bigint) as score,
    try_cast(json_extract_string(result, '$.pass') as boolean) as passed,
    cast(null as bigint) as addition,
    cast(null as bigint) as deduction,
    source_artifact
from isureal_bench_summaries where result is not null;

-- No route-level score breakdown is exposed in the captured participant output.
create or replace view bench_score_routes as
select cast(null as varchar) as run_id, cast(null as varchar) as method,
    cast(null as varchar) as route, cast(null as bigint) as points,
    cast(null as varchar) as source_artifact where false;

create or replace view bench_errors as
select s.run_id, cast(null as bigint) as line_number,
    cast(null as time) as reported_time,
    cast(e.key as bigint) + 1 as error_index,
    json_extract_string(e.value, '$') as message,
    'completion_summary' as time_semantics, s.source_artifact
from isureal_bench_summaries s,
    lateral json_each(json_extract(s.result, '$.messages')) e;

create or replace view isureal_bench_lines as
select run_id, source_artifact,
    unnest(string_split(content, chr(10))) as line,
    generate_subscripts(string_split(content, chr(10)), 1)::bigint as line_number,
    try_cast(regexp_extract(content,
        '(?m)^([^\t\n]+)\tBENCHMARK_START\s*$', 1) as timestamptz) as benchmark_started_at
from bench_error_logs;

-- Only explicitly timestamped warning/error log lines have an event time.
create or replace view bench_warning_events as
select run_id,
    try_cast(regexp_extract(line, '^time=([^ ]+)', 1) as timestamptz) as occurred_at
from isureal_bench_lines
where regexp_matches(line, '^time=[^ ]+ level=(WARN|ERROR) ');

-- A summary message list is not a cumulative time series of all failures.
create or replace view bench_error_counts as
select cast(null as varchar) as run_id, cast(null as bigint) as line_number,
    cast(null as double) as elapsed_s, cast(null as bigint) as error_count where false;
