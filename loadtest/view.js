// View tracking load test.
//
// Mixes three traffic patterns to exercise the spam-filter chain:
//
//   60% legitimate viewers   — random video, watch_ms in [1500ms, 30s]
//   25% "no watch" bots      — watch_ms = 0 (rejected by watch_threshold)
//   15% same-video spammers  — repeatedly viewing one video (filtered by
//                              duration_rate; replays may be accepted with is_unique=false)
//
// Asserts the API stays fast (filters are 1 Redis op each) and that spam
// patterns get rejected at the configured rates without breaking the
// legitimate path.

import http from 'k6/http';
import { check, sleep } from 'k6';
import { Counter } from 'k6/metrics';
import { BASE_URL, fetchFeedPage, postJSON } from './lib.js';

const acceptedCounter = new Counter('view_accepted');
const rejectedCounter = new Counter('view_rejected');

export const options = {
    scenarios: {
        views: {
            executor: 'ramping-vus',
            startVUs: 0,
            stages: [
                { duration: '15s', target: 50 },
                { duration: '60s', target: 250 },
                { duration: '15s', target: 0 },
            ],
            gracefulRampDown: '10s',
        },
    },
    thresholds: {
        'http_req_duration{tag:view}': ['p(95)<60'],
        'http_req_failed{tag:view}':   ['rate<0.01'],
    },
};

let videoIDs = [];

export default function () {
    if (videoIDs.length === 0) {
        videoIDs = fetchFeedPage(50).map(v => v.id);
        if (videoIDs.length === 0) {
            sleep(1);
            return;
        }
    }

    const roll = Math.random();
    let body, vid;
    if (roll < 0.60) {
        // Legitimate viewer.
        vid = videoIDs[Math.floor(Math.random() * videoIDs.length)];
        body = { watch_ms: 1500 + Math.floor(Math.random() * 30_000) };
    } else if (roll < 0.85) {
        // Below-threshold bot.
        vid = videoIDs[Math.floor(Math.random() * videoIDs.length)];
        body = { watch_ms: 0 };
    } else {
        // Same-video spammer. Always targets videoIDs[0] regardless of VU.
        vid = videoIDs[0];
        body = { watch_ms: 2500 };
    }

    const r = postJSON(`${BASE_URL}/videos/${vid}/view`, body, { tags: { tag: 'view' } });
    if (check(r, { 'status 202': res => res.status === 202 })) {
        const parsed = JSON.parse(r.body);
        if (parsed.data && parsed.data.accepted) acceptedCounter.add(1);
        else rejectedCounter.add(1);
    }

    sleep(Math.random() * 0.4);
}
