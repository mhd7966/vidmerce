# k6 load test results

Generated: 2026-05-19 01:20:53 UTC
API base: `http://localhost:8080`

Raw logs and JSON summaries: [`loadtest/results/`](results/).

Re-run:

```bash
make up && make migrate-all
make run-api    # terminal 1
make run-worker # terminal 2
bash hack/run-loadtest.sh
```

## Summary

| Scenario | http_reqs | http_req_failed | http_req_duration | checks |
|----------|-----------|-----------------|-------------------|--------|
| bootstrap | 52 | 0.00% | p95=16.65ms avg=12.63ms | 2/0 pass/fail |
| feed | 29771 | 0.00% | p95=3.51ms avg=2.29ms | 59542/0 pass/fail |
| like | 6212 | 15.33% | p95=4703.39ms avg=750.13ms | 5308/880 pass/fail |
| view | 54384 | 0.00% | p95=12.93ms avg=6.08ms | 54134/0 pass/fail |
| stats | 112741 | 0.00% | p95=18.91ms avg=5.39ms | 225480/0 pass/fail |

## Per-scenario logs

- [`bootstrap.log`](results/bootstrap.log)
- [`bootstrap-summary.json`](results/bootstrap-summary.json)
- [`feed.log`](results/feed.log)
- [`feed-summary.json`](results/feed-summary.json)
- [`like.log`](results/like.log)
- [`like-summary.json`](results/like-summary.json)
- [`view.log`](results/view.log)
- [`view-summary.json`](results/view-summary.json)
- [`stats.log`](results/stats.log)
- [`stats-summary.json`](results/stats-summary.json)
