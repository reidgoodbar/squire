const INSTALL_URL =
  "https://raw.githubusercontent.com/reidgoodbar/squire/main/install.sh";
const GITHUB_URL = "https://github.com/reidgoodbar/squire";

function text(body, status = 200, headers = {}) {
  return new Response(body, {
    status,
    headers: {
      "content-type": "text/plain; charset=utf-8",
      "cache-control": "public, max-age=60",
      ...headers,
    },
  });
}

function html(body, status = 200) {
  return new Response(body, {
    status,
    headers: {
      "content-type": "text/html; charset=utf-8",
      "cache-control": "public, max-age=300",
    },
  });
}

async function installScript() {
  const upstream = await fetch(INSTALL_URL, {
    headers: {
      "user-agent": "squire.run installer proxy",
      accept: "text/plain,*/*",
    },
    cf: { cacheTtl: 60, cacheEverything: true },
  });
  if (!upstream.ok) {
    return text("squire install: installer temporarily unavailable\n", 503, {
      "cache-control": "no-store",
    });
  }
  const body = await upstream.text();
  return text(body, 200, {
    "content-type": "text/x-shellscript; charset=utf-8",
    "cache-control": "public, max-age=60",
  });
}

export default {
  async fetch(request) {
    const url = new URL(request.url);
    if (url.hostname === "www.squire.run") {
      url.hostname = "squire.run";
      return Response.redirect(url.toString(), 301);
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      return text("method not allowed\n", 405, { allow: "GET, HEAD" });
    }
    if (url.pathname === "/install.sh") {
      return installScript();
    }
    if (url.pathname === "/healthz") {
      return text("ok\n");
    }
    if (url.pathname === "/" || url.pathname === "") {
      return html(`<!doctype html>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Squire</title>
<style>
  :root { color-scheme: light dark; font-family: ui-sans-serif, system-ui, -apple-system, BlinkMacSystemFont, "Segoe UI", sans-serif; }
  body { margin: 0; min-height: 100vh; display: grid; place-items: center; background: #0f1410; color: #f3f7ef; }
  main { width: min(720px, calc(100% - 40px)); }
  h1 { font-size: 42px; margin: 0 0 12px; letter-spacing: 0; }
  p { color: #c7d0c2; font-size: 18px; line-height: 1.55; }
  code { display: block; overflow-x: auto; padding: 16px; border: 1px solid #344032; border-radius: 8px; background: #151d16; color: #e8f2e4; font-size: 15px; }
  a { color: #9fe870; }
</style>
<main>
  <h1>Squire</h1>
  <p>A local performance layer for AI coding agents. Agent chooses. Squire serves.</p>
  <code>curl -fsSL https://squire.run/install.sh | bash</code>
  <p><a href="${GITHUB_URL}">GitHub</a></p>
</main>`);
    }
    return Response.redirect(GITHUB_URL, 302);
  },
};
