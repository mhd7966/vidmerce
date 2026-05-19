// Bootstrap script — run once before any load test that needs videos in the
// feed (like, view, stats). It registers one creator and posts N videos so
// the feed has stable content for the scenarios to reference.
//
// Usage:
//
//   k6 run loadtest/bootstrap.js
//
// Env knobs:
//
//   BASE_URL=...          — API root, defaults to http://localhost:8080
//   SEED_VIDEO_COUNT=50   — how many videos to create

import { check } from 'k6';
import { BASE_URL, postJSON, postJSONAuthed, randSuffix } from './lib.js';

const COUNT = parseInt(__ENV.SEED_VIDEO_COUNT || '50', 10);

export const options = { vus: 1, iterations: 1 };

export default function () {
    const email = `bootstrap-${randSuffix()}@vidmerce.test`;
    const password = 'bootstrap-password-1234';

    check(
        postJSON(`${BASE_URL}/auth/register`, { email, password }),
        { 'register 201': r => r.status === 201 },
    );
    const loginRes = postJSON(`${BASE_URL}/auth/login`, { email, password });
    if (!check(loginRes, { 'login 200': r => r.status === 200 })) {
        throw new Error(`bootstrap login failed: ${loginRes.status} ${loginRes.body}`);
    }
    const token = JSON.parse(loginRes.body).data.tokens.access_token;

    let created = 0;
    for (let i = 0; i < COUNT; i++) {
        const r = postJSONAuthed(
            `${BASE_URL}/videos`,
            {
                title: `bootstrap-${i}-${randSuffix()}`,
                description: 'k6 bootstrap',
                video_url: `https://cdn.example.com/${randSuffix()}.mp4`,
                duration_sec: 15,
            },
            token,
        );
        if (r.status === 201) created++;
    }
    console.log(`bootstrap: created ${created}/${COUNT} videos`);
}
