// Feed read load test.
//
// Hits GET /feed under a ramp-up / steady / ramp-down profile and asserts
// that p95 stays under a threshold. Run separately against pull and push
// modes (toggle via FEED_MODE on the API) to compare them under identical
// load.
//
//   FEED_MODE=pull make load-feed   # Postgres keyset pagination
//   FEED_MODE=push make load-feed   # Redis ZSET-backed cache

import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE_URL } from './lib.js';

export const options = {
    scenarios: {
        feed_read: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '15s', target: 50 },   // warm-up
                { duration: '45s', target: 200 },  // steady load
                { duration: '15s', target: 0 },    // ramp down
            ],
            gracefulRampDown: '10s',
        },
    },
    thresholds: {
        // Hard fail if 95% of requests take longer than 200ms or error rate > 1%.
        // Tune these numbers per environment; the *shape* (p95 + errors)
        // matters more than the absolute values.
        'http_req_duration{tag:feed}': ['p(95)<200'],
        'http_req_failed{tag:feed}':   ['rate<0.01'],
    },
};

export default function () {
    // 70% of traffic asks for the first page; 30% paginates with a synthetic
    // cursor that is intentionally nonsense — exercises the cursor validation
    // path under load too.
    const limit = 20;
    const url = Math.random() < 0.7
        ? `${BASE_URL}/feed?limit=${limit}`
        : `${BASE_URL}/feed?limit=${limit}&cursor=eyJjIjoiMjAyNS0wMS0wMVQwMDowMDowMFoiLCJpZCI6IjAwMDAwMDAwLTAwMDAtMDAwMC0wMDAwLTAwMDAwMDAwMDAwMCJ9`;

    const r = http.get(url, { tags: { tag: 'feed' } });
    check(r, {
        'status 200 or 400': res => res.status === 200 || res.status === 400,
        'json envelope':     res => res.headers['Content-Type']?.includes('application/json'),
    });
    sleep(Math.random() * 0.5);
}
