<h1 align="center">Command Center</h1>

<p align="center"><a href="README.md">한국어</a> · <b>English</b></p>

<p align="center">
  <b>The Terran main base is the Command Center.</b><br>
  <b>Not Community. Not Pro. We are Human.</b><br>
  Maven · npm · PyPI · Go · Cargo in one server, a package repository that lives in 128 MiB.
</p>

<p align="center">
  <img alt="Race: Terran" src="https://img.shields.io/badge/race-Terran-1e6fd9?style=flat-square">
  <img alt="Edition: Human" src="https://img.shields.io/badge/edition-Human-ff6b6b?style=flat-square">
  <img alt="Pro edition: none" src="https://img.shields.io/badge/Pro%20edition-none-555555?style=flat-square">
  <img alt="License: MIT OR Apache-2.0" src="https://img.shields.io/badge/license-MIT%20%2F%20Apache--2.0-3a3a3a?style=flat-square">
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00add8?style=flat-square&logo=go&logoColor=white">
  <img alt="Memory: 128 MiB is plenty" src="https://img.shields.io/badge/memory-128%20MiB%20is%20plenty-2f7bf5?style=flat-square">
</p>

<p align="center">
  <a href="#the-lore">Lore</a> ·
  <a href="#what-it-does">Features</a> ·
  <a href="#quick-start">Quick start</a> ·
  <a href="#how-light-is-it">How light</a> ·
  <a href="docs/guide.md">Operations guide (Korean)</a> ·
  <a href="#faq">FAQ</a> ·
  <a href="#license">License</a>
</p>

---

## The lore

> In the distant Koprulu Sector... no, right here in your company's infrastructure.

Two kinds of beings live in the package repository universe.

| Race | Main base | Traits |
|---|---|---|
| **Aliens** | **Decepticons** | Highly advanced alien technology. But every technology worth using carries the name **Pro**, and Pro can only be bought with money. |
| **Humans** | **Command Center** | Clunky technology, but everything it has, it shares. This repository is that base. |

The **Protoss** are the race that holds out the **Pro** and makes you **toss** your wallet. At first they hand out Community technology for free and make friends with the humans. Then one day a hologram appears: "This technology is available in Pro tier and above." Usage hits the limit, the feature you need is only ticked in the Pro column, and a quote warps in. The humans fall into confusion. The alarm never stops: more pylons needed... no, more licenses.

So the Terrans built the **Command Center**.

## The Human liberation manifesto

> We do not buy alien technology with money.<br>
> There is no **Pro** in our base. There is only **Human**.<br>
> With nowhere to upgrade to, there is nothing to be downgraded from.

Command Center has exactly one edition, and it is called the **Human Edition**. Not Community, not Pro (alien tech): just an edition for people. It travels the same roads (URLs) as the Protoss base, but it charges no toll.

## What it does

A repository server that follows the widely used `/repository/<repository-name>/...` URL convention. If your clients are already configured with that convention, **just change the host name**.

| Format | hosted (upload) | proxy (remote cache) | group (aggregate) |
|---|:-:|:-:|:-:|
| Maven | ✅ | ✅ | ✅ |
| npm | ✅ | ✅ | ✅ |
| PyPI | ✅ | ✅ | ✅ |
| Go | ✅ | ✅ | ✅ |
| Cargo | ✅ | ✅ | ✅ |

All of this comes with the Human Edition. There is no extra checkout step.

- **Web UI**: browse, search, upload and manage repositories. Korean and English, built into the binary, so it works on air-gapped networks too.
- **Access control**: users, roles, per-repository privileges (browse, read, add, edit, delete) and API tokens.
- **Realms and LDAP**: an LDAPv3 client built with nothing but the Go standard library. It maps groups to roles and fails closed when the directory is down.
- **Multiple blob stores**: pick a storage location per repository, see disk usage, and run blob cleanup (GC) per store.
- **Operations**: cleanup policies, scheduled tasks, activity log, request trends, download counts, and failure notification e-mail (SMTP).
- **Repository online/offline switch**: take a single repository down for maintenance.
- **Break-glass recovery**: forgot the admin password? `commandcenter user reset-password` gets you back in. No need to call customer support. There is no customer support.

## Quick start

All you need is Docker.

```bash
docker compose build
```

```bash
docker run --rm -i commandcenter:latest hash-password
```

Copy `config.example.yaml` to `config.yaml` and put the hash printed above into `passwordHash`.

```bash
cp config.example.yaml config.yaml
```

```bash
docker compose up -d
```

Open `http://localhost:8081/` and you are done. No screen asks for a license key, because there isn't one.

Client settings for each format (Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, Cargo `config.toml`) are ready to copy from each repository's page in the web UI and from the [operations guide](docs/guide.md#포맷별-클라이언트-설정) (Korean).

## Production checklist

The quick start is for trying it out. Before your team relies on it every day, read the production deployment section of the [operations guide](docs/guide.md#프로덕션-배포) (Korean). In short:

- Terminate TLS at a reverse proxy and set `baseURL` to the `https://` address.
- For a private registry, turn anonymous read (`anonymousRead`) off.
- Limit upload size, body timeouts and connections per client at the proxy.
- Keep `meta.db` on local disk or block storage, never on NFS or SMB, and run exactly one instance.
- Give the server 60 seconds to stop (`stop_grace_period` in compose).
- Back up regularly and before every upgrade. Downgrades are not supported.
- Reference Kubernetes manifests are in [deploy/kubernetes](deploy/kubernetes/commandcenter.yaml).

## How light is it

A single Go binary with no CGO. It runs in a 128 MiB container with `GOMEMLIMIT=80MiB`.

| Measurement (`test/e2e.sh`) | Result |
|---|---|
| Smoke tests per format (Maven, npm, PyPI, Go, Cargo, UI, LDAP, mail) | 6 to 37 MiB |
| Load test: concurrent 100 MiB downloads, concurrent uploads, hostile uploads | 43 MiB peak RSS |

Compare that with the memory recommended for JVM-based servers that do the same job, and you will see why we called it human liberation. Your server is human too. Probably.

## Honest status

The Human Edition was made by humans. So it has bugs. You will not see an "all problems solved" hologram here.

- We work through the findings of black-box QA runs one by one, and every fix gets a regression test. Some findings are still open.
- Known limitations and what is not supported yet are in the [limitations section of the operations guide](docs/guide.md#제한-사항) (Korean); what each release fixed and changed is in the [CHANGELOG](CHANGELOG.md).
- The server is built to run as a single instance. High availability and running several instances are not supported.
- Report bugs as issues, and security problems privately as the [security policy](SECURITY.md) describes.

## FAQ

**Is there really no Pro edition?**<br>
There isn't, and there are no plans for one. If a Pro ever shows up, that is a Command Center assimilated by the Protoss. When that happens, please fork it and stand with the humans again.

**Why is it called Command Center?**<br>
If the Protoss have a main base, the human main base is the Command Center. It travels the same URL roads, so from the client's point of view you only swap the base.

**What about a Community edition?**<br>
Everyone who contributes is the community. The word is too good to waste on an edition name, so we are saving it.

**What is the Human Edition's limitation?**<br>
Just one: it was made by humans. Help us fix it with issues and PRs.

**Can I move the data I used in the Protoss base?**<br>
There is no automatic migration tool. The URLs are compatible, so client settings only need the host changed, and proxy repositories fetch packages again the first time they are requested.

**Can I use it at work?**<br>
Use it under MIT or Apache-2.0, whichever you prefer. Commercial use is fine. There is no warranty, though. We are only human.

## Documentation

- [Operations guide](docs/guide.md) (Korean): installation, production deployment, configuration reference, authentication and permissions, LDAP, blob stores, client settings per format, backup and upgrades
- [Kubernetes reference manifests](deploy/kubernetes/commandcenter.yaml)
- [Changelog](CHANGELOG.md): what each release fixed and changed
- [Contributing guide](CONTRIBUTING.md) and [Code of Conduct](CODE_OF_CONDUCT.md)
- [Security policy](SECURITY.md): how to report a vulnerability
- [Attribution](ATTRIBUTION.md): authors and licenses of the third-party code we ship

## Contributing

**SCVs wanted.** No one builds a base alone. Bug reports, doc fixes, bug fixes and new formats are all welcome. We can't pay you in minerals, but your name stays in the commit history forever.

Builds and tests run in a container, with no local Go install.

```bash
bash scripts/go.sh test ./...
```

```bash
bash test/e2e.sh
```

`test/e2e.sh` runs smoke tests that drive every format with the real clients (mvn, npm, pip, go, cargo), plus a 128 MiB load test. It takes about 20 minutes, so get a coffee. Coffee is not included in the Human Edition.

Before you send an issue or a PR, please read the [contributing guide](CONTRIBUTING.md): it covers the code conventions and what we expect from a PR.

## License

Choose [MIT](LICENSE-MIT) or [Apache-2.0](LICENSE-APACHE), whichever you like. The required notices are in [NOTICE](NOTICE), and the list of third-party code we ship is in [ATTRIBUTION.md](ATTRIBUTION.md).

Copyright (c) 2026 the Command Center contributors.

<sub>Command Center is an independent open-source project. It is not affiliated with, sponsored by or endorsed by the maker of any commercial repository product, and it contains no code, logos, icons or stylesheets from other products. Maven, npm, PyPI, Go, Cargo and the other names may be trademarks of their respective owners.</sub>

<sub>StarCraft, Protoss, Terran, Zerg, SCV, Pylon and related StarCraft names are trademarks or works of Blizzard Entertainment, Inc. Command Center is not affiliated with, sponsored by or endorsed by Blizzard. This document mentions the game's races and buildings only as a parody, and contains no images, logos, audio or original dialogue from the game.</sub>

<sub>Decepticons and related Transformers names are trademarks of Hasbro, Inc. Command Center is not affiliated with, sponsored by or endorsed by Hasbro. This document mentions the name only as a parody, and contains no related images or logos.</sub>

<p align="center"><sub>Made by Terrans, for humans. 🧑‍🤝‍🧑</sub></p>
