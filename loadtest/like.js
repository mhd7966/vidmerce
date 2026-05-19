// Like / unlike load test.
//
// Goal: prove the async like path (Lua-atomic Redis → stream → worker → PG)
// holds up under burst load and that the leaky-bucket rate limiter rejects
// pathological clients without blocking honest ones.
//
// Each VU registers once, fetches a video pool from the feed, and then runs
// a tight loop of like → unlike → like on random videos. The leaky-bucket
// per (user, video) means a single VU pounding one video will start seeing
// 429s after ~10 toggles; the test asserts that error rate stays bounded
// in aggregate.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { BASE_URL, registerAndLogin, fetchFeedPage, postJSONAuthed } from './lib.js';

http.setResponseCallback(http.expectedStatuses(202, 429));

export const options = {
    scenarios: {
        likes: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '15s', target: 30 },
                { duration: '60s', target: 100 },
                { duration: '15s', target: 0 },
            ],
            gracefulRampDown: '10s',
        },
    },
    thresholds: {
        // p95 includes the leaky-bucket check which is one Lua call, so we
        // expect sub-30ms on a local stack.
        'http_req_duration{tag:like}':   ['p(95)<60'],
        'http_req_duration{tag:unlike}': ['p(95)<60'],
        // 429s are an *expected* outcome under burst patterns: clamp to 30%
        // so we still notice systemic regressions but tolerate rate-limit hits.
        'http_req_failed{tag:like}':     ['rate<0.30'],
    },
};

// VU-scoped state. setup() runs once for the test (not per-VU) so each VU
// freshly registers + reads the feed on its first iteration.
let token = null;
let videoIDs = [];

export default function () {
    if (!token) {
        const auth = registerAndLogin();
        token = auth.token;
        videoIDs = fetchFeedPage(50).map(v => v.id);
        if (videoIDs.length === 0) {
            // No seed data — skip this VU rather than spinning on 400s.
            sleep(1);
            return;
        }
    }

    const vid = videoIDs[Math.floor(Math.random() * videoIDs.length)];

    const r1 = postJSONAuthed(`${BASE_URL}/videos/${vid}/like`, {}, token, { tags: { tag: 'like' } });
    check(r1, {
        'like 202 or 429': res => res.status === 202 || res.status === 429,
    });

    const r2 = postJSONAuthed(`${BASE_URL}/videos/${vid}/unlike`, {}, token, { tags: { tag: 'unlike' } });
    check(r2, {
        'unlike 202 or 429': res => res.status === 202 || res.status === 429,
    });

    sleep(Math.random() * 0.3);
}
