<h1 align="center">Command Center</h1>

<p align="center"><b>한국어</b> · <a href="README.en.md">English</a></p>

<p align="center">
  <b>테란의 본진은 Command Center입니다.</b><br>
  <b>Community도 아니고 Pro도 아닙니다. 우리는 Human입니다.</b><br>
  Maven · npm · PyPI · Go · Cargo를 한 서버에 담은, 128 MiB에서 사는 패키지 저장소.
</p>

<p align="center">
  <img alt="Race: Terran" src="https://img.shields.io/badge/race-Terran-1e6fd9?style=flat-square">
  <img alt="Edition: Human" src="https://img.shields.io/badge/edition-Human-ff6b6b?style=flat-square">
  <img alt="Pro edition: none" src="https://img.shields.io/badge/Pro%20edition-%EC%97%86%EC%9D%8C-555555?style=flat-square">
  <img alt="License: MIT OR Apache-2.0" src="https://img.shields.io/badge/license-MIT%20%2F%20Apache--2.0-3a3a3a?style=flat-square">
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00add8?style=flat-square&logo=go&logoColor=white">
  <img alt="Memory: 128 MiB" src="https://img.shields.io/badge/memory-128%20MiB%EB%A9%B4%20%EC%B6%A9%EB%B6%84-2f7bf5?style=flat-square">
</p>

<p align="center">
  <a href="#세계관">세계관</a> ·
  <a href="#무엇을-하나요">기능</a> ·
  <a href="#빠른-시작">빠른 시작</a> ·
  <a href="#얼마나-가볍나요">가벼움</a> ·
  <a href="docs/guide.md">운영 가이드</a> ·
  <a href="#자주-묻는-질문">FAQ</a> ·
  <a href="#라이선스">라이선스</a>
</p>

---

## 세계관

> 머나먼 코프룰루 구역…… 이 아니라, 바로 당신의 사내 인프라에서.

패키지 저장소의 우주에는 두 종족이 삽니다.

| 종족 | 본진 | 특징 |
|---|---|---|
| **외계인** | **Decepticons** | 고도로 발달한 외계 기술을 가졌습니다. 그런데 쓸 만한 기술에는 꼭 **Pro**라는 이름이 붙고, Pro는 돈으로만 살 수 있습니다. |
| **사람** | **Command Center** | 기술은 투박하지만 가진 건 다 나눕니다. 이 저장소가 바로 그 본진입니다. |

**Protoss**는, **Pro**를 내밀고, 지갑을 **toss**(던지게) 만드는 종족입니다. 처음엔 무료로 Community 기술을 나눠 주며 인간과 친해지다가, 어느 날 "이 기술은 Pro 등급 이상에서 사용할 수 있습니다"라는 홀로그램을 띄웁니다. 사용량이 한도에 닿고, 필요한 기능은 Pro 칸에만 체크되어 있고, 견적서가 워프되어 도착합니다. 인간들은 혼란에 빠집니다. 추가 파일런이, 아니, 추가 라이선스가 필요하다는 경고가 끝없이 울립니다.

그래서 테란은 **Command Center**를 지었습니다.

## 인간 해방 선언

> 우리는 외계 기술을 돈 주고 사지 않는다.<br>
> 우리의 본진에는 **Pro**가 없다. **Human**만 있다.<br>
> 업그레이드할 곳이 없으니 다운그레이드당할 일도 없다.

Command Center의 에디션은 하나뿐이고, 이름은 **Human Edition**입니다. Community(공동체)도 Pro(외계 기술)도 아닌, 그냥 사람을 위한 에디션입니다. 프로토스 본진과 같은 길(URL)로 다니지만, 통행료는 받지 않습니다.

## 무엇을 하나요

업계에서 널리 쓰이는 `/repository/<저장소이름>/...` 경로 규칙을 따르는 저장소 서버입니다. 이미 이 규칙으로 클라이언트를 설정해 두었다면 **호스트 이름만 바꾸면** 됩니다.

| 포맷 | hosted (업로드) | proxy (원격 캐시) | group (묶음) |
|---|:-:|:-:|:-:|
| Maven | ✅ | ✅ | ✅ |
| npm | ✅ | ✅ | ✅ |
| PyPI | ✅ | ✅ | ✅ |
| Go | ✅ | ✅ | ✅ |
| Cargo | ✅ | ✅ | ✅ |

Human Edition에 기본으로 들어 있는 것들입니다. 추가 결제 단계는 없습니다.

- **웹 UI**: 둘러보기, 검색, 업로드, 저장소 관리. 한국어와 영어를 지원하고 바이너리에 내장되어 있어서 폐쇄망에서도 뜹니다.
- **접근 제어**: 사용자, 역할, 저장소별 권한(browse, read, add, edit, delete), API 토큰.
- **Realm과 LDAP**: 표준 라이브러리만으로 만든 LDAPv3 클라이언트. 그룹을 역할로 매핑하고, 디렉터리가 내려가면 안전하게 거부합니다.
- **여러 blob store**: 저장소마다 저장 위치를 고르고, 디스크 사용량을 보고, blob 정리(GC)를 store별로 돌립니다.
- **운영 기능**: Cleanup 정책, 예약 작업, 활동 로그, 요청 추이, 다운로드 수, 실패 알림 메일(SMTP).
- **저장소 Online/Offline 전환**: 점검할 때 저장소 하나만 잠시 내립니다.
- **비상 복구**: 관리자 비밀번호를 잊어도 `commandcenter user reset-password`로 다시 들어옵니다. 고객센터에 전화할 필요는 없습니다. 고객센터도 없습니다.

## 빠른 시작

Docker만 있으면 됩니다.

```bash
docker compose build
```

```bash
docker run --rm -i commandcenter:latest hash-password
```

`config.example.yaml`을 `config.yaml`로 복사하고, 위 명령이 출력한 해시를 `passwordHash` 자리에 넣습니다.

```bash
cp config.example.yaml config.yaml
```

```bash
docker compose up -d
```

`http://localhost:8081/`을 열면 끝입니다. 라이선스 키를 묻는 화면은 나오지 않습니다. 원래 없으니까요.

포맷별 클라이언트 설정(Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, Cargo `config.toml`)은 웹 UI의 저장소 화면과 [운영 가이드](docs/guide.md#포맷별-클라이언트-설정)에 복사해서 쓸 수 있는 형태로 있습니다.

## 프로덕션 체크리스트

빠른 시작은 시험용 구성입니다. 팀이 매일 쓰는 저장소로 올리기 전에 운영 가이드의 [프로덕션 배포](docs/guide.md#프로덕션-배포)를 읽어 주세요. 요약하면 이렇습니다.

- TLS는 리버스 프록시에서 끝내고 `baseURL`을 `https://` 주소로 둡니다.
- 사내 전용이면 익명 읽기(`anonymousRead`)를 끕니다.
- 프록시에서 업로드 크기, 본문 수신 타임아웃, 연결 수를 제한합니다.
- `meta.db`는 로컬 디스크나 블록 스토리지에 둡니다. NFS·SMB는 안 됩니다. 인스턴스는 하나만 띄웁니다.
- 중지 대기 시간을 60초로 늘립니다(compose의 `stop_grace_period`).
- 정기적으로, 그리고 업그레이드 전에 백업합니다. 다운그레이드는 지원하지 않습니다.
- Kubernetes용 참조 매니페스트는 [deploy/kubernetes](deploy/kubernetes/commandcenter.yaml)에 있습니다.

## 얼마나 가볍나요

Go 단일 바이너리이고 CGO를 쓰지 않습니다. 128 MiB 컨테이너에서 `GOMEMLIMIT=80MiB`로 돌립니다.

| 측정 (`test/e2e.sh`) | 결과 |
|---|---|
| 포맷별 스모크 테스트 (Maven, npm, PyPI, Go, Cargo, UI, LDAP, 메일) | 6 ~ 37 MiB |
| 부하 테스트: 100 MiB 파일 동시 다운로드, 동시 업로드, 적대적 업로드 | 최대 RSS 43 MiB |

같은 일을 하는 JVM 기반 서버에게 권장되는 메모리와 비교해 보시면, 왜 "인간 해방"이라고 했는지 아실 겁니다. 여러분의 서버도 인간입니다. 아마도.

## 정직한 현황

Human Edition은 인간이 만들었습니다. 그래서 버그가 있습니다. "모든 문제가 해결되었습니다" 같은 홀로그램은 띄우지 않겠습니다.

- 블랙박스 QA에서 나온 문제를 차례로 고치고 있고, 고친 문제에는 회귀 테스트를 붙입니다. 아직 고치지 못한 문제도 있습니다.
- 알려진 제한과 아직 지원하지 않는 기능은 [운영 가이드의 제한 사항](docs/guide.md#제한-사항)에, 릴리스마다 고친 것과 바뀐 것은 [CHANGELOG](CHANGELOG.md)에 적습니다.
- 서버는 한 대로 돌도록 만들었습니다. 이중화나 여러 대로 늘리는 구성은 지원하지 않습니다.
- 버그는 이슈로, 보안 문제는 [보안 정책](SECURITY.md)대로 비공개로 알려 주세요.

## 자주 묻는 질문

**정말 Pro 에디션이 없나요?**<br>
없습니다. 만들 계획도 없습니다. 어느 날 Pro가 생긴다면, 그건 프로토스에게 동화된 Command Center입니다. 그때는 포크해서 다시 인간 편에 서 주세요.

**왜 이름이 Command Center인가요?**<br>
프로토스에게 본진이 있다면, 인간의 본진은 Command Center니까요. 같은 URL 규칙으로 다니기 때문에 클라이언트 입장에서는 본진만 갈아 끼우면 됩니다.

**그럼 Community 에디션은요?**<br>
기여해 주시는 분들 모두가 커뮤니티입니다. 에디션 이름으로 쓰기엔 아까운 단어라서 아껴 두었습니다.

**Human Edition의 제약은 무엇인가요?**<br>
인간이 만들었다는 것 하나입니다. 이슈와 PR로 함께 고쳐 주세요.

**프로토스 본진에서 쓰던 데이터를 옮길 수 있나요?**<br>
자동 이전 도구는 없습니다. URL이 호환되니 클라이언트 설정은 호스트만 바꾸면 되고, 패키지는 proxy 저장소가 처음 요청될 때 다시 가져옵니다.

**업무용으로 써도 되나요?**<br>
MIT 또는 Apache-2.0 중 원하는 쪽으로 쓰시면 됩니다. 상업적 사용도 됩니다. 다만 보증은 없습니다. 인간이니까요.

## 문서

- [운영 가이드](docs/guide.md): 설치, 프로덕션 배포, 설정 레퍼런스, 인증·권한, LDAP, blob store, 포맷별 클라이언트 설정, 백업과 업그레이드
- [Kubernetes 참조 매니페스트](deploy/kubernetes/commandcenter.yaml)
- [변경 기록](CHANGELOG.md): 릴리스마다 고친 것과 바뀐 것
- [기여 안내](CONTRIBUTING.md)와 [행동 강령](CODE_OF_CONDUCT.md)
- [보안 정책](SECURITY.md): 취약점 신고 방법
- [출처 표기](ATTRIBUTION.md): 함께 배포되는 서드파티 코드의 저자와 라이선스

## 기여

**SCV를 모집합니다.** 본진은 혼자 짓지 못합니다. 이슈 신고, 문서 수정, 버그 수정, 새 포맷 지원 모두 환영합니다. 미네랄은 드리지 못하지만, 커밋 기록에 이름은 영원히 남습니다.

빌드와 테스트는 로컬 Go 없이 컨테이너로 돌립니다.

```bash
bash scripts/go.sh test ./...
```

```bash
bash test/e2e.sh
```

`test/e2e.sh`는 실제 클라이언트(mvn, npm, pip, go, cargo)로 모든 포맷을 돌려 보는 스모크 테스트와 128 MiB 부하 테스트를 함께 실행합니다. 20분쯤 걸리니 커피를 한 잔 준비하세요. 커피는 Human Edition에 포함되어 있지 않습니다.

이슈와 PR을 보내기 전에 [기여 안내](CONTRIBUTING.md)를 읽어 주세요. 코드 규칙과 PR에 기대하는 것을 적어 두었습니다.

## 라이선스

[MIT](LICENSE-MIT) 또는 [Apache-2.0](LICENSE-APACHE) 중 원하는 것을 고르세요. 필요한 고지는 [NOTICE](NOTICE)에, 함께 배포되는 서드파티 코드의 목록은 [ATTRIBUTION.md](ATTRIBUTION.md)에 있습니다.

Copyright (c) 2026 the Command Center contributors.

<sub>Command Center는 독립적인 오픈소스 프로젝트이며 어떤 상용 저장소 제품의 제작사와도 제휴하거나 후원 또는 보증을 받지 않았습니다. 다른 제품의 코드, 로고, 아이콘, 스타일시트를 포함하지 않습니다. Maven, npm, PyPI, Go, Cargo 등 각 이름은 해당 소유자의 상표일 수 있습니다.</sub>

<sub>StarCraft, Protoss, Terran, Zerg, SCV, 파일런 등 스타크래프트 관련 명칭은 Blizzard Entertainment, Inc.의 상표이거나 저작물입니다. Command Center는 Blizzard와 무관하며 후원이나 보증을 받지 않았습니다. 이 문서는 패러디 목적의 비유로만 게임 속 종족과 건물을 언급하고, 게임의 이미지, 로고, 음성, 대사 원문을 포함하지 않습니다.</sub>

<sub>Decepticons와 Transformers 관련 명칭은 Hasbro, Inc.의 상표입니다. Command Center는 Hasbro와 무관하며 후원이나 보증을 받지 않았습니다. 이 문서는 패러디 목적의 비유로만 그 이름을 언급하고, 관련 이미지나 로고를 포함하지 않습니다.</sub>

<p align="center"><sub>Made by Terrans, for humans. 🧑‍🤝‍🧑</sub></p>
