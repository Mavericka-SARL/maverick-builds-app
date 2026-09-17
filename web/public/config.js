// Dev placeholder. index.html loads /config.js before the bundle so a
// container can inject environment-specific values at startup (see
// web/docker-entrypoint.sh). In `npm run dev` nothing injects anything, so
// this empty object simply lets src/config.ts fall through to
// import.meta.env / .env.development exactly as before.
//
// The production image OVERWRITES this file at container start.
window.__MAVERICKS_CONFIG__ = {};
