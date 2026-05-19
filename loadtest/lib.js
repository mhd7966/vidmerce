// Shared helpers for the Vidmerce k6 load tests.
//
// Conventions:
// - Every script imports BASE_URL and the auth helpers from this file.
// - We register one ephemeral user per VU on iteration 0 and reuse the token
//   thereafter; this keeps auth out of the steady-state latency numbers.
// - Tag every request with a `tag` field so InfluxDB / Prometheus output
//   buckets latency by endpoint, not by VU.

import http from 'k6/http';
import { check, fail } from 'k6';

export const BASE_URL = __ENV.BASE_URL || 'http://localhost:8080';

// JSON helper that auto-sets the content-type header.
export function postJSON(url, body, params = {}) {
    params.headers = Object.assign({ 'Content-Type': 'application/json' }, params.headers || {});
    return http.post(url, JSON.stringify(body), params);
}

// Authed JSON helper — caller passes a token; we don't dig it out of cookies.
export function postJSONAuthed(url, body, token, params = {}) {
    params.headers = Object.assign({
        'Content-Type': 'application/json',
        'Authorization': 'Bearer ' + token,
    }, params.headers || {});
    return http.post(url, JSON.stringify(body), params);
}

// Random ASCII string for unique user emails / video titles.
export function randSuffix(n = 10) {
    const alphabet = 'abcdefghijklmnopqrstuvwxyz0123456789';
    let s = '';
    for (let i = 0; i < n; i++) s += alphabet[Math.floor(Math.random() * alphabet.length)];
    return s;
}

// Register a fresh user and log in. Returns { userID, token }.
// Fails fast if either call returns a non-2xx so a misconfigured load test
// crashes loudly instead of measuring 401s.
export function registerAndLogin() {
    const email = `vu${__VU}-${randSuffix()}@vidmerce.test`;
    const password = 'load-test-password-1234';

    let r = postJSON(`${BASE_URL}/auth/register`, { email, password }, { tags: { tag: 'auth_register' } });
    if (!check(r, { 'register 201': res => res.status === 201 })) {
        fail(`register failed: ${r.status} ${r.body}`);
    }

    r = postJSON(`${BASE_URL}/auth/login`, { email, password }, { tags: { tag: 'auth_login' } });
    if (!check(r, { 'login 200': res => res.status === 200 })) {
        fail(`login failed: ${r.status} ${r.body}`);
    }
    const body = JSON.parse(r.body);
    return {
        userID: body.data.user.id,
        token: body.data.tokens.access_token,
    };
}

// Pull the first page of the feed. Returns the array of video objects (may
// be empty if no seed data has been created).
export function fetchFeedPage(limit = 20) {
    const r = http.get(`${BASE_URL}/feed?limit=${limit}`, { tags: { tag: 'feed' } });
    if (r.status !== 200) return [];
    return (JSON.parse(r.body).data || []);
}

// Create N videos under the given token; returns an array of created IDs.
// Used by the bootstrap script to seed a pool of video IDs that the
// scenarios reference.
export function createVideos(token, n) {
    const ids = [];
    for (let i = 0; i < n; i++) {
        const body = {
            title: `loadtest-${randSuffix()}`,
            description: 'k6 seed',
            video_url: `https://cdn.example.com/${randSuffix()}.mp4`,
        };
        const r = postJSONAuthed(`${BASE_URL}/videos`, body, token, { tags: { tag: 'video_create' } });
        if (r.status === 201) ids.push(JSON.parse(r.body).data.id);
    }
    return ids;
}
