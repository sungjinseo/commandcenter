<h1 align="center">Command Center</h1>

<p align="center"><b>한국어</b> · <a href="README.en.md">English</a></p>

<p align="center">
  Nexus Repository를 대신하는, 작고 단단한 오픈소스 아티팩트 레지스트리.<br>
  Maven · npm · PyPI · Go · Cargo · Docker · raw · Helm · Yum · NuGet · APT · RubyGems · Composer · Conda · Conan · R · Git LFS · p2 · Swift · CocoaPods · HuggingFace · Terraform을 한 바이너리에 담아 Kubernetes 파드 하나로 돌립니다.
</p>

<p align="center">
  <a href="https://github.com/sungjinseo/commandcenter/actions/workflows/ci.yml"><img alt="CI" src="https://github.com/sungjinseo/commandcenter/actions/workflows/ci.yml/badge.svg?branch=main"></a>
  <a href="https://github.com/sungjinseo/commandcenter/releases"><img alt="Release" src="https://img.shields.io/github/v/release/sungjinseo/commandcenter?style=flat-square"></a>
  <img alt="Go 1.26" src="https://img.shields.io/badge/Go-1.26-00add8?style=flat-square&logo=go&logoColor=white">
  <img alt="License: MIT OR Apache-2.0" src="https://img.shields.io/badge/license-MIT%20%2F%20Apache--2.0-3a3a3a?style=flat-square">
</p>

<p align="center">
  <a href="#왜-command-center인가">왜</a> ·
  <a href="#지원-포맷">포맷</a> ·
  <a href="#기능">기능</a> ·
  <a href="#빠른-시작">빠른 시작</a> ·
  <a href="#nexus에서-갈아타기">Nexus에서 갈아타기</a> ·
  <a href="docs/guide.md">운영 가이드</a> ·
  <a href="#현황과-제한">현황</a>
</p>

---

## 왜 Command Center인가

사내 패키지 저장소는 대부분 Nexus Repository OSS로 시작합니다. 그러다 JVM 힙 4 GB, 느린 UI, 그리고 "이 기능은 Pro에서만"을 만납니다. Command Center는 그 자리를 그대로 이어받도록 만들었습니다.

- **같은 URL 규칙.** `/repository/<이름>/...`과 Docker의 `/v2/`를 그대로 쓰므로 클라이언트 설정은 호스트 이름만 바꾸면 됩니다.
- **같은 운영 모델.** hosted · proxy · group, 사용자 · 역할 · 권한, Cleanup 정책, 예약 작업, blob store. Nexus를 써 본 사람에게 설명이 필요 없는 구조입니다.
- **Nexus REST API 호환과 이전 도구.** 기존 스크립트와 Terraform 프로바이더가 `/service/rest/v1`로 그대로 동작하고, `import-nexus`가 hosted 저장소를 통째로 옮겨 줍니다.
- **작습니다.** CGO 없는 Go 바이너리 하나. 128 MiB 컨테이너에서 돌고, 부하 테스트 최대 RSS는 43 MiB입니다.
- **에디션이 없습니다.** LDAP, OIDC(SSO), S3 blob store, PostgreSQL은 물론 Nexus Pro 기능(SAML, 사용자 토큰, 스테이징 · 태그, blob store 그룹, retain 정리, 내보내기 · 가져오기, active-active 고가용성)까지 전부 들어 있고, 라이선스는 MIT 또는 Apache-2.0 중 고르면 됩니다.

## 지원 포맷

| 포맷 | hosted | proxy | group | 클라이언트 |
|---|:-:|:-:|:-:|---|
| Maven | ✅ | ✅ | ✅ | mvn, Gradle |
| npm | ✅ | ✅ | ✅ | npm, pnpm, yarn |
| PyPI | ✅ | ✅ | ✅ | pip, twine, uv (PEP 503/658, JSON API) |
| Go | ✅ | ✅ | ✅ | go (GOPROXY) |
| Cargo | ✅ | ✅ | ✅ | cargo (sparse 인덱스) |
| Docker / OCI | ✅ | ✅ | ✅ | docker, skopeo, crane, helm push oci:// |
| raw | ✅ | ✅ | ✅ | curl, 브라우저 (디렉터리 목록) |
| Helm | ✅ | ✅ | ✅ | helm repo add, helm cm-push |
| Yum / DNF | ✅ | ✅ | ✅ | dnf, yum (repodata 자동 생성) |
| NuGet | ✅ | ✅ | ✅ | dotnet nuget, nuget.exe (V3) ※ |
| APT | ✅ | ✅ | ✅ | apt (signed Release 지원) ※ |
| RubyGems | ✅ | ✅ | ✅ | gem, bundler (compact index) ※ |
| Composer | ✅ | ✅ | ✅ | composer 2 (Packagist proxy) ※ |
| Conda | ✅ | ✅ | ✅ | conda, mamba ※ |
| Conan | ✅ | ✅ | ✅ | conan 2, conan 1 (v1 API) ※ |
| R | ✅ | ✅ | ✅ | install.packages, R CMD INSTALL ※ |
| Git LFS | ✅ | ❌ | ❌ | git lfs ※ |
| p2 | ❌ | ✅ | ✅ | Eclipse, Tycho ※ |
| Swift | ✅ | ✅ | ✅ | swift package-registry ※ |
| CocoaPods | ❌ | ✅ | ❌ | pod install (CDN) ※ |
| HuggingFace | ❌ | ✅ | ❌ | huggingface_hub, transformers ※ |
| Terraform | ✅ | ✅ | ✅ | terraform, tofu ※ |

※ 표시한 포맷은 클라이언트 요청을 재현한 프로토콜 테스트로만 확인했고, 실제 클라이언트로는 아직 검증하지 않았습니다. 나머지는 실제 클라이언트로 E2E 시험을 거쳤습니다.

포맷별 알려진 제한은 [제한 사항](docs/guide.md#제한-사항)에 있습니다.

## 기능

- **웹 UI**: 둘러보기, 검색, 업로드, 컴포넌트 메타데이터(의존성·라이선스), 저장소 · 사용자 · 역할 · Realm · Blob store · 작업 관리. 한국어 · 영어, 라이트 · 다크. 바이너리에 내장되어 폐쇄망에서도 뜹니다.
- **인증과 권한**: 로컬 사용자, 역할과 저장소별 권한(browse · read · add · edit · delete), API 토큰, **LDAP**(표준 라이브러리 구현), **OIDC**(Keycloak · Gitea · GitHub 등, 그룹→역할 매핑), Docker 토큰 인증.
- **SSO와 외부 디렉터리**: OIDC, SAML 2.0(서명·암호화된 assertion 검증), Atlassian Crowd, 리버스 프록시 헤더(RUT) Realm. 그룹 → 역할 매핑과 로그인한 모든 사용자에게 주는 기본 역할.
- **사용자 토큰**: 사용자 이름·비밀번호 대신 쓰는 이름 코드·패스 코드 한 쌍(만료, 저장소 접근에 토큰만 받기).
- **운영 가시성**: 저장소별 요청 수 · 오류율 · 응답 시간 · 원격 실패 · 캐시 적중률, 요청 추이, 활동 로그, 상태 점검, 로그 뷰어, 실패 알림 메일.
- **스토리지**: 파일 또는 **S3 호환** blob store(여러 개), 내용 주소 저장으로 중복 제거, blob GC. 메타데이터는 **SQLite** 또는 **PostgreSQL**, 둘 사이의 이전 도구 포함.
- **Blob store 그룹과 S3 장애 조치**: 파일·S3 store 여러 개를 한 store로 묶기(라운드 로빈, 읽기 전용 멤버, 멤버 비우기), 소프트 쿼터, 복제 버킷으로 읽기 장애 조치, SSE-S3 · SSE-KMS.
- **정리와 자동화**: Cleanup 정책(나이 · 마지막 다운로드 · 이름 패턴), 예약 작업과 유지보수 작업(blob 검증, Maven 스냅샷 정리, 메타데이터 재생성, 임시 파일 정리), 저장소 Online/Offline 전환.
- **Cleanup 버전 유지**: 컴포넌트마다 최신 N개 버전을 포맷별 버전 규칙으로 남기기(retain), 제외 패턴, 저장 전 미리보기(CSV).
- **태그와 스테이징**: 컴포넌트에 태그를 붙이고 태그 · 검색 조건으로 hosted 저장소 사이를 옮기기(양쪽 메타데이터 재생성), 서비스 중에 저장소의 blob store 옮기기.
- **내보내기 · 가져오기**: 저장소를 검증 가능한 디렉터리로 내보내고(하드 링크, 증분) 다른 인스턴스로 가져오기.
- **취약점 데이터와 방화벽(OSV)**: 10개 생태계 컴포넌트의 알려진 취약점 · 악성 패키지 보고, proxy 방화벽 격리와 허용 목록, 폐쇄망용 오프라인 덤프.
- **운영 도구**: Prometheus 메트릭 엔드포인트, 지원 번들(support zip), 스레드 덤프, 실행 중 로그 레벨 변경, 외부 HTTP 프록시 설정, SSL 신뢰 저장소(사내 CA).
- **Webhook**: 전역 또는 저장소별로 모든 활동 이벤트를 HTTP POST로 전달(HMAC 서명, 재시도, 전달 기록).
- **라우팅 규칙**: proxy가 원격에 물어볼 경로를 정규식으로 BLOCK/ALLOW(사내 아티팩트 유출 방지).
- **컨텐츠 셀렉터**: CSEL 식(`format`, `path`, `coordinate.groupId`)으로 역할 권한을 경로 단위로 제한.
- **Kubernetes 우선**: Helm 차트(StatefulSet, `replicaCount`로 여러 Pod를 띄우는 Deployment, values 스키마, PostgreSQL · NFS · HTTPRoute 지원, `helm test`), 참조 매니페스트, 릴리스마다 amd64 · arm64 이미지와 SBOM · provenance.
- **Nexus 호환**: `/service/rest/v1`의 repositories · components · assets · search · status · blobstores · cleanup-policies · tasks · tags · staging · user-tokens API, `commandcenter import-nexus`.
- **OpenAPI 문서**: 모든 관리 API와 Nexus 호환 API를 설명하는 OpenAPI 3.0 문서(`/-/api/openapi.json`, `/service/rest/swagger.json`).

## 빠른 시작

릴리스 이미지는 `ghcr.io/sungjinseo/commandcenter`입니다. 아래의 `0.1.1`은 [최신 릴리스](https://github.com/sungjinseo/commandcenter/releases) 버전으로 바꾸세요.

먼저 관리자 비밀번호 해시를 만듭니다(8자 이상).

```bash
docker run --rm -i ghcr.io/sungjinseo/commandcenter:0.1.1 hash-password
```

### Kubernetes (Helm)

```yaml
# my-values.yaml
config:
  baseURL: https://packages.example.com       # 클라이언트가 접속하는 주소
  adminPasswordHash: '<위에서 나온 해시>'
persistence:
  storageClass: <StorageClass>                 # blob 저장소. NFS도 됩니다
database:
  existingSecret: commandcenter-db             # 비우면 SQLite. PostgreSQL은 가이드 참고
```

```bash
helm install commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 0.1.1 \
  --namespace commandcenter --create-namespace -f my-values.yaml --wait
helm test commandcenter -n commandcenter
```

Ingress/HTTPRoute, TLS, PostgreSQL, 비밀값 키, 백업과 업그레이드는 운영 가이드의 [Kubernetes](docs/guide.md#kubernetes)에 있습니다.

### Kubernetes (Helm 없이, YAML 하나)

Helm을 쓰지 않는 클러스터에는 릴리스마다 붙는 매니페스트 파일(`commandcenter-v0.1.1.yaml`, 이미지가 그 릴리스로 고정됨)을 씁니다. 저장소의 [deploy/kubernetes/commandcenter.yaml](deploy/kubernetes/commandcenter.yaml)과 같은 내용입니다.

```bash
curl -LO https://github.com/sungjinseo/commandcenter/releases/download/v0.1.1/commandcenter-v0.1.1.yaml
# 파일 안의 baseURL, passwordHash, storageClassName 세 곳을 채운 뒤
kubectl create namespace commandcenter
kubectl apply -n commandcenter -f commandcenter-v0.1.1.yaml
```

파드 하나짜리 StatefulSet, ConfigMap의 `config.yaml`, PVC, Service가 들어 있습니다. Ingress나 Gateway는 클러스터 방식대로 붙입니다. 차트 파일(`commandcenter-helm-chart-0.1.1.tgz`)도 릴리스에 있어 OCI 레지스트리를 못 쓰는 곳에서는 `helm install commandcenter ./commandcenter-helm-chart-0.1.1.tgz -f my-values.yaml`로 설치합니다.

### Docker compose

```bash
git clone --depth 1 --branch v0.1.1 https://github.com/sungjinseo/commandcenter.git
cd commandcenter
echo COMMANDCENTER_VERSION=0.1.1 >> .env
cp config.example.yaml config.yaml     # passwordHash 자리에 해시를 넣습니다
docker compose up -d
```

`http://localhost:8081/`을 열고 `admin`으로 로그인합니다.

### 클라이언트 설정

저장소 화면의 "사용법" 탭이 포맷별 설정(Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, Cargo `config.toml`, `docker login`, `helm repo add`, `.repo` 파일)을 복사할 수 있는 형태로 보여 줍니다. 전체 목록은 [포맷별 클라이언트 설정](docs/guide.md#포맷별-클라이언트-설정)에 있습니다.

## Nexus에서 갈아타기

1. Command Center에 같은 포맷의 저장소를 만듭니다(웹 UI, 설정 파일, 또는 Nexus 호환 API `POST /service/rest/v1/repositories/<format>/<type>`).
2. hosted 저장소의 내용을 옮깁니다.

   ```bash
   commandcenter import-nexus -from https://nexus.example.com -from-user admin -from-password-file nexus.pw \
     -to https://packages.example.com -to-user ci -to-token-file cc.token -all -dry-run   # 먼저 확인
   commandcenter import-nexus ... -all                                                  # 복사 (재실행 안전)
   ```

   Maven · npm · PyPI · raw · Helm · Docker를 각 포맷의 업로드 프로토콜로 옮기므로 서버 검증이 그대로 적용됩니다. proxy는 새로 만들면 첫 요청 때 다시 채워집니다.
3. 클라이언트 설정의 호스트만 바꿉니다. Nexus REST API를 쓰던 스크립트는 그대로 둡니다.

자세한 절차는 [Nexus에서 이전하기](docs/guide.md#nexus에서-이전하기)와 [Nexus REST API 호환](docs/guide.md#nexus-rest-api-호환)에 있습니다.

## 프로덕션 체크리스트

- TLS는 Ingress나 리버스 프록시에서 끝내고 `baseURL`을 `https://` 주소로 둡니다.
- 사내 전용이면 익명 읽기(`anonymousRead`)를 끕니다. CI에는 비밀번호 대신 API 토큰을 줍니다.
- 저장된 비밀값(SMTP · LDAP · OIDC · Crowd · SAML · 원격 인증 · S3)을 암호화하는 키(`COMMANDCENTER_SECRET_KEY` 또는 `secretKeyFile`)를 두고 백업과 따로 보관합니다.
- 메타데이터는 PostgreSQL에 둡니다. 여러 대를 함께(active-active) 띄우려면 모든 노드가 같은 PostgreSQL DB와 공유 스토리지(ReadWriteMany)를 씁니다.
- 중지 대기 시간 60초(차트와 compose에 들어 있음), 정기 백업, 업그레이드 전 백업. 다운그레이드는 지원하지 않습니다.

전체 목록은 [프로덕션 배포](docs/guide.md#프로덕션-배포)에 있습니다.

## 얼마나 가볍나요

| 측정 (`test/e2e.sh`) | 결과 |
|---|---|
| 포맷별 스모크 테스트 (UI, LDAP, 메일 포함) | 6 ~ 37 MiB |
| 부하 테스트: 100 MiB 파일 동시 다운로드 · 업로드, 적대적 업로드 | 최대 RSS 43 MiB |
| Helm proxy, bitnami 크기(30 MB) index.yaml | 약 40 MiB (스트리밍 처리) |

기본 컨테이너 한도는 128 MiB(`GOMEMLIMIT=80MiB`)입니다.

## 현황과 제한

- 여러 대를 함께 띄우는 active-active 구성을 지원합니다. 모든 노드가 같은 PostgreSQL DB와 공유 스토리지(ReadWriteMany)를 씁니다. 쓰기는 DB에서 하나씩 처리되므로 늘어나는 것은 가용성과 읽기 처리량입니다.
- Nexus REST API는 저장소 · 컴포넌트 · 자산 · 검색 · 상태 · blob store · Cleanup 정책 · 작업 · 태그 · 스테이징 · 사용자 토큰 엔드포인트를 지원합니다. 사용자 · 역할 쓰기, 스크립트 API는 없습니다.
- 알려진 제한은 [제한 사항](docs/guide.md#제한-사항), 릴리스마다 고친 것과 바뀐 것은 [CHANGELOG](CHANGELOG.md)에 있습니다.
- 버그는 이슈로, 보안 문제는 [보안 정책](SECURITY.md)대로 비공개로 알려 주세요.

## 문서

- [운영 가이드](docs/guide.md): 설치, Kubernetes, 프로덕션 배포, 설정 레퍼런스, 인증 · 권한, LDAP · OIDC · SAML · Crowd, blob store, 포맷별 설정, 백업과 업그레이드, Nexus 이전과 호환 API
- [Helm 차트](deploy/helm/commandcenter) · [Kubernetes 참조 매니페스트](deploy/kubernetes/commandcenter.yaml)
- [CHANGELOG](CHANGELOG.md) · [기여 안내](CONTRIBUTING.md) · [행동 강령](CODE_OF_CONDUCT.md) · [보안 정책](SECURITY.md) · [출처 표기](ATTRIBUTION.md)

## 기여

이슈, 문서 수정, 버그 수정, 새 포맷 모두 환영합니다. 개발 환경과 테스트 방법은 [기여 안내](CONTRIBUTING.md)와 [운영 가이드의 개발 절](docs/guide.md#개발)에 있습니다.

## 라이선스

MIT 또는 Apache-2.0 중 원하는 쪽으로 쓰시면 됩니다([LICENSE-MIT](LICENSE-MIT), [LICENSE-APACHE](LICENSE-APACHE)). 웹 UI의 IBM Plex 글꼴은 SIL Open Font License입니다.
