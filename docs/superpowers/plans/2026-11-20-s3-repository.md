# S3 저장소 포맷 구현 계획 (MVP)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** aws cli, mc, boto3, rclone, Terraform S3 backend가 CC의 `s3` 포맷 저장소를 S3 bucket처럼 쓸 수 있게 한다.

**Architecture:** 순수 SigV4 패키지(`internal/sigv4`)가 서명과 aws-chunked 본문을 검증하고, `internal/auth`가 S3 키를 주인 Principal로 바꾼다. `internal/format/s3`가 S3 REST 작업을 기존 `repo.Store`(asset + 내용 주소 blob) 위에 구현하며, 서버는 `s3.host`(Host 헤더) 또는 `s3.listen`(전용 포트)으로 들어온 요청을 기존 `/repository/` 인증 경로를 거치지 않고 이 핸들러로 보낸다.

**Tech Stack:** Go 1.27(표준 라이브러리만, 새 모듈 의존성 없음), PostgreSQL 13+, 기존 Helm 차트.

**Spec:** `docs/superpowers/specs/2026-11-20-s3-repository-design.md`

## Global Constraints

- 새 Go 모듈 의존성 금지(`THIRD_PARTY_LICENSES.txt` 검사). CRC32/CRC32C는 `hash/crc32`, XML은 `encoding/xml`.
- 모든 SQL은 `?` 자리표시자, 텍스트 열은 `COLLATE "C"`, 시각은 Unix ms `BIGINT`(`internal/meta/dialect.go` 머리말 규칙).
- 스키마 변경은 expand-only: `schemaVersion` 9 → 10, `schemaCompat`는 7 유지, `TestSchemaCompat`에 10 추가.
- 클러스터 잠금 키는 포맷 이름 + NUL로 시작: `"s3\x00<bucket>\x00<key>"`.
- secret은 `meta` sealer로 암호화해 저장하고, 로그·이벤트·오류 메시지에 절대 넣지 않는다.
- 테스트는 `COMMANDCENTER_TEST_POSTGRES_URL`의 PostgreSQL에서 돈다. 로컬에는 Go가 없으므로 `golang:1.27.3` 컨테이너에서 `go test`.

## MVP 경계

MVP에 들어가는 것: spec "API 범위" 전체(버킷·객체·목록·Multipart·Copy·조건부 요청·presigned·chunked/trailer), 키 관리 REST API, 최소 UI(저장소 생성 화면에 `s3` 포맷, 내 계정의 S3 키 카드), config/차트의 `s3` 섹션, smoke 스크립트.

MVP 뒤로 미루는 것(별도 작업): admin 전용 전체 키 화면(API `?all=1`은 MVP에 포함), 메트릭 `s3_signature_failures_total`, HA smoke, 복제·내보내기의 `s3_object_meta` 이전, README·가이드 장 전체(MVP는 가이드에 짧은 절만).

## 파일 구조

| 파일 | 책임 |
|---|---|
| `internal/sigv4/sigv4.go` | Authorization 헤더·presigned query 파싱, canonical request, 서명 키 유도, 서명 검증 |
| `internal/sigv4/chunked.go` | `aws-chunked` 본문 디코더(signed, signed+trailer, unsigned+trailer), trailer 체크섬 검증 |
| `internal/sigv4/sigv4_test.go`, `chunked_test.go` | AWS 문서 벡터와 자체 서명기 왕복 테스트 |
| `internal/meta/s3.go` | `s3_keys`, `s3_object_meta`, `s3_uploads` CRUD, 목록 조회 |
| `internal/meta/dialect.go`, `meta.go` | 테이블 3개, schemaVersion 10 |
| `internal/meta/secrets.go` | `RotateSecretKey`가 `s3_keys.secret` 재암호화 |
| `internal/auth/s3key.go` | 키 발급·목록·폐기, 조회 캐시, `S3Principal` |
| `internal/config/config.go` | `S3` 설정 블록, `s3` 포맷, bucket 이름 규칙, hosted 전용 |
| `internal/format/s3/s3.go` | Handler 구조체, `Match`, `ServeHTTP` 진입, `/repository/` 읽기 전용 `Serve` |
| `internal/format/s3/router.go` | bucket/key 추출, 작업 선택 |
| `internal/format/s3/auth.go` | 요청 인증(SigV4 → Principal), 본문 래핑 |
| `internal/format/s3/xml.go` | XML 응답·오류 |
| `internal/format/s3/object.go` | Put/Get/Head/Delete/DeleteObjects/Copy, 조건부 요청, 메타데이터 헤더 |
| `internal/format/s3/bucket.go` | ListBuckets, Head/Create/DeleteBucket, ListObjects V1/V2, 고정 응답 |
| `internal/format/s3/multipart.go` | Multipart 6개 작업과 tmp 디렉터리 |
| `internal/format/s3/*_test.go` | 서버 경유 핸들러 테스트(자체 SigV4 서명 클라이언트) |
| `internal/server/server.go` | `S3` 필드, wrap에서 S3 요청 분기, `S3Handler()` |
| `internal/app/app.go` | 핸들러 생성·등록, `s3.listen` 리스너 |
| `internal/blob/blob.go` | tmp 정리에 `mpu-*` 포함(오래된 것만) |
| `internal/ui/s3keys.go` | `/-/api/s3keys` |
| `internal/ui/static/*` | 포맷 목록에 `s3`, 내 계정 S3 키 카드 |
| 기타 포맷 목록 | `cleanup/policy.go`, `cleanup/versions.go`, `component/component.go`, `ui/access.go`, `transfer/transfer.go` |
| `deploy/helm/commandcenter/*` | `s3` values, Service 포트, Ingress host |
| `test/smoke/s3-smoke.sh` | aws cli·mc 실제 클라이언트 smoke |

---

### Task 1: SigV4 검증 패키지

**Files:** Create `internal/sigv4/sigv4.go`, `internal/sigv4/chunked.go`, tests.

**Interfaces (Produces):**
```go
package sigv4

const (
    UnsignedPayload        = "UNSIGNED-PAYLOAD"
    StreamingSigned        = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD"
    StreamingSignedTrailer = "STREAMING-AWS4-HMAC-SHA256-PAYLOAD-TRAILER"
    StreamingUnsignedTrailer = "STREAMING-UNSIGNED-PAYLOAD-TRAILER"
)

// Request is a parsed SigV4-signed request.
type Request struct {
    AccessKey, Date /* yyyymmdd */, Region, Service string
    Time          time.Time // X-Amz-Date
    SignedHeaders []string
    Signature     string
    Presigned     bool
    Expires       time.Duration // presigned only
    PayloadHash   string        // x-amz-content-sha256 value, UNSIGNED-PAYLOAD for presigned
}

var (
    ErrNotSigned  = errors.New("sigv4: request is not signed") // anonymous
    ErrMalformed  = errors.New("sigv4: malformed authorization")
    ErrSignature  = errors.New("sigv4: signature does not match")
    ErrExpired    = errors.New("sigv4: request has expired")
    ErrSkewed     = errors.New("sigv4: request time too skewed")
    ErrV2         = errors.New("sigv4: signature version 2 is not supported")
)

func Parse(r *http.Request) (*Request, error)
func SigningKey(secret, date, region, service string) []byte
func (q *Request) Verify(r *http.Request, secret string, now time.Time) error // header & presigned
// Body wraps r.Body by PayloadHash: chunked kinds decode and verify; a hex
// hash or UNSIGNED-PAYLOAD returns the body unchanged (the caller checks a
// hex hash against the stored blob's sha256).
func (q *Request) Body(r *http.Request, secret string) (io.ReadCloser, error)
var ErrChunkSignature, ErrTrailerChecksum error
// Sign signs r in place (header form); used by tests and the smoke helper.
func Sign(r *http.Request, accessKey, secret, region string, now time.Time, payloadHash string)
func Presign(r *http.Request, accessKey, secret, region string, now time.Time, expires time.Duration)
```

- [ ] **Step 1: 테스트 작성** — `sigv4_test.go`:
  - AWS 문서 예시 "GET Object"(bucket `examplebucket`, key `test.txt`, `Range: bytes=0-9`, `x-amz-date: 20130524T000000Z`, AK `AKIAIOSFODNN7EXAMPLE`, SK `wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY`, region `us-east-1`) → 서명 `f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41`이 `Verify` 통과, 한 글자 바꾸면 `ErrSignature`.
  - presigned 예시(같은 키, GET `/test.txt`, `X-Amz-Expires=86400`, `20130524T000000Z`) → `aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404` 통과, `now`가 만료 뒤면 `ErrExpired`.
  - `Sign` 후 `Parse`+`Verify` 왕복, 쿼리 키 정렬·URI 인코딩(공백, `~`, 한글 key), 시각 15분 초과 `ErrSkewed`, `Authorization: AWS AK:sig` → `ErrV2`, 헤더 없음 → `ErrNotSigned`.
- [ ] **Step 2: `chunked_test.go`** — AWS 문서 chunked 예시(`chunkObject.txt`, 64 KiB `a` × 65536 + 1024 `a` + 0 chunk, seed 서명 `4f232c4386841ef735655705268965c44a0e4690baa4adea153f7db9fa80a0a9`, chunk1 `ad80c730a21e5b8d04586a2213dd63b9a0e99e0e2307b0ade35a65485a288648`)로 디코드 결과가 원문과 같고, chunk 하나 변조 시 `ErrChunkSignature`. 자체 인코더로 unsigned-trailer(`x-amz-checksum-crc32`, `crc32c`, `sha256`)와 signed-trailer 왕복, 체크섬 불일치 `ErrTrailerChecksum`, chunk 크기 상한(16 MiB) 초과 거부.
- [ ] **Step 3: 실패 확인** — `go test ./internal/sigv4/` → 컴파일 실패.
- [ ] **Step 4: 구현** — canonical URI는 path를 그대로(S3는 이중 인코딩 안 함) `EscapedPath()` 기준 세그먼트별 재인코딩, canonical query는 `X-Amz-Signature` 제외 정렬, 헤더 값 공백 축약, `host`는 `r.Host`. chunk 서명 문자열: `AWS4-HMAC-SHA256-PAYLOAD\n<time>\n<scope>\n<prev>\n<sha256("")>\n<sha256(chunk)>`; trailer 서명: `AWS4-HMAC-SHA256-TRAILER\n<time>\n<scope>\n<prev>\n<sha256(trailer lines)>`.
- [ ] **Step 5: 통과 확인 후 커밋** — `Add a SigV4 verifier with aws-chunked bodies`.

### Task 2: 메타데이터 테이블과 DB 메서드

**Files:** Create `internal/meta/s3.go`, `internal/meta/s3_test.go`; Modify `internal/meta/dialect.go`(schema 끝), `internal/meta/meta.go`(`schemaVersion = 10`, 주석), `internal/meta/secrets.go`(`RotateSecretKey`), 스키마 호환 테스트.

**Schema:**
```sql
CREATE TABLE IF NOT EXISTS s3_keys(
  access_key_id TEXT COLLATE "C" PRIMARY KEY, owner TEXT COLLATE "C" NOT NULL,
  name TEXT COLLATE "C" NOT NULL DEFAULT '', secret TEXT COLLATE "C" NOT NULL,
  created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL DEFAULT 0, last_used_at BIGINT NOT NULL DEFAULT 0);
CREATE INDEX IF NOT EXISTS s3_keys_owner ON s3_keys(owner);
CREATE TABLE IF NOT EXISTS s3_object_meta(
  repo TEXT COLLATE "C" NOT NULL, path TEXT COLLATE "C" NOT NULL,
  blob_sha256 TEXT COLLATE "C" NOT NULL, etag TEXT COLLATE "C" NOT NULL DEFAULT '',
  headers TEXT COLLATE "C" NOT NULL DEFAULT '{}',
  PRIMARY KEY(repo, path), FOREIGN KEY(repo, path) REFERENCES assets(repo, path) ON DELETE CASCADE);
CREATE TABLE IF NOT EXISTS s3_uploads(
  upload_id TEXT COLLATE "C" PRIMARY KEY, repo TEXT COLLATE "C" NOT NULL, path TEXT COLLATE "C" NOT NULL,
  initiator TEXT COLLATE "C" NOT NULL, content_type TEXT COLLATE "C" NOT NULL DEFAULT '',
  headers TEXT COLLATE "C" NOT NULL DEFAULT '{}', started_at BIGINT NOT NULL, updated_at BIGINT NOT NULL);
CREATE INDEX IF NOT EXISTS s3_uploads_repo ON s3_uploads(repo, path);
```

**Interfaces (Produces):**
```go
type S3Key struct { AccessKeyID, Owner, Name string; Created, Expires, LastUsed time.Time }
func (d *DB) PutS3Key(ctx, k S3Key, secret string) error            // seals secret; ErrExists on id clash
func (d *DB) S3KeyByID(ctx, id string) (S3Key, string, error)       // opens secret; ErrNotFound
func (d *DB) ListS3Keys(ctx, owner string) ([]S3Key, error)         // owner "" = all, ordered by owner, created
func (d *DB) DeleteS3Key(ctx, id string) (S3Key, error)             // returns deleted row; ErrNotFound
func (d *DB) DeleteS3KeysOf(ctx, owner string) (int64, error)
func (d *DB) TouchS3Key(ctx, id string, at time.Time)               // best effort, logs nothing

type S3ObjectMeta struct { BlobSHA256, ETag string; Headers map[string]string }
func (t *Tx) PutS3ObjectMeta(ctx, repo, path string, m S3ObjectMeta) error // upsert
func (d *DB) GetS3ObjectMeta(ctx, repo, path string) (S3ObjectMeta, error) // ErrNotFound

type S3Upload struct { ID, Repo, Path, Initiator, ContentType string; Headers map[string]string; Started, Updated time.Time }
func (d *DB) PutS3Upload(ctx, u S3Upload) error
func (d *DB) GetS3Upload(ctx, id string) (S3Upload, error)
func (d *DB) TouchS3Upload(ctx, id string, at time.Time) error
func (d *DB) DeleteS3Upload(ctx, id string) error
func (d *DB) ListS3Uploads(ctx, repo, prefix, after string, limit int) ([]S3Upload, error)
func (d *DB) StaleS3Uploads(ctx, before time.Time) ([]S3Upload, error)

// ListObjects returns assets of repo with path >= from (or > from when
// exclusive) whose path starts with prefix, in byte order, at most limit.
func (d *DB) ListObjects(ctx, repo, prefix, from string, exclusive bool, limit int) ([]Asset, error)
func (d *DB) RepoHasAssets(ctx, repo string) (bool, error)
```

- [ ] **Step 1: 테스트** — 키 저장 후 원본 secret으로 다시 읽힘, DB 행의 secret은 `sealedPrefix`로 시작(`SetSecretKey` 있을 때), 같은 id `ErrExists`, owner별 목록, 삭제. object meta: asset 삭제 시 cascade로 사라짐, upsert. uploads: 목록 prefix·after, stale. `ListObjects`: `a`, `a/b`, `a/c`, `b`, 한글 key, prefix `a/`, exclusive 경계, limit.
- [ ] **Step 2: 실패 확인 → Step 3: 구현 → Step 4: 통과**. `TestSchemaCompat`(또는 동등 테스트)에 10을 추가. `RotateSecretKey`가 `s3_keys`를 재암호화하는지 테스트 한 줄 추가.
- [ ] **Step 5: 커밋** — `Store S3 keys, object metadata and multipart uploads`.

### Task 3: config — `s3` 포맷과 `s3` 설정

**Files:** Modify `internal/config/config.go`, `internal/config/config_test.go`, `config.example.yaml`; 포맷 목록 파일들.

**Interfaces (Produces):**
```go
type S3 struct {
    Host        string `yaml:"host"`
    VirtualHost bool   `yaml:"virtualHost"`
    Listen      string `yaml:"listen"`
    Region      string `yaml:"region"` // default "us-east-1"
}
// Config gains: S3 S3 `yaml:"s3"`
func (s S3) Enabled() bool { return s.Host != "" || s.Listen != "" }
func ValidBucketName(name string) bool
```

- [ ] **Step 1: 테스트** — `s3` hosted OK, proxy/group 거부, `My_Bucket`·`ab`·`192.168.0.1`·`a..b` 거부, `s3.region` 기본값, `virtualHost` 켜고 host 비면 오류.
- [ ] **Step 2–4: 구현** — `knownFormats["s3"]`, hosted 전용 규칙(gitlfs 옆), 이름 규칙은 format이 s3일 때만. `cleanup.policyFormats`, `cleanup/versions.go`의 raw 분기, `component.go`의 raw 분기, `transfer.go`의 raw 분기, `ui/access.go` 목록에 `s3` 추가(전부 raw와 같은 "파일 하나가 컴포넌트" 의미).
- [ ] **Step 5: 커밋** — `Add the s3 format and the s3 settings`.

### Task 4: S3 키 인증 (auth)

**Files:** Create `internal/auth/s3key.go`, `internal/auth/s3key_test.go`.

**Interfaces:**
- Consumes: `meta.DB` S3 키 메서드(Task 2), `a.lookup`, `a.throttle`.
- Produces:
```go
type S3KeyStore interface { PutS3Key; S3KeyByID; DeleteS3Key; TouchS3Key } // *meta.DB
const MaxS3KeysPerUser = 10
var ErrTooManyS3Keys, ErrS3KeyUnknown error
func (a *Authenticator) CreateS3Key(ctx, owner, name string, expires time.Time) (meta.S3Key, string /*secret*/, error)
func (a *Authenticator) DeleteS3Key(ctx, id string) (meta.S3Key, error) // drops the cache entry
func (a *Authenticator) ForgetS3Keys()                                  // whole cache (Notify "access")
// S3Check finds the key, runs verify(secret) under the sign-in throttle
// (keyed by the access key id) and returns the owner as a token principal.
func (a *Authenticator) S3Check(ctx, id string, verify func(secret string) error) (Principal, error)
```
- [ ] **Step 1: 테스트** — 발급된 id는 `CCK` + 17자 base32 대문자, secret 40자; 11번째 발급 `ErrTooManyS3Keys`; `S3Check` 성공 시 `Via == ViaToken`, `User` = 주인; verify 오류 → `ErrBadCredentials` 계열; 만료 키 → 실패; 주인 비활성화 → 실패; 삭제 후 즉시 실패(캐시 무효화).
- [ ] **Step 2–4: 구현** — 캐시는 `sync.Map[id]→{key, secret}`, `Reload`(access 토픽)에서 비움. 마지막 사용은 `meta.TokenTouchInterval` 간격으로만 `TouchS3Key`.
- [ ] **Step 5: 커밋** — `Issue S3 access keys and check them as tokens`.

### Task 5: S3 핸들러 — 라우팅, 인증, XML, 버킷·객체·목록

**Files:** Create `internal/format/s3/{s3,router,auth,xml,object,bucket}.go`, `internal/format/s3/s3_test.go`.

**Interfaces:**
- Consumes: Task 1–4, `repo.Store`(`Lock`, `Tx`, `WriteBlob`, `NewAsset`, `Get`, `Open`, `Delete`, `Emit`), `repo.Registry`(`Lookup`, `Snapshot`, `Create`, `Delete` — 실제 이름은 구현 시 registry.go 확인), `repo.ServeAsset`.
- Produces:
```go
type Handler struct {
    Store *repo.Store; Registry *repo.Registry; Auth *auth.Authenticator
    Config config.S3; Now func() time.Time; Log *slog.Logger
    ReadOnly func() string; Unavailable func(*repo.Repository) string
    CreateBucket func(ctx context.Context, name string, p auth.Principal) error // app: registry create with default store
    DeleteBucket func(ctx context.Context, name string, p auth.Principal) error
}
func (h *Handler) Match(r *http.Request) bool                 // Host == s3.host or *.s3.host
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) // full S3 API
func (h *Handler) Serve(w, r, rp *repo.Repository, path string)     // /repository/: GET/HEAD only
func (h *Handler) BucketOf(r *http.Request) string                   // for request metrics
```
- 테스트 클라이언트: `s3_test.go`에 `sigv4.Sign`을 쓰는 `e.s3(method, path, body, opts...)`, raw 테스트의 `newEnv` 패턴(실제 `server.Server` 경유).

- [ ] **Step 1: 테스트** — 서명 없는 PUT 403 `AccessDenied`(XML), 잘못된 서명 403 `SignatureDoesNotMatch`, 잘못된 region 400 `AuthorizationHeaderMalformed` + `<Region>`; PutObject(Content-MD5, x-amz-meta, Content-Type) → ETag=MD5; Get/Head(헤더·메타 복원, Range 206); 같은 key 덮어쓰기는 edit 권한 필요; `If-None-Match: *` 412; DeleteObject 204(없는 key도); DeleteObjects XML 결과; CopyObject 같은 blob 공유(blob 개수 그대로) + `REPLACE`; ListObjectsV2 prefix/delimiter/CommonPrefixes/continuation, V1 marker; ListBuckets는 읽을 수 있는 s3 저장소만; HeadBucket 404 `NoSuchBucket`; CreateBucket 권한 없으면 403, 있으면 200 그리고 `/repository/<b>/` 읽기 가능; DeleteBucket 비어 있지 않으면 409; `GET ?location`, `?versioning`, `?acl`, `?policy` 고정 응답; `PUT ?tagging` 501; presigned GET 성공과 만료; virtual-host(`b.s3.test`) 경로; `/repository/<b>/k` GET 성공, PUT 405.
- [ ] **Step 2–4: 구현** — key 검증(1~1024B, UTF-8, 제어 문자, `.`/`..` 세그먼트, 앞 `/` 거부). PUT: 서명 → 권한(add) → `WriteBlob`(verify: Content-MD5, hex payload hash, `x-amz-checksum-sha256`) → `Lock("s3\x00b\x00k")` → 기존 asset 확인(edit 권한, 조건) → `Tx`에서 asset + `PutS3ObjectMeta` → `Emit("upload")`. 오류 매핑 함수 `writeErr(w, r, err)`가 `repo` 오류 상태를 S3 코드로 변환.
- [ ] **Step 5: 커밋** — `Serve buckets and objects over the S3 API`.

### Task 6: Multipart Upload

**Files:** Create `internal/format/s3/multipart.go`, `internal/format/s3/multipart_test.go`; Modify `internal/blob/blob.go`(tmp 정리), `internal/tasks`의 `blobstore.delete-temp-files`가 `StaleS3Uploads` 행도 지우도록(구현 시 위치 확인).

**Interfaces:**
- Produces: `blob.Store.TmpDir() string`(없으면 추가), tmp 디렉터리 이름 `mpu-<uploadId>`, part 파일 `<n>.part`와 `<n>.md5`.
- [ ] **Step 1: 테스트** — Create → UploadPart 3개(5 MiB, 5 MiB, 1 B) → ListParts → Complete → GET 내용·크기·ETag `md5(md5s)-3`; 순서 틀림 `InvalidPartOrder`; 작은 중간 part `EntityTooSmall`; ETag 불일치 `InvalidPart`; Abort 후 `NoSuchUpload`; ListMultipartUploads; 다른 사용자의 UploadPart 403; 오래된 업로드 정리.
- [ ] **Step 2–4: 구현** — Complete는 200 헤더 후 10초마다 공백을 쓰는 goroutine, 끝에 결과 XML 또는 `<Error>`.
- [ ] **Step 5: 커밋** — `Take multipart uploads into one blob`.

### Task 7: 서버·앱 배선

**Files:** Modify `internal/server/server.go`, `internal/app/app.go`, `internal/server/server_test.go`.

- `Server.S3 interface{ http.Handler; Match(*http.Request) bool; BucketOf(*http.Request) string }`.
- `wrap`: `s3 := s.S3 != nil && (forced || s.S3.Match(r))`이면 crossOrigin 검사·`Authenticate`·익명 본문 기한을 건너뛰고 `s.S3.ServeHTTP`로 보낸다(요청 로그·본문 한도·idle은 그대로). 메트릭 이름은 `BucketOf`.
- `func (s *Server) S3Handler() http.Handler` — 모든 요청을 S3로(전용 포트).
- app: 핸들러 생성, `handlers["s3"]`, `deleters.Register("s3","hosted",...)`가 필요하면 raw와 동일하게, `cfg.S3.Listen`이면 두 번째 `net.Listen` + 같은 종료 절차.
- [ ] **Step 1: 테스트** — Host가 `s3.host`인 요청이 S3 XML을 받고, 다른 Host의 `/repository/`는 기존대로; `S3Handler`로 들어온 `/` 요청이 ListBuckets.
- [ ] **Step 2–4: 구현 → Step 5: 커밋** — `Route the S3 host and port to the S3 API`.

### Task 8: 키 관리 API와 최소 UI

**Files:** Create `internal/ui/s3keys.go`, `internal/ui/s3keys_test.go`; Modify `internal/ui/api.go`(라우트), `internal/ui/static/` 내 계정 화면과 `repoform.js`(`RECIPE_FORMATS`에 `["s3","s3"]`), i18n 문자열.

- `GET /-/api/s3keys`(본인, admin은 `?all=1`), `POST /-/api/s3keys` `{name, expiresAt}`(interactive + UI 헤더, 201 `{accessKeyId, secretAccessKey, ...}`), `DELETE /-/api/s3keys/{id}`(본인 또는 admin, 204). 이벤트 `s3key.create`/`s3key.delete`(secret 제외). 사용자 삭제 시 `DeleteS3KeysOf`.
- [ ] **Step 1: 테스트**(usertokens_test.go 패턴) → **Step 2–4: 구현** → **Step 5: 커밋** — `Manage S3 access keys in the web UI`.

### Task 9: 차트, 문서, smoke

**Files:** Modify `deploy/helm/commandcenter/{values.yaml,values.schema.json,templates/secret-config.yaml,templates/service.yaml,templates/ingress.yaml}`, `docs/guide.md`(짧은 "S3 저장소" 절), `README.md` 포맷 표; Create `test/smoke/s3-smoke.sh`, `test/e2e.sh` checks에 추가.

- smoke: CC + PostgreSQL 컨테이너, admin으로 s3 저장소·키 생성, `amazon/aws-cli`로 `mb`/`cp`(1 KiB, 20 MiB multipart)/`ls`/`presign`+curl/`rm`, `minio/mc`로 `cp`/`ls`/`cat`/`rm`.
- [ ] `helm template --set s3.enabled=true,s3.port=9000` 렌더 확인 → 커밋 `Expose the S3 API in the chart; document it; add the S3 smoke`.

## 자체 점검

- spec 커버: API 범위(Task 5·6), 인증(1·4), 키 관리(4·8), endpoint(3·7), 차트(9), 오류 표(5의 `writeErr`), 정리(6), 테스트(각 Task + 9). 미룬 항목은 "MVP 경계"에 명시.
- 이름 일관성: `S3Check`, `CreateS3Key`, `PutS3ObjectMeta`, `ListObjects`, `Handler.Match/BucketOf/S3Handler`를 Task 간 동일하게 사용.
