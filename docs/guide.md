# Command Center 운영 가이드

> 첫 화면과 소개는 [README](../README.md)에 있습니다. 이 문서는 설치, 설정, 포맷별 클라이언트 설정, 운영을 자세히 다룹니다.

## 소개

Command Center는 저메모리 패키지 저장소입니다. 에디션은 하나뿐인 오픈소스입니다. Go 단일 바이너리이며 Docker로 실행하고, 설정 파일은 `config.yaml` 하나입니다.
업계에서 널리 쓰이는 `/repository/<저장소이름>/...` 경로 규칙을 따르므로, 이미 이 규칙으로 설정한 클라이언트는 호스트만 바꾸면 됩니다.

지원 포맷과 저장소 유형:

| 포맷 | hosted (업로드) | proxy (원격 캐시) | group (묶음) |
|---|---|---|---|
| Maven | O | O | O |
| npm | O | O | O |
| PyPI | O | O | O |
| Go | O | O | O |
| Cargo | O | O | O |

- **hosted**: 직접 업로드한 패키지를 저장합니다.
- **proxy**: 원격 저장소(Maven Central, npmjs, pypi.org, proxy.golang.org, crates.io 등)를 처음 요청할 때 가져와 캐시합니다.
- **group**: 여러 저장소를 하나의 URL로 묶어 읽습니다. `writeMember`를 지정하면 업로드를 그 hosted 멤버로 보냅니다.

메모리 사용량(`test/e2e.sh`로 측정):

- 포맷별 스모크 테스트(`docker stats`): Maven 22 MiB, npm 8.4 MiB, PyPI 21 MiB, Go 11 MiB, Cargo 9.9 MiB, UI 7.1 MiB
- 부하 테스트(`test/load/load.sh`): 128 MiB 컨테이너에서 100 MiB 파일 50개 동시 다운로드, 5개 동시 업로드, 적대적 업로드를 처리하는 동안 최대 RSS 53 MiB 이하

## 빠른 시작

1. 이미지를 빌드합니다.

   ```bash
   docker compose build
   ```

2. 사용자 비밀번호 해시를 만듭니다. 명령을 실행한 뒤 비밀번호를 입력하고 Enter를 누르세요.

   ```bash
   docker run --rm -i commandcenter:latest hash-password
   ```

3. `config.example.yaml`을 `config.yaml`로 복사하고 `passwordHash` 자리에 2번의 출력값을 넣습니다.

   ```bash
   # Git Bash / macOS / Linux
   cp config.example.yaml config.yaml
   ```

   ```powershell
   # PowerShell
   Copy-Item config.example.yaml config.yaml
   ```

   > `docker compose up` 전에 `config.yaml`이 반드시 존재해야 합니다. 없으면 Docker가 같은 이름의 디렉터리를 만들어 서버 시작에 실패합니다.

4. 실행합니다.

   ```bash
   docker compose up -d
   ```

   상태 확인: `http://localhost:8081/service/rest/v1/status` 가 200을 반환하면 정상입니다.

## repolite에서 이전하기

이전 이름(repolite)으로 설치한 서버를 Command Center로 바꿀 때 확인할 것입니다. 데이터 형식(`dataDir`, 디스크 구조)은 바뀌지 않았습니다.

1. **이전 스택을 먼저 내리고 데이터 볼륨을 이어서 쓰기.** 순서가 중요합니다. 먼저 **이전 checkout 폴더에서** 이전 스택을 내립니다(compose 파일을 새 버전으로 받거나 바꾸기 전에).

   ```bash
   docker compose down --remove-orphans   # -v는 붙이지 마세요. 볼륨이 지워집니다
   ```

   그다음 compose 파일을 새 버전으로 받고 볼륨 이름을 지정해 올립니다. compose의 기본 볼륨 이름이 `commandcenter-data`로 바뀌었으므로 그냥 `docker compose up`을 하면 빈 볼륨이 새로 생깁니다. 기존 데이터를 쓰려면 `COMMANDCENTER_VOLUME`에 이전 볼륨 이름을 지정하세요. 이름은 `docker volume ls`로 찾습니다. 보통 `repolite_repolite-data`(compose 프로젝트 이름 + 볼륨 이름)입니다.

   ```bash
   docker volume ls | grep repolite
   ```

   ```powershell
   # PowerShell: 현재 세션에서만 유효
   $env:COMMANDCENTER_VOLUME = "repolite_repolite-data"
   docker compose up -d
   ```

   ```bash
   # Git Bash / macOS / Linux
   COMMANDCENTER_VOLUME=repolite_repolite-data docker compose up -d
   ```

   매번 지정하기 번거로우면 `docker-compose.yml`과 같은 폴더의 `.env` 파일에 적어 두세요. compose가 자동으로 읽습니다.

   ```
   COMMANDCENTER_VOLUME=repolite_repolite-data
   ```

   이전 볼륨(`repolite_repolite-data`)을 지정해 올리면 `volume "…" already exists but was created for project "repolite"` 경고가 나올 수 있습니다. 데이터에는 영향이 없는 무해한 경고입니다. 이전 이름의 컨테이너가 남아 있으면 `docker rm -f repolite`로 정리합니다.
2. **설정 파일 경로.** 컨테이너 안의 설정 경로가 `/etc/repolite/config.yaml`에서 `/etc/commandcenter/config.yaml`로 바뀌었습니다. 새 경로에 파일이 없고 이전 경로에 있으면 이전 경로를 쓰고 시작할 때 경고를 로그에 남깁니다. 그래서 이전 compose 파일을 그대로 써도 동작하지만, `docker-compose.yml`의 마운트를 새 경로로 고치는 것을 권장합니다.
3. **토큰.** 이전에 발급한 `rl_` 토큰은 계속 동작합니다(Bearer, Cargo 토큰, Basic 비밀번호 자리 모두). 새로 발급하는 토큰은 `cc_`로 시작합니다. 클라이언트 설정을 바꿀 필요는 없습니다.
4. **UI 세션 초기화.** 세션 쿠키 이름이 `cc_session`으로 바뀌고 세션은 원래 메모리에만 있으므로, 업그레이드하면 모두 로그아웃되어 한 번 다시 로그인해야 합니다. 로그인과 로그아웃 때 이전 `repolite_session` 쿠키는 만료시켜 지웁니다.
   - **클라이언트 설정의 id.** 웹 UI와 이 문서의 새 클라이언트 설정 예시는 서버 id와 레지스트리 이름으로 `commandcenter`를 씁니다. `repolite`를 쓰는 기존 `settings.xml`과 cargo 설정은 그대로 동작합니다. 다만 `settings.xml`에서는 `<server>`의 `<id>`와 저장소(`<repository>`, `<mirror>`)의 id가 서로 같아야 인증이 붙습니다. 새 이름으로 바꾸려면 둘을 함께 바꾸세요.
5. **이미지와 컨테이너 이름.** 이미지는 `commandcenter:latest`, compose 서비스와 컨테이너는 `commandcenter`입니다. `docker logs commandcenter`, `docker exec commandcenter ...`처럼 이름을 쓰는 스크립트와 모니터링을 고치세요. 바이너리 서브커맨드(`commandcenter hash-password`, `token create`, `gc`, `healthcheck`)도 같은 이름입니다. `token create`는 `-config`를 생략하면 기본 경로(`/etc/commandcenter/config.yaml`)를 쓰고, 없으면 이전 경로로 대체하므로 예시에서는 `-config`를 쓰지 않습니다. 다른 경로의 파일을 쓸 때만 `-config <경로>`를 붙이세요.

## 웹 UI

브라우저에서 `http://localhost:8081/`(또는 `baseURL`)에 접속하면 `/ui/`로 이동합니다. 별도 설치나 외부 리소스 없이 서버에 내장되어 있어 폐쇄망에서도 동작합니다. 정적 파일 합계는 512 KiB 이하이며 바이너리에 포함됩니다. 화면 구조(사이드바, 상단 막대, 카드와 표)는 직접 디자인한 것이고 다른 제품의 로고, 아이콘, CSS 같은 자산은 쓰지 않습니다. 해시 경로는 `#browse/...`, `#admin/...` 형태입니다.

### 언어와 테마

- **언어 (KO/EN)**: 상단 막대의 KO/EN 버튼으로 한국어와 영어를 전환합니다. 선택은 브라우저에 저장됩니다. 저장된 값이 없으면 브라우저 언어가 영어(`en…`)일 때만 영어이고, 그 밖에는 한국어입니다. Hosted, Proxy, Group, Blob Store 같은 기술 용어는 두 언어 모두 영어로 둡니다. 서버가 돌려주는 오류 메시지는 번역하지 않고 영어 그대로 보여 줍니다.
- **테마**: 상단 막대의 테마 버튼을 누를 때마다 라이트, 다크, 시스템 순으로 바뀝니다. 기본값은 시스템(`prefers-color-scheme`)이며 선택은 브라우저에 저장됩니다.

### 화면 구성

- **상단 막대**: 현재 위치(브레드크럼), 컴포넌트 검색창(`/` 키로 포커스, 입력 중에는 동작하지 않음), 테마 버튼, 언어 버튼, 사용자 영역. 로그인 전에는 로그인 버튼이 있고 누르면 사용자 이름과 비밀번호 대화상자가 열립니다. 로그인 후에는 사용자 메뉴에서 계정, API 토큰, 로그아웃을 고릅니다. 화면 너비가 800px 이하이면 사이드바가 66px 폭의 아이콘 레일로 줄어들고 항목 이름은 툴팁으로 보입니다(상단 검색창과 포맷별 검색 하위 링크도 숨겨지며, 검색 화면에서 포맷을 고를 수 있습니다).
- **사이드바**: 아래 항목을 묶음별로 보여 주고, 권한이 없는 항목은 숨깁니다.
  - WORKSPACE: 대시보드, 저장소 탐색, 컴포넌트 검색, 컴포넌트 업로드(업로드 가능한 사용자만)
  - OPERATIONS(관리자): 저장소 설정, Blob Store, Cleanup 정책, 예약 작업
  - ACCESS CONTROL: 사용자·역할·Realm(관리자), API 토큰(로그인한 사용자)
  - SYSTEM(관리자): 일반 설정(이메일 알림 포함), 시스템 상태, 시스템 정보
  - 맨 위의 인스턴스 표시에 접속 호스트와 상태 점(초록/빨강)이 나오고, 맨 아래에 "COMMAND CENTER"와 "오픈소스" 문구가 나옵니다. 버전은 로그인한 사용자에게만 표시됩니다(`/-/api/me`의 `version`).
- 로그인하지 않아도 `anonymousRead`로 읽을 수 있는 저장소는 볼 수 있습니다. 읽을 수 없는 저장소는 목록에서 빠지고 직접 열어도 404입니다.

### 경로

| 경로 | 화면 | 권한 |
|---|---|---|
| `#browse/welcome` | 대시보드: 지표 카드, 주요 저장소, 저장 공간, 바로가기 | 누구나 |
| `#browse/search/generic=keyword%3D<q>` | 컴포넌트 검색: Keyword, Format, Repository | 읽기 가능한 저장소만 |
| `#browse/search/custom` | Custom search: Keyword, Format, Repository, Group, Name, Version | 〃 |
| `#browse/search/<fmt>` (`cargo`, `golang`, `maven`, `npm`, `pypi`) | 포맷별 검색 | 〃 |
| `#browse/browse` | 저장소 탐색: 저장소 표(형식, 이름, 유형, 상태, 파일 수, 크기)와 텍스트·포맷·유형 필터 | 〃 |
| `#browse/browse:<repo>` | 저장소 트리와 상세 패널 | 〃 |
| `#browse/browse:<repo>:<경로>` | 트리에서 해당 폴더·파일 선택 (경로는 `encodeURIComponent`) | 〃 |
| `#browse/upload` | 업로드: 올릴 수 있는 저장소 목록 | 쓰기 가능한 hosted maven/npm/pypi가 있는 사용자 |
| `#browse/upload/<repo>` | 해당 저장소의 업로드 폼 | 〃 |
| `#admin/repository/repositories[:<repo>]` | 저장소 설정 (목록, 상세, 편집, 삭제) | 관리자 |
| `#admin/repository/repositories:create` | 저장소 만들기: 레시피 선택(maven2, npm, pypi, go, cargo × hosted/proxy/group)과 폼 | 관리자 |
| `#admin/security/users[:<id>]` | 사용자 (읽기 전용) | 관리자 |
| `#admin/security/usertoken` | API 토큰: 전체 사용자 (관리자) | 관리자 |
| `#admin/support/status[:<check>]` | 시스템 상태 (상태 점검, 지표) | 관리자 |
| `#admin/support/systeminformation` | 시스템 정보 | 관리자 |
| `#admin/system/tasks[:<id>]` | 예약 작업 | 관리자 |
| `#user/account` | 계정 (사용자, `config.yaml`의 역할, 관리자 여부, 읽기 전용. 역할은 `/-/api/me`가 돌려줍니다) | 로그인 |
| `#user/usertoken` | API 토큰 (본인 토큰) | 로그인 |
| `#login` | 대시보드 위에 로그인 대화상자 | 누구나 |

이전 버전의 `#/...` 북마크는 새 경로로 자동 이동합니다: `#/` 은 `#browse/welcome`, `#/r/<저장소>` 와 `#/r/<저장소>/browse/<경로>` 는 Browse 트리, `#/search?q=` 는 일반 검색, `#/tokens` 는 `#user/usertoken`, `#/admin` 은 `#admin/support/status`, `#/login` 은 `#login`.

### Browse

저장소를 열면 왼쪽에 디렉터리 트리, 오른쪽에 상세 패널이 나옵니다(트리는 방향키, Home/End, Enter로 조작). 한 폴더에는 최대 500개까지 표시하고 넘으면 안내가 나옵니다. 상세 패널은 세 가지입니다.

- **저장소(루트)**: Summary(Format, Type, URL, Remote, Files, Size, Children)와 포맷별 **Repository setup**(Maven `settings.xml`, `.npmrc`, `pip.conf`, `GOPROXY`, `~/.cargo/config.toml` 등, 복사 버튼).
- **컴포넌트**: Summary(Repository, Format, Group, Name, Version), Usage(도구를 고르면 Maven/Gradle, npm, pip, go get, cargo add 같은 사용 예 복사), Assets(파일 목록과 크기). Maven hosted 저장소에서 쓰기 권한이 있으면 "Delete component".
- **파일(asset)**: Summary(Path, Content type, File size, Blob created/updated, Blob reference(SHA-256), 프록시는 Locally cached), Usage, Attributes(checksum: sha1, md5, sha256, sha512 등)와 "Download". Maven hosted 저장소에서 쓰기 권한이 있으면 "Delete asset".
- 폴더: Summary(Repository, Path, Children).

### 검색

헤더 검색창과 Search 화면은 같은 API(`GET /-/api/search/components`)를 씁니다. 기준 중 하나는 2자 이상이어야 하고, 결과는 최대 100개 컴포넌트(컴포넌트당 파일 50개)이며 넘으면 "truncated" 안내가 나옵니다. 결과 표의 열은 Name, Group, Version, Format, Repository, Last Updated이고, 행을 누르면 컴포넌트 상세(Summary, Usage, Assets)와 파일 상세로 들어갑니다. 일치하는 항목이 없으면 "No components matched the filter criteria"가 나옵니다.

- Generic/Custom: Keyword(group, name, version 중 어디든), Format, Repository, 그리고 More criteria(Group, Name, Version).
- Maven: Group Id, Artifact Id, Version, Base Version, Classifier, Extension.
- npm: Scope, Name, Version. PyPI, Go, Cargo: Name, Version.
- 이름 계열은 대소문자를 구분하지 않는 부분 일치입니다. Version은 `*`가 없으면 정확히 일치하고 `*`가 있으면 glob입니다.
- group 저장소는 검색하지 않습니다(멤버 저장소에서 찾습니다).

### 컴포넌트는 파생 값

Command Center는 파일만 저장하므로 컴포넌트는 서버가 요청 시점에 계산합니다(저장하지 않음).

- **Maven**: 경로 `g1/g2/…/artifact/version/파일` 에서 group `g1.g2…`, name `artifact`, version을 얻습니다. `maven-metadata.xml*` 은 컴포넌트가 아닙니다.
- **npm**: `이름/-/이름-버전.tgz`, scope 패키지는 `@scope/이름/-/…` (group `@scope`).
- **Go**: `모듈/@v/버전.(zip|mod|info)`.
- **Go**의 경로는 대소문자 인코딩(`!b` = `B`)을 쓰므로 모듈 이름과 버전은 풀어서 보여 줍니다(`github.com/!burnt!sushi/toml` → `github.com/BurntSushi/toml`).
- **PyPI, Cargo**: hosted 저장소는 packages 테이블의 이름과 버전. PyPI는 파일 이름에서 표시 버전을 뽑습니다.
- **프록시 저장소(npm, PyPI, Cargo)**: packages 테이블에 행이 없으므로 캐시된 파일의 경로(assets 테이블)에서 같은 방식으로 파생합니다.
- 검색은 메타 DB에서 한도까지만 훑습니다: assets 테이블 질의 한 번마다 최대 2000개 파일, packages 테이블은 이름 2000개와 버전 2000개까지 봅니다(`keyword`만 주면 이름으로 좁힌 검사를 먼저 하고, 한도가 남으면 버전 일치를 찾는 전체 검사를 이어서 합니다). 한도에 걸리거나 10초 안에 끝나지 않으면 그때까지의 결과를 `truncated: true`로 돌려줍니다. 검색은 무거운 질의용 세마포어 안에서 실행하므로 메모리 사용이 일정합니다.

### 관리자 화면

- **Repositories**: 목록, 상세, 만들기·편집·삭제(아래 "저장소 관리"). Name, Format, Type, URL, Online(Online/Offline 전환), Blob Store, Remote, Members, 저장소 크기, 그리고 versionPolicy, writePolicy, metadataMaxAge, contentMaxAge, negativeCacheTTL(설정값).
- **Users**: 사용자 목록·만들기·편집·삭제·비밀번호 재설정(User ID, Source, Roles, Status). LDAP 로그인 사용자는 최근에 로그인한 사용자만 `directory` 행으로 보입니다("인증과 권한", "Realm과 LDAP" 참고). 비밀번호 해시는 절대 내보내지 않습니다.
- **Roles**: 역할과 권한(대상 × 동작) 편집. 내장 `admin` 역할은 수정·삭제할 수 없습니다.
- **Realms**: Realm 순서와 켜기/끄기, LDAP 설정과 연결 테스트.
- **Account**: 로그인한 사용자 본인의 비밀번호 변경(로컬 사용자만).
- **Blob Store**: 이름 붙은 blob 저장소 만들기·삭제와 사용량("Blob Store" 참고).
- **General settings**: baseURL, 익명·인증 사용자 읽기, 세션 유휴 시간, 최대 업로드 크기, 기본 언어, 이메일(SMTP) 설정과 테스트 메일("일반 설정", "이메일 알림" 참고).
- **API 토큰**: 관리자는 전체 사용자의 토큰을 조회·폐기합니다(`#admin/security/usertoken`). 일반 사용자는 `#user/usertoken`)에서 본인 토큰을 발급·폐기합니다. 토큰 값은 발급 직후 한 번만 표시되며, `commandcenter token create` 대신 쓸 수 있습니다.
- **Status**: 상태 점검 Database, Blob store, Memory(90% 이상이면 비정상), Remote fetch slots, Repositories. Metrics에는 서버 상태(버전, 가동 시간, RSS, 힙, 고루틴, GC 횟수)와 5초마다 갱신되는 RSS 그래프, 그리고 blob 합계(개수와 총 용량, 같은 내용은 한 번만 셈)가 표시됩니다. 저장소별 용량은 Repositories 화면에 있습니다.
- **System Information**: `commandcenter-status`, `commandcenter-configuration`, `system-runtime`, `system-environment`(GOMEMLIMIT, HTTPS_PROXY 설정 여부, TZ만), `system-filestores`. "Download as JSON" 버튼이 있습니다.
- **Tasks**: 예약 작업 목록, 만들기·편집·삭제, "지금 실행"과 마지막 실행 로그(아래 "예약 작업"). 기본으로 `compact-blobstore` 작업이 하나 있습니다.

### 의도적으로 없는 것

- **Go, Cargo 업로드 화면**: Upload는 Maven, npm, PyPI만 지원합니다. Go와 Cargo는 `curl`(Go) 또는 `cargo publish`로 올립니다.
- **Tags**, 악성 패키지 위험 분석, 라이선스 관리 화면, 유료 에디션 전용 화면, SAML 등. Human Edition에는 유료 에디션이 없습니다.

### 주의할 점

- **세션은 메모리에만 있습니다.** 서버를 재시작하면 모두 로그아웃됩니다. 세션은 최대 1000개, 마지막 사용 후 유휴 시간(일반 설정의 `sessionIdleMinutes`, 기본 720분 = 12시간) 또는 로그인 후 7일이 지나면 만료됩니다. 비활성화하거나 삭제한 사용자, 비밀번호를 바꾼 사용자의 세션은 즉시 무효가 됩니다.
- 세션 쿠키(`cc_session`)는 `HttpOnly`, `SameSite=Strict`입니다. 쓰기 요청(PUT/POST/DELETE)은 `X-Command-Center-UI: 1` 헤더가 있어야 쿠키가 인정됩니다. 작업 실행(Tasks의 Run)은 추가로 관리자이며 비밀번호 또는 세션으로 인증한 경우에만 됩니다(토큰으로는 불가). **https 리버스 프록시 뒤에서 운영하면 `baseURL`을 `https://…`로 설정하세요.** 그래야 쿠키에 `Secure` 플래그가 붙습니다.
- 쿠키는 `Authorization` 헤더가 없는 요청에서만 쓰이므로 `mvn`, `npm` 같은 클라이언트에는 영향이 없습니다.
- 파일·컴포넌트 삭제는 **Maven에서만** 가능합니다. 다른 포맷에는 삭제 버튼이 없습니다.
- 목록은 모두 상한이 있습니다(디렉터리 500, 검색 100, 패키지 버전 1000, 토큰 1000). 넘으면 화면에 안내 문구가 나옵니다.

## 저장소 관리

관리자는 저장소 설정에서 저장소를 만들고, 고치고, 지울 수 있습니다. 서버를 재시작하지 않아도 바로 반영됩니다.

> **경고: `config.yaml`의 `repositories:`는 첫 시작 때 한 번만 읽습니다.** 저장소 정의는 `meta.db`(SQLite)로 옮겨지고, 그 뒤로는 `config.yaml`의 `repositories:`를 고쳐도 **무시**됩니다(시작할 때 파일 이름을 밝히는 경고 로그가 남습니다). 저장소는 UI(또는 `/-/api/repositories` API)로 바꾸세요. 사용자, 역할, 일반 설정도 첫 시작 때 한 번만 가져오며 이후 UI에서 관리합니다("업그레이드" 참고).

- **만들기**: "Create repository"를 누르면 레시피 선택 화면(`maven2`, `npm`, `pypi`, `go`, `cargo` 각각 hosted, proxy, group)이 나오고, 고르면 해당 유형의 폼이 열립니다. 검증은 `config.yaml`과 같고 오류는 폼 맨 위에 표시됩니다. 새 저장소 이름은 영문자와 숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만 쓸 수 있으며 최대 64자입니다. `create`는 예약어라 쓸 수 없습니다(이미 저장된 이름은 그대로 동작합니다).
- **편집**: `name`, `format`, `type`은 만든 뒤 바꿀 수 없습니다(폼에서 비활성). 나머지(remote, 인증, 최대 보관 시간, negative cache TTL, group 멤버와 순서, writeMember, versionPolicy, 배포 정책(writePolicy), anonymousRead, Online/Offline)는 고칠 수 있습니다. 저장소의 Blob Store(`blobStore`)는 만들 때만 고르며 바꿀 수 없습니다("Blob Store" 참고). proxy의 `remote`를 바꿔도 캐시된 파일은 유지되지만, 이전 remote에서 받은 "없음" 기록(negative cache)은 지워집니다.
- **삭제**: 저장소 이름을 직접 입력해야 삭제됩니다. 다음 규칙이 있습니다.
  - group의 멤버(또는 writeMember)인 저장소는 409로 거부하며 메시지에 group 이름이 나옵니다. group에서 먼저 빼세요.
  - 삭제하면 그 저장소의 정의와 자산, 패키지, dist-tag, negative cache 행이 한 트랜잭션으로 지워집니다. blob 파일은 다음 GC(Compact blob store)에서 회수됩니다.
  - 역할이 그 저장소(`repo:<이름>`)에 가진 권한은 남아 있어도 더는 일치하지 않으며 시작 시 경고만 남습니다.
- **입력 검사**: 저장소를 만들거나 편집할 때(UI·API, 첫 시작의 config 가져오기) group의 멤버가 중복되면 거부하고(`duplicate member`), 시간 값(`metadataMaxAge`, `contentMaxAge`, `negativeCacheTTL`)에 음수(`-5m`)를 쓰면 거부합니다(400). "영원히"는 `-1`로만 씁니다. 이전 버전이 이미 저장한 정의는 그대로 읽습니다. 음수 시간은 0(캐시하지 않음, 즉시 만료)으로, 중복 멤버는 한 번만 쓰며 시작 시 저장소마다 경고를 한 줄 남깁니다. 그런 저장소가 있어도 서버는 시작하고 다른 저장소의 편집도 막지 않으며, 그 저장소를 편집할 때 값을 고치면 됩니다. 저장소를 만들거나 지우면 목록의 개수·크기 통계(약 30초 캐시)도 바로 다시 계산하므로, 같은 이름으로 다시 만든 빈 저장소가 이전 값을 보여 주지 않습니다.
- **원격 인증 정보**: proxy의 비밀번호와 Bearer 토큰은 DB에 저장되지만 **어떤 API와 화면에도 다시 보이지 않습니다**(설정되어 있는지 여부만 표시). 편집 때 비워 두면 기존 값을 유지하고, "clear" 체크로 지웁니다. `remote`의 **호스트가 바뀌면 인증 정보를 다시 입력해야 합니다**(토큰을 새 호스트로 옮길 수 없습니다).
- **권한**: 만들기, 편집, 삭제는 관리자이면서 비밀번호 또는 세션으로 인증한 경우에만 됩니다(API 토큰으로는 403). 쓰기 요청에는 `X-Command-Center-UI: 1` 헤더가 필요합니다.
- **백업**: 저장소 정의가 `meta.db`에 있으므로 **백업에 `meta.db`가 반드시 포함되어야 합니다**(데이터 볼륨 전체를 백업하면 됩니다). 정의 없이 blob만 복원하면 저장소가 없는 상태가 됩니다.

## 업로드

사이드바 Upload(또는 Browse의 "Upload component")에서 브라우저로 파일을 올릴 수 있습니다. 쓰기 권한이 있는 hosted Maven, npm, PyPI 저장소가 있을 때만 보입니다. 브라우저가 각 클라이언트와 같은 요청을 만들어 기존 저장소 엔드포인트로 보내므로 검증과 쓰기 정책(`writePolicy`)은 서버 규칙 그대로입니다.

| 포맷 | 필요한 것 |
|---|---|
| Maven | 파일 하나 이상(행마다 Classifier, Extension. 확장자는 파일 이름에서 채움), Group ID, Artifact ID, Version, Packaging. "Generate a POM file"을 켜면 최소 POM을 만들어 같이 올립니다. 선택한 파일 중 `.pom`이 있으면 좌표를 읽어 채우고 고정합니다. 파일마다 `PUT`하며 순서대로 올리다 첫 실패에서 멈춥니다. |
| npm | `.tgz` 하나. 브라우저가 압축을 풀어 `package/package.json`에서 이름과 버전을 읽어 보여 줍니다(1 MiB 제한). `npm publish`와 같은 publish 문서로 올리므로 문서 한도 10 MiB가 적용되며 큰 tarball은 경고가 나옵니다. |
| PyPI | wheel 또는 sdist(`.tar.gz`, `.zip`) 하나. 이름과 버전을 파일 이름에서 읽으며 고칠 수 있습니다. `twine`과 같은 multipart 업로드이고 sha256은 https 또는 localhost(보안 컨텍스트)에서 접속했을 때만 브라우저가 계산하고, 그렇지 않으면 서버가 계산합니다. |

성공하면 "Upload successful"과 Browse 링크가 나옵니다. 이미 있는 파일을 다시 올리면 포맷별 규칙대로 오류가 인라인으로 표시됩니다(Maven `allow_once` 400, npm 403, PyPI 400). 파일은 순서대로 하나씩 올리며, 서버는 기존 스트리밍 경로를 그대로 쓰므로 큰 파일도 메모리에 통째로 올리지 않습니다(npm publish 문서만 10 MiB까지 읽고, 동시 처리는 2개로 제한됩니다). npm publish 슬롯이 모두 사용 중이면 30초까지 기다리다가 `503 too many concurrent publishes, retry`로 거절하므로 잠시 후 다시 시도하세요. 슬롯을 잡은 채 본문 전송이 5분 넘게 멈추면 연결이 끊깁니다. 다른 포맷의 업로드도 마찬가지로 본문을 읽는 중에 60초 동안 한 바이트도 오지 않으면(느려도 계속 오는 전송은 끊지 않습니다) 연결을 끊어, 멈춘 업로드가 같은 경로의 잠금을 붙들고 있지 못하게 합니다. 서버가 본문을 읽기 전에 잠금을 기다리는 시간이나 본문이 없는 요청(DELETE, Cargo yank)은 이 제한에 들어가지 않습니다.

**업로드가 401로 실패할 때**: 브라우저가 `/repository` 경로에 대해 Basic 인증 정보를 캐시해 두었다면(예: 그 주소를 직접 열어 로그인한 경우) 그 값이 UI 세션보다 우선하므로 업로드가 401로 실패할 수 있습니다. 브라우저에 저장된 해당 사이트의 인증 정보를 지우고 다시 시도하세요.

## 설정 레퍼런스

설정 파일은 YAML이며 알 수 없는 키가 있으면 시작이 실패합니다. 설정을 바꾼 뒤에는 `docker compose restart`로 반영하세요.

**한 번만 가져오는 키**: `users`, `roles`, `anonymousRead`, `baseURL`, `maxUploadMiB`는 plan 12부터 **첫 시작 때 한 번만** 읽어 DB로 가져옵니다. 이후에는 파일에 남아 있어도 무시되며(시작 로그에 경고가 한 번 나옵니다) 웹 UI에서 관리합니다("업그레이드"의 이전 안내 참고). `config.yaml`에 계속 쓰는 키는 `listen`, `dataDir`, `memoryLimitMiB`, `metricsFlushSeconds`뿐입니다(`repositories`도 첫 시작에만 가져옵니다).
전체 예시는 `config.example.yaml`을 참고하세요.

### 최상위 키

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `listen` | 문자열 | `":8081"` | 수신 주소. 컨테이너 안에서는 8081을 유지하세요(헬스체크가 127.0.0.1:8081을 확인). |
| `baseURL` | URL | `"http://localhost:8081"` | (가져온 뒤 UI의 일반 설정에서 관리) 클라이언트가 접속하는 주소. npm·PyPI·Cargo 응답의 다운로드 URL이 이 값으로 만들어집니다. 끝의 `/`는 무시되고, `http(s)` URL이어야 합니다. |
| `dataDir` | 경로 | `"/data"` | blob과 `meta.db`(SQLite)를 저장하는 디렉터리. compose에서는 named volume입니다. |
| `memoryLimitMiB` | 정수 | `80` | Go 메모리 한도(MiB). 환경변수 `GOMEMLIMIT`가 있으면 무시됩니다(compose 기본값은 `GOMEMLIMIT: 80MiB`). |
| `maxUploadMiB` | 정수 | `1024` | (가져온 뒤 UI에서 관리, 바로 적용) GET/HEAD가 아닌 요청 본문의 최대 크기(MiB). 허용 범위 1..1048576, 0이면 기본값. 초과하면 모든 포맷에서 413. 포맷별로 더 작은 제한이 있으면 그것이 먼저 적용됩니다. |
| `anonymousRead` | 불리언 | `false` | (가져온 뒤 UI에서 관리) 인증 없는 다운로드 허용 여부. 저장소별로 덮어쓸 수 있습니다. |
| `metricsFlushSeconds` | 정수 | `30` | (숨은 설정) 다운로드 수와 요청 통계를 DB에 반영하는 주기(초). 허용 범위 1..3600. "다운로드 수·요청 추이" 참고. |
| `users` | 목록 | 없음 | (첫 시작 가져오기 전용) 사용자: `name`, `passwordHash`(bcrypt), `roles`. 가져온 사용자는 로컬 사용자가 됩니다. |
| `roles` | 맵 | 없음 | (첫 시작 가져오기 전용) 역할 이름 → `write: [저장소 이름…]`. `"*"`는 모든 저장소. 가져올 때 항목마다 `repo:<이름>`(또는 `*`) 대상에 browse·read·add·edit·delete 전부를 주는 권한이 됩니다(예전의 쓰기는 삭제를 포함했습니다). |
| `repositories` | 목록 | 없음 | 저장소 정의(아래 표). **첫 시작 때만** 읽어 DB로 가져옵니다. 이후에는 무시되므로 UI에서 관리합니다("저장소 관리" 참고). |

`admin` 역할은 내장되어 있어 `roles`에 정의하지 않아도 되며 모든 저장소의 모든 동작과 관리 화면, `/-/metrics` 조회가 가능합니다. `roles`에 `admin`을 적어도 무시하고 내장 역할을 씁니다.

### 저장소 키

아래 키는 UI 폼과 `config.yaml`(첫 시작 가져오기)에서 같은 의미입니다.

| 키 | 대상 | 기본값 | 설명 |
|---|---|---|---|
| `name` | 모두 | 필수 | 저장소 이름. URL의 `/repository/<name>/`. `/`, 공백, `\` 불가, 중복 불가. UI/API로 만들 때는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만, 최대 64자, `create`는 예약어. |
| `format` | 모두 | 필수 | `maven`, `npm`, `pypi`, `go`, `cargo` 중 하나. |
| `type` | 모두 | 필수 | `hosted`, `proxy`, `group` 중 하나. |
| `remote` | proxy | 필수 | 원격 저장소의 `http(s)` URL. Cargo는 `sparse+` 없는 sparse index URL. |
| `remoteAuth` | proxy | 없음 | 원격 인증: `username`/`password` 또는 `bearerToken`(둘 다 있으면 토큰 우선). 설정한 `remote`와 같은 스킴·호스트에만 전송합니다. |
| `metadataMaxAge` | proxy | `"24h"` | 메타데이터(maven-metadata.xml, packument, simple 인덱스, Go `list`/`@latest`, Cargo 인덱스)를 다시 확인하기까지의 시간. |
| `contentMaxAge` | proxy | `"-1"` | 아티팩트 본문(jar, tgz, 휠, zip, crate)의 재확인 주기. 기본은 영구 캐시. |
| `negativeCacheTTL` | proxy | `"10m"` | 원격이 404/410을 준 경로, 그리고 체크섬이 맞지 않은 다운로드를 다시 묻지 않는 시간. |
| `members` | group | 필수 | 멤버 저장소 이름 목록. 순서대로 조회하며 같은 포맷이어야 하고 순환할 수 없습니다. |
| `writeMember` | group | 없음 | 업로드를 받을 hosted 멤버(`members`에 포함되어야 함). 없으면 group에 쓰기는 405. |
| `versionPolicy` | Maven hosted | `release` | `release`, `snapshot`, `mixed`. Maven에서만 쓸 수 있습니다. |
| `writePolicy` | hosted | `allow_once` | `allow`(덮어쓰기 허용), `allow_once`(같은 경로 재업로드 거부), `deny`(읽기 전용). 아래 설명 참고. |
| `anonymousRead` | 모두 | 최상위 값 | 이 저장소만 익명 읽기를 켜거나 끕니다. |
| `cleanupPolicy` | hosted, proxy | 없음 | 연결할 Cleanup 정책 이름 하나(없으면 비움). 정책의 포맷이 `all`이거나 저장소와 같아야 하며 Cargo hosted에는 쓸 수 없습니다("Cleanup 정책" 참고). |
| `blobStore` | hosted, proxy | `default` | 이 저장소의 blob을 둘 blob store 이름("Blob Store" 참고). 만들 때만 정할 수 있고 바꿀 수 없으며 group에는 없습니다. 없는 store 이름은 거부됩니다. |
| `online` | 모두 | `true` | `false`이면 저장소가 Offline이 되어 요청에 503을 돌려줍니다("저장소 Online/Offline" 참고). |

**기간 형식**: Go 기간 문자열(`"30m"`, `"1h"`, `"24h"`)이며 `d` 단위는 없습니다. `"-1"`은 만료 없음(영구)입니다. 생략하거나 `0`이면 기본값입니다.
원격에 접속할 수 없을 때는 만료된 캐시가 있으면 그대로 제공하고 `Warning: 110` 헤더를 붙입니다.

**writePolicy의 적용 범위**: Maven은 세 값을 모두 따릅니다. Go는 `allow`가 아니면 같은 파일을 거부하고 `deny`는 업로드를 막습니다. PyPI와 Cargo는 `deny`만 효과가 있으며 같은 파일·버전 재업로드는 항상 거부합니다. npm은 `writePolicy`를 쓰지 않으며 이미 게시된 버전은 항상 거부합니다.

## 인증과 권한

plan 12부터 사용자, 역할, Realm, LDAP 설정은 `config.yaml`이 아니라 **DB에서 웹 UI로 관리**합니다(첫 시작 때 `config.yaml`의 `users`/`roles`를 한 번 가져옵니다. "업그레이드" 참고). 변경은 바로 적용되며 재시작이 필요 없습니다.

### 사용자

- **Basic 인증**: 사용자 이름과 비밀번호. UI에서 사용자를 만들 때 비밀번호(8자 이상, 72바이트 이하)를 입력합니다. `commandcenter hash-password`는 첫 시작 때 `config.yaml`에서 가져올 해시를 만듭니다(가져온 뒤에는 `config.yaml`의 사용자를 고쳐도 반영되지 않습니다). 관리자가 모두 잠겼을 때는 "비상 복구"의 `user reset-password`를 쓰세요. 사용자 ID는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `@`, `-`만, 최대 64자이며 `create`는 예약어입니다. 첫 가져오기에서도 같은 규칙을 적용하므로 규칙에 맞지 않는 ID(예: `:`이 들어간 ID)가 `config.yaml`에 있으면 가져오기가 실패하고 서버가 시작되지 않습니다.
- **상태**: `active` 또는 `disabled`. 비활성화하거나 삭제한 사용자는 Basic 인증, 세션, 토큰 모두 곧바로 거부됩니다. 사용자를 삭제하면 그 사용자의 API 토큰도 함께 지워집니다.
- **본인 비밀번호 변경**: 로그인한 로컬 사용자는 계정 화면(`PUT /-/api/account/password`)에서 현재 비밀번호를 확인한 뒤 바꿉니다. 비밀번호나 세션으로 인증해야 하고 토큰으로는 안 됩니다. 바꾸면 그 사용자의 세션이 모두 끊기고 현재 브라우저만 새 세션을 받습니다. LDAP 사용자는 비밀번호를 디렉터리에서 관리하므로 400입니다. 현재 비밀번호는 로컬 Realm으로만 확인하므로 같은 이름의 LDAP 사용자의 비밀번호로는 바꿀 수 없습니다. 관리자는 다른 로컬 사용자의 비밀번호를 재설정할 수 있고, 그러면 그 사용자의 세션이 모두 끊깁니다.
- **API 토큰**: UI의 API 토큰 화면 또는 `docker compose exec commandcenter commandcenter token create -user ci`가 `cc_…` 토큰을 만듭니다. `Authorization: Bearer <토큰>`, 토큰 값만 보내는 형태(Cargo), 또는 **Basic 인증의 비밀번호 자리**(사용자 이름은 토큰 소유자와 같아야 하며 다르면 401)로 쓸 수 있어 Maven·pip·Go CI에서 비밀번호 대신 넣을 수 있습니다. 토큰으로는 새 토큰을 발급·폐기하거나 웹 UI에 로그인할 수 없습니다(실제 비밀번호나 로그인 세션만 가능). 토큰은 만료되지 않지만 사용자를 비활성화·삭제하거나 `token` Realm을 끄면 무효가 됩니다. 토큰은 해시만 저장되므로 분실하면 다시 발급하세요. LDAP 사용자의 토큰은 소유자가 `ldap:<id>`입니다("Realm과 LDAP" 참고).
- **`npm login`**: 사용자 이름과 비밀번호로 로그인하면 서버가 토큰을 발급해 `~/.npmrc`에 저장합니다.
- **세션**: 웹 UI 로그인 세션은 메모리에만 있고 사용자당 최대 20개입니다. 넘으면 그 사용자의 가장 오래된 세션이 끊깁니다.

### 역할과 권한

역할은 **권한(privilege)** 목록입니다. 권한 하나는 **대상**과 **동작**의 묶음입니다.

- **대상**: `*`(모든 저장소), `format:<포맷>`(`maven`, `npm`, `pypi`, `go`, `cargo`), `repo:<저장소 이름>`.
- **동작**: `browse`, `read`, `add`, `edit`, `delete`. 여러 권한과 여러 역할은 합쳐지며(어느 하나라도 허용하면 허용) 거부 규칙은 없습니다.
  - **browse**: UI의 저장소 목록, 탐색, 파일 정보, 검색, 컴포넌트 검색. 내용 다운로드(`/repository/`)는 안 되며 파일 화면의 Download 버튼도 나오지 않습니다. `read`가 있으면 browse도 됩니다.
  - **read**: 다운로드(GET/HEAD)와 메타데이터 조회.
  - **add**: 새 파일·버전 올리기. Maven `PUT`(새 경로), npm `publish`(새 버전), PyPI `twine upload`, Go 업로드, Cargo `publish`, UI 업로드.
  - **edit**: 이미 있는 것을 바꾸기. `writePolicy: allow`인 Maven·Go 저장소에서 같은 경로 덮어쓰기, npm `dist-tag`와 `deprecate`, Cargo `yank`/`unyank`. (`writePolicy`가 `allow_once`이거나 PyPI·Cargo처럼 재업로드를 항상 거부하는 포맷에서는 edit이 있어도 덮어쓸 수 없습니다.)
  - **delete**: 삭제. 지금은 Maven `DELETE`(파일·컴포넌트)에만 해당합니다. 다른 포맷에는 삭제가 없습니다.
- group에 쓰는 요청은 같은 동작을 `writeMember` 저장소에 대해 검사합니다.
- **내장 `admin` 역할**은 모든 대상에 모든 동작을 가지며 관리 화면과 `/-/metrics`도 쓸 수 있습니다. 수정·삭제할 수 없습니다.
- 역할을 지우려는데 사용자가 쓰고 있거나 LDAP 설정의 `groupMappings[].roles` 또는 `defaultRoles`에 들어 있으면 409입니다(LDAP 참조는 메시지에 어느 매핑인지 나옵니다). 존재하지 않는 저장소를 대상으로 한 권한도 저장되지만 시작 로그에 경고가 나옵니다.
- 예: `repo:maven-releases`에 `browse`, `read`, `add`만 있는 역할의 사용자가 Maven에 `PUT`하면 201, `DELETE`하면 403입니다.

### 읽기 권한

- **`authenticatedRead`**(기본 켜짐): 켜져 있으면 로그인한 사용자는 권한 없이도 모든 저장소를 읽고 browse할 수 있습니다. 끄면 로그인한 사용자도 `read`(UI 목록은 `browse` 또는 `read`) 권한이 있어야 합니다. 쓰기(add/edit/delete)는 항상 권한이 필요합니다.
- **익명 읽기**: 일반 설정의 `anonymousRead`(저장소별 `anonymousRead`가 있으면 그 값이 우선)가 켜진 저장소는 인증 없이 읽을 수 있습니다. group의 읽기 검사는 멤버가 아니라 **group 자신의** 값을 따릅니다. 익명 사용자의 쓰기는 항상 거부됩니다.
- **응답 코드**: 인증이 필요한데 없으면 401(`WWW-Authenticate` 포함), 인증은 됐으나 권한이 없으면 403. 잘못된 자격 증명은 `anonymousRead`가 켜져 있어도 401입니다. 저장소가 Offline이면 권한 검사 **뒤에** 503입니다("저장소 Online/Offline" 참고).

### 잠금 방지

"로그인할 수 있는 관리자"가 없어지는 변경은 409로 거부됩니다. 관리자가 로그인할 수 있는 경로는 (a) `local` Realm이 켜져 있고 비밀번호가 있는 활성 로컬 `admin` 사용자가 있거나, (b) `ldap` Realm이 켜져 있고 설정이 완전하며 그룹 매핑 또는 기본 역할이 `admin`을 주는 경우입니다. 대상이 되는 변경은 사용자 삭제·비활성화·역할 변경, `local`/`ldap` Realm 끄기, LDAP 매핑 편집입니다. 추가로 **마지막 로컬 관리자(활성, 비밀번호 있음, `admin` 역할)는 LDAP 관리자 매핑이 있어도 삭제·비활성화·강등할 수 없습니다.** 로컬 비상용 관리자를 항상 남기기 위해서입니다. 그래서 LDAP만 쓰는 설치에도 로컬 관리자 한 명은 남습니다(`local` Realm 자체는 끌 수 있습니다).

사용자, 역할, Realm, LDAP 설정의 변경은 서버 안에서 하나씩 차례로 처리합니다(읽기 → 잠금 방지 검사 → 저장 → 적용). 그래서 두 관리자가 동시에 서로를 비활성화해도 하나는 409를 받고 관리자 한 명은 남습니다.

### 비상 복구(break-glass)

모든 관리자가 잠겼거나(비밀번호 분실, 비활성화, `local` Realm 꺼짐) 웹 UI로 들어갈 수 없을 때는 **서버를 멈추고** DB를 직접 고칩니다.

```bash
docker compose stop commandcenter
# TTY에서 직접 입력하면 비밀번호가 화면에 그대로 보이므로 파이프로 넘기세요(8자 이상, 72바이트 이하)
printf '%s
' "$PW" | docker compose run --rm -T commandcenter user reset-password -user admin -admin
docker compose start commandcenter
```

`user reset-password`는 표준 입력에서 비밀번호를 한 줄 읽고(API와 같은 규칙), 한 트랜잭션으로 다음을 바꾼 뒤 바꾼 내용을 출력합니다.

- 사용자의 비밀번호를 설정하고 상태를 `active`로 바꿉니다. 사용자가 없으면 로컬 사용자로 만듭니다. 이미 있는 `source=ldap` 덮어쓰기 행의 ID이면, 관리자가 비활성화한 디렉터리 계정이 되살아나거나 그 역할이 비밀번호 로그인으로 옮겨지는 일을 막기 위해 `user "<id>" is an LDAP role-override record; choose another id for a local recovery account` 오류로 거부하고 아무것도 바꾸지 않습니다(0이 아닌 코드로 종료). 이때는 다른 ID를 쓰세요.
- `-admin`을 주면 `admin` 역할을 더합니다.
- `local` Realm을 켭니다. Realm 목록에 없으면 맨 앞에 넣습니다.

첫 시작 전이라 `config.yaml`을 아직 가져오지 않았다면 가져오기를 먼저 합니다. `-config`는 `token create`와 같습니다. 실행 중인 서버는 시작할 때의 스냅샷을 계속 쓰므로 바뀐 내용이 반영되지 않습니다. 반드시 서버를 멈춘 상태에서 실행하거나, 실행한 뒤 서버를 다시 시작하세요.

### 보호

- **저장소 콘텐츠**: `/repository/` 응답에는 `Content-Security-Policy: sandbox allow-downloads; default-src 'none'`과 `X-Content-Type-Options: nosniff`가 붙어, 업로드된 HTML이 브라우저에서 이 서버의 권한으로 실행되지 않습니다. 인증된 요청의 응답은 `Cache-Control: private`입니다.
- bcrypt 비교는 동시에 최대 4개이며, 없는 사용자도 같은 비용으로 비교해 사용자 존재 여부를 응답 시간으로 알 수 없습니다. 성공한 비밀번호 확인은 1분간 캐시되고(비밀번호를 바꾸면 바로 무효), 토큰 마지막 사용 시각은 1분에 한 번만 기록합니다. 로그인 이름이 256바이트, 비밀번호가 1024바이트를 넘으면 어떤 Realm에도 보내지 않고 거부합니다.

## Realm과 LDAP

**Realm**은 인증 방식입니다. Realm 화면에서 순서와 켜기/끄기를 정합니다(`GET/PUT /-/api/realms`).

| Realm | 하는 일 |
|---|---|
| `local` | DB에 저장된 로컬 사용자의 비밀번호 로그인(UI와 Basic 인증) |
| `token` | API 토큰 |
| `ldap` | LDAP 디렉터리의 사용자·비밀번호 로그인 |

비밀번호 로그인은 켜진 Realm을 **목록 순서대로** 시도합니다(기본은 `local`, `token`이며 `ldap`은 설정을 마친 뒤 켭니다). 앞 Realm에서 실패하면 다음으로 넘어가므로 같은 이름이 로컬과 LDAP에 모두 있으면 순서가 우선순위입니다. `token` Realm을 끄면 모든 토큰이 거부됩니다. `ldap`은 URL, 사용자 기준 DN, 필터를 채우기 전에는 켤 수 없고, `local`이나 `ldap`을 끄는 변경에는 위의 잠금 방지가 적용됩니다.

`local`과 `ldap` Realm은 **비밀번호 로그인만** 막습니다. `local` Realm을 꺼도 로컬 사용자의 이미 로그인한 웹 세션과 API 토큰(토큰은 `token` Realm이 판단)은 계속 동작합니다. 끊어야 하면 그 사용자를 비활성화하거나 토큰을 폐기하세요.

### LDAP 설정 필드

| 필드 | 기본값 | 설명 |
|---|---|---|
| `url` | 없음 | `ldap://호스트[:포트]` 또는 `ldaps://호스트[:포트]` |
| `startTLS` | `false` | `ldap://`에서 StartTLS로 암호화(`ldaps://`와는 같이 쓸 수 없음) |
| `insecureSkipVerify` | `false` | 서버 인증서 검증 생략. 시험용으로만 쓰세요. |
| `bindDN`, `bindPassword` | 없음 | 검색에 쓰는 서비스 계정. `bindDN`이 없으면 익명 검색. 비밀번호는 저장만 되고 API 응답·로그·이벤트에 나오지 않습니다(`bindPasswordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearBindPassword: true`로 지웁니다. 단 **서버의 호스트:포트를 바꾸거나, `ldaps`에서 `ldap`으로 바꾸거나, `startTLS`를 끄거나, `insecureSkipVerify`를 켤 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 더 약한 연결로 새어 나가지 않게 하는 규칙입니다. |
| `timeoutSeconds` | `5` | 연결·응답 제한(1..60) |
| `userBaseDN` | 필수 | 사용자를 검색할 기준 DN |
| `userFilter` | `(uid={user})` | 검색 필터. `{user}`가 필수이며 로그인 이름은 필터에 넣기 전에 이스케이프합니다. 정확히 한 항목이 나와야 합니다. |
| `userIdAttr` | `uid` | 사용자 ID로 쓸 속성 |
| `nameAttr`, `emailAttr` | `cn`, `mail` | 표시 이름, 이메일 |
| `groupAttr` | `memberOf` | 사용자의 그룹을 담은 속성 |
| `groupMappings` | 없음 | `[{group, roles}]` 그룹 → 역할 |
| `defaultRoles` | 없음 | 모든 LDAP 사용자에게 주는 역할 |

### 로그인 흐름

1. 서비스 계정으로 바인드(`bindDN`이 있을 때)하고 `userBaseDN` 아래에서 `userFilter`로 사용자를 한 명 찾습니다(0명이나 2명 이상이면 실패).
2. 찾은 항목의 DN으로 사용자 비밀번호를 바인드합니다. 이름이나 비밀번호가 비어 있으면 서버에 보내지 않고 거부합니다(익명 바인드 방지).
3. 사용자의 `groupAttr` 값을 그룹 매핑에 대응시켜 역할을 정합니다. 성공한 비밀번호 확인은 로컬과 같이 1분간 캐시합니다.

LDAP 사용자는 DB에 사용자 행이 따로 없고, 로그인하면 메모리에 항목이 생깁니다. 세션과 토큰의 소유자 키는 `ldap:<id>`이며(예: `ldap:alice`) 같은 ID의 로컬 사용자와 섞이지 않습니다. 토큰을 Basic 인증의 비밀번호로 쓸 때 사용자 이름은 `ldap:`을 뺀 `alice`입니다. 역할은 요청마다 최신 설정으로 계산하므로 매핑을 바꾸면 곧바로 반영됩니다.

### 그룹 매핑

`groupMappings[].group`은 사용자의 그룹 속성 값과 **전체 DN**(`cn=cc-admins,ou=groups,dc=example,dc=org`) 또는 **첫 RDN의 값인 CN**(`cc-admins`)으로, 대소문자 구분 없이 비교합니다. 일치한 매핑의 역할과 `defaultRoles`의 합집합이 사용자 역할입니다. 주의하세요.

- **CN만 적은 매핑은 같은 CN의 다른 그룹과도 일치합니다.** 서로 다른 OU에 같은 이름의 그룹이 있을 수 있으면 전체 DN으로 적으세요. 특히 `admin` 역할을 주는 매핑은 DN으로 적는 것이 안전합니다.
- `defaultRoles`에 `admin`을 넣으면 디렉터리의 모든 사용자가 관리자가 됩니다(UI가 경고합니다).
- `memberOf` 같은 속성을 서버가 채워 줘야 합니다(OpenLDAP은 memberOf overlay가 필요). 중첩 그룹은 풀지 않습니다.
- Active Directory에서 비활성화된 계정은 자동으로 막히지 않습니다. 필터가 지원하는 `&`, `|`, `!`, 등호, 존재(`attr=*`) 조건으로 허용 그룹을 `userFilter`에 넣으세요(예: `(&(sAMAccountName={user})(memberOf=CN=허용그룹,…))`). AD 확장 매칭 규칙(`:1.2.840…:=`), 부분 일치, 비교 필터는 지원하지 않습니다.

### 로컬 역할 덮어쓰기

사용자 화면에서 `source`가 `ldap`인 사용자를 같은 ID로 만들면 **역할 덮어쓰기 행**이 됩니다. 이 행은 그룹 매핑이 준 역할에 자신의 역할을 **더하고**, `disabled`로 두면 그 LDAP 사용자의 로그인을 막습니다. 이 행은 로그인 주체가 아니며(비밀번호가 없고, 세션·토큰도 이 행으로는 생기지 않음) 디렉터리에서 사용자가 사라져도 남습니다.

### 1분 재확인과 장애 시 동작

- LDAP 사용자의 항목은 1분이 지나면 다음 사용 때 서비스 계정으로 다시 검색해 확인합니다(**세션, 토큰, 비밀번호 캐시 모두**). 디렉터리에서 사라졌거나 검색 결과가 모호하면 즉시 거부합니다.
- 디렉터리에 닿을 수 없으면 **닫힌 채로(fail closed)** 거부합니다. 재확인이 실패하면 10초 동안은 다시 시도하지 않고 거부합니다. 재시작 직후에도 LDAP 사용자의 세션과 토큰은 한 번 재확인을 통과해야 합니다. 로컬 관리자는 영향을 받지 않습니다.
- 재확인은 `userFilter`의 `{user}`가 `userIdAttr`와 같은 값을 가리킨다고 가정합니다(예: `(uid={user})`와 `uid`). 다르면 재확인이 실패해 1분 뒤 세션·토큰이 거부됩니다.
- 로그인 경로의 **장애 백오프**: 연결·TLS 단계(TCP, TLS/StartTLS, 서비스 바인드의 네트워크 오류)가 연달아 3번 실패하면 15초 동안 로그인에서 `ldap` Realm을 건너뛰어(뒤의 Realm이 판단) 로컬 로그인이 느려지지 않게 합니다. 잘못된 비밀번호, 검색 실패, 너무 큰 응답 같은 요청별 실패는 장애로 세지 않습니다. 디렉터리 응답 메시지는 1 MiB로 제한합니다.
- 디렉터리에서 사용자가 사라져도 토큰과 세션을 지우지는 않고 접근만 거부합니다(기준 DN 오타로 전부 폐기되는 일을 막기 위해서입니다). 같은 ID가 다시 생기면 이전 토큰이 되살아날 수 있으므로 재사용되지 않는 값을 `userIdAttr`로 쓰세요.

### 연결 테스트

`POST /-/api/realms/ldap/test`(`{user, password}`, 저장된 설정을 사용)는 `connect`, `starttls`, `bind`, `search`, `userBind`, `groups` 단계를 하나씩 실행해 `{ok, steps:[{step, ok, detail}]}`로 돌려줍니다. 사용자 이름을 주면 `search`까지, 비밀번호도 주면 `userBind`와 `groups`(매핑에 일치한 그룹)까지 갑니다. 응답에 비밀번호는 나오지 않습니다. 설정 화면의 테스트 버튼이 이 호출입니다.

### 예: bitnami/openldap 스타일

```
url:           ldap://openldap:1389
bindDN:        cn=admin,dc=example,dc=org
bindPassword:  (서비스 계정 비밀번호)
userBaseDN:    ou=people,dc=example,dc=org
userFilter:    (&(objectClass=inetOrgPerson)(uid={user}))
userIdAttr:    uid          groupAttr: memberOf
groupMappings: [{ "group": "cc-admins", "roles": ["admin"] }]
```

`test/smoke/ldap-smoke.sh`가 이 구성을 컨테이너로 띄워 로그인, 토큰, 잠금 방지를 확인합니다.

## Blob Store

blob(파일 내용)은 **blob store** 디렉터리에 sha256 이름으로 저장됩니다.

- **default store**: `<dataDir>/blobs`. 기존 설치와 같은 위치이며 이름은 `default`입니다. 삭제할 수 없습니다. 현재 `dataDir`를 기준으로 하므로 `dataDir`를 옮겨도 따라갑니다.
- **이름 붙은 store**: Blob Store 화면(`POST /-/api/blobstores`)에서 만듭니다. 경로를 비우면 `<dataDir>/blobstores/<이름>`이고, 절대 경로를 주면 그곳을 씁니다. `<경로>/blobs/sha256`이 **없거나 비어 있어야** 하고(이미 blob이 있으면 거부, 다른 파일이 있는 디렉터리는 괜찮습니다), 쓸 수 있어야 하며, 다른 store의 blob 디렉터리와 겹치거나 심볼릭 링크로 같은 곳을 가리키면 거부됩니다. 기존 store의 경로를 확인할 수 없으면(끊어진 심볼릭 링크 등) 겹치지 않는다고 확신할 수 없으므로 그 store 이름을 알려 주며 거부합니다. 이름 규칙은 저장소와 같습니다.
- **저장소는 만들 때 store를 고릅니다**(`blobStore`, 생략하면 `default`). 저장소를 만든 뒤에는 **바꿀 수 없습니다.** group에는 store가 없습니다.
- store마다 **중복 제거와 GC가 독립**입니다. 같은 파일을 서로 다른 store의 저장소에 올리면 각각 저장되고, GC는 자기 store의 저장소가 참조하지 않는 blob만 지웁니다. 존재하지 않는 store를 가리키는 저장소가 있으면(설정 가져오기 등) 조용히 default로 가지 않고 오류로 응답하며, GC는 그 저장소가 모든 store를 참조 중인 것으로 보고 지우지 않습니다.
- **사용량**: Blob Store 화면과 `GET /-/api/blobstores`가 store별 blob 수·용량(그 store의 저장소가 참조하는 것), 저장소 수, 파일시스템 전체/남은 용량(리눅스)을 보여 줍니다.
- **삭제**: 저장소가 쓰고 있는 store는 409입니다. 삭제해도 **디렉터리와 파일은 디스크에 남습니다**(관리자가 직접 정리하세요).
- **compact 작업**(`blobstore.compact`)은 선택 파라미터 `blobStore`로 한 store만 정리하고, 생략하면 모든 store를 정리합니다("예약 작업" 참고).

## 일반 설정

일반 설정 화면(`GET/PUT /-/api/settings`, 관리자)의 값은 저장 즉시 적용됩니다. 바꿀 항목만 보내면 되고, 보낸 키만 저장합니다.

| 키 | 기본값 | 설명 |
|---|---|---|
| `baseURL` | 가져온 값 또는 `http://localhost:8081` | 클라이언트에게 알려 주는 주소(npm·PyPI·Cargo 응답의 URL, 세션 쿠키의 `Secure` 여부). 바로 반영됩니다. |
| `anonymousRead` | `false` | 인증 없는 읽기(저장소별 값이 우선) |
| `authenticatedRead` | `true` | 로그인한 사용자의 읽기·browse를 권한 없이 허용 |
| `sessionIdleMinutes` | `720` | 세션 유휴 만료(5..10080분). 이미 로그인한 세션에도 적용됩니다. |
| `maxUploadMiB` | `1024` | 업로드 본문 최대 크기(1..1048576). 요청마다 읽으므로 바로 적용됩니다. |
| `defaultLanguage` | `ko` | `ko` 또는 `en`. 브라우저에 저장된 언어 선택이 없을 때 웹 UI가 쓰는 언어이고(언어 버튼으로 고르면 그 선택이 저장되어 우선합니다), 작업 실패 알림 메일의 언어입니다. `GET /-/api/me`가 로그인하지 않은 사용자에게도 알려 줍니다. |

## 이메일 알림

SMTP 설정(`GET/PUT /-/api/settings/smtp`, 관리자)으로 예약 작업 실패를 메일로 알립니다.

- **필드**: `enabled`, `host`, `port`, `security`, `username`, `password`, `from`. 켜려면 host, port, 올바른 `from`이 필요합니다. 끄면 일부만 채워도 저장됩니다. PUT은 전체 교체이므로 폼의 모든 값을 보내야 하고, 생략한 `password`만 기존 값을 유지합니다.
- **보안 모드**: `none`(암호화 없음), `starttls`(연결 뒤 업그레이드, 서버가 STARTTLS를 광고해야 함), `tls`(처음부터 TLS, 보통 465). TLS 인증서는 항상 검증하며 건너뛰는 옵션은 없습니다. 인증은 `username`을 채웠을 때만 **PLAIN**(`PlainAuth`)이며 LOGIN, CRAM-MD5 등은 지원하지 않습니다. Go의 PlainAuth는 암호화하지 않은 연결에서 localhost가 아닌 서버로는 비밀번호를 보내지 않으므로, 인증이 필요하면 `starttls` 또는 `tls`를 쓰세요.
- **비밀번호**: 저장만 되고 응답·로그·이벤트에 나오지 않습니다(`passwordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearPassword: true`로 지웁니다. **서버(host나 port)를 바꾸거나 `tls`/`starttls`에서 `none`으로 낮출 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 암호화되지 않은 연결로 새어 나가지 않게 하는 규칙입니다.
- **테스트 메일**: `POST /-/api/settings/smtp/test`(`{to}`)는 저장된 설정으로 한 통을 바로 보내고 성공하면 200입니다. 이메일이 꺼져 있으면 400, **10초 안에 다시 호출하면 429**, 발송 실패는 502입니다. 10초 창은 이메일이 켜져 있는지 확인한 뒤 **보내기 전에** 잡으므로 실패한 발송도 한 번으로 셉니다.
- **작업 실패 알림**: 작업의 `notifyEmail`(최대 20개 주소)에 수신자를 넣으면, 그 작업의 실행이 **Failed**로 끝날 때만(성공, 중지, 서버 종료 중 실패는 제외) 이메일이 켜져 있을 때 메일을 보냅니다. 제목에 작업 이름이 들어가고 본문에는 시각, 결과 메시지, **마지막 로그 20줄**이 담깁니다. 언어는 `defaultLanguage`를 따릅니다. 메일은 백그라운드로 보내므로 작업을 지연시키지 않고, 큐(256건)가 차면 버립니다. 발송 실패는 서버 로그(WARN)에 남고 활동 기록에 `email.failed` 이벤트(제목과 오류 메시지)로도 기록됩니다.

## 저장소 Online/Offline

저장소 편집 화면(또는 API의 `online`)에서 저장소를 **Offline**으로 바꿀 수 있습니다(기본은 Online).

- Offline 저장소의 `/repository/<이름>/…` 요청은 **503**(`repository is offline`)입니다. 권한 검사를 먼저 하므로 읽을 수 없는 사용자는 Offline인지 알 수 없고 401/403을 받습니다.
- **group은 Offline 멤버를 건너뜁니다.** `writeMember`가 Offline이면 그 group에 쓰는 요청은 (쓰기 권한이 있는 사용자에게) 503 `repository is offline`이고, UI 저장소 목록의 `canWrite`도 `false`입니다. group 자체를 Offline으로 두면 그 group이 503입니다.
- **cleanup 정책과 compact 작업은 영향을 받지 않고** Offline 저장소에서도 실행됩니다.

## 포맷별 클라이언트 설정

### Maven 클라이언트 설정

`~/.m2/settings.xml`:

```xml
<settings>
  <servers>
    <server><id>commandcenter</id><username>ci</username><password>비밀번호</password></server>
  </servers>
  <mirrors>
    <mirror><id>commandcenter</id><mirrorOf>*</mirrorOf>
      <url>http://localhost:8081/repository/maven-public/</url></mirror>
  </mirrors>
</settings>
```

배포 대상은 `pom.xml`의 `distributionManagement`에 `http://localhost:8081/repository/maven-releases/`(id `commandcenter`)로 지정합니다.
다른 컨테이너에서 접근할 때는 `localhost` 대신 `host.docker.internal`을 사용하세요.

Maven 동작 메모:

- SNAPSHOT 버전 디렉터리의 `maven-metadata.xml`은 서버가 만듭니다. 그 디렉터리의 파일을 올리거나 지울 때마다, 그리고 `repository.rebuild-metadata` 작업이 돌 때 가장 높은 timestamp 빌드를 기준으로 다시 만듭니다. 클라이언트(`mvn deploy`)가 올린 같은 문서는 성공(200/201)으로 답하되 무시합니다.
- group은 `.meta/prefixes.txt`에 404로 답합니다(멤버의 접두사 목록을 그대로 내주면 다른 멤버의 아티팩트가 없다고 오해하기 때문입니다). 병합한 메타데이터의 `Last-Modified`는 멤버가 오프라인이 되어 내용이 바뀌어도 뒤로 가지 않고 앞으로만 움직입니다.
- 업로드 경로의 이름에 `?`, `#`, `;`나 제어 문자(NUL 포함)가 있으면 400으로 거부합니다(조회는 막지 않으므로 이미 있는 파일은 계속 받을 수 있습니다). release 정책 저장소는 timestamp가 붙은 SNAPSHOT 버전(`1.4-20260101.000000-1`)도 400으로 거부합니다.

### npm 클라이언트 설정

레지스트리를 그룹 저장소로 지정합니다.

```bash
npm config set registry http://localhost:8081/repository/npm-all/
```

로그인하면 토큰이 발급되어 `~/.npmrc`에 저장됩니다. 사용자 이름과 비밀번호는 config.yaml의 사용자입니다.

```bash
npm login --registry http://localhost:8081/repository/npm-all/
```

`npm publish`는 그룹의 `writeMember`(npm-hosted)에 저장됩니다. 같은 버전을 다시 publish하면 거부됩니다(403).
`npm install`, `npm dist-tag`, `npm deprecate`, `npm whoami`, `npm logout`(그 토큰을 폐기)을 지원하며, `npm unpublish`(405로 실패)와 `npm search`, `npm audit`(501)은 지원하지 않습니다.
새 버전은 SemVer 2.0 형식(`1.2.3`, `1.2.3-beta.1`)이어야 하고, dist-tag 이름은 버전이나 범위처럼 보이면(`1.0.0`, `^1`, `1.x`) 거부됩니다.
그룹의 `latest`는 멤버 순서상 처음으로 `latest`를 가진 멤버의 값입니다(hosted를 proxy보다 앞에 두세요).
그룹의 packument와 tarball은 그 패키지를 처음 가진 멤버보다 앞선 멤버가 "없음"(404)이 아닌 오류를 내면 5xx(502 또는 그 멤버의 5xx)로 답합니다. hosted의 일시 오류 때문에 proxy에 있는 같은 이름의 공개 패키지가 대신 나가지 않게 하기 위해서입니다. 패키지를 가진 멤버보다 뒤에 있는 멤버의 오류는 로그만 남기고 건너뛰므로, hosted 뒤의 proxy가 장애여도 hosted 패키지는 계속 받을 수 있습니다(반대로 proxy를 hosted보다 앞에 두면 proxy 장애 때 그룹이 5xx를 냅니다).
`npm unpublish`는 `405`(받는 메서드가 없으므로 빈 `Allow` 헤더)로, `npm audit`는 `501`로 답하므로 npm이 "삭제했다"고 잘못 보고하지 않습니다. 삭제가 필요하면 관리자에게 요청하세요.

**baseURL 주의**: config.yaml의 `baseURL`은 클라이언트가 실제로 접속하는 레지스트리 주소(스킴, 호스트, 포트)와 정확히 같아야 합니다.
응답의 tarball URL이 `baseURL`로 만들어지기 때문입니다. 예를 들어 다른 컨테이너의 클라이언트가 접속한다면
`baseURL: "http://host.docker.internal:8081"`로 설정하고 레지스트리 주소도 같은 값(`http://host.docker.internal:8081/repository/npm-all/`)을 사용하세요.

**토큰 오류**: `~/.npmrc`의 토큰이 유효하지 않으면(예: 데이터 볼륨을 다시 만든 뒤) `anonymousRead`가 켜져 있어도 모든 요청이 401이 됩니다.
`npm login`을 다시 실행하거나 `~/.npmrc`에서 해당 `_authToken` 줄을 지우세요.

### PyPI 클라이언트 설정

pip는 그룹의 simple 인덱스를 사용합니다. `trusted-host`는 접속 호스트가 localhost가 아닐 때(예: `host.docker.internal`, LAN IP)만 필요합니다. HTTP는 localhost에 한해 pip가 경고 없이 허용합니다.

```bash
pip config set global.index-url http://localhost:8081/repository/pypi-all/simple/

# localhost가 아닌 호스트로 접속할 때
pip config set global.index-url http://192.168.0.10:8081/repository/pypi-all/simple/
pip config set global.trusted-host 192.168.0.10
```

`anonymousRead: false`이면 인덱스 URL에 인증 정보를 넣거나(`http://user:pass@host:8081/repository/pypi-all/simple/`) keyring을 사용하세요.

업로드는 twine으로 hosted 저장소(또는 writeMember가 있는 그룹)에 합니다. 업로드 URL은 반드시 `/`로 끝나야 합니다.

```bash
twine upload --repository-url http://localhost:8081/repository/pypi-hosted/ -u ci dist/*
```

사용자 이름 자리에 `__token__`을 쓰면 비밀번호 자리의 API 토큰이 누구 것이든 그 주인으로 인증됩니다(`twine upload -u __token__ -p cc_…`). 다른 사용자 이름은 토큰 주인과 같아야 합니다. 이 규칙은 모든 포맷의 Basic 인증에 같습니다.

업로드 검사: 파일은 `.whl`, `.tar.gz`, `.zip`만 받습니다(그 밖은 400). `version`은 PEP 440 형식이어야 하고 파일 이름에서 읽은 버전과 같아야 합니다(정규화 후 비교). 같은 파일인지는 이름이 아니라 정규화한 프로젝트 이름, 버전, wheel 태그로 판단하므로 `Foo_Bar-1.0.0`과 `foo-bar-1.0`처럼 표기만 다른 것도 중복입니다. `Requires-Python`은 폼 값보다 파일 안의 메타데이터(wheel `METADATA`, sdist `PKG-INFO`)를 우선하며, 폼 값이 지정자로 올바르지 않으면 400입니다. 메타데이터는 첫 빈 줄까지의 헤더 부분만 읽으므로 긴 README(long_description)가 있어도 업로드는 거부되지 않습니다. 헤더 부분만으로 1 MiB가 넘으면 파일의 값을 무시하고 폼 값을 씁니다.

같은 파일을 다시 올리면 `File already exists.`(400)로 거부됩니다. twine은 서버의 오류 메시지를 기본으로 숨기므로, 원인을 보려면 `twine upload --verbose ...`로 실행하세요. 파일 URL은 config의 `baseURL`로 만들어지므로, pip가 접속하는 주소와 `baseURL`이 같아야 합니다.

### Go 모듈 설정

`GOPROXY`를 그룹 저장소로 지정합니다.

```bash
go env -w GOPROXY=http://localhost:8081/repository/go-all
go env -w GONOSUMDB=example.com
```

사내 모듈은 공개 체크섬 DB(sum.golang.org)에 없으므로 `GONOSUMDB`에 경로 접두사를 넣으세요.
`GOPRIVATE`는 프록시를 거치지 않게 만들므로 Command Center의 hosted 모듈에는 쓰지 마세요.

hosted 저장소에는 `.mod`와 `.zip`을 HTTP PUT으로 올립니다(`.info`는 없으면 자동 생성).
zip 안의 모든 파일은 `모듈경로@버전/` 아래에 있어야 하며, 대문자는 `!소문자`로 인코딩한 경로를 씁니다.

```bash
curl -u ci -X PUT --data-binary @go.mod     http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.mod
curl -u ci -X PUT --data-binary @hello.zip  http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.zip
```

같은 버전 파일을 다시 올리면 409로 거부됩니다(`writePolicy: allow`이면 허용).

`@v/list`와 `@latest`는 `.zip`이 있는 버전만 보여 줍니다(`.mod`만 올라간 버전은 목록에 나오지 않으며, 이미 올라간 파일은 직접 요청하면 받을 수 있습니다). zip은 Go 모듈 zip 규칙의 핵심을 검사합니다. 같은 이름의 항목이 둘 이상이거나 대소문자만 다른 이름이 겹치거나, 경로에 `\`·`:`나 빈 요소(`//`), `..` 요소, `con`·`nul`·`com1`처럼 Windows 예약 이름이 있으면 400이고, 압축을 푼 크기 합계는 500 MiB까지입니다.

모듈 경로의 메이저 버전 접미사는 버전과 일치해야 합니다(`example.com/hello/v2`는 `v2.x.y`, `gopkg.in/yaml.v3`는 `v3.x.y`). 접미사가 없는 경로에 `v2` 이상을 올리려면 `+incompatible` 버전을 쓰며, 이 zip에는 루트 `go.mod`가 없어야 합니다.

주의 사항:

- `anonymousRead: false`이면 go 명령이 인증 정보를 필요로 합니다. `~/.netrc`에 `machine <호스트> login <사용자> password <비밀번호>`를 설정하세요.
- 기본 정책(`allow_once`)에서 `.info`를 직접 올릴 때는 `.mod`/`.zip`보다 먼저 올려야 합니다. 나중에 올리면 자동 생성된 `.info`와 충돌해 409가 납니다.
- group 조회는 hosted 모듈 경로도 proxy.golang.org로 전달합니다(list, latest, 버전 쿼리). 따라서 사내 모듈 경로 이름이 외부 프록시에 노출됩니다.
- 폐쇄망에서는 공개 모듈에도 `GOSUMDB=off` 또는 `GONOSUMDB`를 설정하세요(sum.golang.org에 접속할 수 없습니다).
- 업로드 크기 제한: `.zip` 500 MiB, `.mod` 16 MiB, `.info` 1 MiB. 초과하면 413을 반환합니다.

### Cargo 설정

`~/.cargo/config.toml`에 레지스트리를 추가하고 crates.io를 Command Center로 대체합니다.

```toml
[registries.commandcenter]
index = "sparse+http://localhost:8081/repository/cargo-all/"

[source.crates-io]
replace-with = "commandcenter"
```

publish·yank에는 API 토큰이 필요합니다. 서버에서 발급한 뒤 `cargo login`으로 저장하세요.

```bash
docker compose exec commandcenter commandcenter token create -user ci
cargo login --registry commandcenter
cargo publish --registry commandcenter
```

같은 버전을 다시 publish하면 409로 거부됩니다. `cargo yank`/`cargo yank --undo`를 지원합니다.
publish는 `.crate`를 검사합니다. gzip으로 압축한 tar여야 하고, 모든 항목이 `<이름>-<버전>/` 아래에 있어야 하며(절대 경로와 `..` 불가), `<이름>-<버전>/Cargo.toml`이 있어야 하고, 링크나 sparse 항목은 받지 않습니다(위반하면 400). 저장된 crate는 `crates/<이름>/<버전>.crate` 경로로도 받을 수 있습니다(UI와 API가 이 주소를 알려 줍니다).
그룹 인덱스는 멤버 순서상 그 crate를 처음 가진 멤버의 인덱스를 그대로 냅니다(멤버끼리 합치지 않으므로 hosted를 proxy보다 앞에 두세요). 멤버가 "없음"이 아닌 오류를 내면 다음 멤버로 넘어가지 않고 5xx로 답합니다. 건너뛰면 오래된 버전이 조용히 선택될 수 있기 때문입니다.

- `anonymousRead: false`인 저장소는 config.json이 `auth-required: true`를 알립니다. cargo 1.74 이상은 읽기에도 토큰이 필요하므로, `cargo login --registry commandcenter` 실행 시 `token create`로 출력된 토큰을 입력하세요.
- 설정 파일의 `baseURL`은 `index = "sparse+http://…"`에 쓰는 주소와 같아야 합니다. config.json의 `dl`·`api`가 이 값으로 만들어집니다.
- proxy 저장소의 `remote`는 `sparse+` 접두사를 뗀 sparse index URL입니다. 예: `https://index.crates.io/`

## 운영

| 작업 | 명령 |
|---|---|
| 로그 | `docker compose logs -f` |
| 메모리 확인 | `docker stats commandcenter` |
| 사용하지 않는 blob 정리 | `docker compose exec commandcenter commandcenter gc` |
| API 토큰 발급 | `docker compose exec commandcenter commandcenter token create -user ci` |
| 비밀번호 해시(첫 가져오기용) | `docker run --rm -i commandcenter:latest hash-password` |
| 관리자 비상 복구(서버 정지 후) | `docker compose run --rm -T commandcenter user reset-password -user admin -admin` |
| 버전 확인 | `docker compose exec commandcenter commandcenter version` |

### 로그

표준 출력에 JSON 한 줄씩 기록합니다. 요청마다 `id`, `method`, `path`, `status`, `bytes`, `ms`, `user`, `remote` 필드가 있습니다.
클라이언트가 보낸 `X-Request-Id`는 1–64자의 `[A-Za-z0-9._-]`일 때만 쓰고, 아니면 서버가 새로 만듭니다. 500 오류 메시지에 나오는 request id로 로그를 검색할 수 있습니다.

### 메트릭

`GET /-/metrics`는 `admin` 사용자만 호출할 수 있습니다(JSON).

```bash
curl -u admin http://localhost:8081/-/metrics
```

필드: `heapAllocBytes`, `heapSysBytes`, `sysBytes`, `numGC`, `goroutines`, `rssBytes`(Linux에서만 값이 있음).

### Cleanup 정책

오래되었거나 더 이상 내려받지 않는 컴포넌트를 자동으로 지우는 규칙입니다. 메뉴는 OPERATIONS → Cleanup 정책입니다. 정책은 이름, 대상 포맷(`all` 또는 다섯 포맷 중 하나)과 아래 기준으로 이루어지며, 저장소 폼의 **Cleanup policy** 선택(저장소 정의의 `cleanupPolicy`)으로 hosted·proxy 저장소에 연결합니다. 정책의 포맷은 `all`이거나 저장소의 포맷과 같아야 하고, 저장소가 쓰는 정책은 지울 수 없습니다(409, 저장소 이름을 알려 줌).

| 기준 | 의미 |
|---|---|
| `componentAgeDays` | 컴포넌트의 **가장 새로운 에셋**이 N일보다 오래전에 만들어졌다. 0이면 사용 안 함. |
| `lastDownloadedDays` | 컴포넌트의 가장 최근 다운로드가 N일보다 오래되었다. 한 번도 받지 않았으면 생성 시각을 씁니다. 0이면 사용 안 함. |
| `releaseType` | `any`, `release`, `prerelease`(SNAPSHOT 또는 semver 프리릴리스). |
| `namePattern` | `group:name`에 맞추는 RE2 정규식(선택, 200자 이하). |

- 두 기간 중 **하나 이상**은 정해야 하며, 정한 기준은 모두 **AND**로 결합합니다. 모든 기준을 만족하는 컴포넌트만 후보입니다.
- **미리보기**(정책 화면의 "후보 목록 보기")는 후보 개수·용량과 최대 500개의 목록을 보여 줍니다. 저장소마다 에셋을 20 000개까지만 훑으므로 목록이 잘렸거나 저장소가 그보다 크면 `truncated`가 켜지고, 이때 개수와 용량은 **하한**입니다.
- **Cargo hosted 저장소는 제외**됩니다. crate는 불변이므로 yank를 쓰세요. 정책은 Cargo hosted를 건너뛰고 UI에도 안내가 나옵니다. Cargo proxy는 대상입니다.
- hosted의 `writePolicy`는 정리에서 **무시**됩니다. `deny`인 저장소에서도 정리는 지웁니다.
- 정책은 저장만으로 아무것도 지우지 않습니다. **`repository.cleanup` 예약 작업**이 실행할 때만 지우며(다음 절), 한 번에 컴포넌트 5 000개까지, 개수와 용량을 작업 로그와 활동에 기록합니다. proxy는 캐시된 행만 지우고 다음 요청 때 다시 받아 옵니다.
- 작업 실행도 저장소마다 **한 번에 에셋 20 000개**까지만 훑습니다. 큰 저장소는 작업·저장소별 **스캔 위치**(settings 테이블)를 기억해 다음 실행이 마지막으로 훑은 컴포넌트 다음부터 이어 가고, 끝에 닿으면 처음으로 돌아갑니다. 그래서 여러 번 실행하면 저장소 전체를 차례로 평가합니다. 작업 로그에 어디서부터 몇 개를 훑었고 다음 실행이 어디서 시작하는지가 나옵니다. 작업을 지우면 그 위치도 지웁니다.
- 평가 직전에 메모리에 모인 다운로드 수를 먼저 DB에 반영하므로 방금 내려받은 컴포넌트도 보호됩니다.
- **평가한 것만 지웁니다.** 삭제는 평가 때 본 에셋 목록, 가장 새로운 생성·수정 시각, 마지막 다운로드 시각을 삭제 트랜잭션 안에서 다시 확인하고, 그 뒤로 에셋이 더해졌거나(예: 새 SNAPSHOT 빌드) 다시 올라왔거나 내려받아졌으면 그 컴포넌트는 지우지 않고 "평가 후 변경"으로 로그에 남깁니다(실패로 치지 않음).
- 실행 중에도 편집을 따릅니다. 컴포넌트마다 저장소 정의를 다시 보고(메모리), 정책은 50개마다 또는 정책이 수정될 때 다시 읽어, 저장소가 바뀌었거나 정책이 해제·변경되었으면 그 저장소의 삭제를 멈춥니다.
- 지운 에셋의 **blob은 바로 반환되지 않습니다.** 다음 `blobstore.compact` 작업이 회수합니다.
- 설정 PUT에서 `cleanupPolicy`를 빼면 기존 값이 유지됩니다.

### 예약 작업

OPERATIONS → 예약 작업(Tasks)에서 만들고 편집하고 지웁니다. 작업은 한 번에 하나씩 실행하며, 실행 중에 시간이 된 다른 작업은 다음 확인(30초마다) 때 실행됩니다.

| 종류 | 설명 | 파라미터 |
|---|---|---|
| `blobstore.compact` | 쓰이지 않는 blob 정리(`commandcenter gc`와 같음) | `blobStore`(선택, 한 store만. 비우면 모든 store. 없는 이름은 만들 때 400) |
| `repository.cleanup` | 정책이 연결된 저장소를 정리 | `repos`(비우면 정책이 있는 모든 저장소) |
| `repository.rebuild-metadata` | 모든 GA의 `maven-metadata.xml`을 에셋에서 다시 만듦 | `repo`(Maven hosted만) |

첫 시작 때 작업이 하나도 없으면 `compact-blobstore`(`blobstore.compact`, `daily:03:00`, 사용)를 만듭니다. 이 기본 작업은 지울 수 없습니다(`DELETE`는 409). 지우면 재시작해도 다시 생기지 않아 compaction이 조용히 멈추기 때문이며, 끄고 싶으면 "사용"을 끄세요.

**실패 알림**: 작업의 `notifyEmail`에 주소를 넣으면 실행이 Failed로 끝날 때 메일을 보냅니다("이메일 알림" 참고). 편집 화면의 "실패 알림 이메일" 입력란에 쉼표나 줄바꿈으로 구분해 적습니다.
**일정 문법**

| 일정 | 뜻 |
|---|---|
| `manual` | 지금 실행으로만 |
| `once:<RFC3339>` | 한 번, 예: `once:2026-11-01T02:00:00+09:00`. 오프셋이 없으면(`once:2026-11-01T02:00`) 서버 시간대의 시각입니다. 화면은 오프셋 없이 서버 시간대로 보내고, 시각을 바꾸지 않고 저장하면 원래 값을 그대로 둡니다. |
| `hourly:<MM>` | 매시 MM분 |
| `daily:<HH:MM>` | 매일 |
| `weekly:<mon..sun>:<HH:MM>` | 매주 해당 요일 |
| `monthly:<1-28>:<HH:MM>` | 매월 해당 일(29일 이후는 쓸 수 없음) |

- 시각은 서버의 시간대 기준이며 환경변수 `TZ`로 정합니다(기본 UTC). 예: compose의 `environment`에 `TZ: Asia/Seoul`. 서머타임으로 존재하지 않는 시각은 그 직후 첫 유효한 시각에 실행합니다.
- **지금 실행**은 비동기입니다. 요청은 바로 202를 돌려주고 작업은 뒤에서 돌며, 화면(또는 `GET /-/api/tasks/{id}`)의 상태가 Running에서 바뀌면 끝난 것입니다. 다른 작업이 실행 중이면 409("a task is already running", `POST /-/api/gc`도 같음)이고 응답의 `running`·`runningName`이 그 작업을 알려 줍니다. 수동 실행은 다음 예정 시각(`next_run`)을 바꾸지 않습니다.
- **중지**: 실행 중인 작업은 상세 화면의 "중지"(또는 `POST /-/api/tasks/{id}/stop`, 관리자)로 멈춥니다. 실행 중이면 202, 아니면 409이며, 작업은 진행 중인 단계(cleanup은 지우던 컴포넌트)를 마치고 멈춰 결과가 **Stopped**로 남습니다.
- 실행 하나는 최대 6시간이며 넘으면 취소됩니다. 실행 기록을 저장하지 못하면(DB 오류) 다음 예정 시각을 메모리에서 넘겨 틱마다 다시 실행하지 않습니다.
- 실행 로그는 최대 200줄·64 KiB이고 넘치면 **앞부분과 뒷부분**만 남기고 가운데를 생략했다는 줄을 넣습니다. 그래서 마지막 줄(성공·실패)은 항상 보입니다. 로그는 시스템 로그에도 함께 기록됩니다.
- 기존 `POST /-/api/gc`는 compact 실행의 별칭으로 남아 있습니다.

### 로그·활동

- **로그**: SYSTEM → 시스템 상태·로그에서 서버 로그를 봅니다. 최근 **2 000건 또는 1 MiB** 중 먼저 닿는 한도까지 메모리에 보관하며, 재시작하면 사라집니다(표준 출력의 JSON 로그는 그대로 `docker compose logs`로 남습니다). 수준(INFO/WARN/ERROR)과 텍스트로 거르고, "다운로드"는 `commandcenter.log` 텍스트 파일을 내려줍니다(`GET /-/api/logs`, `/-/api/logs/download`, 관리자 전용).
- 로그에는 비밀 값이 들어가지 않습니다. 요청 경로의 토큰은 가려지고 NUL 같은 제어 문자는 `\x00` 꼴로 바뀌어 기록되며, proxy 원격 URL의 **자격 증명(userinfo)과 쿼리 값은 제거**하며, 인증 헤더는 기록하지 않습니다. 작업 로그에 나오는 URL도 같은 처리를 합니다.
- **활동(Events)**: 대시보드의 "최근 운영 활동"과 `GET /-/api/events`(관리자, 최대 200건씩, `before`·`kind`로 페이징·필터). DB에 최근 10 000건만 남기며, 기록은 비동기라 큐(1 000건)가 가득 차면 요청을 막지 않고 그 이벤트를 버립니다. 종류:

  | kind | 시점 |
  |---|---|
  | `upload` | hosted 쓰기 성공(Maven 주 아티팩트, npm publish, PyPI 업로드, Go zip, Cargo publish) |
  | `delete` | 에셋·컴포넌트 삭제 |
  | `repo.create` / `repo.update` / `repo.delete` | 저장소 변경 |
  | `token.create` / `token.revoke` | API 토큰 발급·폐기 |
  | `task.run` | 작업 실행(결과와 요약) |
  | `cleanup` | 정리 결과(개수, 용량) |
  | `policy.create` / `policy.update` / `policy.delete` | 정책 변경 |

### 다운로드 수·요청 추이

- 에셋 본문을 GET으로 성공적으로 받을 때마다(HEAD는 제외) 그 에셋의 다운로드가 1 늘고 마지막 다운로드 시각이 갱신됩니다. proxy가 캐시를 채우는 요청도 셉니다. 컴포넌트 상세의 기본 정보와 Assets 탭, Browse 파일 목록에 표시되며 Cleanup의 `lastDownloadedDays`가 이 값을 씁니다.
- 요청마다 DB에 쓰지 않습니다. 카운터는 메모리에 모았다가 **30초마다**(또는 항목이 10 000개에 닿으면, 종료할 때) 한 트랜잭션으로 반영합니다. 그래서 화면의 값은 최대 30초 늦습니다. 주기는 숨은 설정 `metricsFlushSeconds`(1..3600, 기본 30)로 바꿉니다.
- 요청 추이는 `/repository/<이름>/…` 요청 수와 응답 바이트를 시간 단위(UTC)로 집계해 대시보드의 7일 차트와 시스템 상태의 24시간 차트에 그립니다(`GET /-/api/stats/requests?range=24h|7d|30d&repo=`). 24h는 시간별, 7d·30d는 일별이며 **90일**이 지난 집계는 하루 한 번 지웁니다.

### blob 정리 (`commandcenter gc`)

어떤 자산에도 참조되지 않는 blob 파일(업로드 실패나 덮어쓰기로 남은 것)을 지웁니다. 수정된 지 1시간이 안 된 blob은 진행 중인 업로드를 보호하기 위해 지우지 않습니다. 결과는 `removed N unreferenced blobs`로 출력됩니다.

### 백업과 복원

데이터는 named volume에 있습니다. 볼륨 이름은 기본 `commandcenter-data`이며 `COMMANDCENTER_VOLUME`으로 바꿀 수 있습니다. 이전 repolite 설치의 데이터를 쓰려면 `COMMANDCENTER_VOLUME=repolite_repolite-data`(또는 `docker volume ls`에 보이는 이름)를 지정하세요. 일관된 백업을 위해 서버를 멈춘 상태에서 하는 것을 권장합니다.

**백업 (PowerShell)**

```powershell
docker compose stop
docker run --rm -v commandcenter-data:/data -v "${PWD}:/backup" busybox tar czf /backup/data.tgz /data
docker compose start
```

**백업 (Git Bash)** — Git Bash는 컨테이너 안 경로(`/backup`, `/data`)를 윈도우 경로로 바꾸므로 `MSYS_NO_PATHCONV=1`과 `pwd -W`가 필요합니다.

```bash
docker compose stop
MSYS_NO_PATHCONV=1 docker run --rm -v commandcenter-data:/data -v "$(pwd -W)":/backup busybox tar czf /backup/data.tgz /data
docker compose start
```

Linux/macOS는 `-v "$PWD":/backup`을 쓰면 됩니다.

**복원** — 오래된 `meta.db-wal`/`-shm` 파일이 섞이지 않도록 반드시 빈 볼륨에서 시작합니다. `docker compose down -v`는 데이터 볼륨을 삭제하므로 백업 파일을 먼저 확인하세요.

```powershell
docker compose down -v
docker compose create
docker run --rm -v commandcenter-data:/data -v "${PWD}:/backup" busybox tar xzf /backup/data.tgz -C /
docker compose up -d
```

Git Bash에서는 복원의 `docker run` 앞에 `MSYS_NO_PATHCONV=1`을 붙이고 `-v "$(pwd -W)":/backup`을 씁니다.

### 업그레이드

```bash
docker compose build && docker compose up -d
```

데이터 볼륨은 그대로 유지됩니다.

**plan 12 이후 첫 시작(설정 가져오기)**: `config.yaml`의 `users`, `roles`, `anonymousRead`, `baseURL`, `maxUploadMiB`를 DB로 **한 번** 가져옵니다(`repositories`를 가져오던 plan 7과 같은 방식). 가져온 뒤에는 이 키들이 파일에 있어도 무시되며 시작 로그에 경고가 한 번 남습니다. 이 값들은 UI(사용자, 역할, Realm, 일반 설정)에서 관리합니다. `config.yaml`에는 `listen`, `dataDir`, `memoryLimitMiB`, `metricsFlushSeconds`만 남기면 됩니다. 가져오기 전 `users`나 `roles` 항목이 잘못되어 있으면 시작이 실패하고, 가져온 뒤에 생긴 문제는 경고로만 남습니다. 기존 API 토큰과 `meta.db`는 그대로 쓰이고, 세션은 서버 재시작으로 끊깁니다. 예전 `write` 역할은 해당 저장소(또는 `*`)의 browse, read, add, edit, delete 전부로 바뀌며(예전 쓰기는 삭제를 포함했습니다), 로그인한 사용자의 읽기는 `authenticatedRead` 기본값(켜짐) 덕분에 예전과 같습니다. 가져온 뒤에는 `config.yaml`을 고쳐도 사용자·역할이 바뀌지 않으므로 UI를 쓰세요.
### 메모리 설정

- `memoryLimitMiB`(기본 80)는 Go 메모리 한도를 정합니다. 단, `GOMEMLIMIT` 환경변수가 있으면 그 값이 우선하며 compose 파일은 `GOMEMLIMIT: 80MiB`를 설정합니다. 한도를 바꾸려면 둘 중 실제로 쓰이는 쪽(compose 사용 시 `GOMEMLIMIT`)을 고치세요.
- compose의 `mem_limit: 128m`은 컨테이너 하드 한도입니다. 위 두 값보다 충분히 커야 합니다.
- 원격 다운로드는 동시에 32개까지만 진행합니다.

### 업로드 크기 제한

| 대상 | 제한 | 초과 시 |
|---|---|---|
| 모든 포맷의 업로드(GET/HEAD 외) 본문 | `maxUploadMiB`(기본 1024 MiB) | 413 |
| npm publish 문서(tarball 첨부 제외) | 10 MiB | 413 |
| PyPI 업로드 폼 | 사용하는 필드 각 4 KiB, 그 밖의 필드 합계 16 MiB, 필드 최대 256개 | 413 |
| Go `.zip` / `.mod` / `.info` | 500 MiB / 16 MiB / 1 MiB | 413 |
| Go zip 구조 | 항목 50000개, 중앙 디렉터리 4 MiB 이하 | 400 |
| Cargo crate / publish 메타데이터 | 50 MiB / 1 MiB | 413 |

## 제한 사항

- 웹 UI로 저장소 만들기·편집·삭제, 사용자·역할·Realm(LDAP)·Blob Store·일반 설정·이메일 관리, Maven, npm, PyPI 업로드를 할 수 있습니다. SAML, Go·Cargo 업로드 화면은 없습니다. 저장소 정의는 DB에 있고 `config.yaml`의 `repositories:`는 첫 시작에만 쓰입니다. Docker 포맷, S3 등 원격 blob 저장소, 포맷 고유의 검색(`npm search` 포함)도 없습니다.
- **plan 12에서 의도적으로 넣지 않은 것**: 저장소·사용자 **할당량(quota)**, 에디션(Pro/OSS) 구분, **SMTP의 PLAIN 외 인증**(LOGIN, CRAM-MD5, OAuth), TLS 검증 생략 옵션, **AD 확장 매칭 필터**(`:1.2.840…:=`), LDAP 부분 일치·비교 필터, 중첩 그룹 해석, LDAP 사용자 자동 삭제·토큰 자동 폐기, S3 같은 원격 blob store(경로는 로컬 디렉터리만), 이미 만든 저장소의 blob store 이동.
- `npm unpublish`는 지원하지 않습니다(`npm deprecate`, `npm dist-tag`는 지원).
- PyPI는 PEP 658 메타데이터 파일과 `/pypi/<프로젝트>/json`을 제공하지 않습니다.
- Go 체크섬 DB(`sumdb/`)는 프록시하지 않습니다. 사내 모듈에는 `GONOSUMDB`를 쓰세요.
- 삭제는 Maven(`DELETE`)에서만 가능합니다. 다른 포맷은 삭제를 지원하지 않습니다.
- 같은 파일·버전을 다시 올릴 때의 응답:

  | 포맷 | 응답 |
  |---|---|
  | Maven (`allow_once`) | 400 |
  | npm | 403 |
  | PyPI | 400 `File already exists.` |
  | Go | 409 (`writePolicy: allow`이면 허용) |
  | Cargo | 409 |

  Cargo는 `-`/`_`와 대소문자만 다른 이름, 빌드 메타데이터(`+…`)만 다른 버전도 같은 것으로 보고 409로 거부합니다.
- **group과 의존성 혼동**: group은 멤버를 순서대로 조회합니다. Maven과 Go는 `latest`를 멤버 전체 중 가장 새로운 버전으로 고르므로, hosted 멤버를 앞에 두어도 proxy 쪽에 더 높은 버전이 있으면 그것이 `latest`가 됩니다. npm의 `latest`와 Cargo의 인덱스는 멤버 순서가 정합니다(처음으로 가진 멤버의 것). PyPI는 파일 단위로 합치며 같은 파일은 앞 멤버가 우선합니다. PyPI는 같은 프로젝트라도 버전마다 다른 멤버의 파일이 섞일 수 있습니다. 사내 패키지 이름을 공개 저장소에서 선점당하지 않으려면 사내 전용 group을 따로 두거나 proxy 멤버를 넣지 마세요.
- 문서를 가진 멤버가 하나뿐이면(Maven 메타데이터·npm packument) 병합하지 않고 그 문서를 그대로 제공합니다. 다만 Maven 그룹에 이미 병합해 저장한 문서가 있으면 내용이 달라졌을 때 새로 저장하여 `Last-Modified`가 앞으로 움직입니다.
- 405 응답은 `Allow` 헤더로 그 경로가 실제로 받는 메서드를 알려 줍니다(예: npm dist-tag 경로 `/-/package/{pkg}/dist-tags/{tag}`는 `PUT, DELETE`, proxy 저장소의 쓰기 경로는 `GET, HEAD`). 받는 메서드가 없는 경로(npm `-rev`, proxy의 Cargo publish·PyPI 업로드 경로)는 빈 `Allow`를 보냅니다.
- proxy가 받은 파일의 체크섬이 원격 인덱스와 다르면 첫 요청은 502이고, 이후 `negativeCacheTTL` 동안은 404입니다. 이 404는 그 사이 원격 인덱스가 바뀌어도 만료될 때까지 유지됩니다.

## 문제 해결

- **`baseURL` 불일치**: npm/pip/cargo가 "다운로드 실패", 엉뚱한 호스트로 접속, `ECONNREFUSED`를 내면 응답 안의 URL이 클라이언트가 접속하는 주소와 다른 것입니다. `baseURL`을 클라이언트가 쓰는 주소(스킴, 호스트, 포트)와 똑같이 맞추고 `docker compose restart` 하세요.
- **npm 401**: `~/.npmrc`의 토큰이 서버에 없으면(예: 볼륨을 다시 만든 뒤) `anonymousRead`가 켜져 있어도 모든 요청이 401입니다. `npm login`을 다시 하거나 `_authToken` 줄을 지우세요.
- **`508 proxy loop detected`**: proxy 저장소의 `remote`가 결국 같은 Command Center 인스턴스를 가리키면(예: 자기 자신의 그룹) 요청이 돌고 돌지 않도록 서버가 508로 거절합니다. 요청마다 `X-Command-Center-Hop` 헤더에 인스턴스 ID를 덧붙여 알아내므로, 서로 다른 두 인스턴스가 이어지는 것(A가 B를 proxy)은 막지 않습니다. `remote`를 고치세요.
- **`config.yaml`이 디렉터리**: `config.yaml` 없이 `docker compose up`을 하면 Docker가 같은 이름의 디렉터리를 만듭니다. `docker compose down`, 디렉터리 삭제, 파일 복사 후 다시 실행하세요.
- **Windows 경로**: Git Bash에서 `docker run -v`에 `/c/...` 같은 경로를 쓰면 경로가 변환되어 마운트가 어긋날 수 있습니다. `MSYS_NO_PATHCONV=1`을 설정하고 `$(pwd -W)`(또는 PowerShell의 `${PWD}`)를 쓰세요. 데이터는 바인드 마운트보다 named volume(compose 기본값)이 SQLite 잠금과 I/O에 안정적입니다.
- **시작 실패 / 동작 이상**: 먼저 `docker logs commandcenter`(또는 `docker compose logs`)를 확인하세요. 설정 오류는 시작할 때 항목별로 출력됩니다.
- **proxy 저장소가 비어 있음**: 컨테이너에서 원격 레지스트리로 HTTPS 접속이 되어야 합니다. 사내 프록시가 있으면 compose의 `HTTPS_PROXY` 환경변수를 설정하세요.

## 개발

로컬에 Go를 설치하지 않고 컨테이너(golang:1.26)에서 테스트합니다.

```bash
scripts/go.sh test ./...      # 단위·통합 테스트
scripts/go.sh vet ./...

test/e2e.sh                   # 스모크 테스트 5개 + 부하 테스트를 모두 실행하고 요약 출력

test/smoke/maven-smoke.sh     # 실제 클라이언트로 포맷별 E2E
test/smoke/npm-smoke.sh
test/smoke/pypi-smoke.sh
test/smoke/go-smoke.sh
test/smoke/cargo-smoke.sh

test/load/load.sh             # 128 MiB 컨테이너 메모리 부하 테스트
```
