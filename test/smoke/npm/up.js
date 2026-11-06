// Sets what upstream.js serves, from the client container:
//   up.js add <name> <version> <path>  packs name@version, keeps the tarball at
//                                      that path and lists it in the document
//   up.js drop <name> <version>...     the remote unpublishes these versions
//   up.js stub <name>                  the remote leaves the stub of an unpublished package
//   up.js gone <name>                  the remote drops the package (404)
//   up.js mode normal|html|fail        every answer is as usual, a sign-in page, a 500
//   up.js asked <path>                 prints how many times path was asked for
const { execSync } = require('child_process');
const crypto = require('crypto');
const fs = require('fs');
const os = require('os');
const path = require('path');

const base = 'http://upstream:8080';
const control = (method, p, type, body) => fetch(`${base}/_/${p}`, { method, headers: { 'Content-Type': type || 'text/plain' }, body });
const getDoc = async (name) => (await control('GET', `store/${name}`)).json();
const putDoc = (name, doc) => control('PUT', `store/${name}`, 'application/json', JSON.stringify(doc));

const commands = {
  async add(name, version, at) {
    const dir = fs.mkdtempSync(path.join(os.tmpdir(), 'up-'));
    const man = { name, version, main: 'index.js' };
    fs.writeFileSync(path.join(dir, 'package.json'), JSON.stringify(man));
    fs.writeFileSync(path.join(dir, 'index.js'), `exports.v = ${JSON.stringify(`${name}@${version}`)};\n`);
    const file = execSync(`npm pack --silent --cache ${dir}/.cache`, { cwd: dir }).toString().trim();
    const data = fs.readFileSync(path.join(dir, file));
    await control('PUT', `store${at}`, 'application/octet-stream', data);
    const held = await control('GET', `store/${name}`);
    const doc = held.ok ? await held.json() : { _id: name, name, 'dist-tags': {}, versions: {}, time: { created: '2020-01-01T00:00:00.000Z' } };
    const sha = (alg, enc) => crypto.createHash(alg).update(data).digest(enc);
    doc.versions[version] = { ...man, _id: `${name}@${version}`, dist: { tarball: base + at, shasum: sha('sha1', 'hex'), integrity: `sha512-${sha('sha512', 'base64')}` } };
    doc['dist-tags'].latest = version;
    doc.time[version] = '2020-01-01T00:00:00.000Z';
    await putDoc(name, doc);
  },
  async drop(name, ...versions) {
    const doc = await getDoc(name);
    for (const v of versions) {
      await control('DELETE', `store${new URL(doc.versions[v].dist.tarball).pathname}`);
      delete doc.versions[v];
      delete doc.time[v];
    }
    doc['dist-tags'].latest = Object.keys(doc.versions).at(-1);
    await putDoc(name, doc);
  },
  stub: (name) => putDoc(name, { _id: name, name, time: { created: '2020-01-01T00:00:00.000Z', unpublished: { time: '2026-01-01T00:00:00.000Z' } } }),
  gone: (name) => control('DELETE', `store/${name}`),
  mode: (m) => control('PUT', `mode/${m}`),
  asked: async (p) => console.log(await (await control('GET', `asked${p}`)).text()),
};

const [command, ...args] = process.argv.slice(2);
commands[command](...args).catch((err) => {
  console.error(err);
  process.exit(1);
});
