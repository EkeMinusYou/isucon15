"""Regression checks using the participant log's actual saved wire format."""
import json
from pathlib import Path
import subprocess
import unittest

ROOT = Path(__file__).resolve().parents[2]
LOG = '''2026-10-31T01:08:00Z\tBENCHMARK_START
time=2026-10-31T01:08:58.340Z level=INFO msg="asia 1 日目: 18 人"
time=2026-10-31T01:09:50.922Z level=INFO msg="グランプリ: アジア 0 人, ヨーロッパ 4 人, アメリカ 1 人"
{
  "pass": true,
  "score": 2993,
  "messages": [
    "asia の投稿期間を開けません: POST /api/admin/posting-window: 応答がタイムアウトしました"
  ]
}
SCORE: 2993
BENCHMARK_PASS
2026-10-31T01:10:41Z\tBENCHMARK_END
'''

class BenchAnalysisTest(unittest.TestCase):
    def test_participant_log_and_unknown_window(self):
        sql = 'CREATE TABLE load_windows(run_id VARCHAR, started_at TIMESTAMPTZ, status VARCHAR);\n'
        for filename in ['bench-errors-semantic.sql', 'bench-progress.sql']:
            sql += (ROOT / 'tools/contest/analysis-schema' / filename).read_text() + '\n'
        sql += "INSERT INTO bench_error_logs VALUES ('sample', '" + LOG.replace("'", "''") + "', 'bench.log');\n"
        sql += '''SELECT struct_pack(
            score := (SELECT score FROM bench_results),
            passed := (SELECT passed FROM bench_results),
            errors := (SELECT count(*) FROM bench_errors),
            warning_events := (SELECT count(*) FROM bench_warning_events),
            unknown_times := (SELECT count(*) FROM bench_progress WHERE elapsed_s IS NULL),
            metrics := (SELECT list(struct_pack(metric := metric, value := value) ORDER BY metric) FROM bench_progress)
        ) AS result;'''
        sql += "INSERT INTO load_windows VALUES ('sample','2026-10-31T01:08:58Z','ok');\n"
        sql += "SELECT min(elapsed_s) AS first_elapsed_s, max(elapsed_s) AS last_elapsed_s FROM bench_progress;"
        run = subprocess.run(['duckdb', '-no-agent', '-json', '-batch', '-bail', ':memory:'],
                             input=sql, capture_output=True, text=True, check=True)
        decoder, offset, results = json.JSONDecoder(), 0, []
        while offset < len(run.stdout):
            if run.stdout[offset].isspace():
                offset += 1
                continue
            result, offset = decoder.raw_decode(run.stdout, offset)
            results.append(result)
        result = results[0][0]['result']
        self.assertEqual((result['score'], result['passed'], result['errors'], result['warning_events']),
                         (2993, True, 1, 0))
        self.assertEqual(result['unknown_times'], 4)
        self.assertEqual(result['metrics'], [
            {'metric': 'daily_users.asia.1 日目', 'value': 18.0},
            {'metric': 'grand_prix_users.america', 'value': 1.0},
            {'metric': 'grand_prix_users.asia', 'value': 0.0},
            {'metric': 'grand_prix_users.europe', 'value': 4.0},
        ])
        self.assertAlmostEqual(results[1][0]['first_elapsed_s'], 0.340)
        self.assertAlmostEqual(results[1][0]['last_elapsed_s'], 52.922)

if __name__ == '__main__':
    unittest.main()
