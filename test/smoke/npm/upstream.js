// The remote of npm-smoke.sh's npm-odd proxy: a registry that can keep its
// tarballs anywhere, drop what it had and answer with a sign-in page or a
// failure. What it serves is set through /_/ (up.js) and kept in memory.
const http = require('http');

const store = new Map(); // path: { type, body }
const asked = new Map(); // path: how many GETs
let mode = 'normal';

http.createServer(async (req, res) => {
  const chunks = [];
  for await (const c of req) chunks.push(c);
  const path = decodeURIComponent(new URL(req.url, 'http://upstream').pathname);
  const send = (code, type, body) => {
    res.writeHead(code, { 'Content-Type': type });
    res.end(body);
  };
  if (path.startsWith('/_/mode/')) {
    mode = path.slice('/_/mode/'.length);
    return send(204, 'text/plain');
  }
  if (path.startsWith('/_/asked/')) return send(200, 'text/plain', String(asked.get(path.slice('/_/asked'.length)) || 0));
  if (path.startsWith('/_/store/')) {
    const key = path.slice('/_/store'.length);
    if (req.method === 'PUT') store.set(key, { type: req.headers['content-type'], body: Buffer.concat(chunks) });
    if (req.method === 'DELETE') store.delete(key);
    const held = store.get(key);
    if (req.method === 'GET' && !held) return send(404, 'text/plain', 'no such key');
    return req.method === 'GET' ? send(200, held.type, held.body) : send(204, 'text/plain');
  }
  asked.set(path, (asked.get(path) || 0) + 1);
  if (mode === 'html') return send(200, 'text/html', '<!DOCTYPE html><html><body>Sign in</body></html>');
  if (mode === 'fail') return send(500, 'text/plain', 'down for maintenance');
  const held = store.get(path);
  return held ? send(200, held.type, held.body) : send(404, 'application/json', '{"error":"not found"}');
}).listen(8080);
