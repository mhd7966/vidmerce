// Stats endpoint load test — focused on the stampede-protection design.
//
// The whole point of the three-layer defense (cache → singleflight →
// distributed lock) is that N concurrent requests for the same video
// produce O(1) backend queries. This test runs 200 VUs hammering ONE video
// id and asserts that:
//
//   - p95 latency is bounded (Redis-cache fast path most of the time).
//   - Error rate stays tiny.
//   - The server's ClickHouse / Postgres QPS (observed externally — e.g.
//     via ClickHouse's system.query_log) stays at roughly 1 query per
//     STATS_CACHE_TTL window. We can't assert this directly from k6, but
//     a manual side-by-side comparison of CH query count between this
//     test and a "no-cache" baseline makes the protection visible.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE_URL, fetchFeedPage } from './lib.js';

export const options = {
    scenarios: {
        stampede: {
            executor: 'constant-vus',
            vus: 200,
            duration: '60s',
        },
    },
    thresholds: {
        'http_req_duration{tag:stats}': ['p(95)<80'],
        'http_req_failed{tag:stats}':   ['rate<0.01'],
    },
};

let targetVideoID = null;

export function setup() {
    // Pick exactly one video for the entire test run. All VUs hit it.
    const videos = fetchFeedPage(1);
    if (videos.length === 0) throw new Error('no seed videos found; run bootstrap first');
    return { vid: videos[0].id };
}

export default function (data) {
    targetVideoID = data.vid;
    const r = http.get(`${BASE_URL}/videos/${targetVideoID}/stats`, { tags: { tag: 'stats' } });
    check(r, {
        'status 200': res => res.status === 200,
        'has engagement_rate': res => res.body.includes('engagement_rate'),
    });
    sleep(Math.random() * 0.2);
}
