# S3 저장소 포맷 설계

- 날짜: 2026-11-20
- 상태: 설계 승인, 구현 계획 전
- 브랜치: `s3-repository` (origin/main `00c050a` 기준)

## 목표

Command Center(CC)를 통합 아티팩트 저장소로 쓰면서, S3 API 클라이언트(aws cli, mc, boto3, rclone, Terraform S3 backend)가 CC에 직접 객체를 올리고 받을 수 있게 한다. MinIO를 따로 두지 않고 CC 하나로 S3 호환 객체 저장소 역할까지 맡긴다.

## 결정 사항

| 항목 | 결정 |
|---|---|
| bucket ↔ 저장소 | 새 `s3` 포맷(hosted 전용). 저장소 하나 = bucket 하나. raw 저장소와는 별개 |
| 첫 버전 범위 | 기본 객체 저장소(아래 "API 범위"). versioning, policy, lifecycle, ACL, tagging은 제외 |
| endpoint | 전용 호스트 이름(`s3.host`, path-style 기본, virtual-hosted 선택)과 전용 포트(`s3.listen`) 둘 다 지원 |
| 자격 증명 | 사용자가 직접 발급하는 S3 access key/secret. 권한은 키 주인의 역할을 그대로 따름 |
| Multipart | part를 store `tmp/`에 받고 Complete 때 일반 blob 하나로 합침(접근안 1) |

### 고려했다가 뺀 것

- **raw 저장소를 S3로 노출**: raw 경로 규칙이 S3 key와 맞지 않고, S3 메타데이터 의미를 온전히 지킬 수 없어 새 포맷을 택했다.
- **part별 blob + asset이 part 목록을 가리킴**: "asset 하나 = blob 하나" 전제가 깨져 GC, 복제, 내보내기, 체크섬, store 이동을 모두 고쳐야 한다.
- **S3 blob store의 native multipart 위임**: 코드 경로가 둘로 갈리고, 내용 주소(sha256 키)는 Complete 뒤에야 정해져 버킷 안 재복사가 필요하다. 필요해지면 Complete 단계만 나중에 최적화한다.
- **기존 사용자 토큰 재사용**: pass code를 SHA-256 해시로만 저장해(`internal/auth/usertoken.go`) SigV4 HMAC을 계산할 수 없다.

## API 범위 (첫 버전)

- 버킷: ListBuckets, HeadBucket, CreateBucket, DeleteBucket(비었을 때만), GetBucketLocation
- 객체: PutObject, GetObject(Range), HeadObject, DeleteObject, DeleteObjects(최대 1000), CopyObject
- 목록: ListObjectsV2, ListObjects(V1)
- Multipart: CreateMultipartUpload, UploadPart, CompleteMultipartUpload, AbortMultipartUpload, ListParts, ListMultipartUploads
- 메타데이터: Content-Type, `x-amz-meta-*`, Cache-Control, Content-Disposition, Content-Encoding, ETag
- 조건부 요청: If-Match, If-None-Match(`*` 포함), If-Modified-Since, If-Unmodified-Since
- 인증: SigV4 헤더, presigned URL(query SigV4), `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`, `STREAMING-UNSIGNED-PAYLOAD-TRAILER`(crc32, crc32c, sha256), `UNSIGNED-PAYLOAD`
- 클라이언트 호환을 위한 고정 응답: `GET ?versioning`(비어 있음), `GET ?acl`(주인 FULL_CONTROL), `GET ?policy`(`NoSuchBucketPolicy`)
- 그 밖의 하위 리소스 쓰기(`?tagging`, `?policy`, `?lifecycle`, `?acl` PUT 등)는 `NotImplemented` 501

제외: versioning, bucket policy, lifecycle XML, ACL 쓰기, tagging, SSE-C, SigV2, Object Lock, S3 Select, UploadPartCopy.

## 구성 요소

### 1. 포맷 등록

- `config.knownFormats`, `cleanup.policyFormats`, `config.ReplicationFormats`, `internal/app/app.go`의 handler 맵에 `s3` 추가.
- proxy와 group 정의는 거부.
- `s3` 저장소 이름은 S3 bucket 규칙을 추가로 지킨다: 소문자·숫자·`-`·`.`, 3~63자, 영숫자로 시작·끝, IP 주소 형태 금지, `..`·`.-`·`-.` 금지.
- `/repository/<bucket>/<key>`는 읽기 전용(GET, HEAD, 디렉터리 목록). 쓰기는 S3 API로만 받고, 그 밖의 method는 405.

### 2. `internal/format/s3/` (새 패키지)

| 파일 | 역할 |
|---|---|
| `router.go` | path-style과 virtual-hosted에서 bucket과 key를 꺼내고, method와 query(`uploads`, `uploadId`, `partNumber`, `list-type`, `delete`, `location` 등)로 작업을 고름 |
| `object.go` | Put, Get, Head, Delete, Copy, 조건부 요청 |
| `list.go` | ListBuckets, ListObjectsV1·V2 |
| `multipart.go` | Multipart 전체 |
| `xml.go` | S3 XML 응답과 오류 |

저장은 기존 `repo.Store`(`WriteBlob`, `Put`, `Get`, `Open`, `Delete`, `Lock`, `Tx`)를 쓴다. 따라서 모든 blob store 종류(file, S3, Azure, GCS, group)와 저장소별 볼륨 분리가 그대로 적용된다.

### 3. `internal/auth/s3sig.go`, `internal/auth/s3key.go` (새 파일)

SigV4 검증과 키 조회·캐시. 결과는 기존 `Principal`이고 권한 검사는 `Authenticator.Can`을 쓴다.

### 4. 메타데이터 테이블

| 테이블 | 열 |
|---|---|
| `s3_keys` | `access_key_id`(PK), `owner`, `name`, `secret`(sealer로 암호화), `created_at`, `expires_at`(null 가능), `last_used_at` |
| `s3_object_meta` | `repo`, `path`(PK 쌍), `etag`, `headers`(JSON: `x-amz-meta-*`, Cache-Control, Content-Disposition, Content-Encoding). asset 삭제 시 함께 삭제 |
| `s3_uploads` | `upload_id`(PK), `repo`, `path`, `initiator`, `started_at`, `updated_at`, `content_type`, `headers`(JSON) |

- `assets(repo, path COLLATE "C")` 인덱스가 없으면 추가(목록을 바이트 순서로 읽기 위해).
- 스키마 마이그레이션은 기존 방식을 따르고, S3 저장소를 모르는 이전 버전이 열지 못하도록 구조 버전을 올리는지 구현 단계에서 확인한다.

### 5. 진입점

- `internal/server/server.go`의 `wrap`(요청 ID, 인증, 로그, 메트릭, idle timeout) 안에서 `Host`가 `s3.host` 또는 `*.s3.host`(virtualHost 켠 경우)면 S3 핸들러로 보낸다.
- `s3.listen`이 있으면 그 리스너의 모든 요청을 S3 핸들러로 보낸다.
- S3 요청은 서명으로 보호되므로 `crossOrigin`(CSRF) 검사에서 제외한다.

### 6. 설정

```yaml
s3:
  host: ""            # s3.cc.example.com. 비우면 호스트 기반 라우팅 끔
  virtualHost: false  # <bucket>.<host> 허용. 와일드카드 DNS와 인증서 필요
  listen: ""          # ":9000"이면 전용 포트
  region: us-east-1   # SigV4 검증 region
```

host와 listen이 모두 비어 있으면 S3 API는 꺼진다. `/repository/` 읽기는 그대로 동작한다.

## 요청 흐름

공통: `wrap` → S3 라우터 → SigV4 검증(서명이 없으면 익명) → `Can(p, bucket, 권한)` → 처리. 실패는 S3 XML 오류.

### Key 규칙

1~1024바이트 UTF-8. 제어 문자, `.`·`..` 경로 조각, 앞의 `/`는 거부(`/repository/` 경로 탐색 방지). 끝이 `/`인 0바이트 폴더 마커는 허용.

### PutObject (add, 덮어쓰기면 edit)

1. 본문을 store `tmp/`에 받으며 sha256·MD5·SHA1·SHA512 계산(`Store.WriteBlob`). `aws-chunked`는 chunk 서명이나 trailer 체크섬을 검증하며 푼다.
2. verify: `Content-MD5`, `x-amz-content-sha256`, `x-amz-checksum-*`이 계산값과 다르면 `BadDigest`. asset은 바뀌지 않는다.
3. key 잠금 `Store.Lock("s3:<bucket>/<key>")`(클러스터 잠금) → 조건 평가(`If-None-Match: *`이고 있으면 412, `If-Match` 불일치면 412).
4. 한 트랜잭션에서 asset과 `s3_object_meta` upsert.
5. `ETag` = MD5 hex.

### CopyObject (원본 read, 대상 add)

같은 blob store면 새 asset이 같은 sha256 blob을 가리킨다(바이트 복사 없음). store가 다르면 스트림 복사. `x-amz-metadata-directive`는 `COPY`(기본) 또는 `REPLACE`. 응답은 `CopyObjectResult` XML.

### GetObject / HeadObject (read, 익명은 anonymousRead)

기존 `repo.ServeAsset` 경로(Range 206, 조건부 요청, S3 store면 버킷 Range GET). `s3_object_meta`의 헤더와 ETag를 붙이고, presigned의 `response-*` query로 응답 헤더를 덮어쓴다. 다운로드 수와 마지막 다운로드 시각을 기존처럼 기록한다.

### DeleteObject / DeleteObjects (delete)

key 잠금 → asset·meta 삭제 → blob은 참조가 없어지면 compact가 지운다. 없는 key도 204. DeleteObjects는 key별 결과 XML(`Quiet` 지원).

### ListObjectsV2 / V1 (read)

`repo = ? AND path >= <start> AND path LIKE <prefix>% ORDER BY path COLLATE "C" LIMIT n`. delimiter가 있으면 CommonPrefixes로 접고, 접힌 prefix 아래는 다음 조회 시작점을 prefix의 바로 다음 값으로 옮겨 건너뛴다. `continuation-token`은 마지막 key의 base64, `max-keys` 상한 1000. V1은 `marker`/`NextMarker`.

### Multipart Upload

| 단계 | 처리 |
|---|---|
| Create (add) | `s3_uploads` 행, `tmp/mpu-<id>/` 생성. uploadId는 무작위 |
| UploadPart (add, initiator 본인 또는 admin) | `tmp/mpu-<id>/<n>.part`에 받으며 MD5·sha256 계산 후 원자적 rename. 같은 번호는 덮어씀. part 번호 1~10000. ETag = part MD5. `updated_at` 갱신 |
| Complete (add) | ① part 목록 검증(오름차순, ETag 일치, 마지막 외 5 MiB 이상) ② 200 헤더를 먼저 보내고 10초마다 공백 전송 ③ part를 이어 `WriteBlob` ④ key 잠금 → 조건 평가 → asset·meta 기록 ⑤ 결과나 오류를 응답 XML로 ⑥ tmp 디렉터리와 행 삭제 |
| Abort | 행과 디렉터리 삭제 |
| ListParts / ListMultipartUploads | 행과 디렉터리 기준 |

- Multipart ETag = `md5(각 part MD5 바이트를 이어 붙인 값)-N`, `s3_object_meta`에 저장.
- 최대 객체 크기 5 TiB. `maxUploadMiB`는 part와 단일 PUT 각각에 적용.
- 정리: `blobstore.delete-temp-files` 작업이 `updated_at`이 N일(기본 7일, `olderThanDays`로 조정) 넘은 업로드의 행과 `mpu-*` 디렉터리를 지운다. 서버 시작 때는 지우지 않는다.
- replica가 여럿이면 `tmp/`가 공유 볼륨이라 다른 노드에서 part를 이어 올리거나 Complete할 수 있다.

### 기존 기능과의 연결

- cleanup 정책(나이, 마지막 다운로드, 이름 패턴), 이벤트·웹훅, UI 둘러보기는 asset 단위로 그대로 동작.
- 복제, 내보내기·가져오기, store 이동(`repository.change-blobstore`)이 `s3_object_meta`를 함께 옮기도록 확인·보완한다.
- origin의 `feat/online-backup-nexus-import` 브랜치(온라인 백업, Nexus 가져오기)가 아직 main에 없다. 먼저 머지되면 백업 대상에 새 테이블 3개가 들어가는지 확인한다.

## 인증과 키 관리

### 키

- access key id: `CCK` + 대문자 base32 17자(20자).
- secret: 무작위 30바이트의 base64(40자). 발급할 때 한 번만 반환.
- secret은 sealer로 암호화해 저장하고, `commandcenter secret-key rotate` 대상에 포함.
- 사용자당 최대 10개.

### 검증

- 헤더 서명과 presigned(query) 서명. presigned `X-Amz-Expires`는 최대 7일이고 키 만료를 넘을 수 없다.
- 페이로드: 서명된 sha256, `UNSIGNED-PAYLOAD`, `STREAMING-AWS4-HMAC-SHA256-PAYLOAD`(chunk 서명), `STREAMING-UNSIGNED-PAYLOAD-TRAILER`(최신 aws cli·SDK의 HTTPS 기본값).
- service는 `s3`. region이 `s3.region`과 다르면 `AuthorizationHeaderMalformed`(올바른 region 포함).
- 시각 차이 15분 초과는 `RequestTimeTooSkewed`.
- 서명 비교는 `subtle.ConstantTimeCompare`.
- SigV2는 `AccessDenied`와 SigV4 안내.
- 서명은 클라이언트가 보낸 `Host`와 경로를 그대로 쓴다. 이를 바꾸는 프록시 rewrite는 지원하지 않는다(가이드에 명시).

### 주인과 권한

- 요청마다 주인을 확인: 비활성화·삭제된 사용자의 키는 즉시 `InvalidAccessKeyId`. LDAP·OIDC·SAML 사용자는 기존 `Lookup`.
- Principal은 `ViaToken=true`: 저장소 읽기·쓰기만 가능하고, 설정 변경이나 키 발급은 불가.
- 서명 실패는 로그인 실패 제한에 access key id로 집계. 초과 시 503 `SlowDown`.
- "저장소 접근에 사용자 토큰만" 설정에서도 S3 키는 토큰으로 인정.
- CreateBucket은 admin 또는 저장소 생성 권한(`CanApp(repositories, add)`)이 있어야 한다. 기본 blob store에 만든다.

### 캐시

- 노드 메모리에 (access key id → 복호화한 secret, 주인, 만료) 캐시.
- 발급·폐기 시 클러스터 `Notify`로 모든 노드의 캐시를 지운다. 폐기는 모든 노드 반영(최대 30초)을 기다린 뒤 응답.
- `last_used_at`은 다운로드 카운터처럼 주기적으로 몰아서 기록.

### API와 UI

| 요청 | 권한 | 동작 |
|---|---|---|
| `POST /-/api/s3keys` `{name, expiresAt?}` | 로그인 사용자 본인(토큰 불가) | `{accessKeyId, secretAccessKey}` 한 번 반환 |
| `GET /-/api/s3keys` | 본인 | 내 키 목록(secret 제외) |
| `DELETE /-/api/s3keys/{id}` | 본인 또는 admin | 폐기 |
| `GET /-/api/s3keys?all=1` | admin | 전체 목록(주인 포함) |

- 내 계정 화면에 "S3 액세스 키" 카드. 발급 직후 secret과 `aws configure`·`mc alias set` 예시를 한 번 표시.
- admin은 보안 메뉴에서 전체 목록과 폐기.
- 저장소 생성 화면에 `s3` 포맷 추가.
- 키 발급·폐기는 활동 로그에 기록(secret 제외). 사용자 삭제 시 그 사용자의 키도 삭제.

## 오류 처리

모든 오류는 `<Error><Code/><Message/><Resource/><RequestId/></Error>`. `RequestId`는 기존 요청 ID. HEAD는 본문 없이 상태 코드만.

| 상황 | 코드 |
|---|---|
| 서명 | `SignatureDoesNotMatch` 403, `InvalidAccessKeyId` 403, `ExpiredToken` 400, `RequestTimeTooSkewed` 403, `AuthorizationHeaderMalformed` 400 |
| 권한 | `AccessDenied` 403 |
| 대상 없음 | `NoSuchBucket` 404, `NoSuchKey` 404, `NoSuchUpload` 404 |
| 요청 | `InvalidPart`·`InvalidPartOrder`·`EntityTooSmall` 400, `BadDigest` 400, `InvalidArgument` 400, `InvalidBucketName` 400, `BucketNotEmpty` 409, `BucketAlreadyOwnedByYou` 409, `NotImplemented` 501 |
| 조건 | `PreconditionFailed` 412 |
| 용량 | `EntityTooLarge` 400, 디스크 부족·쿼터 위반 `InsufficientStorage` 507 |
| 상태 | 저장소 Offline·읽기 전용 모드 `ServiceUnavailable` 503 |

중간에 실패한 업로드는 tmp 파일만 남기고 asset·blob은 그대로다. 기존 `up-*` 정리가 tmp 파일을 지운다.

## 운영과 차트

```yaml
s3:
  enabled: false
  host: ""
  virtualHost: false
  port: 0             # 0이 아니면 Service에 s3 포트, config에 s3.listen
  region: us-east-1
  ingress:
    enabled: false    # 기존 ingress 설정을 따르고 host만 추가
    tls: []
```

- Ingress 주의(가이드와 values 주석): `proxy-body-size: 0`, Complete 대기를 고려한 긴 타임아웃, `Host`·경로 rewrite 금지, chunked 본문 통과.
- 메트릭: 기존 저장소별 메트릭 + `s3_signature_failures_total`, 진행 중인 multipart 수.
- 문서: README 포맷 표(S3: hosted ✅, aws cli·mc·boto3·rclone·terraform), `docs/guide.md` "S3 저장소" 장(설정, 키 발급, 클라이언트 예시, 제한 사항).
- Nexus 호환 API에서는 포맷 `s3`로만 보인다(Nexus에 대응 포맷 없음).

## 테스트

1. 단위: SigV4(AWS 공식 테스트 스위트 벡터, presigned, chunked, trailer CRC), 라우터(path-style·virtual-host·query 조합), bucket 이름과 key 규칙, ListV2 페이지네이션·delimiter 경계, multipart ETag.
2. 핸들러(PostgreSQL, 기존 `repotest` 패턴): Put·Get·Range·Copy·Delete·DeleteObjects·412, Multipart(정상, 순서 오류, 작은 part, Abort, 방치 정리), 권한(read·add·edit·delete·익명), 키 만료·폐기·주인 비활성화. blob store가 file일 때와 S3(`internal/blob/s3test`)일 때 모두.
3. 실제 클라이언트(`test/smoke/s3-smoke.sh`, `test/e2e.sh`의 `checks`에 추가):
   - aws cli v2: `cp`(작은 파일, 100 MB multipart), `sync`, `ls`, `rm --recursive`, `presign` + curl. trailer 체크섬 기본값 그대로.
   - mc: `mb`, `cp`, `mirror`, `ls`, `rm`, `cat`. path-style과 virtual-host.
   - boto3: `upload_file`, `generate_presigned_url`, `list_objects_v2` paginator.
   - rclone: `copy`, `check`.
   - terraform: S3 backend `use_lockfile = true`로 init, apply, 동시 lock 실패.
   - 전용 포트와 호스트 이름 양쪽에서 반복.
4. HA smoke: 노드 A에서 Create, B에서 UploadPart, C에서 Complete.
5. CI: 차트 `helm lint`, `helm template`, kubeconform에 `s3.enabled=true` 경우 추가.

## 작업 순서

1. 포맷 등록, 테이블, 키 관리(API, UI)
2. SigV4 검증(벡터 테스트부터)
3. 객체 Put·Get·Head·Delete·List
4. Multipart
5. Copy, 조건부 요청, presigned, chunked·trailer
6. 라우팅(host, 포트), 차트
7. 실제 클라이언트 E2E, 문서
