# Command Center 운영 가이드

> 첫 화면과 소개는 [README](../README.md)에 있습니다. 이 문서는 설치, 프로덕션 배포, 설정, 포맷별 클라이언트 설정, 운영을 자세히 다룹니다.

## 소개

Command Center는 저메모리 패키지 저장소입니다. 에디션은 하나뿐인 오픈소스입니다. Go 단일 바이너리이며 Kubernetes(Helm 차트)나 Docker로 실행하고, 설정 파일은 `config.yaml` 하나입니다.
업계에서 널리 쓰이는 `/repository/<저장소이름>/...` 경로 규칙을 따르므로, 이미 이 규칙으로 설정한 클라이언트는 호스트만 바꾸면 됩니다.

지원 포맷과 저장소 유형:

| 포맷 | hosted (업로드) | proxy (원격 캐시) | group (묶음) |
|---|---|---|---|
| Maven | O | O | O |
| npm | O | O | O |
| PyPI | O | O | O |
| Go | O | O | O |
| Cargo | O | O | O |
| Docker / OCI | O | O | O |
| raw | O | O | O |
| Helm | O | O | O |
| NuGet | O | O | O |
| APT | O | O | O |
| Yum / DNF | O | O | O |
| RubyGems | O | O | O |
| Composer | O | O | O |
| Conda | O | O | O |
| Conan | O | O | O |
| R | O | O | O |
| Git LFS | O | - | - |
| p2 | - | O | O |
| Swift | O | O | O |
| CocoaPods | - | O | - |
| HuggingFace | - | O | - |
| Terraform | O | O | O |

- **hosted**: 직접 업로드한 패키지를 저장합니다.
- **proxy**: 원격 저장소(Maven Central, npmjs, pypi.org, proxy.golang.org, crates.io 등)를 처음 요청할 때 가져와 캐시합니다.
- **group**: 여러 저장소를 하나의 URL로 묶어 읽습니다. `writeMember`를 지정하면 업로드를 그 hosted 멤버로 보냅니다.

메모리 사용량(`test/e2e.sh`로 측정):

- 포맷별 스모크 테스트(`docker stats`): Maven 22 MiB, npm 8.4 MiB, PyPI 21 MiB, Go 11 MiB, Cargo 9.9 MiB, UI 7.1 MiB
- 부하 테스트(`test/load/load.sh`): 128 MiB 컨테이너에서 100 MiB 파일 50개 동시 다운로드, 5개 동시 업로드, 적대적 업로드를 처리하는 동안 최대 RSS 53 MiB 이하

## 빠른 시작

Kubernetes에는 Helm 차트로 설치합니다("Kubernetes" 참고). 여기서는 Docker compose로 한 대에 띄우며, compose 파일은 릴리스 이미지 `ghcr.io/sungjinseo/commandcenter`를 씁니다. 아래의 `1.2.3`과 `v1.2.3`은 쓸 [릴리스](https://github.com/sungjinseo/commandcenter/releases)의 버전으로 바꾸세요.

1. 쓸 버전의 태그로 Git 저장소를 받습니다. `docker-compose.yml`과 `config.example.yaml`도 그 버전의 것이어야 합니다(설정 파일에 모르는 키가 있으면 서버가 시작하지 않습니다).

   ```bash
   git clone --depth 1 --branch v1.2.3 https://github.com/sungjinseo/commandcenter.git
   cd commandcenter
   ```

2. `docker-compose.yml`과 같은 폴더에 `.env` 파일을 만들고 쓸 버전과 데이터베이스 비밀번호를 적습니다. 이미지 태그에는 `v`가 없습니다. compose는 모든 명령에서 이 파일을 읽고, 둘 중 하나라도 없으면 시작하지 않습니다.

   ```
   COMMANDCENTER_VERSION=1.2.3
   POSTGRES_PASSWORD=<영문자와 숫자로 된 긴 임의 문자열>
   ```

   `POSTGRES_PASSWORD`는 compose가 함께 띄우는 PostgreSQL(`postgres:17-alpine`)의 비밀번호입니다. URL에 그대로 들어가므로 영문자와 숫자만 쓰세요. 예: `echo POSTGRES_PASSWORD=$(head -c 24 /dev/urandom | od -An -tx1 | tr -d ' \n') >> .env`. PostgreSQL은 처음 시작할 때만 이 값을 읽으므로, 나중에 바꾸려면 DB에서 `ALTER ROLE`로도 바꿔야 합니다.

   **소스에서 빌드**하려면 이 checkout을 빌드하고 `.env`에 그 이미지를 적습니다. 빌드 인자 `VERSION`이 `commandcenter version`과 UI에 나오는 버전이 됩니다(지정하지 않으면 `dev`).

   ```bash
   docker build --build-arg VERSION=dev -t commandcenter:dev .
   ```

   ```
   COMMANDCENTER_IMAGE=commandcenter
   COMMANDCENTER_VERSION=dev
   ```

3. 사용자 비밀번호 해시를 만듭니다. 명령을 실행한 뒤 비밀번호(8자 이상, 72바이트 이하)를 입력하고 Enter를 누르세요. 직접 빌드했다면 이미지 이름은 `commandcenter:dev`입니다.

   ```bash
   docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password
   ```

4. `config.example.yaml`을 `config.yaml`로 복사하고 `passwordHash` 자리에 3번의 출력값을 넣습니다.

   ```bash
   # Git Bash / macOS / Linux
   cp config.example.yaml config.yaml
   ```

   ```powershell
   # PowerShell
   Copy-Item config.example.yaml config.yaml
   ```

   > `docker compose up` 전에 `config.yaml`이 반드시 존재해야 합니다. 없으면 Docker가 같은 이름의 디렉터리를 만들어 서버 시작에 실패합니다.

   > compose 파일이 `COMMANDCENTER_DATABASE_URL`로 DB 주소를 넘기므로 `config.yaml`의 `database.url`은 비워 둡니다. 둘 다 있으면 서버가 시작하지 않습니다.

5. 실행합니다.

   ```bash
   docker compose up -d
   ```

   `docker compose up -d`는 PostgreSQL(`commandcenter-postgres`)을 먼저 띄우고 준비되면 Command Center를 띄웁니다. 메타데이터는 볼륨 `commandcenter-postgres`(이름은 `COMMANDCENTER_DB_VOLUME`)에, blob은 볼륨 `commandcenter-data`(이름은 `COMMANDCENTER_VOLUME`)에 있습니다.

   상태 확인: `http://localhost:8081/service/rest/v1/status` 가 200을 반환하면 정상입니다.

## Kubernetes

Kubernetes에는 Helm 차트로 설치합니다. 차트는 기본으로 서버 Pod 하나(StatefulSet), `/data`의 ReadWriteOnce 볼륨, 메타데이터를 둘 PostgreSQL Pod 하나(StatefulSet), `config.yaml`을 담은 Secret, Service, 그리고 켜면 Ingress를 만듭니다. `replicaCount`를 2 이상으로 두면 PostgreSQL과 ReadWriteMany 볼륨 하나를 함께 쓰는 Deployment로 여러 Pod를 띄웁니다("고가용성(여러 노드)" 참고). 차트는 릴리스마다 `oci://ghcr.io/sungjinseo/charts/commandcenter`에 올라갑니다. 차트 버전은 릴리스 버전(`v1.2.3`이면 `1.2.3`)과 같고, 기본 이미지도 같은 버전의 `ghcr.io/sungjinseo/commandcenter`입니다. OCI 레지스트리의 차트를 받으려면 Helm 3.8 이상이 필요합니다. 차트의 원본은 [`deploy/helm/commandcenter`](../deploy/helm/commandcenter)이고, Helm 없이 설치하려면 "참조 매니페스트"를 보세요.

이 절의 명령은 네임스페이스와 릴리스 이름으로 모두 `commandcenter`를 씁니다. 그러면 StatefulSet과 Service는 `commandcenter`, Pod는 `commandcenter-0`, PVC는 `data-commandcenter-0`, 설정 Secret은 `commandcenter-config`입니다. 차트가 띄우는 PostgreSQL은 StatefulSet `commandcenter-postgresql`, Pod `commandcenter-postgresql-0`, PVC `data-commandcenter-postgresql-0`, 비밀번호 Secret `commandcenter-postgresql`입니다. 릴리스 이름에 `commandcenter`가 들어 있지 않으면 이름이 `<릴리스 이름>-commandcenter`가 됩니다.

### Helm 차트로 설치

1. 관리자 비밀번호의 해시를 만듭니다. 명령을 실행한 뒤 비밀번호(8자 이상, 72바이트 이하)를 입력하고 Enter를 누르세요. Docker가 없으면 GitHub Release의 linux 바이너리로 `commandcenter hash-password`를 실행해도 됩니다.

   ```bash
   docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password
   ```

2. 네임스페이스와 비밀값 키 Secret을 만듭니다. 서버는 이 키로 SMTP 비밀번호, LDAP bind 비밀번호, proxy `remoteAuth`를 암호화해 저장합니다("저장된 비밀값 암호화" 참고). 마지막 명령으로 키 값을 꺼내 볼륨 백업과 **다른 곳**에 보관하세요. 키를 잃으면 서버가 시작하지 않고 저장된 비밀값은 되살릴 수 없습니다. 이 Secret은 Helm이 만든 것이 아니라서 `helm uninstall`로는 지워지지 않지만, 네임스페이스를 지우면 함께 지워집니다.

   ```bash
   kubectl create namespace commandcenter
   kubectl -n commandcenter create secret generic commandcenter-secret-key \
     --from-literal=key="$(head -c 32 /dev/urandom | base64)"
   kubectl -n commandcenter get secret commandcenter-secret-key -o jsonpath='{.data.key}' | base64 -d
   ```

3. 값 파일(예: `commandcenter-values.yaml`)을 만듭니다. 앞의 세 값은 반드시 정합니다("차트 값" 참고). 관리자 비밀번호의 해시가 들어 있으니 비밀번호처럼 다루고, 업그레이드할 때도 이 파일을 다시 씁니다.

   ```yaml
   config:
     baseURL: https://repo.example.com          # 클라이언트가 접속하는 주소
     adminPasswordHash: '<1번의 출력>'            # 작은따옴표로 감쌉니다
   postgresql:
     persistence:
       storageClass: <블록 스토리지 StorageClass>  # 차트가 띄우는 PostgreSQL의 볼륨. NFS·SMB는 안 됩니다
   secretKey:
     existingSecret: commandcenter-secret-key
   ingress:
     enabled: true
     className: nginx
     annotations:                               # "Ingress와 큰 업로드" 참고
       nginx.ingress.kubernetes.io/proxy-body-size: "0"
       nginx.ingress.kubernetes.io/proxy-request-buffering: "off"
       nginx.ingress.kubernetes.io/proxy-read-timeout: "300"
       nginx.ingress.kubernetes.io/proxy-send-timeout: "300"
     hosts:
       - host: repo.example.com
         paths: [/]
     tls:
       - secretName: repo-example-com-tls       # 인증서 Secret(cert-manager 등으로 만듭니다)
         hosts: [repo.example.com]
   ```

4. 설치하고 테스트합니다. `--wait`는 Pod가 준비될 때까지 기다립니다.

   ```bash
   helm install commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3 \
     -n commandcenter -f commandcenter-values.yaml --wait
   helm test commandcenter -n commandcenter
   ```

   `https://repo.example.com/`에서 `admin`과 1번의 비밀번호로 로그인합니다. 클라이언트도 이 주소를 씁니다.

### 차트 값

모든 값은 차트의 `values.yaml`에 주석과 함께 있습니다(`helm show values oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3`).

- **`config.baseURL`(필수)**: 클라이언트가 접속하는 주소입니다. TLS를 Ingress에서 끝내면 그 `https://` 주소입니다("TLS와 리버스 프록시" 참고).
- **`config.adminPasswordHash`(필수)**: `admin` 사용자의 bcrypt 해시(`hash-password`의 출력)입니다. 필수 값이 비어 있으면 차트가 설치를 거부합니다.
- **`replicaCount`(기본 `1`)**: 1이면 지금과 같은 StatefulSet(Pod 하나)을 만듭니다. 2 이상이면 Deployment(롤링 업데이트는 새 Pod를 먼저 띄우고 이전 Pod를 하나씩 멈춥니다. `maxSurge: 1`, `maxUnavailable: 0`), 모든 Pod가 마운트하는 ReadWriteMany PVC 하나(`<이름>-data`. `helm.sh/resource-policy: keep`이라 `helm uninstall`로 지워지지 않습니다. `persistence.existingClaim`을 주면 그 PVC), PodDisruptionBudget(`maxUnavailable: 1`), 그리고 `affinity`가 비어 있으면 Pod를 서로 다른 노드에 두려는 preferred podAntiAffinity(`kubernetes.io/hostname`)를 만듭니다. 2 이상인데 `persistence.accessModes: [ReadWriteMany]`(또는 `persistence.existingClaim`)가 없으면 차트가 설치를 거부합니다.
- **`postgresql.*`(기본: 차트가 띄움)**: `database.existingSecret`이 비어 있으면 차트가 PostgreSQL(`postgres:17-alpine`) Pod 하나를 StatefulSet `<이름>-postgresql`로 띄우고, 그 볼륨(`postgresql.persistence`, 기본 `8Gi`)에 메타데이터를 둡니다. 비밀번호는 설치할 때 만들어 Secret `<이름>-postgresql`(키 `password`, 서버가 읽는 URL은 키 `url`)에 두고 업그레이드해도 그대로 씁니다. 이 Secret과 볼륨은 `helm uninstall` 뒤에도 남습니다. Pod 하나라 고가용성은 아닙니다. OpenShift에서는 `postgresql.podSecurityContext`의 `runAsUser`, `runAsGroup`, `fsGroup`을 `null`로 둡니다.
- **`postgresql.persistence.storageClass`(반드시 정하기)**: 차트가 띄우는 PostgreSQL 볼륨의 StorageClass입니다. **블록 스토리지만 됩니다.** 비워 두면 클러스터의 기본 StorageClass를 쓰는데 그것이 NFS인 클러스터도 있으니 이름을 직접 적으세요. `kubectl get storageclass`의 PROVISIONER를 보고 고릅니다.
  - 됩니다: 클라우드 블록 디스크(AWS EBS `ebs.csi.aws.com`, GCE PD `pd.csi.storage.gke.io`, Azure Disk `disk.csi.azure.com`, NKS의 `nks-block-storage`), Ceph RBD, Longhorn, iSCSI, 로컬 볼륨
  - 안 됩니다: NFS(`nfs.csi.k8s.io`, nfs-subdir-external-provisioner), NAS 상품, AWS EFS, Azure Files, GKE Filestore, CephFS
  - `-`를 적으면 `storageClassName: ""`이 되어 미리 만들어 둔 PV에 묶입니다.
- **`database.existingSecret`, `database.key`(기본 `url`)**: 직접 운영하거나 관리형으로 쓰는 PostgreSQL의 URL이 든 Secret입니다. 지정하면 차트의 PostgreSQL은 만들지 않습니다("메타데이터 데이터베이스" 참고). `config.existingSecret`의 `config.yaml`에 `database.url`을 넣었다면 차트가 알 수 없으므로 `postgresql.enabled: false`로 둡니다. `database.maxConns`는 Pod당 연결 풀 크기입니다(0이면 서버 기본값 10).
- **`persistence.storageClass`**: `/data` 볼륨(default blob store와 업로드 임시 파일)의 StorageClass입니다. 블록 스토리지와 NFS 모두 됩니다. `replicaCount`가 2 이상이면 ReadWriteMany 클래스(NFS, CephFS, EFS 등)여야 합니다. `-`를 적으면 `storageClassName: ""`이 되어 미리 만들어 둔 PV에 묶입니다.
- **첫 시작에만 쓰는 값**: `config`의 `baseURL`, `adminPasswordHash`, `anonymousRead`, `repositories`, 그리고 `config.extra`에 넣은 `users`, `roles`, `maxUploadMiB`는 처음 시작할 때 한 번만 DB로 가져옵니다("설정 레퍼런스" 참고). 설치한 뒤에는 웹 UI에서 바꾸세요. 값 파일에서 바꿔 `helm upgrade`를 해도 Pod만 다시 시작되고 반영되지 않습니다.
- `secretKey.existingSecret`, `secretKey.key`(기본 `key`): 비밀값 키가 든 Secret의 이름과 그 안의 키 이름입니다. 서버에는 환경변수 `COMMANDCENTER_SECRET_KEY`로 들어갑니다. 비워 두면 비밀값을 평문으로 저장합니다.
- `ingress.enabled`, `ingress.className`, `ingress.annotations`, `ingress.hosts`, `ingress.tls`: "Ingress와 큰 업로드" 참고.
- `persistence.size`(기본 `50Gi`): blob이 들어갑니다. proxy 캐시는 Cleanup 정책이 지울 때까지 계속 커집니다. `persistence`의 값은 설치한 뒤 `helm upgrade`로 바꿀 수 없습니다("차트 업그레이드" 참고).
- `config.anonymousRead`(기본 `false`), `config.repositories`(첫 시작에 만들 저장소, `config.example.yaml`의 형식), `config.extra`(그 밖의 `config.yaml` 최상위 키. `users` 목록을 넣으면 `admin` 사용자 대신 그 목록을 씁니다), `config.existingSecret`(직접 만든 Secret의 `config.yaml` 키를 그대로 씁니다. 이때 다른 `config` 값은 비워 둡니다).
- `image.repository`, `image.tag`(기본은 차트의 appVersion), `imagePullSecrets`: 사내 레지스트리로 옮긴 이미지나 직접 빌드한 이미지를 쓸 때 바꿉니다. 태그는 버전으로 고정하고 `latest`는 쓰지 마세요.
- `resources`(기본: 메모리 요청과 한도 `128Mi`, CPU 요청 `100m`, CPU 한도 없음), `goMemLimit`(기본 `80MiB`): 메모리 한도를 올리면 `goMemLimit`도 한도의 60~65 % 정도로 함께 올립니다("리소스", "메모리 설정" 참고).
- `proxy.http`, `proxy.https`, `proxy.noProxy`: proxy 저장소가 사내 프록시를 거쳐 원격에 접속할 때 씁니다(`HTTP_PROXY`, `HTTPS_PROXY`, `NO_PROXY`). 클러스터 안의 원격은 `proxy.noProxy`에 넣으세요(예: `.svc,.cluster.local`).
- `extraEnv`: 예약 작업의 시간대(`TZ`, "예약 작업" 참고) 같은 환경변수입니다. 예: `extraEnv: [{name: TZ, value: Asia/Seoul}]`
- `extraVolumes`, `extraVolumeMounts`: 이름 붙은 blob store를 둘 볼륨입니다. blob은 NFS에 둘 수 있습니다("저장 공간과 인스턴스" 참고).
- `terminationGracePeriodSeconds`(기본 60), `startupProbe`, `readinessProbe`, `livenessProbe`, `podSecurityContext`, `securityContext`: 바꿀 일이 거의 없습니다("동작 방식" 참고).
- 그 밖에 `service`, `serviceAccount`, `nodeSelector`, `tolerations`, `affinity`, `priorityClassName`, `podAnnotations`, `podLabels`, `nameOverride`, `fullnameOverride`가 있습니다. 릴리스 이름과 `nameOverride`·`fullnameOverride`는 처음 설치할 때 정합니다. 바꾸면 빈 볼륨을 가진 새 StatefulSet이 생깁니다. Pod 수는 `replicaCount`로 정합니다.

### Ingress와 큰 업로드

서버는 평문 HTTP만 받으므로 TLS는 Ingress(또는 Gateway)에서 끝내고, `config.baseURL`을 그 `https://` 주소와 똑같이 둡니다("TLS와 리버스 프록시" 참고). 컨트롤러의 기본값은 패키지 업로드에 맞지 않습니다. ingress-nginx는 기본으로 요청 본문을 1 MiB까지만 받아 큰 jar, tarball, wheel, crate 업로드가 413으로 끊기고, 서버의 응답을 60초까지만 기다립니다. ingress-nginx라면 `ingress.annotations`에 다음을 넣으세요(위 값 파일의 예).

| 어노테이션 | 값 | 뜻 |
|---|---|---|
| `nginx.ingress.kubernetes.io/proxy-body-size` | `"0"` | 본문 크기를 Ingress에서 보지 않고 서버의 `maxUploadMiB`(기본 1024 MiB, 넘으면 413)에 맡깁니다. Ingress에서도 막으려면 `maxUploadMiB` 이상(예: `"1024m"`)으로 둡니다. |
| `nginx.ingress.kubernetes.io/proxy-request-buffering` | `"off"` | 업로드를 컨트롤러에 다 받아 두지 않고 바로 서버로 넘깁니다. 켜 두면 업로드마다 그 크기만큼의 임시 디스크가 컨트롤러 Pod에 필요하므로, 크기 제한을 `"0"`으로 둘 때는 꼭 끕니다. |
| `nginx.ingress.kubernetes.io/proxy-read-timeout` | `"300"` | 서버의 응답을 기다리는 시간(초). proxy 저장소가 원격에서 처음 받아 오는 요청은 응답이 늦을 수 있습니다. |
| `nginx.ingress.kubernetes.io/proxy-send-timeout` | `"300"` | 서버가 요청 본문을 받아 가기를 기다리는 시간(초). |

- 본문 수신 타임아웃과 클라이언트 IP당 연결 수("TLS와 리버스 프록시"의 "프록시에서 제한할 것")는 ingress-nginx ConfigMap의 `client-body-timeout`(기본 60초)과 어노테이션 `nginx.ingress.kubernetes.io/limit-connections`로 겁니다. 연결 수 제한은 컨트롤러가 실제 클라이언트 주소를 볼 때만 쓰세요. 로드밸런서의 주소로 보이면 모든 클라이언트가 한 주소로 묶여 제한됩니다.
- Gateway API(HTTPRoute)를 쓰면 `ingress.enabled: false`로 두고 `httpRoute.enabled: true`, `httpRoute.parentRefs`(붙을 Gateway, 예: `[{name: traefik-gateway, namespace: traefik}]`), `httpRoute.hostnames`를 정합니다. 차트가 Service의 8081 포트로 가는 HTTPRoute를 만듭니다. 본문 크기와 타임아웃은 Gateway 구현에서 정합니다. 예를 들어 Traefik v3는 요청 전체를 읽는 시간(entryPoint의 `transport.respondingTimeouts.readTimeout`)이 기본 60초라서 그보다 오래 걸리는 업로드가 끊기므로 늘려야 합니다.

### 설치 확인(helm test)

```bash
helm test commandcenter -n commandcenter
```

테스트 Pod(`commandcenter-test-status`)가 서버 이미지의 `commandcenter healthcheck -url`로 Service를 거쳐 `GET /service/rest/v1/status`를 부르고, 200이면 성공합니다. 서버 이미지만 쓰므로 폐쇄망에서도 다른 이미지가 필요 없습니다. 성공한 테스트 Pod는 지워지고 실패한 Pod는 남으므로 `kubectl -n commandcenter logs commandcenter-test-status`로 원인을 봅니다. HTTP 서버가 Service 뒤에서 응답하는지만 보며, DB와 디스크는 관리자 화면의 시스템 상태에서 확인합니다.

### 차트 업그레이드

1. 올릴 버전의 [CHANGELOG](../CHANGELOG.md)를 읽습니다.
2. 백업합니다("Kubernetes에서 백업" 참고). 업그레이드한 뒤 되돌릴 수 있는 방법은 이 백업뿐입니다.
3. 설치 때의 값 파일로 새 차트 버전을 적용하고 테스트합니다.

   ```bash
   helm upgrade commandcenter oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.4 \
     -n commandcenter -f commandcenter-values.yaml --wait
   helm test commandcenter -n commandcenter
   ```

- StatefulSet은 이전 Pod가 끝난 뒤에(최대 60초, "정상 종료" 참고) 새 Pod를 띄우므로 그사이 서비스가 잠시 멈춥니다. 새 버전은 처음 시작할 때 DB 스키마를 새 구조로 바꾸며, startupProbe가 5분까지 기다립니다.
- `replicaCount`가 2 이상이면 Deployment가 새 Pod를 먼저 띄우고 이전 Pod를 하나씩 멈추므로 서비스가 멈추지 않습니다. 새 버전이 이전 버전과 함께 돌 수 없는 DB 변경을 하면(CHANGELOG의 Upgrade notes가 compat을 알려 줍니다) 새 Pod는 이유를 로그에 남기고 시작을 거부하며, 이전 Pod가 계속 요청을 받는 채로 롤아웃이 멈춥니다. 그런 버전은 `kubectl -n commandcenter scale deployment commandcenter --replicas=0`으로 모두 멈춘 뒤 올립니다.
- 이미지는 차트 버전을 따라갑니다. `image.tag`를 정해 두었다면 함께 바꾸세요.
- `persistence`와 `postgresql.persistence`의 값(`storageClass`, `size`, `accessModes`)은 바꾸지 않습니다. StatefulSet의 볼륨 템플릿은 만든 뒤 바꿀 수 없어서 `helm upgrade`가 `Forbidden: updates to statefulset spec for fields other than …` 오류로 실패합니다. 볼륨을 늘리려면 StorageClass가 `allowVolumeExpansion: true`일 때 PVC를 직접 늘리고, 값 파일의 `size`는 그대로 둡니다.

  ```bash
  kubectl -n commandcenter patch pvc data-commandcenter-0 -p '{"spec":{"resources":{"requests":{"storage":"100Gi"}}}}'
  ```

- **버전을 올린 업그레이드를 `helm rollback`으로 되돌리지 마세요.** 다운그레이드는 지원하지 않습니다("업그레이드" 참고). 되돌려야 하면 "Kubernetes에서 백업"의 복원으로 업그레이드 전의 백업을 되살리되, 마지막 `--replicas=1` 대신 이전 차트 버전으로 `helm upgrade`를 실행합니다. 차트가 Pod를 이전 이미지로 다시 띄웁니다. 값만 바꾼 리비전은 `helm rollback`해도 됩니다.

### Kubernetes에서 백업

메타데이터는 `pg_dump`로, blob은 볼륨 스냅샷으로 받습니다. 순서는 `pg_dump` 먼저, blob 나중입니다(blob은 내용 주소 방식이라 덤프 뒤에 생긴 blob이 섞여도 무해합니다. "백업과 복원" 참고). 그러면 Pod를 멈추지 않아도 됩니다. 아래는 차트가 띄운 PostgreSQL과 CSI 볼륨 스냅샷(`kubectl get volumesnapshotclass`)을 쓰는 예입니다. 외부 PostgreSQL(`database.existingSecret`)이라면 덤프는 그 서버에서 받습니다(관리형 서비스라면 그 스냅샷도 됩니다).

```bash
NS=commandcenter SNAP=commandcenter-$(date +%Y%m%d-%H%M)
kubectl -n $NS exec commandcenter-postgresql-0 -- pg_dump -U commandcenter -Fc commandcenter > $SNAP.dump
kubectl -n $NS apply -f - <<EOF
apiVersion: snapshot.storage.k8s.io/v1
kind: VolumeSnapshot
metadata:
  name: $SNAP
spec:
  volumeSnapshotClassName: <VolumeSnapshotClass>   # 기본 클래스를 쓰면 이 줄을 지웁니다
  source:
    persistentVolumeClaimName: data-commandcenter-0
EOF
kubectl -n $NS wait volumesnapshot/$SNAP --for=jsonpath='{.status.readyToUse}'=true --timeout=30m
```

- 백업하는 동안에는 compact 작업(`commandcenter gc` 포함)이 돌지 않게 하세요("백업과 복원" 참고).
- `extraVolumes`에 둔 blob store가 있으면 덤프 뒤에 그 볼륨도 찍습니다.
- VolumeSnapshot은 네임스페이스에 속합니다. 네임스페이스를 지우면 함께 지워지고, VolumeSnapshotClass의 `deletionPolicy`가 `Delete`이면 스토리지의 스냅샷도 사라집니다. 오래 둘 백업은 `Retain` 클래스로 찍거나 클러스터 밖에도 두세요.
- 비밀값 키 Secret과 값 파일은 덤프와 스냅샷에 들어가지 않습니다. 따로 보관하되 키는 덤프·스냅샷과 다른 곳에 둡니다.

**복원**: 서버를 0으로 줄이고(`kubectl -n $NS scale statefulset commandcenter --replicas=0`), `kubectl -n $NS exec -i commandcenter-postgresql-0 -- pg_restore -U commandcenter --clean --if-exists --no-owner -d commandcenter < <덤프>`로 되돌린 뒤 blob 볼륨을 같은 시점의 스냅샷으로 되돌리고 다시 1로 늘립니다. blob 볼륨은 PVC를 스냅샷에서 다시 만들며, StatefulSet은 이름이 같은 PVC를 그대로 씁니다. 지금 데이터는 지워지므로 필요하면 먼저 지금 상태도 백업하세요.

```bash
kubectl -n $NS scale statefulset commandcenter --replicas=0
kubectl -n $NS wait --for=delete pod/commandcenter-0 --timeout=120s
kubectl -n $NS exec -i commandcenter-postgresql-0 -- pg_restore -U commandcenter --clean --if-exists --no-owner -d commandcenter < $SNAP.dump
kubectl -n $NS delete pvc data-commandcenter-0
kubectl -n $NS apply -f - <<EOF
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: data-commandcenter-0
spec:
  storageClassName: <persistence.storageClass와 같은 값>
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 50Gi            # 스냅샷의 RESTORESIZE(kubectl get volumesnapshot) 이상
  dataSource:
    apiGroup: snapshot.storage.k8s.io
    kind: VolumeSnapshot
    name: $SNAP
EOF
kubectl -n $NS scale statefulset commandcenter --replicas=1
```

**스냅샷을 쓸 수 없을 때**: 덤프를 받은 뒤 blob 볼륨을 "서버를 멈춰야 하는 작업"의 일회용 Pod에서 복사합니다. 복원은 덤프를 되돌린 뒤 볼륨을 비우고 풉니다.

```bash
kubectl -n $NS exec commandcenter-maint -- tar czf - -C / data > data.tgz                                            # 백업
kubectl -n $NS exec -i commandcenter-maint -- sh -c 'find /data -mindepth 1 -delete && tar xzf - -C /' < data.tgz   # 복원
```

### 서버를 멈춰야 하는 작업

스냅샷 없이 하는 blob 볼륨의 백업·복원과 비밀값 키 교체("저장된 비밀값 암호화" 참고)는 서버 Pod를 내리고, 같은 PVC를 마운트해 서버와 같은 UID 65532로 도는 일회용 Pod에서 합니다. 다른 UID로 돌리면 그 사용자 소유의 파일이 볼륨에 남아 서버가 열지 못할 수 있습니다.

```bash
kubectl -n commandcenter scale statefulset commandcenter --replicas=0
kubectl -n commandcenter wait --for=delete pod/commandcenter-0 --timeout=120s
kubectl -n commandcenter apply -f - <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: commandcenter-maint
spec:
  restartPolicy: Never
  securityContext: {runAsNonRoot: true, runAsUser: 65532, runAsGroup: 65532, fsGroup: 65532, seccompProfile: {type: RuntimeDefault}}
  containers:
    - name: maint
      image: busybox:1.37
      command: ["sleep", "86400"]
      securityContext: {allowPrivilegeEscalation: false, capabilities: {drop: [ALL]}}
      volumeMounts: [{name: data, mountPath: /data}]
  volumes:
    - name: data
      persistentVolumeClaim: {claimName: data-commandcenter-0}
EOF
kubectl -n commandcenter wait --for=condition=Ready pod/commandcenter-maint
# 작업: kubectl -n commandcenter exec -it commandcenter-maint -- sh
kubectl -n commandcenter delete pod commandcenter-maint
kubectl -n commandcenter scale statefulset commandcenter --replicas=1
```

서버의 명령(`secret-key rotate` 등)은 busybox 대신 지금 버전의 서버 이미지로 돌립니다. `command` 대신 `args`에 명령을 주고, 설정 Secret `commandcenter-config`를 `/etc/commandcenter`에 마운트하며, 지금 키를 `COMMANDCENTER_SECRET_KEY`(Secret 참조)로, DB URL을 `COMMANDCENTER_DATABASE_URL`(차트라면 Secret `commandcenter-postgresql`의 키 `url`, 외부 DB라면 `database.existingSecret`)로 줍니다. 명령이 끝나면 Pod가 멈추므로 출력은 `kubectl -n commandcenter logs commandcenter-maint`로 봅니다.

### 삭제(helm uninstall)

```bash
helm uninstall commandcenter -n commandcenter
```

StatefulSet, Service, Ingress, 설정 Secret은 지워지지만 **PVC `data-commandcenter-0`, `data-commandcenter-postgresql-0`와 데이터는 남습니다.** StatefulSet이 만든 PVC는 Helm 릴리스에 속하지 않기 때문입니다. 차트가 띄운 PostgreSQL의 비밀번호 Secret `commandcenter-postgresql`도 남습니다. 같은 네임스페이스에 같은 릴리스 이름으로 다시 설치하면 이 PVC를 그대로 쓰고, `config`의 첫 시작 값은 다시 가져오지 않습니다. 직접 만든 비밀값 키 Secret도 남습니다(스냅샷을 보관한다면 키도 보관하세요). 데이터까지 지우려면 PVC와 비밀번호 Secret을 지웁니다. 되돌릴 수 없고, StorageClass의 `reclaimPolicy`가 `Delete`이면 디스크도 지워집니다.

```bash
kubectl -n commandcenter delete pvc data-commandcenter-0 data-commandcenter-postgresql-0
kubectl -n commandcenter delete secret commandcenter-postgresql
```

### 동작 방식

- **StatefulSet, Pod 하나(기본)**: StatefulSet은 이전 Pod가 완전히 끝난 뒤에 새 Pod를 띄우므로 업데이트 중에도 한 볼륨을 두 Pod가 마운트하지 않습니다. StatefulSet의 `replicas`를 직접 늘리면 Pod마다 빈 볼륨이 따로 생겨 데이터가 서로 다른 서버가 여러 대 생기므로, 여러 대는 `replicaCount`로 띄웁니다. 그러면 StatefulSet 대신 Deployment가 되어 모든 Pod가 같은 PostgreSQL DB와 같은 ReadWriteMany 볼륨을 씁니다("고가용성(여러 노드)" 참고).
- **볼륨**: `/data`는 ReadWriteOnce PVC(`data-commandcenter-0`, 기본 50Gi)입니다. 메타데이터는 차트가 띄운 PostgreSQL의 PVC(`data-commandcenter-postgresql-0`, 기본 8Gi)에 있고, 이 볼륨은 블록 스토리지여야 합니다("차트 값"의 `postgresql.persistence.storageClass`). `replicaCount`가 2 이상이면 모든 Pod가 함께 마운트하는 ReadWriteMany PVC(`commandcenter-data`)입니다.
- **노드 장애**: Pod가 `Terminating`이나 `Unknown`에 머물러도 `kubectl delete pod --force --grace-period=0`으로 지우지 마세요. 이전 노드에서 프로세스가 아직 돌고 있으면 같은 볼륨을 두 프로세스가 쓰게 될 수 있습니다. `replicaCount`가 2 이상이면 다른 Pod가 계속 요청을 받으므로 그대로 기다리면 됩니다.
- **프로브**: startupProbe는 `GET /service/rest/v1/status`(인증 없이 200)로 HTTP 서버가 응답하는지만 봅니다. readinessProbe는 `GET /service/rest/v1/status/writable`로, DB가 응답하고 노드가 종료 중이 아니며 스키마가 맞고 잠금·알림 연결이 살아 있을 때만 200입니다("노드와 준비 상태" 참고. 참조 매니페스트도 같습니다). 여러 Pod일 때 DB나 알림 연결이 끊긴 Pod와 종료 중인 Pod를 Service에서 빼는 것이 이 프로브입니다. 디스크는 보지 않습니다(관리자 화면의 시스템 상태에서 확인합니다). 처음 시작하거나 업그레이드로 DB를 바꾸는 데 시간이 걸려도 재시작되지 않도록 startupProbe가 5분까지 기다립니다. livenessProbe는 포트에 TCP 연결만 해 봅니다(`tcpSocket`). TCP 연결은 서버의 연결 자리 1024개가 모두 차 있어도 커널이 받아 주므로(커널 대기열이 넘치지 않는 한), 서버가 바쁘거나 공격을 받아 느려졌다고 Pod가 재시작되어 진행 중인 전송이 모두 끊기지는 않습니다. 그동안 readinessProbe가 실패하면 Pod는 Service에서 잠시 빠졌다가 다시 들어옵니다. `commandcenter healthcheck`를 exec 프로브로 쓰지 않는 것은 128 MiB 한도 안에서 프로세스를 하나 더 띄우지 않기 위해서입니다.
- **보안**: UID 65532(non-root), 읽기 전용 루트 파일시스템, 모든 capability 제거, seccomp `RuntimeDefault`로 Pod Security Standards의 `restricted` 기준에 맞습니다. 임시 파일은 `/tmp`(emptyDir)에 씁니다. 이름 붙은 blob store는 경로를 비우면 `/data/blobstores/<이름>`에 만들어지고, `/data` 밖의 절대 경로에 두려면 그 경로에 볼륨을 따로 마운트해야 합니다(`extraVolumes`, `extraVolumeMounts`). 루트 파일시스템이 읽기 전용이라 다른 경로에는 만들 수 없고, `/tmp`는 Pod가 다시 만들어지면 비워집니다.
- **리소스**: 메모리 요청과 한도 128Mi, `GOMEMLIMIT=80MiB`(`goMemLimit`)입니다. CPU는 요청(100m)만 두고 한도는 두지 않습니다.
- **운영 명령**: "운영"의 `docker compose exec commandcenter …`는 `kubectl -n commandcenter exec commandcenter-0 -- …`로 바꿔 씁니다(예: `kubectl -n commandcenter exec commandcenter-0 -- commandcenter token create -user ci`). 로그는 `kubectl -n commandcenter logs commandcenter-0`입니다.
- **비상 복구**: `user reset-password`는 실행 중인 Pod에서 돌립니다(`printf '%s\n' "$PW" | kubectl exec -i -n commandcenter commandcenter-0 -- commandcenter user reset-password -user admin -admin`). 서버가 5초 안에 반영합니다("비상 복구" 참고).

### 참조 매니페스트

Helm 없이 설치하려면 [`deploy/kubernetes/commandcenter.yaml`](../deploy/kubernetes/commandcenter.yaml)을 씁니다. PostgreSQL StatefulSet·Service·비밀번호 Secret(값을 채워야 함)과 Command Center의 ConfigMap(`config.yaml`), Service, StatefulSet이 들어 있고, Pod 구성(볼륨, 프로브, 보안, 리소스, 중지 대기 시간)은 차트와 같습니다. 파일 맨 위 주석대로 이미지, `config.yaml`의 `baseURL`과 관리자 `passwordHash`, PostgreSQL 비밀번호, 두 PVC의 `storageClassName`(DB 쪽은 블록 스토리지)을 채운 뒤 적용합니다. 외부 PostgreSQL을 쓰려면 `commandcenter-postgresql` 세 개와 init 컨테이너를 지우고 `COMMANDCENTER_DATABASE_URL`을 Secret에서 넣습니다. 이미지는 버전을 고정한 릴리스 이미지(`ghcr.io/sungjinseo/commandcenter:1.2.3`, "릴리스 이미지와 바이너리" 참고)나 직접 빌드해 올린 이미지입니다.

```bash
kubectl apply -n <네임스페이스> -f deploy/kubernetes/commandcenter.yaml
```

이름(`commandcenter`, `commandcenter-0`, `data-commandcenter-0`, `commandcenter-postgresql-0`)이 차트와 같으므로 위의 백업, 서버를 멈춰야 하는 작업, 동작 방식이 그대로 적용됩니다. 업그레이드는 이미지 태그를 바꿔 다시 적용하고, Ingress와 비밀값 키(주석 처리된 `COMMANDCENTER_SECRET_KEY` 항목)는 직접 넣습니다.

## 프로덕션 배포

빠른 시작은 한 대에서 시험해 보는 구성입니다. 팀이 매일 쓰는 저장소로 올리기 전에 이 절을 확인하세요.

### 체크리스트

1. 검증한 버전을 고정해서 씁니다(`latest`나 기본 브랜치를 그대로 따라가지 않기). 업그레이드는 백업한 뒤에 하고, 다운그레이드는 지원하지 않습니다("업그레이드" 참고).
2. TLS는 리버스 프록시에서 끝내고, 일반 설정의 `baseURL`을 `https://` 주소로 둡니다. 서버 포트 8081은 프록시에서만 닿게 합니다.
3. 사내 전용 저장소라면 익명 읽기를 끕니다. 일반 설정의 `anonymousRead`와 저장소별 `anonymousRead`를 확인하세요. `config.example.yaml`은 시험하기 편하도록 `anonymousRead: true`입니다. 권한이 없는 로그인 사용자의 읽기까지 막으려면 `authenticatedRead`도 끕니다("읽기 권한" 참고).
4. 프록시에서 요청 본문 크기, 본문 수신·응답 전송 타임아웃, 클라이언트당 연결 수를 제한합니다.
5. 메타데이터 DB는 PostgreSQL 13 이상입니다. 차트·compose가 띄우는 PostgreSQL을 쓴다면 그 볼륨을 블록 스토리지에 두고, 운영 규모나 고가용성이 필요하면 복제 구성된 PostgreSQL이나 관리형 서비스를 씁니다(`database.url`, `COMMANDCENTER_DATABASE_URL`, 차트의 `database.existingSecret`). `dataDir`는 영구 볼륨에 두고, 여러 대를 함께 띄우려면 모든 노드가 같은 PostgreSQL DB와 같은 `dataDir`(ReadWriteMany 공유 스토리지)를 씁니다("고가용성(여러 노드)" 참고).
6. 메모리 한도와 `GOMEMLIMIT`을 함께 정합니다.
7. 중지 대기 시간을 60초로 둡니다(compose 파일, Helm 차트, 참조 매니페스트에는 들어 있습니다. "정상 종료" 참고).
8. 정기 백업(`pg_dump`와 blob)을 돌리고, 복원도 한 번 해 봅니다("백업과 복원" 참고).
9. 저장된 비밀값을 암호화할 키(`COMMANDCENTER_SECRET_KEY` 또는 `secretKeyFile`)를 정하고, 키는 DB 백업과 따로 보관합니다("저장된 비밀값 암호화" 참고).

### TLS와 리버스 프록시

서버는 평문 HTTP만 받습니다. 비밀번호와 토큰이 오가므로 사내망이라도 앞에 리버스 프록시(nginx, HAProxy, Caddy, Traefik, Kubernetes Ingress·Gateway 등)를 두고 거기서 TLS를 끝내세요.

- **`baseURL`은 `https://` 주소**: 일반 설정의 `baseURL`을 클라이언트가 실제로 접속하는 주소와 똑같이 맞춥니다. npm·Cargo 응답의 다운로드 URL, PyPI 파일 링크의 경로 앞부분, 세션 쿠키의 이름·`Secure` 플래그(https면 `__Host-cc_session`)가 이 값으로 정해집니다. 서버는 `X-Forwarded-Proto`, `X-Forwarded-Host` 같은 헤더를 보지 않습니다.
- **브라우저의 교차 출처 쓰기 차단**: 다른 사이트의 페이지가 보낸 쓰기 요청(업로드, 삭제, API 변경)은 403으로 거부합니다. 브라우저가 기억한 Basic 인증 정보가 그런 요청에 실려 가도 업로드되지 않게 하기 위해서입니다. `Sec-Fetch-Site`나 `Origin` 헤더를 보내지 않는 명령줄 클라이언트는 영향이 없습니다. 웹 UI는 HTTPS라면 브라우저가 `Sec-Fetch-Site: same-origin`을 보내므로 늘 통과합니다. 평문 HTTP에서는 `Origin`을 `Host`와 비교하는데, 프록시가 `Host`를 바꾸면(nginx 기본값) 둘이 어긋나므로 `baseURL`의 출처(스킴, 호스트, 포트)와 같은 `Origin`도 허용합니다. `baseURL`이 실제 주소와 다르면 이때 웹 UI의 업로드가 403이 됩니다.
- **8081은 프록시만**: compose라면 `ports: ["127.0.0.1:8081:8081"]`처럼 프록시가 있는 호스트에만 열거나, 프록시와 같은 Docker 네트워크에서만 노출하세요. 그러지 않으면 프록시의 TLS와 제한을 건너뛰는 평문 경로가 남습니다.
- **프록시에서 제한할 것**: 서버도 동시 연결을 1024개로 제한하고, 자리가 차면 요청 사이에 쉬고 있는 연결이나 1초 넘게 헤더를 다 보내지 않은 연결부터 닫아 새 연결을 받으며, 멈춘 본문을 끊습니다("메모리 설정", "업로드" 참고). 그러나 요청을 처리 중인 연결은 닫지 않습니다. 응답을 받아 가지 않고 붙들고 있는 연결처럼 처리 중인 연결이 자리를 모두 차지하면 다른 클라이언트는 기다려야 하므로, 외부에 노출한다면 연결 수와 본문 수신·응답 전송 시간 제한은 앞단에서 거세요.
  - 요청 본문 크기: `maxUploadMiB`(기본 1024 MiB) 이상으로 둡니다. 작으면 큰 업로드가 프록시에서 413으로 끊깁니다.
  - 본문 수신 타임아웃: 본문이 일정 시간(예: 60초) 동안 한 바이트도 오지 않으면 끊습니다. 느려도 계속 오는 업로드는 끊기지 않습니다.
  - 응답 전송 타임아웃: 클라이언트가 일정 시간(예: 60초) 동안 응답을 받아 가지 않으면 끊습니다. 서버 자체에는 이 제한이 없습니다(nginx는 `send_timeout`, 기본 60초).
  - 클라이언트 IP당 동시 연결 수: 빌드 서버 여러 대가 NAT 하나 뒤에 있으면 넉넉히 잡으세요.
  - 응답 대기 시간: proxy 저장소가 원격에서 처음 받아 오는 요청은 응답이 늦을 수 있으므로 넉넉히(예: 300초) 둡니다.
- **요청 로그**: 로그의 `remote`는 프록시의 주소가 됩니다(`X-Forwarded-For`는 기록하지 않습니다). 클라이언트 주소는 프록시 로그에서 확인하세요.

nginx 예시:

```nginx
# http { } 블록 안
limit_conn_zone $binary_remote_addr zone=cc_conn:10m;

server {
    listen 443 ssl;
    server_name repo.example.com;
    ssl_certificate     /etc/nginx/tls/repo.example.com.crt;
    ssl_certificate_key /etc/nginx/tls/repo.example.com.key;

    client_max_body_size  1024m;  # maxUploadMiB 이상
    client_header_timeout 10s;
    client_body_timeout   60s;    # 본문이 60초 동안 오지 않으면 끊음
    limit_conn cc_conn 100;       # 클라이언트 IP당 동시 연결

    location / {
        proxy_pass http://127.0.0.1:8081;
        proxy_read_timeout 300s;
        proxy_send_timeout 300s;
    }
}
```

nginx는 기본적으로 요청 본문을 끝까지 받은 다음 서버로 넘깁니다(`proxy_request_buffering on`). 그래서 본문을 멈춘 연결은 서버까지 오지 않습니다. 대신 큰 업로드는 nginx의 임시 디렉터리를 거치므로 그만큼의 디스크가 필요합니다.

### 저장 공간과 인스턴스

- **메타데이터는 PostgreSQL에**: 저장소 정의, 컴포넌트·에셋 목록, 사용자, 설정, 작업 기록은 PostgreSQL에 있고 `dataDir`(기본 `/data`)에는 default blob store(`blobs/`)와 업로드 임시 파일만 있습니다. PostgreSQL의 데이터 디렉터리는 블록 스토리지에 두고, `dataDir`와 blob store는 블록 스토리지나 NFS에 둘 수 있습니다.
- **blob은 NFS에도**: blob은 내용 주소 방식이라 한 번 쓰면 바뀌지 않으므로, `dataDir`와 이름 붙은 blob store("Blob Store" 참고)는 NFS 경로에 둘 수 있습니다. compact 작업이 파일의 수정 시각으로 진행 중인 업로드를 보호하므로 NFS 서버와 시계를 맞춰 두세요(NTP). 여러 노드는 `dataDir` 전체를 공유 스토리지에 둡니다("고가용성(여러 노드)" 참고).
- **blob store 경로는 영구 볼륨에**: 컨테이너 안의 경로(예: `/tmp/…`)에 store를 만들면 컨테이너를 다시 만들 때 blob이 사라지고 DB 기록만 남습니다.
- **인스턴스 수**: 노드 여러 대가 같은 PostgreSQL DB와 같은 `dataDir`(ReadWriteMany 공유 스토리지)를 함께 써서 모두 요청을 받을 수 있습니다(active-active, "고가용성(여러 노드)" 참고). 한 대로 돌려도 재시작은 빠릅니다(중지와 기동 모두 보통 1초 안쪽).
- **디스크 여유**: proxy 캐시는 계속 커집니다. Cleanup 정책과 compact 작업으로 정리하고, Blob Store 화면에서 파일시스템의 남은 용량을 지켜보세요.

### 리소스

- **메모리**: 기본 구성은 컨테이너 한도 128 MiB에 `GOMEMLIMIT=80MiB`이며, 이 구성의 측정값은 "소개"의 메모리 사용량에 있습니다. `docker stats`의 값에는 큰 파일을 읽고 쓸 때의 커널 파일 캐시가 들어가 한도에 가까워 보일 수 있지만, 커널이 회수하는 메모리입니다. 한도를 올릴 때는 `GOMEMLIMIT`도 함께 올리되 한도보다 충분히 낮게 두세요("메모리 설정" 참고).
- **CPU**: 대부분 업로드 해시 계산(sha256, sha1, md5, sha512)과 bcrypt 비밀번호 확인에 씁니다. CPU 한도를 낮게 걸면 로그인과 큰 업로드가 느려집니다.
- **디스크**: 같은 blob store 안에서는 같은 내용을 한 번만 저장하므로(hosted와 proxy가 같은 파일을 공유) 필요한 용량은 hosted 패키지와 proxy 캐시를 합친 크기입니다. 디스크가 차면 업로드가 실패합니다.

### 정상 종료

서버는 SIGTERM(또는 SIGINT)을 받으면 새 연결을 받지 않고 진행 중인 요청을 최대 30초 기다리며, 그때까지 끝나지 않은 요청은 끊습니다. 연결 자리 1024개가 모두 처리 중일 때도 30초에 끝나며, 자리를 기다리던 새 연결은 바로 닫습니다. 그 뒤 다운로드 수·요청 통계, 보낼 메일, 활동 기록을 각각 최대 10초 동안 마저 쓰고 DB를 닫습니다. 실행 중인 예약 작업은 취소됩니다.

Docker의 기본 중지 대기 시간은 10초라서, 그 안에 끝나지 않으면 프로세스가 강제로 종료됩니다. 진행 중인 업로드가 끊기고 마지막 통계가 사라질 수 있습니다(DB는 트랜잭션으로 쓰므로 깨지지 않습니다). 대기 시간을 60초로 두세요.

- compose: 들어 있는 `docker-compose.yml`에 `stop_grace_period: 60s`가 있습니다. 다른 compose 파일을 쓰면 서비스에 넣으세요.
- `docker run`으로 띄웠다면: `docker run --stop-timeout 60 …`으로 띄우거나 `docker stop -t 60 commandcenter`로 멈춥니다.
- Kubernetes: `terminationGracePeriodSeconds: 60`(차트의 기본값이고 참조 매니페스트에도 들어 있습니다)


### 노드와 준비 상태

- **노드 등록**: `serve`로 띄운 서버는 시작할 때 메타데이터 DB의 `nodes` 테이블에 자신을 노드로 등록합니다. 노드 ID는 `<호스트 이름>-<16진수 4자리>`이며 시작할 때마다 새로 정해집니다. 노드는 10초마다 DB 서버의 시계로 하트비트를 갱신하고, 마지막 하트비트가 30초 안이면 살아 있는 노드로 봅니다. 정상 종료하면 행을 지우고, 비정상 종료로 남은 행은 24시간이 지나면 정리됩니다. 노드 ID는 모든 로그 레코드의 `node` 속성, `/-/metrics/prometheus`의 `commandcenter_node_info{node,version} 1`, 시스템 정보의 `commandcenter-status.node`, 지원 번들의 `info.json`과 `nodes.json`에 들어갑니다. 등록된 노드는 `GET /-/api/nodes`(관리자)로 봅니다. `self`는 응답한 노드이고, 노드마다 `id`, `hostname`, `version`, `schema`(빌드의 스키마 버전), `startedAt`, `heartbeatAt`, `alive`, `draining`, `leader`(예약 작업 임대를 가진 노드), `runningTask`(그 노드가 실행 중인 예약 작업의 ID, 없으면 빈 문자열)가 있습니다.
- **여러 노드**: 같은 PostgreSQL DB에 `serve`를 여러 개 띄우면 모두 노드로 등록하고 함께 요청을 처리합니다(active-active, "고가용성(여러 노드)" 참고). 시작할 때 다른 노드를 기다리거나 거부하지 않습니다(이전 버전은 살아 있는 다른 노드가 있으면 시작을 거부했습니다). `token create`, `gc` 같은 CLI 명령은 노드로 등록하지 않으므로 실행 중인 서버 옆에서 그대로 쓸 수 있습니다.
- **노드 사이의 잠금과 캐시**: 서버의 잠금은 DB의 advisory lock이라 같은 DB를 쓰는 모든 노드에 걸칩니다. 같은 경로·패키지·인덱스를 건드리는 업로드와 삭제(모든 포맷, 메타데이터 재생성, 저장소 가져오기, Docker 업로드 세션 포함), 저장소·blob store·정리 정책·라우팅 규칙의 변경, 사용자·역할·Realm·설정의 변경은 어느 노드로 들어오든 한 번에 하나씩 처리됩니다. 한 노드에서 저장소, 라우팅 규칙, blob store, 웹훅, 사용자·역할·설정, 정리 정책, 로그 수준을 바꾸면 다른 노드는 DB 알림(LISTEN/NOTIFY)을 받아 곧바로 DB에서 다시 읽습니다. 알림을 놓쳐도 5초마다 다시 읽으므로 늦어도 몇 초 안에 맞춰집니다. 다른 노드에서 방금 만든 저장소를 가리키는 요청(`/repository/…`, `/v2/…`)은 그 자리에서 DB를 확인하므로 바로 처리됩니다. 저장소마다 세대 번호(`gen`)를 DB에 두므로, 요청이 진행되는 동안 어느 노드에서든 같은 이름의 저장소를 지우고 다시 만들면 그 요청이 트랜잭션으로 쓰는 내용은 새 저장소에 들어가지 않고 `503 repository "<이름>" changed during the request; retry`로 거절됩니다. 다시 시도하면 됩니다(raw 파일과 Maven 아티팩트 파일 하나를 올리는 쓰기는 이 검사 대상이 아닙니다). Blob Store 이동과 blob store 그룹 멤버 변경도 모든 노드에 맞춰집니다("고가용성(여러 노드)" 참고).
- **여러 노드의 예약 작업과 webhook**: 예약 작업과 webhook 전달은 같은 DB를 쓰는 노드가 여럿이어도 한 번씩만 실행되도록 DB로 조정합니다.
  - 예정된 작업은 예약 작업 임대를 가진 노드(`leader`)만 실행합니다. 요청 통계, 네거티브 캐시, 활동 기록의 오래된 행도 리더만 정리합니다. 리더가 죽으면 임대는 30초 안에 다른 노드로 넘어갑니다.
  - 작업은 모든 노드를 통틀어 한 번에 하나만 실행됩니다(클러스터 잠금 `tasks.run`). 다른 노드에서 작업이 실행 중이면 "지금 실행"과 `POST /-/api/gc`는 409입니다. 실행 중인 작업은 어느 노드의 작업 화면에서나 Running으로 보이고, 어느 노드에서나 중지할 수 있으며, 지울 수 없습니다.
  - 작업을 실행하던 노드가 결과를 기록하기 전에 죽으면 잠금이 풀립니다. 작업은 여전히 예정 시각이 지난 상태이므로 다음 리더가 다시 실행합니다(최소 한 번 실행). compact, cleanup, 메타데이터 재생성은 다시 실행해도 결과가 같습니다. 죽은 노드가 남긴 실행 표시는 무시됩니다.
  - `lastDownloadedDays`를 쓰는 cleanup(미리보기 포함)과 SNAPSHOT 정리는 시작할 때 다른 노드들에게 메모리에 쌓인 다운로드 수를 DB에 쓰게 하고 2초 기다립니다. cleanup 정책을 고치면, 어느 노드에서 실행 중인 cleanup이든 다음 컴포넌트 전에 정책을 다시 읽습니다. `PUT /-/api/logging`으로 바꾼 로그 레벨은 모든 노드에 적용됩니다.
  - webhook 전달은 노드 하나가 60초 동안 가져가서(claim) 보내므로, 노드가 여럿이어도 한 번만 나갑니다. 보낸 뒤 기록하기 전에 노드가 죽으면 60초 뒤 다른 노드가 다시 보냅니다(최소 한 번 전달).
- **준비 상태(readiness)**: `GET /service/rest/v1/status/writable`은 서버가 요청을 받아도 될 때만 200을, 아니면 503을 돌려줍니다. DB가 응답하고, 노드가 종료 중(draining)이 아니며, DB 스키마가 이 버전이 돌 수 있는 것이고, 잠금 연결과 변경 알림 연결(`LISTEN cc_changed`)이 살아 있어야 200입니다. 더 새 버전이 DB를 업그레이드해 이 버전이 함께 돌 수 없게 되면, 실행 중인 노드는 다음 하트비트에서 이를 알아채고 오류 로그를 남긴 뒤 준비되지 않은 상태가 됩니다. Helm 차트와 참조 매니페스트의 readinessProbe가 이 경로를 씁니다(startupProbe는 `/service/rest/v1/status`, livenessProbe는 TCP 연결 그대로입니다). 관리자 화면 시스템 상태의 `Cluster` 항목(`/service/rest/v1/status/check`, `commandcenter_health_check{name="Cluster"}`에도 나옵니다)이 살아 있는 노드와 리더를 보여 주고, 이 노드가 준비되지 않았으면 실패로 표시합니다.
- **종료 순서(drain)**: SIGTERM을 받으면 먼저 노드를 draining으로 표시해 준비 상태를 503으로 바꿉니다. 그 뒤 5초 동안 요청을 계속 받으며 Service 엔드포인트와 로드밸런서가 이 Pod를 빼기를 기다립니다(이미지에 셸이 없어 preStop `sleep`을 쓸 수 없으므로 서버가 직접 기다립니다). 이어서 이전과 같이 진행 중인 요청을 최대 30초 기다리고 통계, 메일, 활동 기록을 쓴 다음, 노드 행을 지우고 임대를 내놓습니다. 모두 합쳐 약 45초로 `terminationGracePeriodSeconds: 60` 안에 끝납니다.
- **DB 연결 수**: 노드 하나는 연결 풀(기본 10개) 외에 잠금 연결 1개와 알림 연결 1개를 더 씁니다. 두 연결은 TCP keepalive(30초 뒤 10초 간격으로 3회)를 켜 두므로, DB와 끊긴 노드의 잠금은 약 1분 안에 풀립니다.

### 고가용성(여러 노드)

서버 여러 대(노드)를 같은 PostgreSQL DB와 같은 저장소 데이터로 함께 띄워 모두 요청을 받게 할 수 있습니다(active-active). 앞에 로드밸런서를 두고, 세션 고정(sticky session)은 필요 없습니다. 노드 하나가 죽거나 업그레이드로 내려가도 다른 노드가 계속 응답합니다. 쓰기는 PostgreSQL에서 하나씩 처리되므로 노드를 늘리면 가용성과 읽기 처리량이 늘고 쓰기 처리량은 늘지 않습니다.

**요구 사항**

- **같은 PostgreSQL DB**: 모든 노드가 같은 DB(PostgreSQL 13 이상)를 씁니다(`database.url` 또는 `COMMANDCENTER_DATABASE_URL`). 노드 하나는 연결 풀(기본 10개)에 잠금·알림 연결 2개를 더 쓰므로 노드 3대면 약 36개가 필요합니다. 차트가 띄우는 PostgreSQL은 Pod 하나이므로 고가용성에는 복제 구성된 PostgreSQL(`database.existingSecret`)을 씁니다.
- **공유 스토리지**: `dataDir`를 모든 노드가 같은 경로로 마운트하는 ReadWriteMany 파일시스템(NFS, CephFS, EFS 등)에 둡니다. default blob store(`blobs/`)와 모든 store의 임시 파일·Docker 업로드 세션(`tmp/`)이 여기 있으므로, 이름 붙은 store를 S3에 두더라도 `dataDir`는 공유해야 합니다(S3 store의 임시 파일도 `dataDir` 아래에 있습니다). 이름 붙은 파일 store를 `dataDir` 밖의 절대 경로에 두면 그 경로도 모든 노드에 같은 경로로 공유 마운트합니다. 한 노드에만 있는 경로에 store를 만들면 노드마다 다른 파일을 보게 됩니다. 내보내기와 가져오기 작업은 다른 노드에서 돌 수 있으므로 `exportRoot`도 공유 경로에 둡니다.
- **시계**: 노드와 NFS 서버의 시계를 NTP로 맞춥니다. 임대와 하트비트는 DB 서버의 시계를 쓰지만, compact 작업은 blob 파일의 수정 시각을 노드의 시계와 비교합니다.
- **같은 설정과 같은 비밀값 키**: 모든 노드에 같은 `config.yaml`과 같은 `COMMANDCENTER_SECRET_KEY`(또는 `secretKeyFile`)를 줍니다. 다른 키로, 또는 키 없이 시작한 노드는 시작을 거부합니다. `secret-key rotate`는 모든 노드를 멈추고 실행합니다.
- **같은 버전**: 노드는 같은 버전으로 돌립니다. 두 버전은 롤링 업그레이드 동안에만 함께 돕니다. 이전 버전 노드는 Blob Store 이동과 그룹 멤버 제거의 확인 요청에 답하지 않으므로, 그 작업은 업그레이드를 마친 뒤에 실행합니다.

**띄우는 방법**

- Kubernetes: Helm 차트에서 `replicaCount`를 2 이상으로 두고 `persistence.accessModes: [ReadWriteMany]`(또는 `persistence.existingClaim`)를 정합니다("차트 값" 참고).
- 그 밖: 같은 설정으로 `serve`를 여러 대 띄우고, 로드밸런서의 헬스 체크를 `GET /service/rest/v1/status/writable`(200이면 정상)로 둡니다. SIGTERM을 받은 노드는 5초 동안 503을 내면서도 요청을 계속 받으므로, 그사이 로드밸런서가 그 노드를 뺍니다.
- 관리자 화면 시스템 상태의 `Cluster` 항목과 `GET /-/api/nodes`로 살아 있는 노드와 리더를 확인합니다.

**노드 사이에 맞추는 것**: 업로드·삭제·메타데이터 재생성의 잠금, 저장소·blob store·라우팅 규칙·웹훅·사용자·역할·설정과 그 캐시, 세션과 로그인 제한, SSO 상태, Docker·Conan 토큰, 예약 작업(모든 노드를 통틀어 하나씩, 예정된 실행은 리더만), webhook 전달, Blob Store 이동, blob store 그룹 멤버 변경입니다("노드와 준비 상태" 참고). 저장소의 blob store를 옮기는 작업은 모든 노드가 이동을 알고 이전 store로 가던 쓰기를 마칠 때까지 기다린 뒤 복사를 시작합니다. 그룹 멤버 제거 작업은 모든 노드가 그 멤버를 읽기 전용으로 바꿀 때까지 기다린 뒤 복사합니다. 응답하지 않는 노드(하트비트는 살아 있는데 DB 알림 연결이 끊긴 노드 등)가 있으면, 그 노드가 응답하거나 하트비트가 끊길 때까지 기다리며 30초마다 로그에 남깁니다. 웹 UI에서 그룹 멤버를 바꾸는 요청은 다른 노드를 최대 30초 기다립니다.

**노드마다 따로인 것**(결과는 맞고, 잠시 다를 수 있습니다): proxy 원격 요청의 동시 수 제한과 같은 파일 요청 합치기(캐시가 비어 있으면 노드 수만큼 원격에 갈 수 있습니다), proxy 자동 차단 상태(노드마다 원격 장애를 따로 감지합니다), 원격 레지스트리 토큰 캐시, LDAP·OIDC 공급자 캐시, 비밀번호 확인 캐시, 통계 화면 캐시, 활동 기록과 메일 대기열, 로그 뷰어와 스레드 덤프, 지원 번들, `/-/metrics/prometheus`의 누적 값(노드마다 따로 수집합니다. 대시보드의 요청 통계는 DB에서 합칩니다), IP별 로그인 시도 제한(노드 수만큼 느슨해집니다), 취약점 인덱스(다른 노드의 가져오기를 몇 초 안에 반영합니다), Blob Store 이동의 진행 수치(작업을 돌리는 노드만 복사 수를 보여 줍니다. 다른 노드는 작업이 도는 동안 `copying`으로만 보여 줍니다), blob store 소프트 쿼터 이벤트(노드마다 한 번씩 기록될 수 있습니다).

**장애 시 동작**

- **노드가 죽음**: 그 노드에서 진행 중이던 요청은 실패하고 클라이언트가 다시 시도합니다. 반쯤 쓴 DB 기록은 남지 않습니다. 그 노드의 잠금은 DB 세션과 함께 풀립니다(네트워크가 끊긴 경우 약 1분 안에). Docker 업로드 세션은 공유 `tmp/`에 있으므로 다른 노드로 이어서 올릴 수 있습니다.
- **실행 중이던 작업**: 결과를 기록하지 못한 작업은 다음 리더가 처음부터 다시 실행합니다(최소 한 번). 죽은 노드가 리더였다면 임대가 30초 뒤 다른 노드로 넘어갑니다.
- **webhook**: 보낸 뒤 기록하기 전에 노드가 죽으면 60초 뒤 다른 노드가 다시 보냅니다(최소 한 번. 같은 `X-Nexus-Webhook-Delivery`로 중복을 거를 수 있습니다). 그 노드 메모리에 있던 최근 30초의 다운로드 수, 아직 쓰지 못한 활동 기록과 메일, 아직 저장하지 못한 webhook 이벤트는 사라집니다(한 대일 때의 강제 종료와 같습니다).
- **PostgreSQL 재시작·장애 조치**: 모든 노드의 쓰기가 실패하고 준비 상태가 503이 됩니다. 다시 연결되면 노드마다 캐시를 모두 다시 읽습니다. 로그인 실패 횟수(UNLOGGED 테이블)는 초기화됩니다.
- **DB와 끊긴 노드**: 쓰지 못하므로 데이터가 갈라지지 않습니다(split brain 없음). 준비 상태가 503이 되어 로드밸런서가 뺍니다.
- **쓰기 몰림**: 쓰기는 DB에서 하나씩 처리되므로 업로드가 몰리거나 긴 인덱스 재생성이 겹치면 5초의 잠금 대기 뒤 5xx가 날 수 있습니다. 노드가 많으면 DB URL에 `lock_timeout=15000`(밀리초)을 붙여 늘리고 오류율을 지켜보세요.

**한 대에서 여러 대로**(한 번의 점검 시간이 필요합니다)

1. 백업하고 서버를 멈춥니다(Kubernetes: `kubectl -n commandcenter scale statefulset commandcenter --replicas=0`).
2. ReadWriteMany 볼륨을 만들고 이전 볼륨의 `/data/blobs`와 `/data/blobstores`(이름 붙은 파일 store)를 `cp -a`로 복사합니다(예: 두 PVC를 함께 마운트한 busybox Pod).
3. 모든 노드를 같은 설정, 같은 비밀값 키, 같은 DB와 공유 볼륨으로 띄웁니다. Helm이라면 `--set replicaCount=3,persistence.existingClaim=<RWX PVC>`로 `helm upgrade`합니다. Helm이 StatefulSet을 Deployment로 바꾸고, 이전 PVC `data-commandcenter-0`은 직접 지울 때까지 남습니다.

다시 한 대로 돌아가려면 `replicaCount: 1`로 둡니다. 그러면 같은 PostgreSQL DB를 쓰는 StatefulSet이 다시 만들어집니다(볼륨은 새 claim 템플릿, 또는 `persistence.existingClaim`).

## Nexus에서 이전하기

Sonatype Nexus Repository 3의 hosted 저장소 내용을 Command Center로 복사할 때는 `commandcenter import-nexus` 서브커맨드를 씁니다. Nexus의 REST API(v1)로 컴포넌트 목록을 읽고, 포맷마다 Command Center가 원래 받는 업로드 방식(Maven PUT, npm publish, PyPI 업로드, raw PUT, helm PUT, docker blob/manifest 업로드)으로 그대로 올리므로 서버의 검증이 모두 적용됩니다. 서버 쪽에 추가 설정은 없습니다.

1. **준비.** Nexus에 읽기 권한이 있는 사용자와 비밀번호, Command Center에 대상 저장소 쓰기 권한이 있는 사용자와 API 토큰(`commandcenter token create`, Basic 비밀번호 자리에 씁니다)을 준비합니다. 비밀번호와 토큰은 파일(첫 줄)이나 환경 변수 `NEXUS_PASSWORD`, `COMMANDCENTER_TOKEN`로 넘깁니다. 명령줄 인자나 URL(`http://user:pw@...`)에는 넣을 수 없고, 출력에도 찍히지 않습니다. 도구를 실행하는 곳에서 두 서버에 모두 접속할 수 있어야 합니다. 사설망 주소의 Nexus도 됩니다.
2. **대상 저장소를 먼저 만들기.** Command Center에 같은 포맷의 hosted 저장소를 먼저 만듭니다. Nexus 포맷과의 대응은 maven2→maven, npm→npm, pypi→pypi, raw→raw, helm→helm, docker→docker입니다. 이름이 같을 필요는 없고 `-repo <Nexus 저장소>=<Command Center 저장소>`로 짝을 지어 줍니다. 이름이 같으면 `-repo <이름>`만 써도 됩니다. Maven 저장소는 `versionPolicy`(release/snapshot)를 Nexus 쪽과 맞춰 두세요. 정책에 맞지 않는 버전은 업로드가 거부되어 오류로 집계됩니다.
3. **먼저 확인(dry run).**

   ```bash
   echo -n '<nexus 비밀번호>' > nexus.pw; echo -n '<cc 토큰>' > cc.token; chmod 600 nexus.pw cc.token
   commandcenter import-nexus -from https://nexus.example.com -from-user admin -from-password-file nexus.pw \
     -to https://cc.example.com -to-user ci -to-token-file cc.token \
     -repo maven-releases -repo npm-hosted=npm-internal -dry-run
   ```

   컴포넌트마다 한 줄(`저장소 포맷 이름 버전 파일수 바이트 상태`)과 저장소별 요약을 출력합니다. dry run은 아무것도 쓰지 않고 `would copy`로 표시합니다.
4. **복사.** `-dry-run`을 빼고 같은 명령을 실행합니다. `-all`을 주면 Nexus의 모든 hosted 저장소를 같은 이름의 Command Center 저장소로 복사합니다. `-concurrency`(기본 4)는 동시에 옮기는 컴포넌트 수입니다. 자체 서명 인증서는 `-ca <PEM>`으로 신뢰시키고, 테스트 환경에서만 `-insecure`를 쓰세요. 오류가 하나라도 있으면 종료 코드가 1이고 마지막 줄에 개수가 나옵니다. 각 줄의 `error:` 메시지는 Command Center 서버가 돌려준 이유(예: POM 좌표 불일치, PEP 440이 아닌 버전)이므로 그 파일만 손보면 됩니다.

**복사되는 것과 되지 않는 것.** hosted 저장소의 컴포넌트와 파일만 복사합니다. Maven은 pom/jar/sources/javadoc/asc 등 파일 전부를 경로 그대로 올리고, 체크섬 파일과 `maven-metadata.xml`은 Command Center가 다시 만들므로 건너뜁니다. npm은 tarball의 package.json으로 publish 문서를 만들어 올리고, 버전을 다 옮긴 뒤 Nexus의 dist-tags(`latest` 등)를 그대로 설정합니다. PyPI는 wheel/sdist 파일을 twine이 보내는 형식으로 올리며, 메타데이터는 서버가 파일 안의 METADATA/PKG-INFO에서 읽습니다. helm은 `<이름>-<버전>.tgz`만 올립니다(`.prov`는 제외). 다음은 복사하지 **않습니다**. proxy·group 저장소(목록에 `skipped`로 표시되고 Command Center에서 새로 만들면 됩니다), go와 cargo 등 지원하지 않는 포맷(마찬가지로 `skipped`), 사용자·역할·권한, cleanup 정책, 예약 작업, 라우팅 규칙, 다운로드 수 같은 통계. 사용자와 역할은 "인증과 권한"을 보고 설정 파일이나 관리자 화면에서 다시 만듭니다.

**Docker.** Nexus의 docker hosted 저장소는 REST API가 아니라 저장소마다 따로 연 커넥터 포트(예: 8082)의 레지스트리 API로 읽어야 합니다. `-docker-from http://nexus.example.com:8082`로 그 저장소의 커넥터 주소를 주고, docker 저장소는 한 번에 하나씩 옮깁니다(`-repo docker-hosted -docker-from ...`). 주지 않으면 docker 저장소는 `skipped`로 표시됩니다. 이미지의 태그마다 매니페스트를 읽어 blob 중 대상에 없는 것만 올리고, 멀티 플랫폼 이미지(index)는 하위 매니페스트를 먼저 올린 뒤 태그를 붙입니다. 대상에서는 `docker pull cc.example.com/<저장소>/<이미지>:<태그>`로 받습니다("Docker 설정").

**다시 실행하기.** 중간에 끊겨도 같은 명령을 다시 실행하면 됩니다. 기본값(`-skip-existing`)으로는 대상에 이미 있는 파일·버전·태그를 HEAD로 확인해 `skipped`로 세고 다시 올리지 않으므로, 몇 번을 돌려도 결과가 같습니다. Nexus에 새로 올라온 것만 추가로 옮기는 데도 같은 방법을 씁니다. 모든 파일은 Nexus가 알려 준 sha256과 대조한 뒤에만 업로드되며, 임시 파일을 거쳐 옮기므로 큰 파일도 메모리를 많이 쓰지 않습니다. `-skip-existing=false`는 존재 확인을 생략할 뿐이고, 이미 있는 것은 저장소의 `writePolicy`가 정한 대로 거부(allow_once)되거나 덮어써집니다(allow).

## Nexus REST API 호환

Sonatype Nexus Repository 3의 REST API(v1) 가운데 스크립트, CI 작업, Terraform provider(`datadrivers/terraform-provider-nexus`), `nexus-cli` 같은 도구가 실제로 쓰는 엔드포인트를 `/service/rest/v1` 아래에서 같은 경로와 같은 JSON 필드 이름으로 제공합니다. 인증은 다른 경로와 같습니다(Basic 비밀번호, API 토큰, UI 세션). 저장소 관리는 admin, 컴포넌트·자산 조회는 해당 저장소의 read 권한, 삭제는 delete 권한이 필요합니다. 익명으로 읽을 수 없으면 `401`(Basic 챌린지 포함), 로그인했지만 권한이 없으면 `403`, 볼 수 없는 저장소는 `404`로 답합니다. 오류 본문은 Nexus처럼 `{"message": "..."}`이고, 저장소 본문의 검증 오류는 `400`에 `[{"id": "...", "message": "..."}]` 배열입니다.

| 엔드포인트 | 지원 |
|---|---|
| `GET /repositories`, `GET /repositories/{name}` | 읽을 수 있는 저장소의 `name`, `format`, `type`, `url`, `attributes.proxy.remoteUrl` |
| `GET /repositorySettings`, `GET /repositories/{format}/{type}/{name}` | admin. `online`, `storage`, `cleanup`, `proxy`, `negativeCache`, `httpClient.authentication`(사용자 이름만), `group`, `maven` 등 Command Center에 대응 값이 있는 항목 |
| `POST /repositories/{format}/{type}` → 201, `PUT /repositories/{format}/{type}/{name}` → 204, `DELETE /repositories/{name}` → 204 | admin. maven, npm, pypi, raw, docker, helm, go, cargo, nuget, apt, yum, rubygems, composer, conda. proxy 본문의 `routingRule`(응답에는 `routingRuleName`도) |
| `GET /components?repository=`, `GET /components/{id}`, `DELETE /components/{id}` | 50개씩, `continuationToken`으로 다음 페이지 |
| `GET /assets?repository=`, `GET /assets/{id}`, `DELETE /assets/{id}` | 삭제는 raw 파일만(아래 차이점) |
| `GET /search`, `GET /search/assets`, `GET /search/assets/download` | `q`, `repository`, `format`, `group`, `name`, `version`(`*` 와일드카드), `sort`, `direction`, `maven.groupId/artifactId/baseVersion/extension/classifier`, `npm.scope`, `docker.imageName/imageTag` |
| `GET /status`, `GET /status/writable`, `GET /status/check` | check는 admin, 상태 화면의 점검 결과를 Nexus 형식으로 |
| `GET /security/users?userId=`, `GET /security/roles` | admin, 읽기 전용 |
| `GET /blobstores`, `POST /blobstores/file` → 204, `PUT /blobstores/file/{name}` → 204, `GET /blobstores/{name}/quota-status`, `GET/POST /blobstores/group`, `GET/PUT /blobstores/group/{name}` | admin. `softQuota`(`type`: `spaceUsedQuota` 또는 `spaceRemainingQuota`, `limit`: MB, 없으면 쿼터 없음), 그룹은 `name`, `members`, `fillPolicy`이고 `GET /blobstores`의 `type`은 `Group`입니다. `quota-status`는 `{isViolation, message, blobStoreName}`("Blob Store" 참고) |
| `GET/POST /routing-rules`, `GET/PUT/DELETE /routing-rules/{name}` | admin. `name`, `description`, `mode`(`BLOCK`/`ALLOW`), `matchers`("라우팅 규칙" 참고) |
| `GET/POST /security/content-selectors`, `GET/PUT/DELETE /security/content-selectors/{name}`, `POST/PUT /security/privileges/repository-content-selector[/{name}]` | admin. `name`, `description`, `expression`; 권한은 `priv:<이름>` 역할로 만들어집니다("컨텐츠 셀렉터" 참고) |
| `GET/POST /cleanup-policies`, `GET/PUT/DELETE /cleanup-policies/{name}` | admin. `name`, `notes`, `format`(`maven2`, 전체는 `*`), `criteriaLastBlobUpdated`(일), `criteriaLastDownloaded`(일), `criteriaReleaseType`(`RELEASES`/`PRERELEASES`), `criteriaAssetRegex`, `retain`, `sortBy`(`version`/`date`). PUT은 204이며 Nexus에 없는 `namePattern`과 `exclude`는 유지합니다 |
| `GET /tasks`, `POST /tasks` → 201, `GET/PUT/DELETE /tasks/{id}`, `POST /tasks/{id}/run`, `POST /tasks/{id}/stop` | admin. Nexus의 TaskTemplateXO(`type`, `name`, `enabled`, `alertEmail`, `notificationCondition`은 `FAILURE`만, `frequency`, `properties`). 종류는 `blobstore.compact`, `repository.cleanup`, `repository.maven.rebuild-metadata`, `repository.maven.remove-snapshots`, `blobstore.delete-temp-files`와 Command Center 고유 id이며, 모르는 종류는 400에 지원 목록을 알려 줍니다. `frequency.schedule`은 manual, once, hourly, daily, weekly(`recurringDays` 하나, 1=일요일…7=토요일), monthly(1–28일 하나), cron(`0 M H * * ?`, `0 M * * * ?`, `0 M H ? * MON`, `0 M H D * ?` 형태)이고, `startDate`(밀리초)와 `timeZoneOffset`의 시각을 서버 시간대의 예약으로 바꿉니다 |
| `GET/PUT /security/user-tokens`, `DELETE /security/user-tokens`, `GET /security/user-tokens/tokens`, `GET/POST/DELETE /security/users/{userId}/{realm}/user-token` | admin. 사용자 토큰 설정(`protectContent`가 `required`), 전체 무효화, 목록, 사용자별 조회·발급·삭제("사용자 토큰" 참고) |
| `GET/POST /tags`, `GET/PUT/DELETE /tags/{name}`, `POST/DELETE /tags/associate/{tagName}`, `POST /staging/move/{destination}`, `POST /staging/delete` | 태그 만들기·수정·삭제는 admin, 연결과 스테이징은 관련 저장소의 권한. 대상은 검색과 같은 파라미터로 고르고, `GET /search?tag=`는 태그로 거르며 컴포넌트에 `tags`가 붙습니다("태그와 스테이징" 참고) |
| `GET /service/rest/atlas/system-information` | admin. `nexus-status`, `nexus-node`, `nexus-configuration`, `nexus-properties`, `system-time`, `system-runtime`(고루틴, 메모리, GOMAXPROCS), `system-filestores`(blob store와 남은 공간). 지원 번들과 같은 API 화면에서 만들므로 비밀번호·키·DB URL은 들어 있지 않습니다 |
| `GET /service/rest/swagger.json` | 인증 없음. `GET /-/api/openapi.json`과 같은 OpenAPI 3.0 문서("OpenAPI 문서" 참고) |

포맷 이름은 Nexus 표기를 따릅니다. Maven 저장소는 응답에서 `"format": "maven2"`이고, 생성·수정 경로는 Nexus와 같이 `/repositories/maven/hosted`처럼 씁니다. `search`의 `format=` 파라미터는 `maven2`와 `maven` 둘 다 받습니다.

자주 쓰는 호출 예(`-u admin:비밀번호`는 Basic 인증입니다):

```bash
# 저장소 목록과 설정
curl -u admin:pw http://localhost:8081/service/rest/v1/repositories
curl -u admin:pw http://localhost:8081/service/rest/v1/repositories/maven/hosted/maven-releases

# Nexus 문서의 본문 그대로 hosted 저장소 만들기(201)
curl -u admin:pw -X POST -H 'Content-Type: application/json' \
  http://localhost:8081/service/rest/v1/repositories/maven/hosted -d '{
  "name": "maven-snapshots", "online": true,
  "storage": {"blobStoreName": "default", "strictContentTypeValidation": true, "writePolicy": "ALLOW"},
  "cleanup": {"policyNames": []},
  "maven": {"versionPolicy": "SNAPSHOT", "layoutPolicy": "STRICT"}}'

# 프록시(분 단위 캐시, 원격 인증)와 그룹
curl -u admin:pw -X POST -H 'Content-Type: application/json' \
  http://localhost:8081/service/rest/v1/repositories/npm/proxy -d '{
  "name": "npm-proxy", "online": true, "storage": {"blobStoreName": "default", "strictContentTypeValidation": true},
  "proxy": {"remoteUrl": "https://registry.npmjs.org", "contentMaxAge": 1440, "metadataMaxAge": 60},
  "negativeCache": {"enabled": true, "timeToLive": 5},
  "httpClient": {"blocked": false, "autoBlock": true, "authentication": {"type": "username", "username": "bob", "password": "secret"}}}'
curl -u admin:pw -X POST -H 'Content-Type: application/json' \
  http://localhost:8081/service/rest/v1/repositories/maven/group -d '{
  "name": "maven-all", "online": true, "storage": {"blobStoreName": "default", "strictContentTypeValidation": true},
  "group": {"memberNames": ["maven-releases", "maven-central"], "writableMember": "maven-releases"}}'

# 컴포넌트 나열(페이지 넘기기)과 삭제
curl -u ci:pw 'http://localhost:8081/service/rest/v1/components?repository=maven-releases'
curl -u ci:pw 'http://localhost:8081/service/rest/v1/components?repository=maven-releases&continuationToken=<이전 응답의 continuationToken>'
curl -u ci:pw -X DELETE http://localhost:8081/service/rest/v1/components/<id>

# 검색과 최신 버전 내려받기(302)
curl 'http://localhost:8081/service/rest/v1/search?repository=maven-releases&maven.groupId=org.x&maven.artifactId=lib&sort=version&direction=desc'
curl -L -o lib.jar 'http://localhost:8081/service/rest/v1/search/assets/download?repository=maven-releases&group=org.x&name=lib&maven.extension=jar&maven.classifier=&sort=version&direction=desc'

# 상태
curl http://localhost:8081/service/rest/v1/status/writable
curl -u admin:pw http://localhost:8081/service/rest/v1/status/check
```

Nexus와 다른 점:

- 컴포넌트와 자산의 `id`는 서버가 만든 불투명한 값입니다(컴포넌트는 저장소·그룹·이름·버전, 자산은 저장소·경로를 base64url로 묶은 것). 응답에서 받은 값을 그대로 쓰면 되고, 저장소를 지우고 다시 만들어도 같은 값이 나오지만 Nexus처럼 값의 형식에 기대지는 마세요.
- `DELETE /assets/{id}`는 raw 파일만 지웁니다. Command Center는 파일 하나가 아니라 컴포넌트 단위로 지우므로 다른 포맷은 `400`으로 답하고 `DELETE /components/{id}`를 쓰라고 알려 줍니다. 컴포넌트 삭제는 웹 UI의 컴포넌트 삭제와 같은 경로로 처리되어 hosted 저장소에서만, `writePolicy: deny`가 아닐 때만 되고 활동 이벤트에 남습니다.
- 저장소 생성·수정·삭제는 웹 UI의 저장소 API를 그대로 호출하므로 검증 규칙과 메시지가 UI와 같고, 비밀번호 또는 세션 로그인이 필요합니다(API 토큰으로는 `403`). 같은 이름이 이미 있으면 `409`가 아니라 `400` 검증 오류입니다.
- 본문 가운데 Command Center에 대응 값이 없는 항목(`docker`, `component.proprietaryComponents`, `httpClient.connection` 등)은 무시합니다. `storage.strictContentTypeValidation`(hosted), `raw.contentDisposition`(`INLINE`/`ATTACHMENT`), `httpClient.blocked`·`httpClient.autoBlock`은 같은 이름의 저장소 키로 옮깁니다("저장소 키" 참고). `cleanup.policyNames`는 첫 번째 정책만 적용되고(저장소당 하나), `negativeCache.enabled: false`는 무시되며 `timeToLive`만 반영됩니다. 그룹 본문의 `storage.blobStoreName`은 버립니다(그룹에는 blob store가 없습니다). 응답의 `httpClient.authentication`에는 사용자 이름만 있고 비밀번호는 나오지 않습니다.
- 시간 값은 Nexus처럼 분 단위 정수이고 `-1`은 만료 없음입니다(`contentMaxAge: -1`은 설정의 `contentMaxAge: "-1"`).
- Docker 컴포넌트는 Nexus처럼 `name`이 이미지, `version`이 태그이며, 삭제는 그 태그가 가리키는 매니페스트(다이제스트)를 지웁니다. raw 컴포넌트는 `name`이 경로, `group`이 디렉터리(`/docs`, 최상위는 `/`), `version`은 `null`입니다. npm의 `group`은 `@` 없는 스코프입니다.
- `search/assets/download`는 자산이 둘 이상이면 Nexus처럼 `400`으로 거절하며, `sort`를 주면 정렬 뒤 첫 번째로 보냅니다(`sort=version&direction=desc`가 최신). `sort=version`의 기본 방향은 desc입니다.
- 컴포넌트 목록과 검색은 요청마다 저장소의 자산 경로를 모두 읽어 파생하므로 자산이 수십만 개인 저장소에서는 한 페이지에 수 초가 걸릴 수 있습니다. 전체 목록에 `continuationToken`으로 이어 받습니다.
- `security/users`, `security/roles`는 읽기 전용이고 역할의 `privileges`는 `"<대상>:<동작>"` 문자열입니다(컨텐츠 셀렉터 권한은 `selector:<이름>:<동작>`). 스크립트 API는 없습니다. 모르는 경로는 JSON `404`입니다.

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
- **컴포넌트**: Summary(Repository, Format, Group, Name, Version), Usage(도구를 고르면 Maven/Gradle, npm, pip, go get, cargo add 같은 사용 예 복사), Assets(파일 목록과 크기). hosted 저장소에 `delete` 권한이 있으면 "Delete component"가 나옵니다("컴포넌트 삭제" 참고).
- **파일(asset)**: Summary(Path, Content type, File size, Blob created/updated, Blob reference(SHA-256), 프록시는 Locally cached), Usage, Attributes(checksum: sha1, md5, sha256, sha512 등)와 "Download". hosted 저장소에 `delete` 권한이 있으면 Maven은 "Delete asset"(그 파일만), 다른 포맷은 "Delete component"(그 파일이 속한 버전 전체)가 나옵니다.
- 폴더: Summary(Repository, Path, Children).

### 검색

헤더 검색창과 Search 화면은 같은 API(`GET /-/api/search/components`)를 씁니다. 기준 중 하나는 2자 이상이어야 합니다. 결과는 저장소, group, 이름, 버전 순으로 정렬해 한 페이지에 최대 100개 컴포넌트(컴포넌트당 파일 50개)씩 돌려줍니다. 다음 페이지가 있으면 응답에 `continuationToken`이 있고, 같은 조건에 `continuationToken=<그 값>`을 더해 다시 부르면 이어지는 100개를 받습니다(마지막 페이지에는 없고, 잘못된 값은 400). Search 화면에서는 표 아래의 "다음 100개 보기" 버튼이 다음 페이지를 표에 덧붙입니다. `truncated: true`는 이 응답에 일치하는 결과가 다 들어 있지 않다는 뜻이라 다음 페이지가 있을 때도 켜집니다. 마지막 페이지에서도 켜져 있으면 검색 한도에 걸렸거나 파일이 50개를 넘는 컴포넌트가 있는 것이고("컴포넌트는 파생 값" 참고), 화면에 "검색 한도에 걸려 일부 결과가 빠졌을 수 있습니다" 안내가 나옵니다. 결과 표의 열은 Name, Group, Version, Format, Repository, Last Updated이고, 행을 누르면 컴포넌트 상세(Summary, Usage, Assets)와 파일 상세로 들어갑니다. 일치하는 항목이 없으면 "No components matched the filter criteria"가 나옵니다.

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
- **PyPI, Cargo**: hosted 저장소는 packages 테이블의 이름과 버전. PyPI는 파일 이름에서 표시 버전을 뽑습니다. Cargo는 packages 테이블과 경로에 소문자로 저장한 이름 대신 올린 표기(대소문자 포함)를 보여 줍니다.
- **프록시 저장소(npm, PyPI, Cargo)**: packages 테이블에 행이 없으므로 캐시된 파일의 경로(assets 테이블)에서 같은 방식으로 파생합니다. Cargo는 경로에 이름이 소문자로 남으므로, 캐시해 둔 인덱스 파일(`index/<인덱스 경로>`) 첫 줄의 `name`에서 올린 표기를 읽어 보여 줍니다. 보여 주는 컴포넌트마다 그 파일의 앞부분(최대 4 KiB)만 읽고 원격에는 묻지 않습니다. 인덱스 파일이 캐시에 없거나 첫 줄의 첫 키가 `name`이 아니면(crates.io와 cargo는 늘 `name`을 먼저 씁니다) 경로의 소문자 이름을 씁니다.
- 검색은 메타 DB에서 한도까지만 훑습니다: assets 테이블 질의 한 번마다 최대 2000개 파일, packages 테이블은 이름 2000개와 버전 2000개까지 봅니다(`keyword`만 주면 이름으로 좁힌 검사를 먼저 하고, 한도가 남으면 버전 일치를 찾는 전체 검사를 이어서 합니다). 한도에 걸리거나 10초 안에 끝나지 않으면 그때까지의 결과를 `truncated: true`로 돌려줍니다. 페이지마다 같은 한도 안에서 처음부터 다시 훑고 정렬한 뒤 잘라 내므로, 데이터가 바뀌지 않는 한 페이지가 겹치거나 빠지지 않습니다(10초 제한에 걸린 경우는 예외). 대신 한도 밖의 결과는 페이지를 넘겨도 나오지 않으니 조건을 좁히세요. 검색은 무거운 질의용 세마포어 안에서 실행하므로 메모리 사용이 일정합니다.

### 컴포넌트 삭제

hosted 저장소에 `delete` 권한이 있으면 잘못 올라간 버전을 바로 지울 수 있습니다. npm packument와 dist-tag, PyPI 프로젝트 페이지, Go `@v/list`와 `@latest`, Cargo 인덱스가 바로 바뀌고 그 저장소의 파일 URL은 404가 됩니다. group은 멤버를 그때그때 읽으므로 그 저장소를 멤버로 둔 group에도 바로 반영됩니다. 삭제마다 활동(Events)에 `delete`가 남고(지운 사람, 대상, 지운 파일 수), blob 파일은 다음 GC(Compact blob store)에서 회수됩니다. `writePolicy: deny`(읽기 전용)이거나 Offline인 저장소에서는 지울 수 없습니다.

- **웹 UI**: 탐색과 검색의 컴포넌트 화면과 파일 화면에 "Delete component"가 나옵니다. Maven은 파일을 하나씩 `DELETE`하고, 파일 화면의 "Delete asset"으로 그 파일만 지울 수도 있습니다. npm·PyPI·Go·Cargo는 서버가 그 버전의 파일을 한 번에 모두 지웁니다(npm은 tarball, PyPI는 그 버전의 wheel과 sdist 전부, Go는 `.mod`·`.info`·`.zip`, Cargo는 `.crate`와 인덱스 줄). 화면 목록이 잘려 일부 파일이 보이지 않아도 모두 지웁니다.
- **API**: `DELETE /-/api/repositories/<저장소>/components?group=<group>&name=<이름>&version=<버전>`. `group`은 Maven의 groupId(필수)나 npm scope(`@scope`, 없으면 비움)이고, 다른 포맷은 비웁니다. `name`과 `version`은 컴포넌트 검색이 돌려주는 값입니다. 다른 쓰기 API처럼 `X-Command-Center-UI: 1` 헤더가 필요하며 API 토큰으로도 됩니다. 성공하면 200과 `{"assets": <지운 파일 수>, "bytes": <크기>}`입니다. `delete` 권한이 없으면 403, hosted가 아니거나 `writePolicy: deny`이거나 인자가 빠지면 400, Offline이면 503, 그런 버전이 없으면 404입니다.

  ```bash
  curl -u admin -X DELETE -H 'X-Command-Center-UI: 1' \
    'http://localhost:8081/-/api/repositories/pypi-hosted/components?name=demo-pkg&version=1.0'
  ```

- **클라이언트**: npm은 `npm unpublish <패키지>@<버전>`과 `npm unpublish <패키지> --force`("npm 클라이언트 설정" 참고), Go는 버전의 `.mod`·`.info`·`.zip` 중 하나에 보내는 `DELETE`("Go 모듈 설정" 참고)로 지웁니다. 둘 다 group으로 보내면 `writeMember`에서 지웁니다. twine과 cargo에는 삭제 명령이 없으므로 PyPI와 Cargo는 웹 UI나 API를 씁니다.
- 지운 버전과 같은 번호로 다시 올릴 수 있습니다. 하지만 내용이 다르면 이미 받아 둔 쪽의 체크섬(npm `package-lock.json`의 `integrity`, Go `go.sum`, `Cargo.lock`)과 맞지 않아 설치가 실패하므로 새 버전 번호를 쓰세요.
- proxy 저장소의 캐시는 이 기능으로 지우지 않습니다(Cleanup 정책을 쓰세요). 웹 UI와 API로는 group에서 지울 수 없으니 멤버 저장소에서 지우세요.

### 태그와 스테이징

태그는 이름과 자유 속성(JSON 객체)을 가진 라벨이며 컴포넌트(저장소·그룹·이름·버전)에 붙습니다(`/-/api/tags`, Nexus 호환 `/service/rest/v1/tags`). 태그 만들기·수정·삭제는 관리자만 할 수 있고, 컴포넌트에 붙이거나 떼려면(`POST`·`DELETE /service/rest/v1/tags/associate/<태그>?<검색 조건>`) 관련된 모든 저장소에 edit 권한이 있어야 합니다. 대상은 Nexus 검색 API와 같은 매개변수(repository, format, group, name, version, q, tag, maven.* 등, `*` 와일드카드)로 고르며, 검색 결과와 정확히 같은 컴포넌트가 선택됩니다. 웹 UI·API·Cleanup·스테이징으로 컴포넌트를 지우거나 태그를 지우면 연결도 함께 사라집니다. 검색은 `tag=`로 걸러 낼 수 있고 각 컴포넌트에 `tags`가 표시됩니다.

스테이징 이동(`POST /service/rest/v1/staging/move/<대상>?repository=<원본>&tag=<태그>`, 또는 `POST /-/api/staging/move?destination=<대상>&repository=<원본>&…`)은 같은 포맷의 hosted 저장소 사이에서 컴포넌트를 옮깁니다. 컴포넌트마다 행과 파일을 대상에 복사하고(두 저장소의 blob store가 다를 때만 바이트를 복사하고 sha256을 확인합니다) 대상의 메타데이터(maven-metadata.xml, npm 패키지 문서, helm index.yaml, yum repodata 등)를 다시 만든 뒤, 원본에서는 포맷의 삭제 기능으로 지웁니다. 태그도 함께 옮겨집니다. 원본에는 읽기와 삭제 권한, 대상에는 추가 권한이 필요하고, 대상의 writePolicy가 allow일 때 기존 버전을 덮어쓰려면 edit 권한도 필요합니다. 컴포넌트마다 따로 처리하므로, 실패한 컴포넌트는 원본에 그대로 남고 응답에 사유가 표시되며(HTTP 409, 나머지는 옮겨집니다). 지원 포맷은 maven, npm, raw, pypi, nuget, helm, yum이며 docker 등 나머지는 400으로 거부합니다. `POST /service/rest/v1/staging/delete?repository=<저장소>&...`는 고른 컴포넌트를 삭제합니다(delete 권한). 한 요청은 최대 5000개 컴포넌트를 처리하며, repository 외에 tag나 검색 조건이 하나 이상 있어야 합니다. 활동 이벤트는 `tag.*`, `staging.move`, `staging.delete`입니다.

- npm은 옮긴 버전의 게시 시각을 그대로 두고, 대상의 `latest`만 갱신합니다(다른 dist-tag는 옮기지 않습니다). PyPI와 npm은 writePolicy와 관계없이 이미 있는 버전을 덮어쓰지 않습니다.
- 프로토콜 클라이언트로 지운 컴포넌트(`npm unpublish`, Maven·raw `DELETE`, docker 매니페스트 `DELETE`)는 태그 연결이 남으므로, 같은 버전을 다시 올리면 태그가 다시 붙습니다.
- 어느 한쪽 저장소의 blob store 이동이 끝나지 않았으면 스테이징은 409입니다("저장소의 blob store 바꾸기" 참고).

### 관리자 화면

- **Repositories**: 목록, 상세, 만들기·편집·삭제(아래 "저장소 관리"). Name, Format, Type, URL, Online(Online/Offline 전환), Blob Store, Remote, Members, 저장소 크기, 그리고 versionPolicy, writePolicy, metadataMaxAge, contentMaxAge, negativeCacheTTL(설정값).
- **Users**: 사용자 목록·만들기·편집·삭제·비밀번호 재설정·API 토큰 모두 폐기(User ID, Source, Roles, Status). LDAP 로그인 사용자는 최근에 로그인한 사용자만 `directory` 행으로 보입니다("인증과 권한", "Realm, LDAP, OIDC, SAML" 참고). 비밀번호 해시는 절대 내보내지 않습니다.
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
- 라이선스 관리 화면, 유료 에디션 전용 화면. 에디션이 하나뿐이라 Nexus Pro의 태그·스테이징, SAML, 사용자 토큰, 취약점 데이터("취약점 데이터와 방화벽(OSV)" 참고)도 모두 같은 바이너리에 들어 있습니다.

### 주의할 점

- **세션은 메모리에만 있습니다.** 서버를 재시작하면 모두 로그아웃됩니다. 세션은 최대 1000개(사용자당 20개), 마지막 사용 후 유휴 시간(일반 설정의 `sessionIdleMinutes`, 기본 720분 = 12시간) 또는 로그인 후 7일이 지나면 만료됩니다. 자리가 없으면 만료된 세션을 먼저 지우고, 그다음 로그인하는 사용자 자신의 가장 오래 쓰지 않은 세션을 닫습니다. 그 사용자의 세션이 하나도 없을 때만 전체에서 가장 오래 쓰지 않은 세션을 닫으므로, 한 사용자가 로그인을 반복해도 다른 사용자의 세션을 계속 밀어내지는 못합니다. 비활성화하거나 삭제한 사용자, 비밀번호를 바꾼 사용자의 세션은 즉시 무효가 됩니다(`user reset-password`처럼 다른 프로세스가 DB를 바꾼 경우에는 5초 안에).
- 세션 쿠키는 `HttpOnly`, `SameSite=Strict`입니다. `baseURL`이 `https://`면 이름이 `__Host-cc_session`이고 `Secure`, `Path=/`에 `Domain`이 없어서, 같은 도메인의 다른 호스트가 이 쿠키를 심거나 가릴 수 없습니다. `http://`면 `cc_session`입니다. 현재 scheme의 이름만 인정하므로 `baseURL`을 http와 https 사이에서 바꾸면 UI에 다시 로그인해야 합니다. 같은 이름의 쿠키가 여러 개 오면(http에서는 같은 도메인의 다른 호스트가 끼워 넣을 수 있습니다) 살아 있는 세션을 가리키는 쿠키만 보고, 서로 다른 사용자의 세션이 섞여 있으면 어느 쪽도 인정하지 않습니다. 다시 로그인하면 요청에 실린 쿠키의 세션을 모두 끝내므로 풀립니다. 쓰기 요청(PUT/POST/DELETE)은 `X-Command-Center-UI: 1` 헤더가 있어야 쿠키가 인정됩니다. 작업 실행(Tasks의 Run)은 추가로 관리자이며 비밀번호 또는 세션으로 인증한 경우에만 됩니다(토큰으로는 불가). **https 리버스 프록시 뒤에서 운영하면 `baseURL`을 `https://…`로 설정하세요.** 그래야 쿠키에 `Secure` 플래그와 `__Host-` 접두사가 붙습니다.
- 쿠키는 `Authorization` 헤더가 없는 요청에서만 쓰이므로 `mvn`, `npm` 같은 클라이언트에는 영향이 없습니다.
- 삭제 버튼은 hosted 저장소에 `delete` 권한이 있고, 저장소가 Online이며 `writePolicy`가 `deny`가 아닐 때만 나옵니다(저장소 API의 `canDelete`). Maven은 파일 단위, 다른 포맷은 버전 단위로 지웁니다("컴포넌트 삭제" 참고).
- 목록은 모두 상한이 있습니다(디렉터리 500, 검색 한 페이지 100, 패키지 버전 1000, 토큰 1000). 넘으면 화면에 안내 문구가 나오고, 검색은 "다음 100개 보기"로 이어서 봅니다.

## 저장소 관리

관리자는 저장소 설정에서 저장소를 만들고, 고치고, 지울 수 있습니다. 서버를 재시작하지 않아도 바로 반영됩니다.

> **경고: `config.yaml`의 `repositories:`는 첫 시작 때 한 번만 읽습니다.** 저장소 정의는 DB로 옮겨지고, 그 뒤로는 `config.yaml`의 `repositories:`를 고쳐도 **무시**됩니다(시작할 때 파일 이름을 밝히는 경고 로그가 남습니다). 저장소는 UI(또는 `/-/api/repositories` API)로 바꾸세요. 사용자, 역할, 일반 설정, 라우팅 규칙, 컨텐츠 셀렉터도 첫 시작 때 한 번만 가져오며 이후 UI에서 관리합니다("업그레이드" 참고).

- **만들기**: "Create repository"를 누르면 레시피 선택 화면(`maven2`, `npm`, `pypi`, `go`, `cargo` 각각 hosted, proxy, group)이 나오고, 고르면 해당 유형의 폼이 열립니다. 검증은 `config.yaml`과 같고 오류는 폼 맨 위에 표시됩니다. 새 저장소 이름은 영문자와 숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만 쓸 수 있으며 최대 64자입니다. `create`는 예약어라 쓸 수 없습니다(이미 저장된 이름은 그대로 동작합니다).
- **편집**: `name`, `format`, `type`은 만든 뒤 바꿀 수 없습니다(폼에서 비활성). 나머지(remote, 인증, 최대 보관 시간, negative cache TTL, 라우팅 규칙(routingRule), group 멤버와 순서, writeMember, versionPolicy, 배포 정책(writePolicy), anonymousRead, Online/Offline)는 고칠 수 있습니다. 저장소의 Blob Store(`blobStore`)는 편집으로는 바꿀 수 없고 `repository.change-blobstore` 작업으로 옮깁니다("저장소의 blob store 바꾸기" 참고). proxy의 `remote`를 바꿔도 캐시된 파일은 유지되지만, 이전 remote에서 받은 "없음" 기록(negative cache)은 지워집니다.
- **삭제**: 저장소 이름을 직접 입력해야 삭제됩니다. 다음 규칙이 있습니다.
  - group의 멤버(또는 writeMember)인 저장소는 409로 거부하며 메시지에 group 이름이 나옵니다. group에서 먼저 빼세요.
  - 삭제하면 그 저장소의 정의와 자산, 패키지, dist-tag, negative cache 행이 한 트랜잭션으로 지워집니다. blob 파일은 다음 GC(Compact blob store)에서 회수됩니다.
  - 같은 트랜잭션에서 모든 역할의 그 저장소 권한(대상이 정확히 `repo:<이름>`인 것)도 지웁니다. 그래서 같은 이름으로 저장소를 다시 만들어도 이전 권한이 따라오지 않습니다. `*`, `format:<포맷>`, 다른 저장소 대상은 그대로이고, 권한이 모두 빠진 역할도 지우지 않습니다. 웹 UI의 삭제 확인 창에 권한이 빠질 역할이 나옵니다. 권한을 지운 역할이 있으면 저장소 이름과 역할을 적은 WARN 로그(`removed the privileges on a deleted repository from roles`) 한 줄과, 역할마다 활동 기록의 `role.update` 이벤트(`removed repo:<이름>`)가 남습니다. 저장소를 지웠다가 같은 이름으로 다시 만드는 스크립트는 역할에 권한을 다시 넣어야 합니다.
  - 이전 버전에서 저장소를 지울 때 남은 권한은 그대로 있고, 서버가 시작할 때 역할과 저장소마다 경고 로그(`role grants privileges on a repository that does not exist`)가 남습니다. 같은 이름으로 저장소를 다시 만들면 그 권한이 새 저장소에 적용되므로, 필요 없으면 역할에서 빼세요.
- **입력 검사**: 저장소를 만들거나 편집할 때(UI·API, 첫 시작의 config 가져오기) group의 멤버가 중복되면 거부하고(`duplicate member`), 시간 값(`metadataMaxAge`, `contentMaxAge`, `negativeCacheTTL`)에 음수(`-5m`)를 쓰면 거부합니다(400). "영원히"는 `-1`로만 씁니다. `remote`와 `remoteAuth`는 proxy에만 쓸 수 있고 `versionPolicy`는 group에 쓸 수 없습니다(400). 웹 UI의 Maven group 폼에는 versionPolicy 칸이 없습니다. 이전 버전이 이미 저장한 정의는 그대로 읽습니다. 음수 시간은 0(캐시하지 않음, 즉시 만료)으로, 중복 멤버는 한 번만 쓰고, hosted·group의 `remote`·`remoteAuth`와 group의 `versionPolicy`는 쓰이지 않던 값이라 버리며, 시작 시 저장소마다 경고를 한 줄 남깁니다. 그런 저장소가 있어도 서버는 시작하고 다른 저장소의 편집도 막지 않으며, 그 저장소를 편집할 때 값을 고치면 됩니다. 저장소를 만들거나 지우면 목록의 개수·크기 통계(약 30초 캐시)도 바로 다시 계산하므로, 같은 이름으로 다시 만든 빈 저장소가 이전 값을 보여 주지 않습니다.
- **원격 인증 정보**: proxy의 비밀번호와 Bearer 토큰은 DB에 저장되지만(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) **어떤 API와 화면에도 다시 보이지 않습니다**(설정되어 있는지 여부만 표시). 편집 때 비워 두면 기존 값을 유지하고, "clear" 체크로 지웁니다. `remote`의 **호스트가 바뀌면 인증 정보를 다시 입력해야 합니다**(토큰을 새 호스트로 옮길 수 없습니다).
- **권한**: 만들기, 편집, 삭제는 관리자이면서 비밀번호 또는 세션으로 인증한 경우에만 됩니다(API 토큰으로는 403). 쓰기 요청에는 `X-Command-Center-UI: 1` 헤더가 필요합니다.
- **백업**: 저장소 정의가 DB에 있으므로 **백업에 DB(`pg_dump`)가 반드시 포함되어야 합니다**("백업과 복원" 참고). 정의 없이 blob만 복원하면 저장소가 없는 상태가 됩니다.

### 라우팅 규칙

라우팅 규칙은 proxy 저장소가 원격에 **어떤 경로를 물어볼 수 있는지** 정합니다(Nexus의 Routing Rules). 규칙은 이름, 설명, 모드, 매처(정규식) 목록으로 이루어지며, proxy 저장소 설정의 `routingRule`에 규칙 이름 하나를 연결합니다. Nexus는 Java 정규식을 쓰지만 Command Center는 Go의 RE2 문법입니다(후방 참조·lookaround 없음). 매처는 요청 경로(`/repository/<저장소>/` 뒤, 앞에 `/`를 붙인 형태, 예 `/org/acme/lib/1.0/lib.jar`)와 `MatchString`으로 비교하므로 앞뒤를 고정하려면 `^`, `$`를 쓰세요.

- **BLOCK**: 매처 중 하나라도 맞는 경로는 원격에 보내지 않습니다. 나머지는 통과합니다.
- **ALLOW**: 매처에 맞는 경로만 원격에 보내고 나머지는 모두 막습니다.
- 막힌 경로는 **캐시를 확인하기 전에** 거부하므로, 이미 캐시된 파일도 규칙이 생기면 더 이상 내려받을 수 없습니다. proxy에 직접 요청하면 권한 검사 뒤에 403(`blocked by routing rule`)이고, group을 통해 오면 그 멤버는 그 경로를 "없는 것"으로 보고 다음 멤버로 넘어갑니다. Docker(`/v2/`)는 proxy 직접 요청도 404입니다.
- 규칙 이름은 1~64자의 영문자, 숫자, `.`, `_`, `-`이고 매처는 1~50개, 각 256자 이하의 올바른 정규식이어야 합니다(400). proxy가 쓰는 규칙은 지울 수 없습니다(409, 메시지에 저장소 이름). proxy 설정에서 없는 규칙 이름을 쓰면 400이고 hosted·group에는 `routingRule`을 쓸 수 없습니다.
- 관리 화면(또는 `/-/api/routing-rules`)의 "테스트"로 경로 하나를 넣어 통과 여부와 어떤 매처가 결정했는지 확인할 수 있습니다. 변경은 활동 기록에 `routingrule.create/update/delete`로 남습니다.
- `config.yaml`의 `routingRules:`와 저장소의 `routingRule:`은 사용자·역할처럼 **첫 시작 때 한 번** 가져오며 이후 UI에서 관리합니다. 없는 규칙을 가리키는 저장소는 시작 시 경고 로그를 남기고 모든 경로를 원격에 보냅니다.
- Nexus REST API: `GET/POST/PUT/DELETE /service/rest/v1/routing-rules[/{name}]`(`name, description, mode, matchers`), 저장소 본문의 `routingRule`(응답에는 `routingRuleName`도 같이).

예: 사내 아티팩트가 공개 저장소로 새어 나가지 않게 막기
```yaml
routingRules:
  - name: no-internal
    description: 사내 그룹은 원격에 묻지 않음
    mode: BLOCK
    matchers: ["^/com/mycompany/", ".*-SNAPSHOT.*"]
repositories:
  - name: maven-central
    format: maven
    type: proxy
    remote: "https://repo1.maven.org/maven2/"
    routingRule: no-internal
```

### 풀 복제(pull replication)

다른 Command Center나 Nexus Repository 인스턴스(원본)의 hosted 저장소에 올라온 파일을 이 인스턴스(대상)의 proxy 저장소가 **요청이 오기 전에 미리** 받아 둡니다. Nexus Pro의 "Pre-emptive Pull"(content replication)에 해당하며 Command Center에서는 무료입니다. 멀리 떨어진 사이트에서도 첫 다운로드를 기다리지 않고, 원본이 멈춰도 받아 둔 파일은 대상이 계속 내려줍니다.

- **설정**: 대상에 proxy 저장소를 만들고 `remote`를 원본 hosted 저장소의 URL(`<원본 base URL>/repository/<저장소>/`, 예 `https://nexus-a.example.com/repository/maven-releases/`)로 둡니다. 원본에 로그인할 계정은 proxy의 원격 인증(`remoteAuth`)에 넣습니다. 이 계정에는 원본 저장소의 읽기 권한이 있어야 합니다. 비밀번호와 토큰은 다른 원격 인증처럼 암호화해 저장하고 어떤 API에도 다시 보이지 않습니다. 그다음 proxy 설정의 `replication`에서 `preemptivePullEnabled`를 켭니다. `assetPathRegex`(RE2 정규식, 비우면 전부)로 복제할 경로를 좁힐 수 있으며, 경로 앞 `/`의 유무와 관계없이 맞춥니다(예 `^com/acme/`, `\.jar$`). `verifyChecksums`를 켜면 받은 파일의 체크섬이 원본 목록과 다를 때 그 파일을 지우고 실패로 셉니다.
- **연결 테스트**: 저장소 설정의 연결 테스트(`POST /-/api/repositories/<이름>/replication/test`)는 원본이 응답하는지, 인증이 통하는지, 같은 포맷의 hosted 저장소인지 확인합니다.
- **실행**: 예약 작업 `repository.pull-replication`(Pull replication)을 만듭니다. `repository`를 비우면 복제가 켜진 모든 proxy가, 이름을 주면 그 proxy 하나가 대상입니다. 가장 짧은 주기는 `hourly:<분>`입니다. 작업은 원본의 `GET /service/rest/v1/assets?repository=<저장소>`(Nexus REST API이며 Command Center도 제공합니다)를 끝까지 읽고, proxy가 같은 체크섬으로 아직 갖고 있지 않은 컴포넌트 파일을 proxy 자신의 경로로 요청합니다. 그래서 라우팅 규칙, 방화벽(OSV), 내용 검사, blob store와 쿼터, 활동 기록이 클라이언트 요청과 똑같이 적용되고, 다운로드 수에는 들어가지 않습니다. 한 번에 4개씩 받습니다.
- **증분**: 끝까지 성공한 실행은 원본 목록에서 본 가장 새로운 `lastModified`를 DB에 기록하고, 다음 실행은 그 시각 1시간 전 이후에 바뀐 파일만 확인합니다. 실패한 파일(5xx, 네트워크 오류, 체크섬 불일치)이 있으면 그 파일의 시각부터 다시 봅니다. `full: true`로 실행하면 기록을 무시하고 모든 파일을 확인하되, 이미 있는 파일은 다시 받지 않습니다. proxy를 다시 만들거나 `remote` 또는 정규식을 바꾸면 처음부터 다시 봅니다. 진행 기록이 DB에 있으므로 여러 노드 중 어느 노드가 다음 실행을 맡아도 이어집니다.
- **로그**: 작업 로그에 proxy마다 한 줄 요약이 남습니다(목록, 대상, 받음과 바이트, 이미 있음, 지난 실행 이후 변경 없음, 원본에서 바뀜, 거부(4xx: 라우팅 규칙, 방화벽, 검증), 실패). 파일별 메시지는 종류마다 20줄까지 남습니다. 실패가 하나라도 있으면 작업이 실패로 끝나므로 실패 알림 메일을 받을 수 있습니다.
- **복제하지 않는 것**: 컴포넌트 파일만 복제합니다. `maven-metadata.xml`, `index.yaml`, repodata 같은 색인 문서는 proxy가 평소처럼 처음 쓸 때 받아 `metadataMaxAge`에 따라 갱신하고, 원본이 멈추면 캐시된 것을 내려줍니다. 원본에서 지운 파일은 대상에서 지우지 않으므로 대상의 cleanup 정책을 쓰세요. 이미 받은 파일이 원본에서 바뀌면(덮어쓰기) proxy의 `contentMaxAge`가 지날 때까지 기존 파일을 유지합니다. 설정, 사용자 같은 구성은 복제하지 않습니다.
- **지원 포맷**: maven, npm, pypi, nuget, rubygems, helm, yum, apt, raw, go, cargo, conda, r, composer, conan, swift, terraform입니다. docker는 이미지를 이름과 태그로 받으므로 지원하지 않고, p2, cocoapods, huggingface는 원본이 될 hosted 저장소가 없어 지원하지 않습니다(400). cargo, terraform, composer 원본은 내려주는 다운로드 주소를 `baseURL`로 만들므로, 원본의 `baseURL`이 대상에서 접근할 수 있는 주소여야 합니다.
- **Nexus REST API**: 저장소 본문의 `replication`(`preemptivePullEnabled`, `assetPathRegex`, Command Center 전용 `verifyChecksums`)을 받고 돌려줍니다. 원본이 Nexus일 때는 저장소 경로 배치가 Command Center와 같은 포맷(maven, npm, pypi, raw, helm, rubygems, yum, r)에서 동작하며, 실제 Nexus 원본으로는 아직 시험하지 않았습니다.

## 업로드

사이드바 Upload(또는 Browse의 "Upload component")에서 브라우저로 파일을 올릴 수 있습니다. 쓰기 권한이 있는 hosted Maven, npm, PyPI 저장소가 있을 때만 보입니다. 브라우저가 각 클라이언트와 같은 요청을 만들어 기존 저장소 엔드포인트로 보내므로 검증과 쓰기 정책(`writePolicy`)은 서버 규칙 그대로입니다.

| 포맷 | 필요한 것 |
|---|---|
| Maven | 파일 하나 이상(행마다 Classifier, Extension. 확장자는 파일 이름에서 채움), Group ID, Artifact ID, Version, Packaging. "Generate a POM file"을 켜면 최소 POM을 만들어 같이 올립니다. 선택한 파일 중 `.pom`이 있으면 좌표를 읽어 채우고 고정합니다. 파일마다 `PUT`하며 순서대로 올리다 첫 실패에서 멈춥니다. |
| npm | `.tgz` 하나. 브라우저가 압축을 풀어 `package/package.json`에서 이름과 버전을 읽어 보여 주고(1 MiB 제한), 서버도 저장한 tarball에서 `package.json`을 다시 읽어 검사합니다("npm 클라이언트 설정" 참고). `npm publish`와 같은 publish 문서로 올리므로 문서 한도 10 MiB가 적용되며 큰 tarball은 경고가 나옵니다. |
| PyPI | wheel 또는 sdist(`.tar.gz`, `.zip`) 하나. 이름과 버전을 파일 이름에서 읽으며 고칠 수 있지만, 서버가 파일 안의 메타데이터(`METADATA`, `PKG-INFO`)와 비교하므로 실제 패키지와 다르면 400입니다. `twine`과 같은 multipart 업로드이고 sha256은 https 또는 localhost(보안 컨텍스트)에서 접속했을 때만 브라우저가 계산하고, 그렇지 않으면 서버가 계산합니다. |

성공하면 "Upload successful"과 Browse 링크가 나옵니다. 이미 있는 파일을 다시 올리면 포맷별 규칙대로 오류가 인라인으로 표시됩니다(Maven `allow_once` 400, npm 403, PyPI 400). 파일은 순서대로 하나씩 올리며, 서버는 기존 스트리밍 경로를 그대로 쓰므로 큰 파일도 메모리에 통째로 올리지 않습니다(npm publish 문서만 10 MiB까지 읽고, 동시 처리는 2개로 제한됩니다). npm publish 슬롯이 모두 사용 중이면 30초까지 기다리다가 `503 too many concurrent publishes, retry`로 거절하므로 잠시 후 다시 시도하세요. 슬롯을 잡은 채 본문 전송이 5분 넘게 멈추면 연결이 끊깁니다. 다른 포맷의 업로드도 마찬가지로 본문을 읽는 중에 60초 동안 한 바이트도 오지 않으면(느려도 계속 오는 전송은 끊지 않습니다) **408**로 답하고(Cargo publish의 메타데이터 부분은 400입니다. Maven `maven-metadata.xml`은 계속 조금씩 오더라도 10초 안에 다 오지 않으면 408입니다) 연결을 끊어, 멈춘 업로드가 같은 경로의 잠금을 붙들고 있지 못하게 합니다. 서버가 본문을 읽기 전에 잠금을 기다리는 시간이나 본문이 없는 요청(DELETE, Cargo yank)은 이 제한에 들어가지 않습니다. 이 제한은 인증보다 먼저 본문이 있는 모든 요청(웹 UI의 `/-/api/…` 포함)에 걸리므로, 401·403·404·503으로 거절된 요청도 본문을 멈춘 채 연결을 붙들 수 없습니다. 서버가 끝까지 읽지 않은 본문은 연결을 다시 쓰려고 남은 부분(최대 256 KiB)을 최대 5초만 기다리고, 그 안에 다 오지 않으면 응답 뒤 연결을 닫습니다. 로그인하지 않은 요청의 본문(웹 UI 로그인, `npm login`)은 바이트가 계속 오더라도 요청 헤더를 받은 뒤 5초 안에 모두 와야 하며, 넘으면 요청을 거절하고 연결을 닫습니다. 익명으로는 업로드할 수 없으므로 인증된 요청인 업로드에는 이 제한이 없습니다.

**업로드가 401로 실패할 때**: 브라우저가 `/repository` 경로에 대해 Basic 인증 정보를 캐시해 두었다면(예: 그 주소를 직접 열어 로그인한 경우) 그 값이 UI 세션보다 우선하므로 업로드가 401로 실패할 수 있습니다. 브라우저에 저장된 해당 사이트의 인증 정보를 지우고 다시 시도하세요.

## 설정 레퍼런스

설정 파일은 YAML이며 알 수 없는 키가 있으면 시작이 실패합니다. 설정을 바꾼 뒤에는 `docker compose restart`로 반영하세요.

**한 번만 가져오는 키**: `users`, `roles`, `anonymousRead`, `baseURL`, `maxUploadMiB`는 **첫 시작 때 한 번만** 읽어 DB로 가져옵니다. 이후에는 파일에 남아 있어도 무시되며(시작할 때마다 경고가 한 줄 남습니다) 웹 UI에서 관리합니다("업그레이드"의 이전 안내 참고). `config.yaml`에 계속 쓰는 키는 `listen`, `dataDir`, `database`, `memoryLimitMiB`, `metricsFlushSeconds`, `secretKeyFile`, `exportRoot`뿐입니다(`repositories`도 첫 시작에만 가져옵니다).
전체 예시는 `config.example.yaml`을 참고하세요.

### 최상위 키

| 키 | 타입 | 기본값 | 설명 |
|---|---|---|---|
| `listen` | 문자열 | `":8081"` | 수신 주소. 컨테이너 안에서는 8081을 유지하세요(헬스체크가 127.0.0.1:8081을 확인). |
| `baseURL` | URL | `"http://localhost:8081"` | (가져온 뒤 UI의 일반 설정에서 관리) 클라이언트가 접속하는 주소. npm·Cargo 응답의 다운로드 URL이 이 값으로 만들어집니다(PyPI 파일 링크는 호스트 없는 경로이며 이 값의 경로 부분만 앞에 붙습니다). 끝의 `/`는 무시되고, 사용자 정보(`user:pass@`), 쿼리(`?…`), 프래그먼트(`#…`)가 없는 `http(s)` URL이어야 합니다. 이 값은 클라이언트에게 주는 URL의 앞부분이 되므로 사용자 정보가 있으면 그 자격 증명이 모든 클라이언트에게 넘어가고, `?`나 `#`가 있으면 URL이 깨집니다. |
| `dataDir` | 경로 | `"/data"` | blob(default blob store)과 업로드 임시 파일을 저장하는 디렉터리. compose에서는 named volume입니다. |
| `database.url` | 문자열 | 없음(필수) | 메타데이터 DB(PostgreSQL 13 이상)의 `postgres://` 또는 `postgresql://` URL. 환경 변수 `COMMANDCENTER_DATABASE_URL`로 대신 줄 수 있고, 둘 다 있으면 시작하지 않습니다. 둘 다 없으면 `no database configured: set database.url (or COMMANDCENTER_DATABASE_URL) to a PostgreSQL URL`로 시작하지 않습니다. 로그와 오류에 URL은 남지 않습니다. "메타데이터 데이터베이스" 참고. |
| `database.maxConns` | 정수 | `10` | 연결 풀 크기(1..1000). |
| `memoryLimitMiB` | 정수 | `80` | Go 메모리 한도(MiB). 환경변수 `GOMEMLIMIT`가 있으면 무시됩니다(compose 기본값은 `GOMEMLIMIT: 80MiB`). |
| `maxUploadMiB` | 정수 | `1024` | (가져온 뒤 UI에서 관리, 바로 적용) GET/HEAD가 아닌 요청 본문의 최대 크기(MiB). 허용 범위 1..1048576, 0이면 기본값. 초과하면 모든 포맷에서 413. 포맷별로 더 작은 제한이 있으면 그것이 먼저 적용됩니다. |
| `anonymousRead` | 불리언 | `false` | (가져온 뒤 UI에서 관리) 인증 없는 다운로드 허용 여부. 저장소별로 덮어쓸 수 있습니다. |
| `metricsFlushSeconds` | 정수 | `30` | (숨은 설정) 다운로드 수와 요청 통계를 DB에 반영하는 주기(초). 허용 범위 1..3600. "다운로드 수·요청 추이" 참고. |
| `secretKeyFile` | 경로 | 없음 | 저장된 비밀값(SMTP 비밀번호, LDAP bind 비밀번호, OIDC client secret, Crowd 애플리케이션 비밀번호, SAML SP 개인키, proxy `remoteAuth`, S3 blob store의 `secretAccessKey` 등)을 암호화하는 키가 든 파일. 환경변수 `COMMANDCENTER_SECRET_KEY`로 줄 수도 있으며, 둘 다 주면 시작하지 않습니다. 없으면 비밀값을 평문으로 저장합니다. "저장된 비밀값 암호화" 참고. |
| `exportRoot` | 경로 | 없음 | 저장소 내보내기·가져오기 작업이 읽고 쓰는 디렉터리(절대 경로). 환경변수 `COMMANDCENTER_EXPORT_ROOT`로 줄 수도 있으며, 둘 다 주면 시작하지 않습니다. 비우면 두 작업을 쓸 수 없습니다("저장소 내보내기와 가져오기" 참고). |
| `users` | 목록 | 없음 | (첫 시작 가져오기 전용) 사용자: `name`, `passwordHash`(bcrypt), `roles`. 가져온 사용자는 로컬 사용자가 됩니다. bcrypt 해시가 아닌 값(예: `config.example.yaml`의 `<commandcenter hash-password 출력값>`)은 비밀번호가 없는 것으로 봅니다. 그 사용자는 비밀번호로 로그인할 수 없고, 사용자 API의 `hasPassword`는 `false`이며, 서버가 시작할 때마다 그런 활성 사용자마다 WARN 로그(`local user cannot sign in with a password`)가 남습니다. 웹 UI나 `user reset-password`로 비밀번호를 정하세요. |
| `roles` | 맵 | 없음 | (첫 시작 가져오기 전용) 역할 이름 → `write: [저장소 이름…]`. `"*"`는 모든 저장소. 가져올 때 항목마다 `repo:<이름>`(또는 `*`) 대상에 browse·read·add·edit·delete 전부를 주는 권한이 됩니다(예전의 쓰기는 삭제를 포함했습니다). |
| `repositories` | 목록 | 없음 | 저장소 정의(아래 표). **첫 시작 때만** 읽어 DB로 가져옵니다. 이후에는 무시되므로 UI에서 관리합니다("저장소 관리" 참고). |

`admin` 역할은 내장되어 있어 `roles`에 정의하지 않아도 되며 모든 저장소의 모든 동작과 관리 화면, `/-/metrics` 조회가 가능합니다. `roles`에 `admin`을 적어도 무시하고 내장 역할을 씁니다.

### 저장소 키

아래 키는 UI 폼과 `config.yaml`(첫 시작 가져오기)에서 같은 의미입니다.

| 키 | 대상 | 기본값 | 설명 |
|---|---|---|---|
| `name` | 모두 | 필수 | 저장소 이름. URL의 `/repository/<name>/`. `/`, 공백, `\` 불가, 중복 불가. UI/API로 만들 때는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `-`만, 최대 64자, `create`는 예약어. |
| `format` | 모두 | 필수 | `maven`, `npm`, `pypi`, `go`, `cargo`, `docker`, `raw`, `helm`, `nuget`, `apt`, `yum`, `rubygems`, `composer`, `conda`, `conan`, `r`, `gitlfs`, `p2`, `swift`, `cocoapods`, `huggingface`, `terraform` 중 하나. |
| `type` | 모두 | 필수 | `hosted`, `proxy`, `group` 중 하나. `gitlfs`는 `hosted`만 됩니다(proxy·group은 400). `p2`는 `proxy`·`group`만, `cocoapods`·`huggingface`는 `proxy`만 됩니다(그 밖은 400). |
| `remote` | proxy | 필수 | 원격 저장소의 `http(s)` URL. Cargo는 `sparse+` 없는 sparse index URL, Conan은 Conan 서버 루트(`https://center.conan.io/`), R은 CRAN 미러나 Posit Package Manager의 루트, p2는 p2 사이트 URL, Swift는 다른 Swift 레지스트리의 루트, CocoaPods는 트렁크 CDN `https://cdn.cocoapods.org/`(Specs git 저장소는 안 됨), HuggingFace는 `https://huggingface.co/`. 원격 인증 정보는 URL에 넣지 말고 `remoteAuth`에 넣으세요(URL은 암호화하지 않습니다. "저장된 비밀값 암호화" 참고). |
| `remoteAuth` | proxy | 없음 | 원격 인증: `username`/`password` 또는 `bearerToken`(둘 다 있으면 토큰 우선). 설정한 `remote`와 스킴·호스트·포트가 같은 곳에만 보내며, 리다이렉트를 따라갈 때도 같습니다. 예외는 같은 호스트의 `http`(80)에서 `https`(443)로 옮기는 리다이렉트로, 첫 요청이 이미 그 호스트에 보냈으므로 자격 증명을 유지합니다. 그 밖에 포트나 스킴이 다른 곳(`https`에서 `http`로 내려가는 경우 포함)과 하위 도메인으로 가는 리다이렉트는 자격 증명 없이 따라갑니다. |
| `metadataMaxAge` | proxy | `"24h"` | 메타데이터(maven-metadata.xml, packument, simple 인덱스, Go `list`/`@latest`, Cargo 인덱스, Docker 태그, Helm `index.yaml`, NuGet 서비스 인덱스·registration·검색 결과, APT `dists/`, Yum `repodata/`, RubyGems compact index·specs, Composer `packages.json`·`p2`, Conda `repodata.json`·`channeldata.json`, Conan `latest`·`revisions`·파일 목록·검색, R `PACKAGES*`·`Meta/`, p2 `p2.index`·`content.*`·`artifacts.*`·`composite*`·`site.xml`, Swift 릴리스 목록·메타데이터·identifiers, CocoaPods 인덱스 파일, HuggingFace `api/…` 문서와 브랜치·태그→커밋 revision 문서)를 다시 확인하기까지의 시간. |
| `contentMaxAge` | proxy | `"-1"` | 아티팩트 본문(jar, tgz, 휠, zip, crate, nupkg, deb, rpm)의 재확인 주기. 기본은 영구 캐시. |
| `negativeCacheTTL` | proxy | `"10m"` | 원격이 404/410을 준 경로, 그리고 체크섬이 맞지 않은 다운로드를 다시 묻지 않는 시간. 만료된 기록은 서버가 시작할 때와 그 뒤 하루에 한 번 DB에서 지웁니다. |
| `routingRule` | proxy | 없음 | 이 proxy에 적용할 라우팅 규칙 이름 하나. 없는 이름은 400이고 hosted·group에는 쓸 수 없습니다("라우팅 규칙" 참고). |
| `members` | group | 필수 | 멤버 저장소 이름 목록. 순서대로 조회하며 같은 포맷이어야 하고 순환할 수 없습니다. |
| `writeMember` | group | 없음 | 업로드를 받을 hosted 멤버(`members`에 포함되어야 함). 없으면 group에 쓰기는 405. |
| `versionPolicy` | Maven hosted | `release` | `release`, `snapshot`, `mixed`. Maven에서만 쓸 수 있습니다. |
| `depth` | yum hosted | `0` | `repodata/` 위치: `0`은 저장소 루트에 하나, `1`은 첫 디렉터리마다("Yum 설정" 참고). |
| `distribution` | apt hosted | `stable` | `dists/<distribution>/` 경로와 sources.list의 suite가 됩니다("APT 설정" 참고). |
| `component` | apt hosted | `main` | `dists/<distribution>/<component>/` 경로와 `pool/<component>/`, sources.list의 component가 됩니다. |
| `signingKey` | apt hosted | 없음 | 비밀번호 없는 RSA(2048비트 이상) OpenPGP 개인키를 ASCII armor로 넣습니다. 있으면 `Release.gpg`·`InRelease`를 만들고 공개키를 `/repository/<저장소>/public.key`로 제공합니다. `remoteAuth`처럼 암호화되어 저장되는 쓰기 전용 값이며 API·UI에는 설정 여부(`signingKeySet`)만 나옵니다. |
| `writePolicy` | hosted | `allow_once` | `allow`(덮어쓰기 허용), `allow_once`(같은 경로 재업로드 거부), `deny`(읽기 전용). 아래 설명 참고. |
| `anonymousRead` | 모두 | 최상위 값 | 이 저장소만 익명 읽기를 켜거나 끕니다. |
| `cleanupPolicy` | hosted, proxy | 없음 | 연결할 Cleanup 정책 이름 하나(없으면 비움). 정책의 포맷이 `all`이거나 저장소와 같아야 하며 Cargo hosted에는 쓸 수 없습니다("Cleanup 정책" 참고). |
| `blobStore` | hosted, proxy | `default` | 이 저장소의 blob을 둘 blob store 이름("Blob Store" 참고). 편집으로는 바꿀 수 없고 `repository.change-blobstore` 작업으로 옮기며("저장소의 blob store 바꾸기" 참고) group에는 없습니다. 없는 store 이름과 blob store 그룹의 멤버는 거부됩니다. |
| `strictContentTypeValidation` | hosted | `false` | 켜면 올린 내용이 경로·선언한 형식과 맞지 않는 업로드를 400으로 거부합니다(Nexus의 strict content type validation). raw는 확장자(`.jar`, `.zip`, `.whl`, `.nupkg`는 zip, `.tgz`·`.gz`·`.crate`는 gzip, `.pom`·`.xml`은 XML, `.rpm`, `.deb`)의 파일 시그니처를 보고, 그 밖에는 `Content-Type`(없으면 확장자로 정한 형식)의 주 형식과 실제 내용을 비교합니다(`text/*`인데 바이너리, `image/*`·`audio/*`·`video/*`인데 다른 형식). `application/octet-stream`과 SVG는 검사하지 않습니다. Maven은 같은 규칙을 아티팩트에 적용합니다(`maven-metadata.xml`과 체크섬 제외). npm, PyPI, NuGet, Helm은 이 값과 관계없이 올린 패키지를 항상 열어 검사합니다. Nexus 호환 API로 만들 때는 `storage.strictContentTypeValidation`을 따르며, Terraform 공급자와 Nexus 예제는 `true`를 보내므로 그렇게 만든 hosted 저장소는 검사가 켜집니다. |
| `contentDisposition` | raw, gitlfs | `inline` | 다운로드 응답의 `Content-Disposition`. `attachment`이면 `attachment; filename=<파일 이름>`을 보내 브라우저가 열지 않고 내려받게 합니다. raw group은 자기 값이 있으면 그 값을, 없으면 파일을 가진 멤버의 값을 씁니다. Nexus 호환 API의 `raw.contentDisposition`(`INLINE`/`ATTACHMENT`)과 같습니다. |
| `autoBlock` | proxy | `true` | 원격이 연달아 실패하면 잠시 원격에 묻지 않습니다(아래 "원격 자동 차단" 참고). 편집할 때 생략하면 저장된 값을 유지합니다. |
| `blocked` | proxy | `false` | `true`이면 원격에 전혀 묻지 않습니다. 캐시된 파일만 내보내고(만료된 것은 `Warning: 110`과 함께), 없는 파일은 502입니다. 편집할 때 생략하면 `false`가 됩니다. |
| `firewall` | proxy(취약점 데이터 대상 포맷) | 꺼짐 | OSV 취약점 데이터로 이 proxy가 내주는 컴포넌트를 격리합니다: `enabled`, `blockMalicious`(기본 `true`), `blockSeverityAtLeast`(`none`·`low`·`medium`·`high`·`critical`, 기본 `none`), `allowList`(`name@version`에 맞추는 RE2 정규식, 1000개까지). 편집할 때 생략하면 저장된 값을 유지합니다("취약점 데이터와 방화벽(OSV)" 참고). |
| `gpgPublicKey` | terraform hosted | 없음 | 프로바이더 `SHA256SUMS` 서명을 확인할 ASCII armor 공개키(`gpg --armor --export`). 다운로드 문서가 key id와 함께 알려 주며, 읽을 수 없는 키는 400입니다("Terraform 설정" 참고). |
| `online` | 모두 | `true` | `false`이면 저장소가 Offline이 되어 요청에 503을 돌려줍니다("저장소 Online/Offline" 참고). |

**기간 형식**: Go 기간 문자열(`"30m"`, `"1h"`, `"24h"`)이며 `d` 단위는 없습니다. `"-1"`은 만료 없음(영구)입니다. 생략하거나 `0`이면 기본값입니다.
원격에 접속할 수 없을 때는 만료된 캐시가 있으면 그대로 제공하고 `Warning: 110` 헤더를 붙입니다. 원격이 응답 헤더를 보낸 뒤, 본문을 기다리는 서버에 60초 동안 한 바이트도 보내지 않을 때도 받기를 중단하고 같은 방식으로 처리합니다. 받은 내용을 blob store에 쓰는 시간은 이 60초에 세지 않으므로, blob store의 디스크(NFS 등)가 잠시 멈춰도 원격이 정상이면 받기가 끊기지 않습니다. 느려도 계속 오는 전송은 끊지 않으며, 한 번 받기의 전체 상한은 30분입니다.

**원격 접속 제한**: proxy는 리다이렉트를 5번까지만 따라가고, 접속하는 순간의 실제 IP를 검사합니다. 그래서 DNS 응답을 중간에 바꾸는 방법(DNS rebinding)으로도 우회할 수 없습니다. 무엇을 허용할지는 `remote` 호스트가 가리키는 주소로 정합니다.

- `remote`가 공인 주소이면, 리다이렉트나 원격 인덱스가 준 URL(PyPI 파일 URL, Cargo `config.json`의 `dl`)이 다음 주소로 향할 때 거절합니다: 사설(10/8, 172.16/12, 192.168/16, 100.64/10, fc00::/7), loopback, link-local(169.254.169.254 같은 클라우드 메타데이터 포함), 미지정(`0.0.0.0`, `::`), 멀티캐스트.
- `remote`가 사설 주소인 사내 저장소(예: 사내 Nexus)에는 그대로 접속하고, 다른 사설 호스트로 가는 리다이렉트도 따라갑니다.
- loopback, link-local, 미지정, 멀티캐스트 주소는 `remote` 자체가 그런 주소일 때만 허용합니다(예: `http://127.0.0.1:8082/`).
- NAT64 주소(`64:ff9b::/96`, `64:ff9b:1::/48`)는 마지막 32비트에 담긴 IPv4 주소로 판단합니다. 예를 들어 `64:ff9b::a00:5`는 `10.0.0.5`와 같은 사설 주소로 봅니다. `64:ff9b:1::/48` 안에서 /96보다 짧은 변환 접두사를 쓰거나 통신사·사내에서 따로 정한 NAT64 접두사를 쓰는 망에서는 이 판단이 맞지 않으므로, NAT64 게이트웨이나 방화벽에서 내부 대역으로 가는 변환을 막으세요.
- IPv4 주소는 점으로 구분한 네 부분의 십진수(`10.0.0.1`)로 적어야 합니다. `10.1`, `167772161`, `0x0a000001`, `012.0.0.1`, `10.0.0.1.`처럼 호스트의 마지막 부분이 숫자인 다른 표기는 많은 리졸버와 사내 프록시가 IPv4 주소로 읽으므로, `remote`가 사내 저장소여도 항상 거절합니다. `remote` 자체를 이렇게 적어도 거절됩니다.
- 호스트 이름은 ASCII로만 적습니다. 국제화 도메인(예: `bücher.example`)은 퓨니코드(`xn--bcher-kva.example`)로 적으세요. ASCII가 아닌 호스트는 접속할 때 ASCII로 바뀌어 위 검사를 비켜 갈 수 있으므로(전각 문자로 쓴 `１０．１`은 `10.1`이 됩니다) `remote`든 리다이렉트나 원격 인덱스가 준 URL이든 항상 거절합니다. UI·API로 저장소를 만들거나 편집할 때 `remote` 호스트가 ASCII가 아니면 400이고, 첫 시작에 `config.yaml`에서 가져올 때는 시작이 실패합니다. 이전 버전이 저장한 그런 저장소가 있어도 서버는 시작하지만 그 저장소의 원격 요청은 모두 거절되므로 `remote`를 퓨니코드로 고치세요.
- 직접 접속할 때 `remote` 호스트가 가리키는 주소는 요청마다 DNS로 다시 확인합니다. 조회가 실패하면 최근 10분 안에 성공한 마지막 조회 결과를 대신 쓰므로, DNS가 잠시 응답하지 않아도 사내 저장소가 공인 저장소로 취급되어 거절되지 않고 이미 열린 연결도 계속 쓸 수 있습니다. 그런 결과가 없으면 `remote`를 공인 주소로 보고 가장 엄격하게 제한합니다.
- 거절되면 클라이언트는 502(만료된 캐시가 있으면 그 캐시)를 받습니다. 로그의 `remote fetch failed` 줄에는 `address refused by the remote egress policy`가 남습니다.
- `HTTPS_PROXY`/`HTTP_PROXY`의 사내 프록시를 거치는 요청은 프록시가 대신 접속하므로 IP 주소로 적힌 목적지만 검사합니다(위의 다른 IPv4 표기와 ASCII가 아닌 호스트의 거절 포함). 호스트 이름으로 적힌 목적지는 내부 주소로 해석되더라도 프록시가 그대로 접속하므로, 프록시에서 내부 대역으로 가는 요청을 막거나 사내 도메인과 대역을 `NO_PROXY`에 넣으세요. 프록시 자체의 주소는 막지 않습니다. `NO_PROXY`에 넣은 호스트에는 직접 접속하므로 위 검사를 모두 받습니다.

**원격 자동 차단**: `autoBlock`(기본 켬)인 proxy는 원격 요청이 연달아 3번 실패하면(연결 실패, 시간 초과, 5xx 응답. 404, 401, 403, 429는 응답이므로 실패가 아닙니다) 30초 동안 원격에 묻지 않습니다. 그동안 만료된 캐시는 `Warning: 110`을 붙여 내보내고, 캐시에 없는 파일은 원격을 기다리지 않고 바로 502로 답합니다. 차단이 풀린 뒤 첫 요청도 실패하면 다시 차단하며, 차단 시간은 두 배씩 늘어 5분까지 갑니다. 원격이 한 번이라도 응답하면 실패 수와 차단 시간이 처음으로 돌아갑니다. 상태는 메모리에만 두므로 서버를 다시 시작하거나 저장소 설정을 저장하면 초기화됩니다. 로그인한 사용자에게는 저장소 목록과 상세(`GET /-/api/repositories`, `GET /-/api/repositories/<이름>`)에 `blocked`, `autoBlock`, `remoteStatus`(`blocked`, 자동 차단이면 `true`인 `auto`, 차단이 끝나는 시각 `until`, 마지막 실패 원인 `lastError`. 원격 주소는 넣지 않습니다)가 나오고, 익명 사용자에게는 나오지 않습니다. 관리자가 `blocked: true`로 저장하면 원격에 전혀 묻지 않습니다(`remoteStatus.auto`는 `false`). Nexus 호환 API의 `httpClient.blocked`, `httpClient.autoBlock`과 같습니다.

**writePolicy의 적용 범위**: Maven은 세 값을 모두 따릅니다. raw는 Maven처럼 세 값을 모두 따릅니다(`allow`에서 같은 경로를 덮어쓰려면 edit 권한이 필요). Helm, Docker, NuGet, APT, Yum, RubyGems, Composer, Conda, Conan, R, Swift도 세 값을 모두 따릅니다("Helm 설정", "Docker 설정", "NuGet 설정", "APT 설정", "Yum 설정", "RubyGems 설정", "Composer 설정", "Conda 설정", "Conan 설정", "R 설정", "Swift 설정" 참고). Git LFS는 같은 oid의 재업로드를 `allow_once`에서도 받고(멱등) `deny`만 업로드·삭제를 막습니다("Git LFS 설정" 참고). Terraform은 `allow_once`에서 같은 파일의 재업로드는 받고 내용이 다르면 409이며, `allow`는 덮어쓰기에 edit 권한이 필요하고 `deny`는 읽기 전용입니다("Terraform 설정" 참고). Go는 `allow`가 아니면 같은 파일을 거부하고 `deny`는 업로드를 막습니다. PyPI와 Cargo는 `deny`만 효과가 있으며 같은 파일·버전 재업로드는 항상 거부합니다. npm은 `writePolicy`를 쓰지 않으며 이미 게시된 버전은 항상 거부합니다. 단, `deny`인 저장소에서는 모든 포맷의 삭제(Maven `DELETE`, `npm unpublish`, Go `DELETE`, 웹 UI·API의 컴포넌트 삭제)가 400입니다. Cleanup 정책은 `writePolicy`를 무시합니다.

## 인증과 권한

사용자, 역할, Realm, LDAP 설정은 `config.yaml`이 아니라 **DB에서 웹 UI로 관리**합니다(첫 시작 때 `config.yaml`의 `users`/`roles`를 한 번 가져옵니다. "업그레이드" 참고). 변경은 바로 적용되며 재시작이 필요 없습니다.

### 사용자

- **Basic 인증**: 사용자 이름과 비밀번호. UI에서 사용자를 만들 때 비밀번호(8자 이상, 72바이트 이하, 공백만으로는 안 됨)를 입력합니다. `commandcenter hash-password`는 첫 시작 때 `config.yaml`에서 가져올 해시를 만들며 같은 비밀번호 규칙을 적용합니다(가져온 뒤에는 `config.yaml`의 사용자를 고쳐도 반영되지 않습니다). 관리자가 모두 잠겼을 때는 "비상 복구"의 `user reset-password`를 쓰세요. 사용자 ID는 영문자·숫자로 시작하고 영문자, 숫자, `.`, `_`, `@`, `-`만, 최대 64자이며 `create`는 예약어입니다. 이미 있는 ID와 대소문자만 다른 ID(예: `admin`이 있을 때 `Admin`)는 만들 수 없고, 409 응답에 기존 ID가 나옵니다(`already exists as "admin"`). 로그인은 대소문자를 구분하므로 비슷해 보이는 다른 계정이 생기지 않게 하기 위해서이며, 이전 버전이 이미 만든 그런 ID는 그대로 둡니다. 첫 가져오기에서도 같은 규칙을 적용하므로 규칙에 맞지 않는 ID(예: `:`이 들어간 ID)나 대소문자만 다른 ID 두 개가 `config.yaml`에 있으면 가져오기가 실패하고 서버가 시작되지 않습니다. 이름과 이메일에는 줄바꿈 같은 제어 문자를 쓸 수 없고, 이메일은 비워 두거나 `user@example.com` 형식의 주소 하나(표시 이름이나 여러 주소 없이)여야 합니다(400). 이전 버전이 저장한 값은 바꾸지 않는 한 다시 검사하지 않으므로 그런 사용자도 편집하거나 비활성화할 수 있습니다.
- **상태**: `active` 또는 `disabled`. 비활성화하거나 삭제한 사용자는 Basic 인증, 세션, 토큰 모두 곧바로 거부됩니다. 사용자를 삭제하면 그 사용자의 API 토큰도 함께 지워집니다.
- **본인 비밀번호 변경**: 로그인한 로컬 사용자는 계정 화면(`PUT /-/api/account/password`)에서 현재 비밀번호를 확인한 뒤 바꿉니다. 비밀번호나 세션으로 인증해야 하고 토큰으로는 안 됩니다. 새 비밀번호가 현재 비밀번호와 같으면 400입니다. 바꾸면 그 사용자의 세션이 모두 끊기고 현재 브라우저만 새 세션을 받습니다. LDAP 사용자는 비밀번호를 디렉터리에서 관리하므로 400입니다. 현재 비밀번호는 로컬 Realm으로만 확인하므로 같은 이름의 LDAP 사용자의 비밀번호로는 바꿀 수 없습니다. 관리자는 다른 로컬 사용자의 비밀번호를 재설정할 수 있고, 그러면 그 사용자의 세션이 모두 끊깁니다. 재설정은 새 비밀번호가 현재 비밀번호와 같은지 보지 않습니다(관리자가 사용자의 현재 비밀번호를 맞혀 볼 수 없게 하기 위해서입니다). 어느 쪽이든 API 토큰은 그대로 유효하므로, 비밀번호가 새어 나갔다고 의심되면 사용자 화면의 **토큰 모두 폐기**도 누르세요(아래 "API 토큰").
- **API 토큰**: UI의 API 토큰 화면 또는 `docker compose exec commandcenter commandcenter token create -user ci`가 `cc_…` 토큰을 만듭니다. `Authorization: Bearer <토큰>`, 토큰 값만 보내는 형태(Cargo), 또는 **Basic 인증의 비밀번호 자리**(사용자 이름은 토큰 소유자와 같아야 하며 다르면 401)로 쓸 수 있어 Maven·pip·Go CI에서 비밀번호 대신 넣을 수 있습니다. 토큰으로는 새 토큰을 발급·폐기하거나 웹 UI에 로그인할 수 없습니다(실제 비밀번호나 로그인 세션만 가능). 토큰은 만료되지 않지만 사용자를 비활성화·삭제하거나 `token` Realm을 끄면 무효가 됩니다. 비밀번호를 바꾸거나 재설정해도 토큰은 남습니다. 관리자는 사용자 화면의 **토큰 모두 폐기**(`DELETE /-/api/users/<id>/tokens`, 관리자이며 비밀번호 또는 세션으로 인증하고 `X-Command-Center-UI: 1` 헤더 필요)로 그 사용자의 토큰을 한 번에 폐기합니다. 같은 ID의 LDAP 사용자 토큰(`ldap:<id>`)도 함께 폐기하고, 응답 `{"revoked": <개수>}`와 활동 기록의 `token.revoke` 이벤트(`all <개수>`)에 폐기한 개수가 나옵니다. 폐기된 토큰을 쓰던 빌드와 CI는 401을 받으므로 새 토큰을 넣으세요. 토큰은 해시만 저장되므로 분실하면 다시 발급하세요. LDAP 사용자의 토큰은 소유자가 `ldap:<id>`입니다("Realm, LDAP, OIDC, SAML" 참고).
- **`npm login`**: 사용자 이름과 비밀번호로 로그인하면 서버가 토큰을 발급해 `~/.npmrc`에 저장합니다.
- **세션**: 웹 UI 로그인 세션은 메모리에만 있고 모두 1000개, 사용자당 20개까지입니다. 넘으면 그 사용자의 세션 가운데 가장 오래 쓰지 않은 것이 끊깁니다("웹 UI"의 "주의할 점" 참고).

### 역할과 권한

역할은 **권한(privilege)** 목록입니다. 권한 하나는 **대상**과 **동작**의 묶음입니다.

- **대상**: `*`(모든 저장소), `format:<포맷>`(`maven`, `npm`, `pypi`, `go`, `cargo` 등 저장소 포맷 이름), `repo:<저장소 이름>`, `selector:<셀렉터 이름>`(경로 단위 권한. 적용 저장소를 `repos`에 함께 적습니다. "컨텐츠 셀렉터" 참고).
- **동작**: `browse`, `read`, `add`, `edit`, `delete`. 여러 권한과 여러 역할은 합쳐지며(어느 하나라도 허용하면 허용) 거부 규칙은 없습니다.
  - **browse**: UI의 저장소 목록, 탐색, 파일 정보, 검색, 컴포넌트 검색. 내용 다운로드(`/repository/`)는 안 되며 파일 화면의 Download 버튼도 나오지 않습니다. `read`가 있으면 browse도 됩니다.
  - **read**: 다운로드(GET/HEAD)와 메타데이터 조회.
  - **add**: 새 파일·버전 올리기. Maven `PUT`(새 경로), npm `publish`(새 버전), PyPI `twine upload`, Go 업로드, Cargo `publish`, UI 업로드.
  - **edit**: 이미 있는 것을 바꾸기. `writePolicy: allow`인 Maven·Go 저장소에서 같은 경로 덮어쓰기, npm `dist-tag`와 `deprecate`, Cargo `yank`/`unyank`. (`writePolicy`가 `allow_once`이거나 PyPI·Cargo처럼 재업로드를 항상 거부하는 포맷에서는 edit이 있어도 덮어쓸 수 없습니다.) Maven·Go 덮어쓰기에는 add와 edit이 모두 필요합니다(모든 Maven `PUT`과 Go 업로드는 add를 검사하고, 이미 있는 경로를 바꿀 때 edit을 한 번 더 검사합니다). npm `dist-tag`·`deprecate`와 Cargo `yank`/`unyank`는 edit만 있으면 됩니다. 역할 편집 화면에도 이 안내가 나옵니다.
  - **delete**: 삭제. Maven `DELETE`(파일), `npm unpublish`, Go `DELETE`(버전 전체), 웹 UI와 API의 컴포넌트 삭제(모든 포맷, "컴포넌트 삭제" 참고). `add`만 있으면 403입니다.
- group에 쓰는 요청은 같은 동작을 `writeMember` 저장소에 대해 검사합니다.
- **내장 `admin` 역할**은 모든 대상에 모든 동작을 가지며 관리 화면과 `/-/metrics`도 쓸 수 있습니다. 수정·삭제할 수 없습니다.
- 역할을 지우려는데 사용자가 쓰고 있거나 LDAP 설정의 `groupMappings[].roles` 또는 `defaultRoles`에 들어 있거나, OIDC·Crowd·RUT·SAML 설정의 매핑·기본 역할 또는 일반 설정의 `defaultRoles`에 들어 있으면 409입니다(LDAP 참조는 메시지에 어느 매핑인지 나옵니다).
- **입력 검사**: `repo:<이름>` 대상은 그 이름의 저장소가 있어야 하며, 없으면 400(`unknown repository "<이름>"`)입니다. 이미 저장된 역할에 있던 대상은 저장소가 없어도(이전 버전이 저장소를 지울 때 남긴 권한 등) 그 역할을 편집할 때 그대로 둘 수 있습니다(새로 넣는 대상만 검사합니다). 저장소를 지우면 그 대상도 역할에서 지워지므로("저장소 관리"의 삭제 참고), 삭제 전에 열어 둔 역할 편집 화면을 그대로 저장하면 400입니다. 한 역할에 같은 대상을 두 번 쓰면 400(`duplicate target`)입니다. 역할 ID도 사용자 ID처럼 이미 있는 ID와 대소문자만 다른 ID는 만들 수 없고(409, `config.yaml` 첫 가져오기에서는 시작 실패), `admin`은 대소문자와 관계없이 예약어입니다. 설명에는 제어 문자를 쓸 수 없습니다. 이전 버전이 저장한 설명은 바꾸지 않는 한 다시 검사하지 않습니다.
- 예: `repo:maven-releases`에 `browse`, `read`, `add`만 있는 역할의 사용자가 Maven에 `PUT`하면 201, `DELETE`하면 403입니다.

### 컨텐츠 셀렉터

컨텐츠 셀렉터는 **경로 단위 권한**을 위한 이름 붙은 식입니다(Nexus의 Content Selectors). 셀렉터를 만든 뒤 역할의 권한에 대상 `selector:<셀렉터 이름>`과 적용 저장소 `repos`(저장소 이름 목록 또는 `["*"]`)를 넣으면, 그 저장소 안에서 셀렉터 식이 맞는 경로에만 동작이 허용됩니다.

식 문법(Nexus CSEL의 부분집합):

- 필드: `format`(Nexus 표기, Maven은 `maven2`), `path`(`/repository/<저장소>/` 뒤 경로, 앞에 `/`가 붙음), `coordinate.groupId`(Maven만, `org.acme` → `/org/acme` 접두어로 바꿔 비교).
- 연산자: `==`, `!=`, `=^`(시작 문자열), `=~`(RE2 정규식, 경로 전체가 맞아야 함).
- 결합: `and`, `or`, `not`, 괄호. `not`이 가장 강하게, 그다음 `and`, 그다음 `or`가 묶입니다. 문자열은 `"…"` 또는 `'…'`이고 최대 1024자입니다.

예:
```
format == "maven2" and path =^ "/org/acme/"
path =~ "/org/acme/.*-SNAPSHOT/.*"
coordinate.groupId == "org.acme" or path == "/readme.txt"
not path =^ "/internal/"
```
동작 규칙:

- 동작은 다른 권한과 같이 `browse`, `read`, `add`, `edit`, `delete`이며 여러 권한과 역할이 합쳐집니다. 요청 경로에 셀렉터가 맞으면 허용, 아니면 403입니다(group에 적용하려면 `repos`에 group 이름을 넣어야 합니다. Docker는 이미지 이름이 경로입니다).
- **목록은 저장소 단위입니다.** 셀렉터 권한이 있는 저장소는 UI 목록·탐색·검색에 보이고, 파일 이름도 셀렉터와 관계없이 나옵니다. 내려받을 수 있는지는 파일 화면의 Download 버튼(`canDownload`)과 실제 `/repository/` 요청에서 경로별로 판단합니다. 즉 셀렉터 사용자는 내려받을 수 없는 이름도 볼 수 있습니다.
- 컴포넌트 삭제(웹 UI, Nexus API)는 파일 여러 개를 지우므로 저장소 전체 delete 권한이 필요합니다. 셀렉터로만 delete를 받은 사용자는 403이고 경로별 `DELETE /repository/...`는 됩니다.
- 역할이 쓰는 셀렉터는 지울 수 없습니다(409, 메시지에 역할 이름). 식을 고치면 바로 적용됩니다. "미리보기"(`POST /-/api/content-selectors/preview`)로 저장소 하나에서 식에 맞는 경로를 최대 100개 확인할 수 있습니다. 변경은 활동 기록에 `selector.create/update/delete`로 남습니다.
- `config.yaml`의 `contentSelectors:`는 첫 시작 때 한 번 가져옵니다.
- Nexus REST API: `/service/rest/v1/security/content-selectors[/{name}]`(`name, description, expression`)과 `POST/PUT /service/rest/v1/security/privileges/repository-content-selector[/{name}]`(`name, description, actions, format, repository, contentSelector`). Nexus의 권한은 독립 객체이지만 여기서는 역할 안에 있으므로, 이 권한은 **`priv:<권한 이름>`** 역할(권한 하나짜리)로 만들어지고 사용자에게 역할로 배정합니다. `repository: "*"`는 모든 저장소이며 `format`은 저장하지 않습니다(식의 `format ==`으로 제한하세요).

예: `org.acme` 아래만 읽고 올릴 수 있는 역할
```yaml
contentSelectors:
  - name: acme
    description: acme 팀 아티팩트
    expression: 'format == "maven2" and path =^ "/org/acme/"'
```
역할 권한(JSON): `{"target":"selector:acme","repos":["maven-releases"],"actions":["browse","read","add"]}`

### 읽기 권한

- **`authenticatedRead`**(기본 켜짐): 켜져 있으면 로그인한 사용자는 권한 없이도 모든 저장소를 읽고 browse할 수 있습니다. 끄면 로그인한 사용자도 `read`(UI 목록은 `browse` 또는 `read`) 권한이 있어야 합니다. 쓰기(add/edit/delete)는 항상 권한이 필요합니다.
- **익명 읽기**: 일반 설정의 `anonymousRead`(저장소별 `anonymousRead`가 있으면 그 값이 우선)가 켜진 저장소는 인증 없이 읽을 수 있습니다. group의 읽기 검사는 멤버가 아니라 **group 자신의** 값을 따릅니다. 익명 사용자의 쓰기는 항상 거부됩니다.
- **응답 코드**: 인증이 필요한데 없으면 401(`WWW-Authenticate` 포함), 인증은 됐으나 권한이 없으면 403. 잘못된 자격 증명은 `anonymousRead`가 켜져 있어도 401이고, 로그인 시도 제한에 걸린 이름은 429입니다("보호" 참고). 저장소가 Offline이면 권한 검사 **뒤에** 503입니다("저장소 Online/Offline" 참고).
- **없는 저장소**: 그 이름의 저장소를 읽을 수 없는 호출자(없는 이름에는 전역 `anonymousRead`와 `authenticatedRead`가 적용됨)에게는 읽을 수 없는 저장소와 똑같은 401/403으로 답하므로, 응답으로 숨은 저장소 이름을 알아낼 수 없습니다. 읽을 수 있는 호출자에게는 404입니다. 남는 예외: 전역 익명 읽기가 켜져 있을 때 저장소별 `anonymousRead: false`로 빠진 저장소는 익명 사용자에게 401이라 없는 저장소(404)와 구별되고, npm 저장소는 토큰 없이 써야 하는 로그인 경로가 인증 없이 답합니다.

### 잠금 방지

"로그인할 수 있는 관리자"가 없어지는 변경은 409로 거부됩니다. 관리자가 로그인할 수 있는 경로는 (a) `local` Realm이 켜져 있고 쓸 수 있는 비밀번호(bcrypt 해시)가 있는 활성 로컬 `admin` 사용자가 있거나, (b) `ldap` Realm이 켜져 있고 설정이 완전하며 그룹 매핑 또는 기본 역할이 `admin`을 주는 경우, (c) `crowd` Realm이 같은 조건인 경우입니다. 대상이 되는 변경은 사용자 삭제·비활성화·역할 변경, `local`/`ldap`/`crowd` Realm 끄기, LDAP·Crowd 매핑 편집입니다. 추가로 **마지막 로컬 관리자(활성, bcrypt 비밀번호 해시 있음, `admin` 역할)는 LDAP 관리자 매핑이 있어도 삭제·비활성화·강등할 수 없습니다.** 로컬 비상용 관리자를 항상 남기기 위해서입니다. 그래서 LDAP만 쓰는 설치에도 로컬 관리자 한 명은 남습니다(`local` Realm 자체는 끌 수 있습니다). placeholder처럼 bcrypt 해시가 아닌 `passwordHash`를 가진 관리자는 로그인할 수 없으므로 세지 않습니다. 그래서 그런 관리자만 남게 되는 삭제·비활성화·강등도 409입니다.

사용자, 역할, Realm, LDAP 설정의 변경은 서버 안에서 하나씩 차례로 처리합니다(읽기 → 잠금 방지 검사 → 저장 → 적용). 그래서 두 관리자가 동시에 서로를 비활성화해도 하나는 409를 받고 관리자 한 명은 남습니다.

### 사용자 토큰

사용자 토큰은 Nexus Pro의 user token과 같은 기능으로, 사용자 이름과 비밀번호 대신 쓰는 **이름 코드(8자)와 패스 코드(44자)** 한 쌍입니다. Basic 인증을 받는 모든 곳(Maven `settings.xml`, npm `_auth`와 `npm login`, pip, `docker login`, Nexus REST API 등)에서 `<이름 코드>:<패스 코드>`를 사용자 이름과 비밀번호 자리에 넣습니다. 이름 코드에는 사용자 이름이 드러나지 않으므로 빌드 설정 파일이 유출돼도 계정 이름과 비밀번호가 함께 노출되지 않습니다.

- **켜기.** 일반 설정의 `userTokens`(`PUT /-/api/settings` `{"userTokens":{"enabled":true,"required":false,"expirationDays":0}}`, 관리자)에서 켭니다. 끄면 발급된 토큰이 모두 거부되지만 지워지지는 않습니다.
- **발급.** 사용자(로컬·LDAP·OIDC·Crowd)마다 토큰을 하나만 가질 수 있습니다. 본인 토큰은 `POST /-/api/me/usertoken`으로 만들고, 이미 있으면 새 코드로 바꿉니다(재설정, 이전 코드는 즉시 무효). 패스 코드는 **발급 응답에서 한 번만** 보이며 서버에는 해시만 저장됩니다. 비밀번호나 웹 세션으로 로그인해야 하고, API 토큰이나 사용자 토큰으로는 발급할 수 없습니다. `GET /-/api/me/usertoken`은 본인 토큰의 정보(가린 이름 코드, 생성·만료·마지막 사용 시각)를, `DELETE /-/api/me/usertoken`은 삭제를 합니다.
- **만료.** `expirationDays`(0은 만료 없음, 1~3650일)는 토큰을 만든 시각부터 계산하며, 설정을 바꾸면 이미 발급된 토큰에도 바로 적용됩니다. 만료된 토큰은 401로 거부되고 목록에 `expired: true`로 남습니다. 유지보수 작업 `security.purge-expired-user-tokens`("Purge expired user tokens")가 만료된 토큰을 지웁니다.
- **필수(required).** `required: true`이면 저장소 엔드포인트(`/repository/*`, Docker `/v2/*`, `npm login`)가 **사용자 이름과 비밀번호를 거부**하고(401, "a user token is required") 사용자 토큰과 API 토큰만 받습니다. 관리자도 예외가 아닙니다. 웹 UI 로그인, `/-/api/`, `/service/rest/`는 영향을 받지 않습니다. Docker는 `docker login` 단계부터 비밀번호가 거부됩니다.
- **회수.** 사용자를 비활성화하거나 삭제하면 그 사용자의 토큰은 즉시 동작을 멈춥니다(삭제하면 토큰도 지워집니다). 비밀번호를 바꿔도, 사용자 화면의 **토큰 모두 폐기**(API 토큰)를 눌러도 사용자 토큰은 그대로이므로 유출이 의심되면 토큰을 재설정하세요. 관리자는 `GET /-/api/usertokens`로 전체 목록(사용자, 이름 코드, 생성·만료·마지막 사용 시각)을 보고, `POST /-/api/usertokens/<소유자>`로 다른 사용자의 토큰을 발급·재설정하며, `DELETE /-/api/usertokens/<소유자>`(예: `dev`, `ldap:alice`, `crowd:bob`)로 한 사용자의 토큰을, `DELETE /-/api/usertokens?confirm=true`로 모든 토큰을 무효화합니다. 마지막 사용 시각은 최대 1분에 한 번 기록합니다.
- **잠금.** 틀린 패스 코드는 비밀번호와 같은 로그인 실패 제한을 받습니다(이름 코드마다 5회 실패 후 잠시 거부).
- **활동.** `settings.userTokens`, `usertoken.create`, `usertoken.reset`, `usertoken.delete`가 남으며 패스 코드는 어디에도 기록하지 않습니다.
- **Nexus REST API.** `GET/PUT /service/rest/v1/security/user-tokens`(`{"enabled","protectContent","expirationEnabled","expirationDays"}`, `protectContent`가 `required`), `DELETE /service/rest/v1/security/user-tokens`(전체 무효화), `GET /service/rest/v1/security/user-tokens/tokens`, `GET/POST/DELETE /service/rest/v1/security/users/<userId>/<realm>/user-token`(realm은 `NexusAuthenticatingRealm`, `LdapRealm`, `Crowd` 또는 `local`, `ldap`, `oidc`, `crowd`)을 제공합니다(관리자). 응답 필드 이름은 공개 문서에 없어 Command Center가 정한 것입니다.

Maven `settings.xml` 예:

```xml
<server>
  <id>commandcenter</id>
  <username>이름 코드</username>
  <password>패스 코드</password>
</server>
```

`required`를 켠 뒤 빌드가 모두 막혔다면, 관리자가 웹 UI에 비밀번호로 로그인해(UI 로그인은 영향을 받지 않습니다) 일반 설정에서 `required`를 끄면 됩니다. 웹 UI에도 들어갈 수 없으면 아래 "비상 복구"를 따르세요.

### 세션과 SSO 상태

- **세션:** UI 세션은 메타데이터 데이터베이스(`sessions` 테이블)에 저장합니다. 재시작해도 로그인이 유지되며, 어느 노드에서 로그인해도 모든 노드가 같은 세션을 받습니다. 유휴 시간 제한은 일반 설정 값을 요청마다 확인합니다. 마지막 사용 시각은 1분에 한 번만 기록하므로 세션이 최대 1분 일찍 끝날 수 있습니다. 사용자당 세션은 20개까지이며, 그 이상 로그인하면 가장 오래 쓰지 않은 세션이 끝납니다. 세션은 시작 후 7일이 지나면 끝납니다.
- **SSO 신원:** OIDC, SAML, RUT, Crowd 사용자의 신원을 데이터베이스(`sso_identities`)에 저장하므로, 재시작한 뒤나 다른 노드에서도 세션과 토큰이 그대로 동작합니다. 신원은 마지막 로그인 후 7일 동안 유효하며, 그 뒤에는 다시 로그인할 때까지 토큰이 거부됩니다. Realm 설정을 바꾸면 이전 설정으로 기록된 신원은 쓰지 않습니다. 단, 클라이언트 시크릿·SP 키·애플리케이션 비밀번호만 바꾸면 신원은 유지됩니다. Crowd 사용자는 다른 노드에서도 `cacheMinutes` 동안 Crowd에 다시 묻지 않습니다. OIDC·SAML의 진행 중인 로그인 상태와 SAML assertion 재사용 기록도 데이터베이스에 둡니다.
- **로그인 제한:** 로그인 이름별 실패 횟수를 데이터베이스(`auth_failures`, UNLOGGED)에 기록하므로, 노드가 여러 대여도 같은 이름에 허용하는 추측 횟수가 늘지 않습니다. 주소별 제한은 노드마다 따로 세므로 노드 수만큼 약해집니다. 실패 횟수를 읽을 수 없으면 비밀번호를 확인하지 않고 거부합니다. PostgreSQL이 비정상 종료되면 UNLOGGED 테이블이 비워질 수 있습니다. `user reset-password`는 해당 이름의 실패 기록도 지웁니다.
- **토큰 서명 키:** Docker와 Conan 토큰을 서명하는 키를 settings의 `registry.tokenKey`에 한 번 저장하고 비밀값 키로 암호화합니다. 그래서 한 노드에서 받은 토큰을 다른 노드도 받아들입니다. 비밀값 키가 없으면 이 키가 평문으로 저장되므로 비밀값 키를 설정하세요.
- **비밀값 키 확인:** 비밀값 키로 처음 시작할 때 키 확인값(settings의 `secretKey.check`, HMAC)을 저장합니다. 이후 다른 키로 시작하거나, 키 없이 시작하면 시작을 거부합니다. 모든 노드가 같은 키를 써야 합니다. 키를 바꾸려면 모든 노드를 멈추고 `commandcenter secret-key rotate`를 실행한 뒤 새 키로 시작합니다.

### 비상 복구(break-glass)

모든 관리자가 잠겼거나(비밀번호 분실, 비활성화, `local` Realm 꺼짐) 웹 UI로 들어갈 수 없을 때는 DB를 직접 고칩니다. 서버를 멈출 필요는 없습니다.

```bash
# TTY에서 직접 입력하면 비밀번호가 화면에 그대로 보이므로 파이프로 넘기세요(8자 이상, 72바이트 이하)
# 서버가 실행 중일 때
printf '%s\n' "$PW" | docker compose exec -T commandcenter commandcenter user reset-password -user admin -admin
# 서버가 멈춰 있을 때
printf '%s\n' "$PW" | docker compose run --rm -T commandcenter user reset-password -user admin -admin
```

`user reset-password`는 표준 입력에서 비밀번호를 한 줄 읽고(API와 같은 규칙), 한 트랜잭션으로 다음을 바꾼 뒤 바꾼 내용을 출력합니다.

- 사용자의 비밀번호를 설정하고 상태를 `active`로 바꿉니다. 사용자가 없으면 로컬 사용자로 만듭니다. 대소문자만 다른 사용자가 이미 있으면(예: `Admin`이 있는데 `-user admin`) 만들지 않고 `already exists as "Admin"` 오류로 끝나므로(0이 아닌 코드) 나온 ID를 그대로 쓰세요. 이미 있는 `source=ldap` 덮어쓰기 행의 ID이면, 관리자가 비활성화한 디렉터리 계정이 되살아나거나 그 역할이 비밀번호 로그인으로 옮겨지는 일을 막기 위해 `user "<id>" is an LDAP role-override record; choose another id for a local recovery account` 오류로 거부하고 아무것도 바꾸지 않습니다(0이 아닌 코드로 종료). 이때는 다른 ID를 쓰세요.
- `-admin`을 주면 `admin` 역할을 더합니다.
- `local` Realm을 켭니다. Realm 목록에 없으면 맨 앞에 넣습니다.

첫 시작 전이라 `config.yaml`을 아직 가져오지 않았다면 가져오기를 먼저 합니다. `-config`는 `token create`와 같습니다. 명령이 실행 중인 서버들에 알리므로 바뀐 내용이 곧바로 반영되며(`token create`도 같습니다. 알림을 놓쳐도 서버가 사용자·역할·Realm·설정을 5초마다 DB에서 다시 읽습니다) 재시작할 필요가 없습니다. 웹 UI에서 바꿀 때와 같이 비밀번호가 바뀐 사용자의 기존 세션은 끝나고 그 이름의 로그인 시도 제한(아래 "보호")도 풀리며, API 토큰은 그대로 유효합니다(필요하면 웹 UI의 사용자 화면에서 **토큰 모두 폐기**를 누르세요). 출력의 마지막 줄은 `done; sign in as <id> (a running server applies the change within a few seconds)`입니다.

### 보호

- **저장소 콘텐츠**: `/repository/` 응답에는 `Content-Security-Policy: sandbox allow-downloads; default-src 'none'`과 `X-Content-Type-Options: nosniff`가 붙어, 업로드된 HTML이 브라우저에서 이 서버의 권한으로 실행되지 않습니다. 인증된 요청의 응답은 `Cache-Control: private`입니다.
- bcrypt 비교는 동시에 최대 4개이며, 없는 사용자도 같은 비용으로 비교해 사용자 존재 여부를 응답 시간으로 알 수 없습니다. 성공한 비밀번호 확인은 1분간 캐시되고(비밀번호를 바꾸면 바로 무효), 토큰 마지막 사용 시각은 1분에 한 번만 기록합니다. 로그인 이름이 256바이트, 비밀번호가 1024바이트를 넘으면 어떤 Realm에도 보내지 않고 거부합니다.
- **로그인 시도 제한**: 비밀번호 확인(`/repository/`와 API의 Basic 인증, 웹 UI 로그인, `npm login`, 본인 비밀번호 변경의 현재 비밀번호)은 로컬과 LDAP 모두 로그인 이름마다 실패를 셉니다. 성공 없이 **5번** 실패한 이름은 마지막 실패로부터 **1분** 동안 비밀번호를 확인하지 않고 `429 Too Many Requests`로 거절됩니다(`Retry-After` 헤더에 남은 초, 웹 UI에는 기다릴 시간이 나옵니다). 그 뒤에도 실패하면 거절 시간이 실패마다 두 배가 되어 최대 **15분**까지 늘어납니다. 성공하거나 **1시간** 동안 실패가 없으면 잊습니다. 이름은 대소문자를 구분하지 않고 셉니다. 없는 이름도 같은 규칙이라 거절 여부로 계정이 있는지 알 수 없고, 디렉터리 장애로 실패한 LDAP 로그인도 실패로 셉니다. 같은 이름의 확인은 한 번에 하나씩 하므로 병렬 요청으로 한꺼번에 여러 비밀번호를 시험할 수 없고, 맞는 비밀번호를 쓰는 병렬 빌드는 첫 확인을 기다렸다가 캐시로 통과합니다.
  - 출력 가능한 ASCII(영문자, 숫자, 기호, 공백)가 아니거나 앞뒤에 공백이 있거나 공백이 연달아 있는 이름은 어떤 Realm에도 보내지 않고 틀린 비밀번호와 똑같이 401로 거부하며, 실패로 세지도 않습니다(웹 UI 로그인 창은 이름 앞뒤의 공백을 지우고 보냅니다). LDAP 디렉터리는 이름을 찾을 때 대소문자, 전각·반각, 유니코드 정규화 형태, 앞뒤·연속 공백의 차이를 무시하므로(RFC 4518), 그런 이름까지 받으면 한 계정을 철자만 바꿔 가며 제한 없이 시도할 수 있기 때문입니다. 로컬 사용자 ID는 원래 이 규칙에 맞으므로 영향이 없고, LDAP도 `uid`, `sAMAccountName`, ASCII `mail`이나 `Alice Smith`처럼 공백이 하나씩인 이름은 그대로 로그인합니다. 다만 `userFilter`가 한글 `cn`처럼 ASCII가 아닌 값으로 사용자를 찾는 설정에서는 그 값으로 로그인할 수 없으므로 `uid` 같은 ASCII 속성으로 찾게 하세요.
  - 한 클라이언트 주소(IPv6는 /64)가 첫 실패로부터 15분 안에 **20번** 실패하면, 그 15분이 끝날 때까지 그 주소에서는 이름마다 한 번만 실패해도 거절됩니다(여러 계정에 흔한 비밀번호를 대입하는 공격 대비). 주소 전체를 막지는 않습니다. 리버스 프록시 뒤에서는 모든 클라이언트가 프록시 주소로 보이므로(`X-Forwarded-For`는 보지 않습니다) 한 클라이언트가 모두를 막지 못하게 하기 위해서이며, 그 대신 프록시 뒤에서는 주소 기준 제한을 모든 클라이언트가 함께 받습니다.
  - 거절은 저절로 풀리고, 관리자가 그 사용자의 비밀번호를 재설정하거나 `user reset-password`를 실행해도 풀립니다. 이름별 기록은 데이터베이스에 두고("세션과 SSO 상태"의 로그인 제한), 주소별 기록은 노드의 메모리에만 있어(최대 10000개) 재시작하면 지워집니다. 이름이나 주소가 한도에 닿으면 WARN 로그(`sign-in throttled`)가 한 번 남습니다. 한도는 고정값이며 설정으로 바꿀 수 없습니다.
  - API 토큰과 이미 로그인한 세션은 영향을 받지 않습니다. 누군가 일부러 계속 틀리면 그 이름의 비밀번호 로그인은 계속 거절되므로, 빌드와 CI는 비밀번호 대신 API 토큰을 쓰세요. 토큰 형식의 값(`cc_`나 `rl_` 뒤에 16진수 64자리)을 Basic 비밀번호로 보냈다가 토큰 확인에 실패한 요청(폐기했거나 잘못된 토큰)은 실패로 세지 않습니다.

### 메타데이터 데이터베이스

저장소 정의, 컴포넌트·에셋 목록, 사용자·역할·설정, 토큰 해시, 작업과 활동 기록은 PostgreSQL(13 이상)에 둡니다. blob은 데이터베이스가 아니라 blob store(`dataDir` 또는 S3)에 있습니다. docker-compose와 Helm 차트는 PostgreSQL을 함께 띄우며, 운영 환경에서는 복제 구성된 PostgreSQL이나 관리형 서비스를 쓰는 것을 권장합니다.

```yaml
database:
  url: postgres://cc:비밀번호@db.example:5432/commandcenter?sslmode=require
  maxConns: 10        # 생략 시 10
```

- `database.url` 또는 환경 변수 `COMMANDCENTER_DATABASE_URL` 중 하나가 필요합니다(Kubernetes에서는 Secret으로 넣는 것을 권장합니다. Helm 차트의 `database.existingSecret`). 둘 다 없으면 `no database configured: set database.url (or COMMANDCENTER_DATABASE_URL) to a PostgreSQL URL` 오류로, 둘 다 있으면 `set COMMANDCENTER_DATABASE_URL or database.url, not both` 오류로 시작하지 않습니다.
- `postgres://`와 `postgresql://`만 허용합니다. URL과 비밀번호는 로그나 오류 메시지에 남지 않습니다. 사용자 이름과 비밀번호의 특수문자는 퍼센트 인코딩하십시오.
- 데이터베이스는 서버가 쓰는 역할을 소유자로 미리 만들어 둡니다(`CREATE DATABASE commandcenter OWNER <역할>`; PostgreSQL 15부터는 소유자가 아니면 `public` 스키마에 테이블을 만들 수 없습니다). 테이블은 첫 시작 때 그 역할이 만듭니다. 테이블을 만들고 업그레이드할 때만 소유자 권한이 필요하고, 스키마 버전이 현재 버전과 같은 데이터베이스는 테이블에 대한 SELECT/INSERT/UPDATE/DELETE와 스키마 USAGE만 있는 역할로도 열립니다. 권한이 모자라면 시작 오류에 `SQLSTATE 42501`과 함께 소유자가 필요하다는 안내가 나옵니다. 스키마 버전은 `schema_version` 테이블에 기록되고, 더 새 버전이 쓴 데이터베이스는 시작을 거부합니다("업그레이드" 참고).
- 모든 CLI 명령(`token create`, `user reset-password`, `gc`, `secret-key rotate`)이 같은 설정을 읽습니다.
- 쓰기는 advisory lock으로 한 번에 하나씩 처리하고(쓰기 잠금 대기 최대 5초, URL의 `lock_timeout`으로 바꿀 수 있습니다), 읽기는 쓰기를 기다리지 않습니다. 같은 데이터베이스로 여러 노드를 함께 띄울 수 있으며("고가용성(여러 노드)" 참고), 노드 하나는 연결 풀 외에 잠금 연결과 알림 연결을 하나씩 더 씁니다("노드와 준비 상태" 참고).
- 시스템 정보 화면의 메타데이터 DB 크기는 `pg_database_size` 값입니다. 백업은 `pg_dump`로 합니다("백업과 복원" 참고).
- NUL이나 잘못된 UTF-8이 든 이름은 저장할 수 없으므로 400으로 거부합니다.

### 저장된 비밀값 암호화

SMTP 비밀번호, LDAP bind 비밀번호, Crowd 애플리케이션 비밀번호, proxy 저장소의 `remoteAuth` 비밀번호와 Bearer 토큰은 원격 서버에 그대로 보내야 하고 SAML SP 개인키는 서명·복호화에 써야 하므로 사용자 비밀번호나 토큰처럼 해시로 저장할 수 없습니다. **비밀값 키를 정하면** 이 값들을 AES-256-GCM으로 암호화해 DB에 저장하고(`enc:v1:`로 시작하는 값) 쓸 때 풉니다. 그러면 DB, 덤프, 백업 파일이 새어도 키 없이는 비밀값을 알 수 없습니다. 키가 없으면 평문으로 저장하고, 시작할 때마다 WARN 로그(`no secret key is configured`)가 한 줄 남습니다.

- **키 만들기**: 32바이트 난수를 base64로 적은 값입니다. `head -c 32 /dev/urandom | base64`나 `openssl rand -base64 32`로 만들고, 셸이 없는 PC에서는 `docker run --rm busybox sh -c 'head -c 32 /dev/urandom | base64'`를 씁니다.
- **키 주기**: 환경변수 `COMMANDCENTER_SECRET_KEY`에 그 값을 넣거나, 그 값을 담은 파일을 `config.yaml`의 `secretKeyFile`로 가리킵니다(파일은 서버 UID 65532만 읽을 수 있게 두세요). 둘 다 주면 시작하지 않습니다. compose는 `docker-compose.yml`의 주석 처리된 `COMMANDCENTER_SECRET_KEY` 줄을 쓰면 됩니다. Kubernetes에서는 키를 Secret으로 만들고 차트의 `secretKey.existingSecret`에 그 이름을 적습니다("Helm 차트로 설치" 참고. 참조 매니페스트라면 주석 처리된 `COMMANDCENTER_SECRET_KEY`(Secret 참조) 항목). `gc`, `token create` 명령도 같은 키가 필요하고(`docker exec`는 컨테이너의 환경변수를 물려받습니다), `user reset-password`는 키 없이도 됩니다.
- **처음 켤 때**: 키를 주고 시작하면 평문으로 남아 있던 비밀값을 그 자리에서 암호화하고 INFO 로그(`encrypted the stored secrets that were plain text`)를 남깁니다. 그 뒤 저장하는 값은 바로 암호화됩니다. 키와 비밀값은 로그에 남지 않습니다.
- **`remote` URL에 넣은 인증 정보는 암호화하지 않습니다.** `https://user:token@host/…`처럼 URL에 넣은 사용자 이름·비밀번호나 `?token=…` 같은 쿼리는 키를 정해도 DB에 평문으로 남습니다. 사용자 이름과 비밀번호는 URL에서 빼고 `remoteAuth`의 `username`/`password`에 넣으세요(원격이 Bearer 토큰을 받으면 `bearerToken`). 키를 정했는데 이런 proxy가 있으면 시작할 때마다 그 저장소 이름을 적은 WARN 로그(`the remote URL of these proxies holds a user name, password or query…`)가 한 줄 남습니다. URL은 로그에 남지 않습니다. 쿼리로만 토큰을 받는 원격은 옮길 곳이 없으니 DB와 백업의 접근을 제한하세요.
- **키를 잃으면 비밀값을 되살릴 수 없습니다.** 키는 반드시 백업하되 DB 백업과 **다른 곳**에 두세요. 키 없이 시작하면 `it is encrypted, and no secret key is set`, 다른 키로 시작하면 `it was encrypted with another key` 오류로 시작하지 않습니다(다른 키로는 아무것도 다시 쓰지 않습니다). 키를 정말 잃었다면 Command Center를 멈추고(PostgreSQL은 그대로 둡니다) 백업을 받은 뒤, `psql`로 DB에 접속해(compose: `docker compose exec postgres psql -U commandcenter -d commandcenter`, Kubernetes: `kubectl -n commandcenter exec -it commandcenter-postgresql-0 -- psql -U commandcenter -d commandcenter`) 아래처럼 암호화된 값을 비우고 키 확인값(`secretKey.check`)과 토큰 서명 키(`registry.tokenKey`)를 지웁니다. 접근 키를 저장한 S3 blob store는 비밀 키를 비우면 시작하지 못하므로 평문으로 다시 넣습니다(시작할 때 새 키로 암호화합니다). 그다음 새 키로 시작하고 UI에서 비밀번호, 키, webhook 비밀값을 다시 입력합니다.

  ```sql
  BEGIN;
  UPDATE settings SET value = jsonb_set(value::jsonb, '{password}', '""')::text WHERE key = 'smtp';
  UPDATE settings SET value = jsonb_set(value::jsonb, '{bindPassword}', '""')::text WHERE key = 'ldap';
  UPDATE settings SET value = jsonb_set(value::jsonb, '{clientSecret}', '""')::text WHERE key = 'oidc';
  UPDATE settings SET value = jsonb_set(value::jsonb, '{applicationPassword}', '""')::text WHERE key = 'crowd';
  UPDATE settings SET value = jsonb_set(value::jsonb, '{spPrivateKey}', '""')::text WHERE key = 'saml';
  UPDATE settings SET value = jsonb_set(value::jsonb, '{proxy,password}', '""')::text WHERE key = 'http' AND value::jsonb ? 'proxy';
  DELETE FROM settings WHERE key IN ('secretKey.check', 'registry.tokenKey');
  UPDATE repositories SET config = ((config::jsonb - 'signingKey') #- '{remoteAuth,password}' #- '{remoteAuth,bearerToken}')::text
    WHERE config::jsonb ?| array['remoteAuth', 'signingKey'];
  UPDATE webhooks SET secret = '' WHERE secret <> '';
  -- 접근 키를 저장한 S3 blob store마다
  UPDATE blob_stores SET config = jsonb_set(config::jsonb - 'secretKey', '{secretAccessKey}', to_jsonb('<S3 비밀 키>'::text))::text WHERE name = '<store 이름>';
  COMMIT;
  ```

- **키 교체**: `secret-key rotate`가 저장된 비밀값을 지금 키에서 새 키로 한 트랜잭션에 다시 암호화합니다. 실행 중인 서버는 시작할 때 읽은 키를 메모리에 계속 들고 있어서, 서버가 도는 동안 바꾸면 바뀐 값을 읽지 못하고 그 뒤 저장하는 값은 이전 키로 암호화해 두 키가 섞입니다. 그래서 **서버를 먼저 멈추고** 실행한 뒤 새 키로 시작합니다. 실행 전에 백업을 받아 두세요("백업과 복원" 참고).

  ```bash
  docker compose stop
  head -c 32 /dev/urandom | base64 > new.key   # 새 키. 컨테이너의 UID 65532가 읽을 수 있어야 합니다
  docker compose run --rm -v "${PWD}/new.key:/run/new.key:ro" commandcenter secret-key rotate -new-key-file /run/new.key
  # 끝나면 COMMANDCENTER_SECRET_KEY(.env 등)나 secretKeyFile이 가리키는 파일을 new.key의 값으로 바꿉니다
  docker compose up -d
  ```

  - 지금 키는 서버와 같이 `COMMANDCENTER_SECRET_KEY`나 `secretKeyFile`에서 읽으므로, 키 설정은 명령이 끝난 **뒤에** 바꿉니다. 아직 키를 정한 적이 없다면 지금 키는 없어도 됩니다. 새 키는 `-new-key-file`이 가리키는 파일에서 같은 형식(32바이트 난수의 base64, 위의 "키 만들기" 참고)으로 읽습니다. `-new-key-file`이 없거나, 그 파일을 읽지 못하거나, 형식이 틀리면 아무것도 바꾸지 않고 오류로 끝납니다.
  - 지금 키로 암호화된 값은 새 키로 다시 암호화하고, 평문으로 남아 있던 값도 새 키로 암호화합니다. 지금 키로 풀리지 않는 값이 하나라도 있으면(`it was encrypted with another key`, `it is encrypted, and no secret key is set`) 아무것도 바꾸지 않고 `stored secrets (nothing was changed): …` 오류와 0이 아닌 코드로 끝납니다. 교체를 마친 뒤 같은 명령을 다시 실행해도 이렇게 거부됩니다.
  - 출력은 개수뿐이며 키와 비밀값은 나오지 않습니다(예: `re-encrypted 3 stored secrets with the new key and encrypted 1 that were plain text`). 마지막 줄은 `done; start the server with the new key in COMMANDCENTER_SECRET_KEY or secretKeyFile`입니다.
  - `.env`의 값을 바꿨다면 `docker compose start`가 아니라 `docker compose up -d`로 시작하세요. `start`는 컨테이너를 만들 때의 환경변수를 그대로 씁니다. Git Bash에서는 "백업과 복원"처럼 `MSYS_NO_PATHCONV=1`을 앞에 붙이고 `"$(pwd -W)/new.key:/run/new.key:ro"`를 씁니다.
  - 교체 전에 받은 백업은 이전 키로만 풀립니다. 그 백업을 보관하는 동안은 이전 키도 따로 보관하세요. `new.key`는 키를 옮긴 뒤 지웁니다.
  - Kubernetes에서는 "서버를 멈춰야 하는 작업"의 일회용 Pod를 서버 이미지로 바꿔 실행하고(지금 키의 `COMMANDCENTER_SECRET_KEY` Secret 참조, 설정 Secret과 새 키 파일의 마운트를 함께 둡니다), 키 Secret(차트의 `secretKey.existingSecret`)을 새 키로 바꾼 뒤 `--replicas=1`로 되돌립니다.
- 키를 켠 뒤 이 기능이 없는 이전 버전으로 내리면, 이전 버전은 암호화된 값을 그대로 비밀번호로 보내 원격·SMTP·LDAP 인증이 실패합니다("업그레이드" 참고).
- 키가 없을 때는 `enc:v1:`로 시작하는 비밀번호를 저장하지 않습니다(암호화된 값과 구별할 수 없기 때문입니다).

### SSL 인증서(신뢰 저장소)

사내 CA가 서명한 인증서를 쓰는 원격 저장소(proxy의 `remote`), 웹훅 대상, OIDC 제공자, SAML IdP 메타데이터 URL, Crowd 서버, OSV API, LDAP 서버, SMTP 서버에 연결하려면 그 CA 인증서를 신뢰 저장소에 넣습니다. 시스템 루트 인증서(와 `SSL_CERT_DIR`로 마운트한 CA)에 더해 신뢰되며, 서버를 다시 시작하지 않아도 바로 적용됩니다. OIDC의 `caCert`가 있으면 그쪽이 우선하고, LDAP·OIDC의 `insecureSkipVerify`는 여전히 검증을 건너뜁니다.

관리자 화면의 SSL 인증서 페이지나 API로 관리합니다(관리자; 추가·삭제·가져오기는 비밀번호·세션 로그인과 `X-Command-Center-UI: 1` 헤더).

- `GET /-/api/ssl/certificates`: 저장된 인증서 목록. 항목마다 `id`(SHA-256 지문), `subject`, `issuer`, `notBefore`, `notAfter`, `expired`, `isCA`, `pem`이 있습니다.
- `POST /-/api/ssl/retrieve` `{"host":"ldap.corp.local","port":636}`: 서버에 TLS로 접속해 서버가 내민 인증서 체인(리프부터 루트까지)을 검증 없이 가져옵니다. 포트를 생략하면 443이고, HTTPS가 아닌 LDAPS·SMTPS 포트도 됩니다. 각 인증서의 `trusted`로 이미 저장됐는지 알 수 있습니다. 화면에서는 "서버에서 가져오기"로 체인을 본 뒤 CA 인증서를 골라 "신뢰 저장소에 추가"합니다. 가져온 인증서를 추가하기 전에 지문을 다른 경로로 확인하는 것이 안전합니다(처음 접속한 서버의 인증서를 그대로 믿으면 중간자 공격을 막지 못합니다). 접속은 proxy와 같은 송신 정책과 `http.proxy` 설정을 따르고, 닿지 않으면 502입니다.
- `POST /-/api/ssl/certificates` `{"pem":"-----BEGIN CERTIFICATE-----..."}`: PEM을 직접 붙여 넣어 추가합니다(여러 장이 들어 있으면 새 것만 추가; 이미 있으면 409). 보통은 서버 인증서가 아니라 서명한 CA(루트 또는 중간) 인증서를 넣습니다.
- `DELETE /-/api/ssl/certificates/<id>`: 삭제합니다.

활동 이벤트 `ssl.add`·`ssl.remove`가 남습니다. 인증서는 일반 설정의 `trustedCertificates`에 PEM으로 저장되므로 메타데이터 데이터베이스 백업에 포함됩니다. Nexus REST 호환 계층에서는 `GET /service/rest/v1/security/ssl`(목록), `GET /service/rest/v1/security/ssl?host=&port=`(가져오기), `POST /service/rest/v1/security/ssl/truststore`(본문이 PEM), `DELETE /service/rest/v1/security/ssl/truststore/<id>`가 같은 저장소를 다룹니다.

```bash
# 사내 CA 추가
curl -u admin -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' \
  -d "$(jq -Rs '{pem: .}' < corp-root-ca.pem)" https://commandcenter.example.com/-/api/ssl/certificates
```

## Realm, LDAP, OIDC, SAML

**Realm**은 인증 방식입니다. Realm 화면에서 순서와 켜기/끄기를 정합니다(`GET/PUT /-/api/realms`).

| Realm | 하는 일 |
|---|---|
| `local` | DB에 저장된 로컬 사용자의 비밀번호 로그인(UI와 Basic 인증) |
| `token` | API 토큰 |
| `ldap` | LDAP 디렉터리의 사용자·비밀번호 로그인 |
| `oidc` | OpenID Connect 제공자(Gitea, Keycloak 등)를 통한 브라우저 SSO 로그인("OIDC 설정" 참고) |
| `crowd` | Atlassian Crowd(Jira·Confluence 사용자 디렉터리)의 사용자·비밀번호 로그인("Crowd 설정" 참고) |
| `rutauth` | 앞단 리버스 프록시·SSO 게이트웨이가 헤더로 알려 주는 사용자("RUT 인증(리버스 프록시 헤더)" 참고) |
| `saml` | SAML 2.0 IdP(Keycloak, Entra ID, Okta, ADFS 등)를 통한 브라우저 SSO 로그인("SAML 설정" 참고) |

비밀번호 로그인은 켜진 Realm을 **목록 순서대로** 시도합니다(기본은 `local`, `token`이며 `ldap`·`crowd`는 설정을 마친 뒤 켭니다). 앞 Realm에서 실패하면 다음으로 넘어가므로 같은 이름이 로컬과 LDAP에 모두 있으면 순서가 우선순위입니다. `token` Realm을 끄면 모든 토큰이 거부됩니다. `ldap`은 URL, 사용자 기준 DN, 필터를 채우기 전에는 켤 수 없고, `local`, `ldap`, `crowd`를 끄는 변경에는 위의 잠금 방지가 적용됩니다. `oidc`와 `saml`은 브라우저 로그인, `rutauth`는 프록시가 보낸 헤더로 인증하므로 이 순서와 관계없습니다.

`local`과 `ldap` Realm은 **비밀번호 로그인만** 막습니다. `local` Realm을 꺼도 로컬 사용자의 이미 로그인한 웹 세션과 API 토큰(토큰은 `token` Realm이 판단)은 계속 동작합니다. 끊어야 하면 그 사용자를 비활성화하거나 토큰을 폐기하세요.

### LDAP 설정 필드

| 필드 | 기본값 | 설명 |
|---|---|---|
| `url` | 없음 | `ldap://호스트[:포트]` 또는 `ldaps://호스트[:포트]` |
| `startTLS` | `false` | `ldap://`에서 StartTLS로 암호화(`ldaps://`와는 같이 쓸 수 없음) |
| `insecureSkipVerify` | `false` | 서버 인증서 검증 생략. 시험용으로만 쓰세요. 사내 CA가 발급한 인증서라면 검증을 끄지 말고, 그 CA 인증서(PEM 파일)를 컨테이너의 한 디렉터리에 마운트한 뒤 환경변수 `SSL_CERT_DIR`로 그 디렉터리를 알려 주거나, 재시작 없이 화면에서 추가하려면 "SSL 인증서(신뢰 저장소)"에 넣으세요(예: `SSL_CERT_DIR=/etc/ssl/internal-ca`). 이 설정은 SMTP와 proxy의 원격 연결에도 적용되며, 기본 인증서 묶음도 계속 읽으므로 공개 CA를 쓰는 연결에는 영향이 없습니다. |
| `bindDN`, `bindPassword` | 없음 | 검색에 쓰는 서비스 계정. `bindDN`이 없으면 익명 검색. 비밀번호는 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) API 응답·로그·이벤트에 나오지 않습니다(`bindPasswordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearBindPassword: true`로 지웁니다. 단 **서버의 호스트:포트를 바꾸거나, `ldaps`에서 `ldap`으로 바꾸거나, `startTLS`를 끄거나, `insecureSkipVerify`를 켤 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 더 약한 연결로 새어 나가지 않게 하는 규칙입니다. |
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
2. 찾은 항목의 DN으로 사용자 비밀번호를 바인드합니다. 이름이나 비밀번호가 비어 있으면 서버에 보내지 않고 거부합니다(익명 바인드 방지). 출력 가능한 ASCII가 아니거나 앞뒤·연속 공백이 있는 이름도 디렉터리에 보내지 않습니다("보호"의 로그인 시도 제한 참고).
3. 사용자의 `groupAttr` 값을 그룹 매핑에 대응시켜 역할을 정합니다. 성공한 비밀번호 확인은 로컬과 같이 1분간 캐시합니다.

LDAP 사용자는 DB에 사용자 행이 따로 없고, 로그인하면 메모리에 항목이 생깁니다. 세션과 토큰의 소유자 키는 `ldap:<id>`이며(예: `ldap:alice`) 같은 ID의 로컬 사용자와 섞이지 않습니다. `/-/api/me`는 `user`에 `alice`를, `owner`에 `ldap:alice`를 돌려주고, API 토큰 화면은 `owner`로 본인 토큰을 찾습니다. LDAP 사용자의 계정 화면에도 이 소유자 키가 나옵니다. 사용자 목록에서는 같은 ID의 로컬 사용자와 LDAP 사용자가 Realm이 다른 두 행으로 따로 나옵니다. LDAP 사용자는 같은 ID의 역할 덮어쓰기 행(아래)이 있을 때만 그 행과 한 행으로 합쳐지고, 그 행에는 그룹 매핑이 준 역할까지 더한 역할이 나옵니다. 토큰을 Basic 인증의 비밀번호로 쓸 때 사용자 이름은 `ldap:`을 뺀 `alice`입니다. 역할은 요청마다 최신 설정으로 계산하므로 매핑을 바꾸면 곧바로 반영됩니다.

### 그룹 매핑

`groupMappings[].group`은 사용자의 그룹 속성 값과 **전체 DN**(`cn=cc-admins,ou=groups,dc=example,dc=org`) 또는 **첫 RDN의 값인 CN**(`cc-admins`)으로, 대소문자 구분 없이 비교합니다. 일치한 매핑의 역할과 `defaultRoles`의 합집합이 사용자 역할입니다. 주의하세요.

- **CN만 적은 매핑은 같은 CN의 다른 그룹과도 일치합니다.** 서로 다른 OU에 같은 이름의 그룹이 있을 수 있으면 전체 DN으로 적으세요. 특히 `admin` 역할을 주는 매핑은 DN으로 적는 것이 안전합니다.
- `defaultRoles`에 `admin`을 넣으면 디렉터리의 모든 사용자가 관리자가 됩니다(UI가 경고합니다).
- `memberOf` 같은 속성을 서버가 채워 줘야 합니다(OpenLDAP은 memberOf overlay가 필요). 중첩 그룹은 풀지 않습니다.
- Active Directory에서 비활성화된 계정은 자동으로 막히지 않습니다. 필터가 지원하는 `&`, `|`, `!`, 등호, 존재(`attr=*`) 조건으로 허용 그룹을 `userFilter`에 넣으세요(예: `(&(sAMAccountName={user})(memberOf=CN=허용그룹,…))`). AD 확장 매칭 규칙(`:1.2.840…:=`), 부분 일치, 비교 필터는 지원하지 않습니다.

### 로컬 역할 덮어쓰기

사용자 화면에서 `source`가 `ldap`인 사용자를 같은 ID로 만들면 **역할 덮어쓰기 행**이 됩니다. 이 행은 그룹 매핑이 준 역할에 자신의 역할을 **더하고**, `disabled`로 두면 그 LDAP 사용자의 로그인을 막습니다. 행을 지우면 그 사용자가 다시 로그인할 수 있으므로 `disabled`인 행은 지울 수 없습니다(409, `this disabled record blocks LDAP user <id>`). 다시 허용하려는 것이 맞다면 먼저 `active`로 바꾼 뒤 지우세요. 이 행은 로그인 주체가 아니며(비밀번호가 없고, 세션·토큰도 이 행으로는 생기지 않음) 디렉터리에서 사용자가 사라져도 남습니다.

### 1분 재확인과 장애 시 동작

- LDAP 사용자의 항목은 1분이 지나면 다음 사용 때 서비스 계정으로 다시 검색해 확인합니다(**세션, 토큰, 비밀번호 캐시 모두**). 디렉터리에서 사라졌거나 검색 결과가 모호하면 즉시 거부합니다.
- 디렉터리에 닿을 수 없으면 **닫힌 채로(fail closed)** 거부합니다. 재확인이 실패하면 그 시도가 끝난 때부터 10초 동안은 다시 시도하지 않고 거부합니다(클라이언트가 먼저 연결을 끊은 경우는 실패로 치지 않습니다). 한 사용자의 재확인은 한 번에 하나만 하고 같은 사용자의 다른 요청은 그 결과를 기다리며, 기다리는 요청은 클라이언트가 끊으면 바로 끝납니다. 재시작 직후에도 LDAP 사용자의 세션과 토큰은 한 번 재확인을 통과해야 합니다. 로컬 관리자는 영향을 받지 않습니다.
- 재확인은 `userFilter`의 `{user}`가 `userIdAttr`와 같은 값을 가리킨다고 가정합니다(예: `(uid={user})`와 `uid`). 다르면 재확인이 실패해 1분 뒤 세션·토큰이 거부됩니다.
- 로그인 경로의 **장애 백오프**: 연결·TLS 단계(TCP, TLS/StartTLS, 서비스 바인드의 네트워크 오류)가 연달아 3번 실패하면 15초 동안 로그인에서 `ldap` Realm을 건너뛰어(뒤의 Realm이 판단) 로컬 로그인이 느려지지 않게 합니다. 잘못된 비밀번호, 검색 실패, 너무 큰 응답 같은 요청별 실패는 장애로 세지 않습니다. 디렉터리 응답 메시지는 1 MiB로 제한합니다. 검색 결과 항목 하나에는 속성 64개, 속성 하나에는 값 1024개까지 받으며, 넘는 사용자(예: 직접 속한 그룹이 1024개를 넘는 사용자)는 로그인할 수 없습니다.
- 디렉터리에서 사용자가 사라져도 토큰과 세션을 지우지는 않고 접근만 거부합니다(기준 DN 오타로 전부 폐기되는 일을 막기 위해서입니다). 같은 ID가 다시 생기면 이전 토큰이 되살아날 수 있으므로 재사용되지 않는 값을 `userIdAttr`로 쓰세요.

### 연결 테스트

`POST /-/api/realms/ldap/test`(`{user, password}`, 저장된 설정을 사용)는 `connect`, `starttls`, `bind`, `search`, `userBind`, `groups` 단계를 하나씩 실행해 `{ok, steps:[{step, ok, detail}]}`로 돌려줍니다. 사용자 이름을 주면 `search`까지, 비밀번호도 주면 `userBind`와 `groups`(매핑에 일치한 그룹)까지 갑니다. 사용자 이름이 로그인 규칙(출력 가능한 ASCII, 앞뒤 공백과 연속 공백 없음)에 맞지 않으면 맨 앞에 `name` 단계가 실패로 붙고 `ok`는 `false`가 됩니다. 디렉터리가 그 이름으로 항목을 찾더라도 로그인은 그 이름을 거부하기 때문입니다. API는 이름을 보낸 그대로 검사합니다(설정 화면은 앞뒤 공백을 지우고 보냅니다). 응답에 비밀번호는 나오지 않습니다. 설정 화면의 테스트 버튼이 이 호출입니다.

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

### OIDC 설정

`oidc` Realm은 OpenID Connect 제공자(IdP)로 **브라우저에서** 로그인합니다(authorization code + PKCE). 로그인 화면의 SSO 버튼(`GET /-/api/auth/sso`가 알려 주는 `{"providers":[{"id":"oidc","name":"<displayName>","url":"/-/sso/oidc/start"}]}`, Realm이 꺼져 있거나 `issuer`가 비어 있으면 빈 목록)을 누르면 제공자로 갔다가 `<baseURL>/-/sso/oidc/callback`으로 돌아와 웹 세션이 생깁니다. 비밀번호가 오가지 않으므로 Basic 인증이나 `npm login` 같은 **CLI 클라이언트는 OIDC로 로그인할 수 없고, 웹 UI에서 발급한 API 토큰을 씁니다**(토큰 소유자는 `oidc:<id>`, Basic 인증의 사용자 이름은 `oidc:`를 뺀 ID). 설정은 Realm 화면(`GET/PUT /-/api/realms/oidc`, 관리자)에서 하고, `issuer`와 `clientId`를 채우기 전에는 Realm을 켤 수 없습니다.

| 필드 | 기본값 | 설명 |
|---|---|---|
| `issuer` | 없음 | 제공자의 issuer URL. `https://`여야 하고(`http://`는 `localhost`/`127.0.0.1`만 허용) 사용자 정보·쿼리·프래그먼트가 없어야 합니다. `<issuer>/.well-known/openid-configuration`을 읽어 각 엔드포인트를 알아내며, 문서의 `issuer`가 설정과 다르면 거부합니다(1시간 캐시). |
| `clientId`, `clientSecret` | 없음 | 제공자에 등록한 OAuth2 애플리케이션. 비밀값은 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) 응답·로그·이벤트에 나오지 않습니다(`clientSecretSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearClientSecret: true`로 지웁니다. **`issuer`를 바꾸거나 `insecureSkipVerify`를 켤 때는 비밀값을 다시 입력해야 합니다**(400). 토큰 교환은 `client_secret_basic`이고, 제공자가 `token_endpoint_auth_methods_supported`에서 basic을 빼면 `client_secret_post`로 보냅니다. 비밀값이 비어 있으면 공개 클라이언트로(PKCE만) 교환합니다. |
| `displayName` | `SSO` | 로그인 화면의 버튼 이름 |
| `scopes` | `openid profile email groups` | 요청할 scope. `openid`는 필수입니다. |
| `usernameClaim` | `preferred_username` | 사용자 ID로 쓸 claim. 값은 소문자로 바꾼 뒤 사용자 ID 규칙(영문·숫자·`.`·`_`·`@`·`-`, 64자 이하)에 맞아야 하며 아니면 400입니다. |
| `groupsClaim` | `groups` | 그룹 목록(문자열 배열) claim. ID 토큰에 없으면 `userinfo` 엔드포인트에서 가져오고, 거기에도 없으면 그룹 없음으로 봅니다. |
| `groupMappings`, `defaultRoles` | 없음 | LDAP와 같은 그룹 → 역할 매핑(그룹 이름은 대소문자 구분 없이 비교) |
| `caCert` | 없음 | 시스템 인증서 묶음에 **더해** 신뢰할 CA 인증서(PEM, 여러 장 가능). Kubernetes 안의 자체 서명 IdP에 쓰세요. |
| `insecureSkipVerify` | `false` | 인증서 검증 생략. 시험용으로만 쓰세요. |
| `timeoutSeconds` | `10` | 제공자 호출(discovery, JWKS, 토큰, userinfo)의 제한(1..60) |

로그인 흐름과 검증은 다음과 같습니다.

1. `GET /-/sso/oidc/start?next=<#해시>`가 state·nonce·PKCE verifier를 만들어 데이터베이스에 10분간 보관하고(state 쿠키 `cc_oidc_state`, `https` baseURL에서는 `__Host-cc_oidc_state`) 제공자의 authorization 엔드포인트로 302합니다. `next`는 `#`으로 시작하는 512자 이하의 해시여야 하며 아니면 무시합니다. `baseURL`이 비어 있으면 503입니다.
2. 제공자가 `/-/sso/oidc/callback?code&state`로 돌려보내면 쿠키의 state와 비교하고 보관한 항목을 **한 번만** 씁니다(재사용·만료는 400). 토큰 엔드포인트에서 code를 교환하고 ID 토큰을 검증합니다: 서명(RS256·ES256, JWKS는 캐시하고 모르는 `kid`가 오면 1분에 한 번만 다시 받음), `iss`, `aud`(여럿이면 `azp`), `exp`(60초 허용), `iat`, `nonce`. 제공자가 `error`로 돌려보내면 로그인 실패 페이지(400)입니다. 응답 본문은 모두 1 MiB로 제한하고, 제공자 호출은 proxy와 같은 송신 정책(사설·루프백 주소 차단, `caCert`와 `insecureSkipVerify` 적용)을 따릅니다.
3. 사용자 ID와 그룹을 데이터베이스에 기록하고 소유자 `oidc:<id>`의 세션을 만든 뒤 `/` + `next`로 302합니다. 역할은 요청마다 최신 매핑과 `source`가 `oidc`인 **역할 덮어쓰기 행**(LDAP와 같은 규칙: 역할을 더하고, `disabled`면 로그인 거부 403)으로 계산합니다. 사용자 화면에는 로그인한 OIDC 사용자가 `source: oidc`, `realm: OIDC` 행으로 보이고 `/-/api/me`의 `realm`은 `OIDC`입니다.

OIDC 사용자의 항목은 제공자에 다시 묻지 않습니다. 신원은 데이터베이스에 두므로 재시작한 뒤나 다른 노드에서도 세션과 토큰이 그대로 동작합니다. 마지막 로그인 후 7일이 지나거나 OIDC 설정을 바꾸면(클라이언트 시크릿만 바꾼 경우는 제외) 이전 항목을 쓰지 않으므로, 그 사용자의 세션과 API 토큰은 다시 로그인할 때까지 거부됩니다(브라우저는 SSO 버튼을 다시 누르면 됩니다. "세션과 SSO 상태" 참고). 진행 중인 로그인 state도 데이터베이스에 둡니다. 제공자에서 사용자를 지워도 Command Center가 바로 알지는 못하므로 즉시 막으려면 `source: oidc` 덮어쓰기 행을 `disabled`로 두거나 토큰을 폐기하세요.

`POST /-/api/realms/oidc/test`(PUT과 같은 본문, 저장하지 않음)는 보낸 설정으로 discovery 문서를 읽어 `{issuer, authorizationEndpoint, tokenEndpoint, jwksUri, scopesSupported, algs}`를 돌려주고, 실패하면 400과 이유입니다(`caCert`가 맞는지 확인할 때 쓰세요). 설정 변경은 활동 기록에 `realms.oidc.update`로 바뀐 키 이름만 남습니다.

예: Gitea 1.26을 제공자로 쓰는 경우입니다. Gitea의 **설정 → 애플리케이션**에서 OAuth2 애플리케이션을 만들고 리디렉션 URI에 `<baseURL>/-/sso/oidc/callback`(예: `https://repo.example.com/-/sso/oidc/callback`)을 넣은 뒤 클라이언트 ID와 비밀값을 가져옵니다. Gitea의 `groups` claim은 사용자가 속한 조직과 팀을 `조직:팀` 꼴(예: `platform:owners`)로 주므로 그 이름을 매핑에 적습니다.

```
issuer:         https://gitea.example
clientId:       (Gitea가 준 Client ID)
clientSecret:   (Gitea가 준 Client Secret)
scopes:         openid profile email groups
usernameClaim:  preferred_username    groupsClaim: groups
groupMappings:  [{ "group": "platform:owners", "roles": ["admin"] },
                 { "group": "platform:developers", "roles": ["deployer"] }]
caCert:         (Gitea가 사내 CA 인증서를 쓰면 그 PEM)
```

저장한 뒤 테스트 버튼으로 discovery를 확인하고 Realm 화면에서 `oidc`를 켜면 로그인 화면에 SSO 버튼이 나타납니다. CI와 빌드 도구는 OIDC로 로그인한 사용자가 API 토큰 화면에서 만든 토큰을 씁니다.

### Crowd 설정

`crowd` Realm은 사용자 이름과 비밀번호를 Atlassian Crowd에 확인받습니다. Crowd에 Command Center를 **애플리케이션**(이름과 비밀번호, 필요하면 허용 주소)으로 등록한 뒤 Realm 화면(`GET/PUT /-/api/realms/crowd`, 관리자)에서 설정합니다. `url`과 `applicationName`을 채우기 전에는 Realm을 켤 수 없습니다. 웹 UI 로그인, Basic 인증, `npm login` 모두 Realm 목록 순서대로 시도합니다.

| 필드 | 기본값 | 설명 |
|---|---|---|
| `url` | 없음 | Crowd 주소(컨텍스트 경로까지, 예: `https://crowd.example.com/crowd`). `http://` 또는 `https://`이고 사용자 정보·쿼리·프래그먼트가 없어야 합니다. 뒤에 `/rest/usermanagement/1/...`을 붙여 호출하며, 프록시 설정과 SSL 신뢰 저장소를 따릅니다. `https://`를 권장합니다. |
| `applicationName`, `applicationPassword` | 없음 | Crowd에 등록한 애플리케이션. 비밀번호는 저장만 되고(비밀값 키를 정하면 암호화해서) 응답·로그·이벤트에 나오지 않습니다(`applicationPasswordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearApplicationPassword: true`로 지웁니다. **주소의 scheme·호스트·포트를 바꿀 때는 비밀번호를 다시 입력해야 합니다**(400). |
| `timeoutSeconds` | `10` | Crowd 호출 하나의 제한 시간(1~60초) |
| `cacheMinutes` | `1` | 로그인 성공(사용자 이름과 비밀번호의 해시로 기억하며 비밀번호 자체는 저장하지 않음)과 사용자의 그룹을 Crowd에 다시 묻지 않고 쓰는 시간(1~60분) |
| `groupMappings`, `defaultRoles` | 없음 | LDAP와 같은 그룹 → 역할 매핑. 사용자가 속한 **중첩 그룹까지** 대소문자 구분 없이 비교합니다. |

- **로그인.** `POST <url>/rest/usermanagement/1/authentication?username=<이름>`으로 비밀번호를 확인하고 `GET .../user/group/nested`로 그룹을 읽습니다. 비밀번호가 틀리거나 사용자가 없거나 비활성(`INACTIVE_ACCOUNT`)이면 401입니다. Crowd가 돌려준 사용자 이름이 사용자 ID 규칙(영문·숫자·`.`·`_`·`@`·`-`, 64자 이하)에 맞지 않으면 거부합니다. Crowd SSO 쿠키는 쓰지 않습니다(비밀번호 로그인만).
- **사용자와 역할.** Crowd 사용자의 세션과 토큰 소유자는 `crowd:<id>`이고 `/-/api/me`의 `realm`은 `Crowd`입니다. 역할은 그룹 매핑, 기본 역할, 그리고 같은 ID의 `source: crowd` 로컬 행(사용자 화면에서 만들며, 역할을 더하거나 비활성화로 로그인을 막습니다)으로 정해집니다. 최근 로그인한 Crowd 사용자는 사용자 목록에 `directory` 행으로 보입니다.
- **캐시와 장애.** `cacheMinutes` 안에 같은 비밀번호로 다시 로그인하면 Crowd에 묻지 않습니다. Crowd에서 비밀번호를 바꾸면 새 비밀번호로 로그인하는 순간 이전 비밀번호의 캐시가 지워지지만, 그 전까지는 이전 비밀번호가 최대 `cacheMinutes` 동안 통할 수 있습니다. 세션과 토큰은 `cacheMinutes`가 지나면 Crowd에 사용자를 다시 확인하며, Crowd에서 비활성화되거나 삭제된 사용자는 그때 거부됩니다. Crowd에 연결할 수 없으면 Crowd 사용자는 **모두 거부**되고(fail closed) 로컬 관리자는 영향을 받지 않습니다. 실패한 재확인은 10초 동안 다시 시도하지 않습니다. 상태 화면의 "Crowd reachable" 점검이 애플리케이션 인증(`GET .../config/cookie`)을 확인합니다.
- **연결 테스트.** `POST /-/api/realms/crowd/test`(`{"username":"...","password":"..."}`, 둘 다 생략 가능)는 저장된 설정으로 애플리케이션 인증, 사용자 비밀번호(비밀번호를 비우면 사용자 존재), 그룹과 매핑된 역할을 단계별로 보여 줍니다.
- **잠금 방지.** Crowd의 그룹 매핑이나 기본 역할이 admin을 주면 Crowd도 관리자가 로그인할 수 있는 경로로 셉니다.
- 설정 변경은 활동 기록에 `realms.crowd.update`(바뀐 키 이름만), 연결 테스트는 `realms.crowd.test`로 남습니다.

### RUT 인증(리버스 프록시 헤더)

앞단의 리버스 프록시나 SSO 게이트웨이(예: OAuth2 Proxy, Apache `mod_auth_*`, 사내 SSO 에이전트)가 사용자를 인증하고 사용자 이름을 헤더로 넘겨 주는 구성에서는 `rutauth` Realm(Nexus의 Rut Auth)을 씁니다. Realm 화면 또는 `GET/PUT /-/api/realms/rutauth`(관리자)에서 설정합니다.

| 필드 | 기본값 | 설명 |
|---|---|---|
| `header` | `REMOTE_USER` | 사용자 이름을 담은 헤더 |
| `trustedProxies` | 없음(필수) | 헤더를 믿을 프록시의 CIDR 목록(예: `10.0.0.0/8`, `192.168.1.10/32`) |
| `groupHeader` | 없음 | 쉼표로 구분한 그룹 목록을 담은 헤더(선택) |
| `groupMappings` | 없음 | 그룹을 역할로 바꾸는 매핑(`groupHeader`가 있어야 하며 LDAP 그룹 매핑과 같은 형식) |
| `defaultRoles` | 없음 | 이 Realm으로 들어온 모든 사용자에게 주는 역할 |

- 요청이 `trustedProxies`에 든 주소에서 **직접** 들어오고(TCP 연결의 상대 주소만 봅니다. `X-Forwarded-For`, `X-Real-IP`, `Forwarded`는 보지 않습니다), `header`가 있고, `Authorization`이 없으면 그 사용자로 인증합니다. 사용자 이름은 앞뒤 공백을 떼고 소문자로 바꾸며, 사용자 id 규칙에 맞지 않으면 익명으로 처리합니다. 로그인 화면을 거치지 않고 `GET /-/api/me`가 바로 그 사용자를 보여 줍니다(`realm`: `RUT`).
- `Authorization`(Basic, Bearer, 토큰)이 있으면 그쪽이 우선하고, 세션 쿠키보다는 헤더가 우선합니다.
- **프록시는 클라이언트가 보낸 같은 이름의 헤더를 반드시 지우거나 덮어써야 합니다.** 그렇지 않으면 누구나 그 헤더로 다른 사용자가 될 수 있습니다. Command Center 포트에는 그 프록시만 접속할 수 있게 하세요. nginx는 기본적으로 밑줄이 든 헤더(`REMOTE_USER`)를 버리므로 `X-Forwarded-User` 같은 이름을 쓰거나 `underscores_in_headers on`을 켜세요.
- 역할은 그룹 매핑, 이 Realm의 `defaultRoles`, 일반 설정의 `defaultRoles`, 같은 id의 덮어쓰기 사용자(`source: "rutauth"`)의 역할을 합친 것입니다. 덮어쓰기 사용자를 비활성화하면 그 사용자는 들어올 수 없습니다. 로컬 사용자와 이름이 같아도 로컬 사용자의 역할은 받지 않습니다.
- `header`와 `trustedProxies`를 채우기 전에는 Realm을 켤 수 없고, 켜진 동안 `trustedProxies`를 비울 수 없습니다.
- RUT 사용자도 API 토큰을 만들 수 있습니다(소유자 `rutauth:<id>`). 토큰은 Realm이 켜져 있고 서버가 시작한 뒤 그 사용자가 프록시를 거쳐 한 번 이상 들어온 동안만 통하므로, 서버를 다시 시작하면 그 사용자가 웹 UI에 한 번 들어올 때까지 토큰이 거부됩니다.
- 웹 UI의 쓰기 작업은 세션 로그인과 같은 규칙(`X-Command-Center-UI` 헤더와 교차 출처 검사)을 따릅니다. 설정을 바꾸면 활동 이벤트 `realms.rutauth.update`가 남습니다.

### SAML 설정

`saml` Realm은 SAML 2.0 IdP(Keycloak, Microsoft Entra ID, Okta, ADFS 등)로 **브라우저에서** 로그인합니다(SP-initiated 웹 SSO: AuthnRequest는 HTTP-Redirect 바인딩, Response는 HTTP-POST 바인딩). 로그인 화면의 SSO 버튼(`GET /-/api/auth/sso`가 알려 주는 `{"id":"saml","name":"<displayName>","url":"/-/sso/saml/login"}`)을 누르면 IdP로 갔다가 IdP가 `<baseURL>/-/sso/saml/acs`로 응답을 보내 웹 세션이 생깁니다. OIDC처럼 비밀번호가 오가지 않으므로 CLI 클라이언트는 웹 UI에서 발급한 API 토큰을 씁니다(토큰 소유자는 `saml:<id>`). 일반 설정의 `baseURL`이 있어야 합니다(없으면 503).

설정은 Realm 화면(`GET/PUT /-/api/realms/saml`, 관리자. PUT은 비밀번호·세션 로그인과 `X-Command-Center-UI: 1` 헤더)에서 합니다. PUT은 설정 전체를 바꾸며 보내지 않은 값은 기본값이 됩니다. IdP 메타데이터를 넣기 전에는 Realm을 켤 수 없고, 켜진 동안 메타데이터를 비울 수 없습니다.

| 필드 | 기본값 | 설명 |
|---|---|---|
| `idpMetadataXML`, `idpMetadataURL` | 없음 | IdP 메타데이터. 붙여 넣거나(1 MiB 이하) URL을 줍니다(둘 중 하나만). URL은 `https://`여야 하고(`http://`는 localhost만) 프록시 설정과 SSL 신뢰 저장소를 따라 받으며, 하루마다 다시 받고 받지 못하면 이전 사본을 계속 씁니다. 메타데이터의 entity ID, SSO 주소(HTTP-Redirect 바인딩), 서명 인증서를 씁니다. |
| `displayName` | `SAML` | 로그인 화면의 버튼 이름 |
| `entityId` | `<baseURL>/-/sso/saml/metadata` | 이 서버(SP)의 entity ID. IdP에 등록하는 값이며 응답의 audience가 이 값이어야 합니다. |
| `usernameAttribute` | `NameID` | 사용자 ID로 쓸 속성 이름. `NameID`는 subject의 NameID입니다. 값은 앞뒤 공백을 떼고 소문자로 바꾼 뒤 사용자 ID 규칙(영문·숫자·`.`·`_`·`@`·`-`, 64자 이하)에 맞아야 하며 아니면 로그인이 실패합니다(400). |
| `firstNameAttribute`, `lastNameAttribute`, `emailAttribute` | 없음 | 표시 이름과 이메일을 읽을 속성 |
| `groupsAttribute` | 없음 | 그룹 목록을 담은 속성(값이 여럿) |
| `groupMappings`, `defaultRoles` | 없음 | LDAP와 같은 그룹 → 역할 매핑(대소문자 구분 없이 비교) |
| `validateResponseSignature`, `validateAssertionSignature` | `true`, `true` | Response의 서명과 assertion의 서명을 검사합니다. 둘 다 끌 수는 없습니다. 켠 위치에 있는 서명은 모두 맞아야 하고, 그런 서명이 적어도 하나 있어야 합니다. |
| `allowSHA1Signatures` | `false` | RSA-SHA1 서명도 받습니다(기본은 RSA-SHA256과 ECDSA-SHA256). |
| `allowIdpInitiated` | `false` | IdP에서 시작한 로그인(이 서버가 보낸 요청에 대한 답이 아닌 응답)도 받습니다. |
| `clockSkewSeconds` | `120` | 유효 기간 검사에서 허용하는 시계 차이(0~600초) |
| `signAuthnRequests` | `false` | AuthnRequest에 SP 키로 서명합니다(RSA-SHA256). IdP가 서명된 요청을 요구하면(메타데이터의 `WantAuthnRequestsSigned`) 켜세요. |

- **SP 키와 메타데이터.** 처음 저장할 때 SP 키 쌍(RSA 2048비트, 10년짜리 자체 서명 인증서)을 만듭니다. 개인키는 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) API·로그·이벤트에 나오지 않으며, 인증서는 `spCertificate`와 `spCertificateInfo`(주체, SHA-256 지문, 유효 기간)로 보입니다. 이 키로 암호화된 assertion을 풀고 요청에 서명합니다. IdP에 등록할 SP 메타데이터(entity ID, ACS `<baseURL>/-/sso/saml/acs`, 인증서)는 `GET <baseURL>/-/sso/saml/metadata`(IdP 메타데이터를 넣은 뒤, 인증 없이)나 `GET /-/api/realms/saml/sp-metadata`(관리자, 언제나)에서 받습니다. GET 응답의 `idp`는 읽어 들인 IdP 메타데이터(entity ID, SSO 주소, 인증서와 만료 여부)이고, 읽지 못하면 `idpError`에 이유가 나옵니다.
- **검증.** 응답은 256 KiB 이하이고 Response 바로 아래에 assertion이 정확히 하나 있어야 하며, DTD가 있으면 거부합니다. 서명은 IdP 메타데이터의 인증서로만 확인하고 서명이 덮는 부분만 읽으므로 서명 감싸기(signature wrapping) 공격이 통하지 않습니다. 그 밖에 Destination과 SubjectConfirmation의 Recipient가 ACS 주소인지, InResponseTo가 이 로그인이 보낸 요청인지, Issuer가 IdP인지, audience가 `entityId`인지, NotBefore·NotOnOrAfter·SessionNotOnOrAfter를 확인하고, 이미 받은 assertion ID는 거부합니다(재전송 방지). 암호화된 assertion(RSA-OAEP와 AES-CBC·AES-GCM)도 받습니다. 실패하면 로그인 실패 페이지(400)에 이유가 나오고 서버 로그에 경고가 남습니다.
- **사용자와 역할.** 세션과 토큰의 소유자는 `saml:<id>`이고 `/-/api/me`의 `realm`은 `SAML`입니다. 역할은 그룹 매핑, 이 Realm의 `defaultRoles`, 일반 설정의 `defaultRoles`, 같은 ID의 `source: saml` 덮어쓰기 행(역할을 더하고, `disabled`면 로그인 거부 403)으로 정해집니다. 최근 로그인한 SAML 사용자는 사용자 목록에 `directory` 행으로 보입니다. 그룹 매핑이나 기본 역할에 쓰인 역할은 지울 수 없습니다(409).
- **상태 저장.** 진행 중인 로그인(10분)과 재전송 방지 기록은 데이터베이스에 두므로 노드가 바뀌어도 동작합니다. SAML 사용자의 항목도 데이터베이스에 두어 재시작과 다른 노드에서도 유지됩니다("세션과 SSO 상태" 참고). 받아 둔 IdP 메타데이터는 노드마다 따로 받습니다.
- **로그아웃.** Single Logout(SLO)은 없습니다. 로그아웃은 Command Center 세션만 끝내고 IdP 세션은 그대로 둡니다.
- **https.** 로그인 상태 쿠키(`__Host-cc_saml_state`)는 `https` baseURL에서 `SameSite=None; Secure`입니다. IdP의 응답은 다른 사이트에서 오는 POST이므로 `http` baseURL에서는 브라우저가 쿠키를 보내지 않아 로그인이 실패할 수 있습니다. `baseURL`을 `https://`로 두세요.
- 설정 변경은 활동 기록에 `realms.saml.update`로 바뀐 키 이름만 남습니다(webhook `realm.updated`).

## Blob Store

blob(파일 내용)은 **blob store** 디렉터리에 sha256 이름으로 저장됩니다.

- **default store**: `<dataDir>/blobs`. 기존 설치와 같은 위치이며 이름은 `default`입니다. 삭제할 수 없습니다. 현재 `dataDir`를 기준으로 하므로 `dataDir`를 옮겨도 따라갑니다.
- **이름 붙은 store**: Blob Store 화면(`POST /-/api/blobstores`)에서 만듭니다. 경로를 비우면 `<dataDir>/blobstores/<이름>`이고, 절대 경로를 주면 그곳을 씁니다. 상대 경로는 400입니다(이전 버전은 상대 경로를 말없이 무시하고 기본 위치에 만들었습니다). 경로가 데이터 디렉터리 밖이면 응답과 `GET /-/api/blobstores`의 `warning`, Blob Store 상세 화면에 경고가 나옵니다. 그런 경로는 영구 볼륨으로 따로 마운트하고 따로 백업해야 합니다("저장 공간과 인스턴스", "백업과 복원" 참고). `<경로>/blobs/sha256`이 **없거나 비어 있어야** 하고(이미 blob이 있으면 거부, 다른 파일이 있는 디렉터리는 괜찮습니다), 쓸 수 있어야 하며, 다른 store의 blob 디렉터리와 겹치거나 심볼릭 링크로 같은 곳을 가리키면 거부됩니다. 기존 store의 경로를 확인할 수 없으면(끊어진 심볼릭 링크 등) 겹치지 않는다고 확신할 수 없으므로 그 store 이름을 알려 주며 거부합니다. 이름 규칙은 저장소와 같습니다.
- **저장소는 만들 때 store를 고릅니다**(`blobStore`, 생략하면 `default`). 저장소를 만든 뒤에는 편집으로 바꿀 수 없고 `repository.change-blobstore` 작업으로 옮깁니다("저장소의 blob store 바꾸기" 참고). group 저장소에는 store가 없습니다(blob store 그룹과는 다릅니다).
- store마다 **중복 제거와 GC가 독립**입니다. 같은 파일을 서로 다른 store의 저장소에 올리면 각각 저장되고, GC는 자기 store의 저장소가 참조하지 않는 blob만 지웁니다. 단 **default store의 GC는 어느 저장소든 참조하는 blob은 지우지 않습니다.** 이름 붙은 store를 모르는 이전 버전으로 돌린 적이 있으면 그 저장소들의 파일이 default store에 쓰였을 수 있고, 그것이 유일한 사본일 수 있기 때문입니다("업그레이드"의 다운그레이드 안내 참고). 그래서 같은 파일이 default store의 저장소에서 지워진 뒤에도, 이름 붙은 store의 저장소가 참조하는 동안은 default store의 사본이 남습니다. 존재하지 않는 store를 가리키는 저장소가 있으면(설정 가져오기 등) 조용히 default로 가지 않고 오류로 응답하며, GC는 그 저장소가 모든 store를 참조 중인 것으로 보고 지우지 않습니다.
- **사용량**: Blob Store 화면과 `GET /-/api/blobstores`가 store별 blob 수·용량(그 store의 저장소가 참조하는 것), 저장소 수, 파일시스템 전체/남은 용량(리눅스)을 보여 줍니다.
- **소프트 쿼터**: store마다 `quota`를 둘 수 있습니다(그룹도 됩니다). `{"type": "spaceUsedBytes", "limitBytes": N}`은 그 store의 저장소가 쓰는 용량이 N바이트를 넘으면, `{"type": "spaceRemainingBytes", "limitBytes": N}`은 store 파일시스템의 남은 용량이 N바이트보다 적으면 위반입니다(S3·Azure store는 남은 용량을 알 수 없으므로 `spaceRemainingBytes`가 위반되지 않습니다). 만들 때(`POST /-/api/blobstores`의 `quota`) 정하거나 `PUT /-/api/blobstores/<이름>`에 `{"quota": {...}}`(없애려면 `{"quota": null}`)를 보내 바꿉니다. 파일·S3 store는 이 PUT으로 쿼터만 바꿀 수 있고, 그룹은 같은 요청에 `members`·`fillPolicy`를 함께 보낼 수 있으며 멤버를 바꿔도 쿼터는 유지됩니다. 소프트 쿼터이므로 넘어도 업로드는 계속 받습니다. 상태 검사(시스템 상태 화면, `GET /-/api/status/checks`, `GET /service/rest/v1/status/check`, Prometheus의 `commandcenter_health_check`)가 쿼터가 있는 store마다 `Blob store quota: <이름>` 항목을 평가해 위반이면 실패로 표시하고(`soft quota exceeded: …`), `GET /-/api/blobstores`는 `quota`와 `quotaViolated`를 보여 줍니다. 위반이 시작되거나 풀릴 때마다 활동 이벤트 `blobstore.quota`(행위자 `system`)가 한 번씩 남습니다. 이 판단은 메모리에만 두므로 서버를 다시 시작한 뒤 처음 위반을 발견하면 다시 남습니다. 쿼터는 상태 검사를 읽을 때만 평가하므로, 이벤트를 받으려면 모니터링이 상태 검사나 메트릭을 주기적으로 읽어야 합니다. Nexus 호환 API에서는 `softQuota`(`type`: `spaceUsedQuota` 또는 `spaceRemainingQuota`, `limit`: MB)와 `GET /service/rest/v1/blobstores/<이름>/quota-status`로 다룹니다.
- **삭제**: 저장소가 쓰고 있는 store, 그룹의 멤버, 저장소가 옮겨 오거나 떠나는 중인 store는 409입니다. 삭제해도 **디렉터리와 파일은 디스크에 남습니다**(관리자가 직접 정리하세요).
- **compact 작업**(`blobstore.compact`)은 선택 파라미터 `blobStore`로 한 store만 정리하고, 생략하면 모든 store를 정리합니다("예약 작업" 참고).
- **S3 store**: Blob Store 화면(`POST /-/api/blobstores`)에서 `type: "s3"`로 만들면 blob이 디렉터리 대신 S3 호환 버킷(AWS S3, MinIO, Ceph RGW)에 `<prefix>/<sha256>` 키로 저장됩니다. 필드는 `s3.bucket`(필수), `s3.prefix`(선택, 앞뒤 `/` 없이), `s3.region`(기본 `us-east-1`), `s3.endpoint`(비우면 `https://s3.<region>.amazonaws.com`, MinIO/Ceph는 그 주소), `s3.pathStyle`(`<endpoint>/<bucket>/<key>` 방식, MinIO는 보통 켬), `s3.accessKeyId`/`s3.secretAccessKey`(둘 다 비우면 `AWS_ACCESS_KEY_ID`, `AWS_SECRET_ACCESS_KEY`, `AWS_SESSION_TOKEN` 환경 변수), `s3.sse`(`AES256` 또는 `aws:kms`, 업로드마다 `x-amz-server-side-encryption` 헤더). 만들 때 버킷을 목록 조회해 자격 증명과 버킷을 확인하며(실패하면 400), 같은 endpoint·버킷·prefix를 쓰는 다른 S3 store가 있으면 거부합니다. `secretAccessKey`는 다른 비밀값처럼 비밀값 키로 암호화해 저장하고(`secretKeyFile`), 응답과 화면에는 보이지 않습니다(`GET /-/api/blobstores`는 값 대신 `secretSet: true/false`만 돌려줍니다). 64 MiB를 넘는 파일은 멀티파트 업로드로 올리고, 다운로드는 Range 요청을 그대로 버킷의 Range GET으로 넘깁니다. `path`(비우면 `<dataDir>/blobstores/<이름>`)는 업로드 중인 임시 파일과 Docker 업로드 세션만 두는 로컬 디렉터리이므로 작지만 영구 볼륨에 두세요. compact 작업(GC)은 prefix 아래를 나열해 참조되지 않는 객체를 지웁니다. 삭제해도 버킷의 객체는 남습니다. S3 store를 모르는 이전 버전(구조 버전 1)은 이 store의 경로를 빈 디렉터리로 열어 파일을 찾지 못하므로 내리지 마세요("업그레이드" 참고).

### 저장소의 blob store 바꾸기

관리자는 저장소의 blob store를 옮길 수 있습니다(`POST /-/api/repositories/<이름>/blobstore`, 본문 `{"blobStore":"<대상>"}`, 비밀번호·세션 로그인과 `X-Command-Center-UI: 1` 헤더). 응답은 202와 작업 ID이며, 이동은 그 저장소의 `repository.change-blobstore` 작업(저장소마다 하나를 다시 씀)으로 백그라운드에서 실행됩니다. 진행 상황은 작업 실행 로그와 저장소 목록의 `blobStoreMove`(`source`, `target`, `state`, `copied`, `total`)에서 봅니다. 이동하는 동안 읽기는 새 store에 없으면 이전 store에서 읽으므로 끊기지 않고, 새 업로드는 바로 새 store에 씁니다. 모든 blob을 복사하며 sha256을 확인한 뒤 한 트랜잭션에서 저장소 설정의 `blobStore`를 바꿉니다. 이전 store의 사본은 다음 compact 작업이 지웁니다(default store는 어느 저장소든 참조하는 blob을 모두 보관하므로 default에서 옮긴 사본은 남습니다).

- 이동이 실패하거나 중지되거나 서버 재시작으로 끊기면 미완료(`failed`, 재시작 뒤에는 `interrupted`)로 남아 계속 양쪽에서 읽고 새 store에 씁니다. 작업을 다시 실행하면 이어서 끝냅니다.
- 이동이 끝나지 않은 동안에는 관련된 두 store를 삭제할 수 없고(409), 그 저장소의 컴포넌트를 스테이징할 수 없으며, 다른 대상으로의 이동도 거부합니다.
- 이동을 시작할 때 진행 중이던 Docker 업로드 세션은 `BLOB_UPLOAD_UNKNOWN`으로 실패하므로 다시 올려야 합니다. 이동은 작업 실행기를 차지하므로 그동안 다른 예약 작업은 기다립니다.

### Blob store 그룹과 S3 장애 조치

**그룹**(`type: "group"`)은 blob을 여러 파일·S3 store(멤버)에 나눠 담는 store입니다. 저장소는 그룹을 다른 store처럼 고릅니다. `POST /-/api/blobstores`에 `{"name":"g","type":"group","members":["a",{"name":"b","readOnly":true}],"fillPolicy":"roundRobin"}`처럼 보냅니다. 멤버는 이름만 쓰거나 `{"name","readOnly"}`로 씁니다.

- **쓰기**: `roundRobin`(기본)은 쓰기 가능한 멤버에 차례로, `writeToFirst`는 첫 번째 쓰기 가능한 멤버에 씁니다. 이미 가진 멤버가 알려진 blob은 그 멤버에 다시 씁니다. 쓰기 가능한 멤버가 하나는 있어야 합니다. 고른 멤버에 쓰지 못하면(S3 멤버 장애, 디스크 가득 참) 다른 멤버로 다시 시도하지 않고 업로드가 실패합니다.
- **읽기 전용 멤버**는 읽기만 하고 새 blob을 받지 않습니다(비우는 중인 멤버). 바꾸는 요청이 끝나면 그 멤버로 가던 쓰기도 모두 끝난 상태입니다.
- **읽기**는 그 blob을 마지막으로 준 멤버부터, 없으면 멤버 순서대로 찾습니다.
- **멤버 조건**: 파일, S3 또는 Azure store여야 하고(그룹 안에 그룹은 안 됩니다), 한 그룹에만 속하며, default store가 아니고, 그 store를 쓰는 저장소가 없어야 합니다. 멤버 store를 직접 고르는 저장소는 만들 수 없고, 멤버는 삭제할 수 없습니다(409). 그룹을 지우면(쓰는 저장소가 없을 때) 멤버는 따로 쓰는 store로 돌아갑니다.
- **편집**: `PUT /-/api/blobstores/{name}`에 `members`와 `fillPolicy`(비우면 유지)를 보냅니다. 같은 요청에 `quota`도 보낼 수 있고, 멤버를 바꿔도 그룹의 쿼터는 유지됩니다. 멤버는 언제든 추가할 수 있습니다. blob이 든 멤버를 목록에서 빼면 400이며, 아래 작업으로 빼야 합니다. 빈 멤버는 바로 뺄 수 있습니다.
- **멤버 제거 작업**(`blobstore.group-remove-member`, 파라미터 `group`, `member`): 멤버를 읽기 전용으로 바꾸고, 다른 멤버에 없는 blob을 쓰기 가능한 멤버로 복사하며 SHA-256을 확인한 뒤 그룹에서 뺍니다. 그동안 읽기는 계속됩니다. 손상된 사본이 있으면 실패하고 멤버는 읽기 전용으로 남으며, 다시 실행하면 이어서 합니다. 뺀 store는 따로 남고, 쓰는 저장소가 없으므로 다음 compact 작업이 그 사본을 지웁니다. 확인한 뒤 삭제하세요.
- **그룹으로 바꾸기**: `POST /-/api/blobstores/{name}/convert-to-group`에 `{"newMemberName":"<새 이름>"}`을 보내면 기존 store가 그 이름의 멤버가 되고(디렉터리·버킷은 그대로), 원래 이름의 그룹이 생깁니다. 그 store를 쓰던 저장소와 작업은 그대로 그룹을 가리킵니다. default store도 바꿀 수 있습니다.
- **용량·정리**: 그룹 화면의 blob 수·용량은 그룹 저장소의 에셋 기준이고, 파일시스템 용량은 표시하지 않습니다. `blobstore.recalculate-size`는 멤버별 실제 blob 수와 바이트, 그 합인 그룹 용량을 로그에 남깁니다. compact 작업은 그룹을 멤버마다 정리하며, 두 멤버에 같은 blob이 있으면 쓰이지 않을 때 둘 다 지웁니다. 멤버만 따로 compact할 수는 없습니다. 재시작 직후에는 어느 멤버가 blob을 가졌는지 기억하지 못하므로 `roundRobin`에서 같은 파일을 다시 올리면 다른 멤버에 사본이 하나 더 생길 수 있습니다(공간만 더 씁니다).
- **상태 확인**: `GET /-/api/blobstores/{name}/health`가 디렉터리, S3 버킷(주 버킷과 장애 조치 버킷 각각), 그룹은 멤버마다 응답하는지 보여 줍니다. 이 결과는 `GET /-/api/status/checks`에는 들어가지 않습니다.
- Nexus 호환 API: `GET/POST /service/rest/v1/blobstores/group`, `GET/PUT /service/rest/v1/blobstores/group/{name}`(`{"name","members":[...],"fillPolicy"}`).

**S3 장애 조치**: S3 store의 `s3.failover`에 `[{"region":"eu-west-1","bucket":"replica","endpoint":"…"}]`처럼 복제 버킷을 최대 5개 적습니다(복제는 클라우드의 교차 리전 복제가 합니다. region을 비우면 주 버킷의 region이고, prefix·자격 증명·path style은 같습니다). 주 버킷이 응답하지 않거나 5xx로 응답하면 읽기가 장애 조치 버킷을 순서대로 시도합니다. 403·404는 넘기지 않으며, 이미 열린 파일은 처음 읽은 버킷에서 계속 읽습니다. 쓰기, 삭제, 목록, compact는 주 버킷에서만 하므로 주 버킷이 내려가 있으면 업로드와 compact는 실패합니다.

**서버 측 암호화**: `s3.encryption`에 `{"type":"s3ManagedEncryption"}`(SSE-S3, `AES256`) 또는 `{"type":"kmsManagedEncryption","kmsKeyId":"<키 ID·ARN·alias>"}`(SSE-KMS)를 줍니다. KMS 키를 주면 업로드마다 `x-amz-server-side-encryption-aws-kms-key-id` 헤더를 보내고, 비우면 버킷의 AWS 관리 키를 씁니다. 기존 `s3.sse`와 함께 주면 같은 값이어야 합니다.

그룹을 모르는 이전 버전(구조 버전 3 이하)은 그룹을 빈 디렉터리로 열고 멤버의 blob을 지우므로, 이 버전은 DB 구조 버전 4를 기록하고 이전 버전은 시작을 거부합니다("업그레이드" 참고).

### Azure Blob Storage store

**Azure store**(`type: "azure"`)는 blob을 Azure Blob Storage 컨테이너에 `<prefix>/<sha256>` 이름의 블록 blob으로 저장합니다. Blob Store 화면(`POST /-/api/blobstores`)에서 `azure.accountName`(필수, 소문자·숫자 3~24자), `azure.containerName`(필수, **이미 있는 컨테이너**), `azure.prefix`(선택, 앞뒤 `/` 없이), `azure.endpoint`(비우면 `https://<accountName>.blob.core.windows.net`. Azurite는 `http://<호스트>:10000/<계정>`, 소버린 클라우드·프라이빗 엔드포인트는 그 주소)를 줍니다.

- **인증**(`azure.authenticationMethod`): `accountKey`(기본, `accountKey`에 계정 액세스 키, Shared Key 서명), `sasToken`(`sasToken`에 SAS 쿼리 문자열, `r`·`w`·`d`·`l` 권한 필요), `clientSecret`(Microsoft Entra ID 앱의 `tenantId`·`clientId`·`clientSecret`), `managedIdentity`(VM·노드의 관리 ID. 사용자 할당 ID는 `clientId`, 메타데이터 서비스 주소는 `imdsEndpoint`), `workloadIdentity`(Kubernetes 워크로드 ID. AKS 웹후크가 넣는 `AZURE_FEDERATED_TOKEN_FILE`·`AZURE_TENANT_ID`·`AZURE_CLIENT_ID`를 씁니다). 소버린 클라우드의 Entra ID는 `authorityHost`(비우면 `AZURE_AUTHORITY_HOST` 또는 `https://login.microsoftonline.com`)로 지정합니다. Entra ID 방식은 그 ID에 Storage Blob Data Contributor 역할이 있어야 합니다. 토큰은 만료 5분 전에 새로 받고, 받지 못하면 기존 토큰을 만료될 때까지 쓰며 30초마다 다시 시도합니다.
- 만들 때 컨테이너를 목록 조회해 자격 증명을 확인하고(실패하면 400), 같은 endpoint·컨테이너·prefix를 쓰는 다른 Azure store가 있으면 거부합니다. 컨테이너는 만들지 않습니다.
- `accountKey`·`sasToken`·`clientSecret`은 비밀값 키로 암호화해 저장하고 응답과 화면에는 `accountKeySet`·`sasTokenSet`·`clientSecretSet`만 보입니다. 고른 방식이 쓰지 않는 비밀값은 저장하지 않습니다.
- **자격 증명 교체**(SAS 만료, 키 순환): `PUT /-/api/blobstores/<이름>`에 `{"azure": {...}}`를 보냅니다. 계정·컨테이너·prefix·endpoint는 바꿀 수 없습니다(보내지 않거나 같은 값). 방식을 비우면 기존 방식이고, 같은 방식에서 비밀값을 비우면 기존 값을 유지하며, `tenantId`·`clientId`·`authorityHost`·`imdsEndpoint`는 보낸 값으로 바뀝니다. 새 자격 증명으로 목록 조회가 성공해야 저장되고, 모든 노드의 열린 store에 바로 적용됩니다(진행 중인 요청은 이전 자격 증명으로 끝납니다).
- 64 MiB 이하는 한 번에(Put Blob), 넘으면 64 MiB 블록(Put Block)으로 올린 뒤 블록 목록(Put Block List)으로 확정합니다. 블록마다 MD5를 보내 서비스가 확인하며, 실패한 업로드의 블록은 확정되지 않고 일주일 뒤 저절로 사라집니다. 다운로드는 Range 요청을 범위 읽기로 넘깁니다. 응답이 없거나 429·5xx면 0.5·1·2초 간격으로 세 번 다시 보냅니다.
- `path`(비우면 `<dataDir>/blobstores/<이름>`)는 S3 store처럼 업로드 중 임시 파일과 Docker 세션만 둡니다. compact 작업(GC)은 prefix 바로 아래를 나열해 참조되지 않는 blob을 지우며 `<prefix>/meta/` 아래는 건드리지 않습니다. 삭제해도 컨테이너의 blob은 남습니다. 쿼터를 둘 수 있고(`spaceRemainingBytes`는 위반되지 않습니다) 그룹의 멤버가 될 수 있습니다. `GET /-/api/blobstores/<이름>/health`는 컨테이너 목록 조회 결과를 보여 줍니다.
- Nexus 호환 API: `POST /service/rest/v1/blobstores/azure`, `GET`·`PUT /service/rest/v1/blobstores/azure/<이름>`(`{"name","softQuota","bucketConfiguration":{"accountName","containerName","authentication":{"authenticationMethod":"ACCOUNTKEY"|"MANAGEDIDENTITY","accountKey"}}}`). Command Center 확장으로 `bucketConfiguration.prefix`·`endpoint`와 `SASTOKEN`·`CLIENTSECRET`·`WORKLOADIDENTITY`도 받지만 그 방식의 비밀값과 ID는 `/-/api/blobstores`로 설정합니다. GET은 계정 키를 돌려주지 않고, `GET /service/rest/v1/blobstores`에서 형식은 `Azure Cloud Storage`입니다.
- Helm 차트에서 워크로드 ID는 기존 값으로 켭니다: `serviceAccount.annotations`에 `azure.workload.identity/client-id: "<ID의 client id>"`, `podLabels`에 `azure.workload.identity/use: "true"`.
- **구조 버전**: 이 버전은 DB 구조 버전 7을 기록하지만 이것만으로는 DDL이 없어, 첫 Azure store를 만들기 전까지는 이전 버전(6)과 함께 돌 수 있습니다(롤링 업그레이드). 첫 Azure store를 만들면 호환 버전이 7로 올라가 이전 버전은 시작을 거부하고, 실행 중인 이전 버전은 준비 안 됨 상태가 됩니다(이전 버전은 Azure store를 빈 디렉터리로 엽니다). 버전 6 노드가 살아 있으면 Azure store 생성은 거부됩니다. Azure store를 만든 뒤 이전 버전으로 되돌리려면 그 전에 받은 백업이 필요합니다.

## 일반 설정

일반 설정 화면(`GET/PUT /-/api/settings`, 관리자)의 값은 저장 즉시 적용됩니다. 바꿀 항목만 보내면 되고, 보낸 키만 저장합니다.

| 키 | 기본값 | 설명 |
|---|---|---|
| `baseURL` | 가져온 값 또는 `http://localhost:8081` | 클라이언트에게 알려 주는 주소(npm·Cargo 응답의 URL, PyPI 파일 링크의 경로 앞부분, 세션 쿠키의 이름과 `Secure` 여부, 교차 출처 쓰기 검사에서 허용하는 출처). 바로 반영됩니다. 사용자 정보(`user:pass@`), 쿼리, 프래그먼트가 있으면 400입니다. 이전 버전이 저장한 그런 값은 바꿀 때까지 그대로 쓰이고, 서버가 시작할 때마다 WARN 로그(`the stored baseURL is no longer accepted`, 값은 기록하지 않음)가 남습니다. |
| `anonymousRead` | `false` | 인증 없는 읽기(저장소별 값이 우선) |
| `authenticatedRead` | `true` | 로그인한 사용자의 읽기·browse를 권한 없이 허용 |
| `sessionIdleMinutes` | `720` | 세션 유휴 만료(5..10080분). 이미 로그인한 세션에도 적용됩니다. |
| `maxUploadMiB` | `1024` | 업로드 본문 최대 크기(1..1048576). 요청마다 읽으므로 바로 적용됩니다. |
| `defaultLanguage` | `ko` | `ko` 또는 `en`. 브라우저에 저장된 언어 선택이 없을 때 웹 UI가 쓰는 언어이고(언어 버튼으로 고르면 그 선택이 저장되어 우선합니다), 작업 실패 알림 메일의 언어입니다. `GET /-/api/me`가 로그인하지 않은 사용자에게도 알려 줍니다. |
| `metricsAnonymous` | `false` | `GET /-/metrics/prometheus`를 인증 없이 허용("메트릭" 참고) |
| `http.userAgentSuffix` | 빈 값 | 원격 저장소·웹훅·OIDC 요청의 `User-Agent`(`commandcenter/1`) 뒤에 붙이는 문자열(100자 이하의 ASCII). 상위 저장소 운영자가 요청 출처를 구분할 때 씁니다. |
| `http.connectTimeoutSeconds` | `10` | 원격에 연결을 맺을 때까지 기다리는 시간(1..600초) |
| `http.timeoutSeconds` | `30` | 요청을 보낸 뒤 응답 헤더가 올 때까지 기다리는 시간(1..600초). 본문 전송 시간은 포함하지 않습니다. |
| `http.retries` | `0` | 네트워크 오류(연결 끊김·시간 초과)가 났을 때 GET/HEAD 요청을 1초 간격으로 다시 보내는 횟수(0..10). 4xx·5xx 응답은 다시 보내지 않습니다. |
| `http.proxy.host`, `http.proxy.port` | 빈 값 | 모든 외부 HTTP·HTTPS 요청이 거치는 forward proxy. 호스트가 비어 있으면 환경 변수 `HTTP_PROXY`·`HTTPS_PROXY`·`NO_PROXY`를 그대로 씁니다. |
| `http.proxy.username`, `http.proxy.password` | 빈 값 | 프록시 인증(Basic). 비밀번호는 저장만 되고 API가 돌려주지 않으며(`passwordSet`), 바꾸지 않을 때는 비워 보내고 지우려면 `clearPassword: true`를 보냅니다. 비밀값 키가 있으면 암호화해 저장합니다. |
| `defaultRoles` | `[]` | 로그인한 모든 사용자에게 자기 역할에 더해 주는 역할 id 목록(Nexus의 Default Role Realm). 로컬 사용자(비밀번호·세션·API 토큰), LDAP, OIDC, Crowd, RUT, SAML 사용자가 받으며 익명 사용자는 받지 않습니다. 없는 역할, 중복, `admin`은 400입니다. 이 목록에 있는 역할은 지울 수 없습니다(409). 바꾸면 활동 이벤트 `settings.defaultRoles`가 남습니다. |
| `userTokens` | `{"enabled":false,"required":false,"expirationDays":0}` | 사용자 토큰 켜기, 저장소 접근에 토큰 필수, 만료 일수(0은 만료 없음, 3650까지). 한 묶음으로 보냅니다("사용자 토큰" 참고). |
| `terraformDefaultRepository` | 빈 값 | 호스트 루트의 `/.well-known/terraform.json`이 가리킬 terraform 저장소. 비우면 이름순으로 첫 terraform 저장소입니다. terraform 저장소가 아닌 이름은 400입니다("Terraform 설정" 참고). |
| `http.proxy.nonProxyHosts` | `[]` | 프록시를 거치지 않고 직접 연결할 호스트 패턴: `mirror.corp.local`, `*.corp.local`(하위 도메인 전체), `10.*`(주소 앞부분) |

`http` 값은 한 묶음으로 보냅니다(`PUT /-/api/settings`에 `"http": {...}` 전체). 저장 즉시 원격 저장소 가져오기·웹훅·OIDC 연결에 적용되고, 열려 있던 유휴 연결은 끊고 새 설정으로 다시 맺습니다. 활동 이벤트 `settings.http`에는 바뀐 키 이름만 남습니다. 프록시를 거치는 요청도 원격 egress 정책("설정 레퍼런스"의 원격 접속 제한 참고)을 따르되, 주소를 해석하는 쪽이 프록시이므로 IP 리터럴만 검사할 수 있습니다.

## 이메일 알림

SMTP 설정(`GET/PUT /-/api/settings/smtp`, 관리자)으로 예약 작업 실패를 메일로 알립니다.

- **필드**: `enabled`, `host`, `port`, `security`, `username`, `password`, `from`. 켜려면 host, port, 올바른 `from`이 필요합니다. 끄면 일부만 채워도 저장됩니다. host에 공백이나 제어 문자(CR, LF 등)가 있으면 꺼져 있어도 400입니다. PUT은 전체 교체이므로 폼의 모든 값을 보내야 하고, 생략한 `password`만 기존 값을 유지합니다.
- **보안 모드**: `none`(암호화 없음), `starttls`(연결 뒤 업그레이드, 서버가 STARTTLS를 광고해야 함), `tls`(처음부터 TLS, 보통 465). TLS 인증서는 항상 검증하며 건너뛰는 옵션은 없습니다. 인증은 `username`을 채웠을 때만 **PLAIN**(`PlainAuth`)이며 LOGIN, CRAM-MD5 등은 지원하지 않습니다. Go의 PlainAuth는 암호화하지 않은 연결에서 localhost가 아닌 서버로는 비밀번호를 보내지 않으므로, 인증이 필요하면 `starttls` 또는 `tls`를 쓰세요.
- **헤더**: 메일마다 `Date`와 서버가 만든 `Message-ID`(`<임의 문자열@from 주소의 도메인>`)를 붙입니다. `Message-ID`가 없으면 수신 서버가 대신 붙이는데, 그러면 스팸 점수가 오를 수 있습니다.
- **비밀번호**: 저장만 되고(비밀값 키를 정하면 암호화해서, "저장된 비밀값 암호화" 참고) 응답·로그·이벤트에 나오지 않습니다(`passwordSet`만 표시). 비워서 보내면 기존 값을 유지하고 `clearPassword: true`로 지웁니다. **서버(host나 port)를 바꾸거나 `tls`/`starttls`에서 `none`으로 낮출 때는 비밀번호를 다시 입력해야 합니다**(400). 저장된 비밀번호가 다른 서버나 암호화되지 않은 연결로 새어 나가지 않게 하는 규칙입니다.
- **테스트 메일**: `POST /-/api/settings/smtp/test`(`{to}`)는 저장된 설정으로 한 통을 바로 보내고 성공하면 200입니다. 이메일이 꺼져 있으면 400, **10초 안에 다시 호출하면 429**, 발송 실패는 502입니다. 10초 창은 이메일이 켜져 있는지 확인한 뒤 **보내기 전에** 잡으므로 실패한 발송도 한 번으로 셉니다.
- **작업 실패 알림**: 작업의 `notifyEmail`(최대 20개 주소)에 수신자를 넣으면, 그 작업의 실행이 **Failed**로 끝날 때만(성공, 중지, 서버 종료 중 실패는 제외) 이메일이 켜져 있을 때 메일을 보냅니다. 제목에 작업 이름이 들어가고 본문에는 시각, 결과 메시지, **마지막 로그 20줄**이 담깁니다. 언어는 `defaultLanguage`를 따릅니다. 메일은 백그라운드로 보내므로 작업을 지연시키지 않고, 큐(256건)가 차면 버립니다. 발송 실패는 서버 로그(WARN)에 남고 활동 기록에 `email.failed` 이벤트(제목과 오류 메시지)로도 기록됩니다.

## Webhook

Command Center는 Nexus Repository의 webhook처럼 서버에서 일어난 일을 HTTP POST로 외부 시스템에 알립니다. 관리자 화면의 Webhook 페이지(또는 `/-/api/webhooks` API)에서 등록하며, 전역 webhook은 모든 저장소의 이벤트를, 저장소 webhook은 그 저장소의 이벤트만 받습니다. 구독할 이벤트 종류는 `GET /-/api/webhooks/events`가 설명과 함께 나열하며 `*`는 전부를 뜻합니다. 주요 종류는 `component.created/deleted`, `asset.created/deleted`, `repository.created/updated/deleted`, `user.*`, `role.*`, `token.*`, `realm.updated`, `task.finished`, `cleanup.finished`, `webhook.*`, `blobstore.updated`, `staging.move`, `staging.delete`, `firewall.blocked`, `firewall.released`, `vulnerability.updated`입니다. 활동(activity) 목록에 기록되는 모든 이벤트가 webhook으로도 나가며, 이 버전이 모르는 새 종류는 이름 그대로 `*` 구독자에게 전달됩니다.

요청은 `Content-Type: application/json`이고 `X-Command-Center-Event`(이벤트 종류), `X-Command-Center-Delivery`(전달 ID) 헤더가 붙습니다. 비밀값(secret)을 설정하면 본문 전체의 HMAC-SHA256을 `X-Command-Center-Signature: sha256=<hex>`로, Nexus 호환용으로 HMAC-SHA1을 `X-Nexus-Webhook-Signature`로 함께 보냅니다(`X-Nexus-Webhook-Id`, `X-Nexus-Webhook-Delivery` 헤더도 붙지만 본문은 Command Center 고유 형식이므로 Nexus용 수신기는 본문 처리를 맞춰야 합니다). 본문은 다음과 같습니다.

```json
{"id":"<uuid>","timestamp":"2026-10-29T09:00:00Z","event":"component.created","repository":"npm-hosted","actor":"alice",
 "data":{"format":"npm","label":"@scope/util@2.0.0","group":"@scope","name":"util","version":"2.0.0"}}
```

`data`는 종류별로 다릅니다: 컴포넌트는 `format, group, name, version`, 파일(asset)은 `path, size, sha256`, 저장소는 `name, format, type`, 작업은 `name, type, result`(OK 또는 Failed)입니다. `repository`는 저장소와 무관한 이벤트에서는 `null`입니다.

전달은 백그라운드에서 4개의 작업자가 요청당 10초 제한으로 보내며, 2xx 응답이면 성공입니다. 그 외 응답이나 연결 실패는 30초, 2분, 10분, 1시간 뒤에 다시 시도하고(최대 5회) 리다이렉트는 따라가지 않습니다. 대기 중인 전달은 데이터베이스에 저장되므로 서버를 재시작해도 이어서 보냅니다. 비활성화한 webhook에는 보내지 않으며(대기 중인 전달은 다시 활성화할 때까지 멈춥니다), webhook을 지우면 그 전달 기록도 지워집니다. webhook마다 최근 200건의 전달 기록(상태, 시도 횟수, 응답 코드, 오류)을 보관하며 화면이나 `GET /-/api/webhooks/{id}/deliveries`에서 확인하고 `redeliver`로 다시 보낼 수 있습니다. "테스트" 버튼(`POST /-/api/webhooks/{id}/test`)은 `ping` 이벤트를 즉시 보내고 응답 코드를 알려 줍니다. 대상 URL은 프록시 원격과 같은 송신 정책을 따릅니다: 사설 주소도 허용되지만 `10.1`처럼 숫자로만 적은 IPv4 주소나 ASCII가 아닌 호스트 이름은 거부됩니다. 비밀값은 다른 비밀값과 같이 비밀값 키로 암호화해 저장하고 API로 돌려주지 않으며(`secretSet`만 알려 줍니다), 로그에도 본문과 비밀값은 남지 않습니다. 전달은 `serve`로 띄운 서버만 보냅니다. CLI 명령(`gc`, `token create` 등)에서 생긴 이벤트의 전달은 DB에 저장만 하고, 실행 중인 서버가 보냅니다. 실행 중인 서버가 없으면 다음에 시작하는 서버가 보냅니다. 각 전달은 그것을 가져간(claim) 노드 하나만 보냅니다. 보내는 도중 서버가 죽으면 다시 시작할 때 다시 보냅니다. 다른 노드가 있으면 60초 뒤 그 노드가 보냅니다. 따라서 같은 전달이 두 번 갈 수 있으므로(최소 한 번 전달), 수신 쪽은 `X-Command-Center-Delivery`(또는 `X-Nexus-Webhook-Delivery`)로 중복을 거르세요.

등록 예:

```bash
curl -u admin -H 'X-Command-Center-UI: 1' -H 'Content-Type: application/json' \
  -d '{"name":"ci","url":"https://ci.example/hook","secret":"s3cret","events":["component.created","component.deleted"],"repo":"npm-hosted"}' \
  https://repo.example/-/api/webhooks
```

수신 쪽 서명 검증(Python):

```python
import hmac, hashlib
expected = "sha256=" + hmac.new(secret.encode(), body, hashlib.sha256).hexdigest()
assert hmac.compare_digest(expected, request.headers["X-Command-Center-Signature"])
```

## 취약점 데이터와 방화벽(OSV)

Command Center는 OSV(https://osv.dev)의 공개 데이터로 컴포넌트의 알려진 취약점과 악성 패키지를 보고하고, 프록시 저장소에 방화벽(격리)을 적용합니다. Nexus Pro의 Repository Health Check·Firewall에 해당하는 기능이며 별도 라이선스가 필요 없습니다.

대상 포맷과 OSV 생태계는 npm(npm), PyPI(PyPI), Maven(Maven, 이름은 groupId:artifactId), Go(Go), Cargo(crates.io), NuGet(NuGet), RubyGems(RubyGems), Composer(Packagist), Conan(ConanCenter), R(CRAN)입니다. Docker, conda, apt, yum, Helm, raw, Git LFS는 OSV 데이터가 없거나 배포판 정보가 필요해 대상이 아닙니다.

### 취약점 데이터 설정

관리자 설정(`GET/PUT /-/api/vulnerability/settings`, PUT은 보낸 값만 바꿉니다)에서 켭니다. 기본값은 꺼짐입니다. 상태는 `GET /-/api/vulnerability/status`(권고 수, 생태계별 개수, 마지막 가져오기, 조회 실패 수)로 봅니다.

- `mode: online`(기본): 로컬 DB에 없는 정보를 OSV API(`apiURL`, 기본 https://api.osv.dev)에 묻고 답을 `cacheHours`(기본 24시간) 동안 재사용합니다. 요청은 일반 설정의 HTTP 프록시와 SSL 신뢰 저장소를 따릅니다.
- `mode: offline`: 로컬 DB만 씁니다. `vulnerability.sync` 작업이 `<dumpURL>/<생태계>/all.zip`을 내려받거나(대상은 `ecosystems`, 비우면 전체), 폐쇄망에서는 관리자가 all.zip을 `POST /-/api/vulnerability/import`(본문이 zip 파일)로 올립니다. 업로드는 `maxImportMiB`(기본 1024)와 일반 설정의 `maxUploadMiB` 중 작은 값으로 제한되며, 경로가 `..`나 절대 경로인 항목이 있는 압축 파일은 거부됩니다. 업로드는 HTTP 요청 안에서 처리되므로 큰 덤프는 리버스 프록시의 시간 제한에 걸릴 수 있습니다. 그럴 때는 sync 작업을 쓰세요.
- 설정 변경과 가져오기는 활동 이벤트 `vulnerability.settings`, `vulnerability.import`(webhook `vulnerability.updated`)로 남습니다.

### 방화벽(격리)

프록시 저장소 설정의 `firewall`을 켜면 그 저장소가 내주는 파일을 컴포넌트 단위로 검사합니다("저장소 키" 참고).

- `blockMalicious`(기본 true): OpenSSF malicious-packages(MAL-로 시작하는 ID)에 오른 패키지를 막습니다.
- `blockSeverityAtLeast`: `none`(기본, 심각도로는 막지 않음), `low`, `medium`, `high`, `critical`. 심각도는 CVSS 3.x 기본 점수(직접 계산)나 권고가 주는 점수·등급입니다(CVSS 4는 권고가 준 점수나 등급만 씁니다). 등급이 없는 권고는 기준에 걸리지 않습니다.
- `allowList`: `name@version`에 맞추는 RE2 정규식 목록(Maven은 `groupId:artifactId@version`, npm 스코프는 `@scope/name@version`)입니다.

막힌 파일은 403과 함께 권고 ID를 본문(일반 텍스트)에 담아 응답합니다. 이미 캐시된 파일도 나중에 취약점이 알려지면 다음 다운로드부터 막히며, group을 거쳐도 그 proxy 멤버의 정책이 적용됩니다. 파일은 격리 상태로 캐시에 남으며, 격리 목록(`GET /-/api/firewall/quarantine`)에서 해제(`POST /-/api/firewall/quarantine/release`, 관리자)하면 allowList에 `^name@version$` 항목이 추가되고 다시 제공됩니다. 처음 격리될 때마다 `firewall.blocked` 이벤트(webhook)가 기록됩니다. hosted 저장소는 막지 않고 보고만 합니다.

온라인 조회가 실패하면(시간 초과 15초 포함) 파일을 내주고 결과를 `unknown`으로 표시합니다(fail open). 이때도 로컬 DB(내려받거나 올린 데이터, 이전 조회에서 저장된 권고)는 그대로 적용되므로, 로컬에 있는 악성 패키지는 API가 응답하지 않아도 막힙니다. 방화벽은 취약점 데이터가 켜져 있을 때만 동작합니다.

### 취약점 보고

`vulnerability.scan` 작업이 hosted·프록시 저장소의 컴포넌트를 검사해 결과를 저장합니다. 저장소 목록에는 심각도별 개수가, 저장소 요약(`GET /-/api/repositories/{name}/vulnerabilities/summary`)에는 상위 컴포넌트가, 컴포넌트 화면(`GET /-/api/repositories/{name}/components/vulnerabilities?group=&name=&version=`)에는 권고 목록(수정 버전, osv.dev 링크 포함)이 표시됩니다. group 저장소는 자기 컴포넌트가 없으므로 요약이 비어 있습니다.

## 저장소 Online/Offline

저장소 편집 화면(또는 API의 `online`)에서 저장소를 **Offline**으로 바꿀 수 있습니다(기본은 Online).

- Offline 저장소의 `/repository/<이름>/…` 요청은 **503**(`repository is offline`)입니다. 권한 검사를 먼저 하므로 읽을 수 없는 사용자는 Offline인지 알 수 없고 401/403을 받습니다.
- **Maven·Go·Cargo group은 Offline 멤버를 건너뜁니다. npm·PyPI group은 건너뛰지 않고 실패합니다**: 요청한 패키지를 가진 멤버보다 앞에 Offline 멤버가 있으면(어느 멤버도 그 패키지를 갖지 않을 때도) 503 `group member "<이름>" is offline`으로 답합니다. 점검하려고 hosted를 잠시 Offline으로 돌렸을 때 proxy에 있는 같은 이름의 공개 패키지가 대신 설치되지 않게 하기 위해서입니다. 패키지를 가진 멤버보다 뒤에 있는 멤버는 조회하지 않으므로 Offline이어도 영향이 없습니다. `writeMember`가 Offline이면 그 group에 쓰는 요청은 (쓰기 권한이 있는 사용자에게) 503 `repository is offline`이고, UI 저장소 목록의 `canWrite`도 `false`입니다. group 자체를 Offline으로 두면 그 group이 503입니다.
- group의 멤버가 **모두** Offline이면(멤버인 group의 멤버가 모두 Offline인 경우 포함) 그 group을 읽는 요청도 503 `repository is offline`입니다. 404를 돌려주면 Maven 같은 클라이언트가 not-found를 캐시해 멤버를 Online으로 되돌린 뒤에도 바로 받지 못하기 때문입니다. 쓰기는 위의 `writeMember` 규칙을 그대로 따르고, npm `login`처럼 권한이 필요 없는 경로는 영향을 받지 않습니다(읽기 권한이 필요한 `npm ping`도 503입니다). 멤버가 하나도 없는 group은 Offline으로 보지 않습니다.
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
  <profiles>
    <profile>
      <id>commandcenter</id>
      <repositories>
        <repository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases>
          <snapshots><enabled>true</enabled></snapshots></repository>
      </repositories>
      <pluginRepositories>
        <pluginRepository><id>central</id><url>http://central</url>
          <releases><enabled>true</enabled></releases>
          <snapshots><enabled>true</enabled></snapshots></pluginRepository>
      </pluginRepositories>
    </profile>
  </profiles>
  <activeProfiles>
    <activeProfile>commandcenter</activeProfile>
  </activeProfiles>
</settings>
```

`<mirror>`만 있으면 Maven 기본 저장소 `central`의 SNAPSHOT 꺼짐 정책이 그대로 적용되어 SNAPSHOT 의존성을 받지 못합니다(BUILD FAILURE). profile이 `central`을 SNAPSHOT을 켠 채로 다시 선언하고, 그 요청은 mirror를 거쳐 이 서버로 옵니다(`http://central`에는 실제로 접속하지 않습니다). 웹 UI의 proxy·group 저장소 Repository setup도 같은 설정을 보여 줍니다.

배포 대상은 `pom.xml`의 `distributionManagement`에 `http://localhost:8081/repository/maven-releases/`(id `commandcenter`)로 지정합니다.
다른 컨테이너에서 접근할 때는 `localhost` 대신 `host.docker.internal`을 사용하세요.

Maven 동작 메모:

- SNAPSHOT 버전 디렉터리의 `maven-metadata.xml`은 서버가 만듭니다. 그 디렉터리의 파일을 올리거나 지울 때마다, 그리고 `repository.rebuild-metadata` 작업이 돌 때 가장 높은 timestamp 빌드를 기준으로 다시 만듭니다. 클라이언트(`mvn deploy`)가 올린 같은 문서는 201로 답하되 무시합니다.
- GA 디렉터리(`groupId/artifactId/`)의 `maven-metadata.xml`도 서버가 저장된 아티팩트로 만듭니다. 버전 목록, `latest`, `release`, `groupId`, `artifactId`는 아티팩트를 올리거나 지울 때마다 다시 계산합니다. 클라이언트(`mvn deploy`)가 올린 문서는 201로 답하되 플러그인 그룹 문서의 `<plugins>`만 저장된 문서에 합치고, 나머지(버전, `<latest>`, `<release>`, 경로와 다른 groupId/artifactId)는 무시합니다. 그래서 배포 권한만으로는 아티팩트가 없는 버전을 끼워 넣어 `[1.0,)` 같은 버전 범위 해석을 바꿀 수 없습니다. 다만 groupId/artifactId가 경로의 GA를 가리키는 문서(`mvn deploy`가 아티팩트를 다 올린 뒤 올리는 문서)를 받으면 그 GA의 문서를 저장된 아티팩트로 다시 만듭니다. 그래서 아티팩트 업로드 뒤의 갱신이 실패했더라도(아티팩트 업로드는 201, 서버 로그에 `maven-metadata.xml rebuild failed`) 배포의 마지막 단계에서 바로잡힙니다. 이때 다시 만들기가 실패하면 그 메타데이터 업로드가 5xx로 실패합니다. GA 디렉터리가 플러그인 그룹의 groupId 디렉터리이기도 하면(groupId `org.example`의 플러그인 목록과 GA `org:example`이 함께 쓰는 `org/example/maven-metadata.xml`) 아티팩트나 컴포넌트를 지울 때(Cleanup 포함)와 `repository.rebuild-metadata` 작업이 문서를 다시 만들 때도 `<plugins>`는 그대로 남고, 그 GA의 마지막 버전을 지우면 `<plugins>`만 남습니다. 이전 버전에서 클라이언트 문서로 들어간 버전은 그 GA에 아티팩트나 GA 문서를 하나 더 올리거나(다음 `mvn deploy`) `repository.rebuild-metadata` 작업을 돌리면 사라집니다(업그레이드 후 한 번 돌리기를 권장합니다). 아티팩트가 하나도 없는 GA의 문서는 이 작업이 버전을 지웁니다(`<plugins>`가 있으면 그것만 남기고, 없으면 문서를 지웁니다). 문서에 적힌 `groupId`/`artifactId`가 없거나 다른 GA를 가리켜도, 문서가 다른 GA의 디렉터리 안(GA `org:example` 아래의 `org/example/app/`)에 있어도 마찬가지입니다.
- 올리는 `maven-metadata.xml`은 1 MiB까지 받습니다(넘으면 413). 업로드한 메타데이터는 동시에 2개까지만 처리하며, 슬롯이 모두 사용 중이면 30초까지 기다리다가 `503 too many concurrent maven-metadata.xml uploads, retry`로 거절하므로 잠시 후 다시 시도하세요. 슬롯을 잡은 업로드는 본문을 10초 안에 다 보내야 하며(계속 조금씩 보내도 마찬가지입니다), 넘기면 `408 maven-metadata.xml not received within 10s`로 답합니다. 그래서 느린 클라이언트 둘이 슬롯을 모두 잡아도 다른 업로드는 10초 남짓만 기다립니다. 본문은 슬롯을 잡은 뒤에 읽으므로, 슬롯을 기다리는 업로드는 본문을 메모리에 올리지 않습니다.
- 올리는 `.pom`(classifier가 없는 것)은 저장하기 전에 내용을 검사합니다. 1 MiB까지 받고(넘으면 413), 올바른 XML이면서 루트가 `<project>` 하나여야 하며, `groupId`(없으면 `<parent>`의 값), `artifactId`, `version`(없으면 `<parent>`의 값)이 업로드 경로의 좌표와 같아야 합니다. 그렇지 않으면 400입니다(예: `400 .pom declares "evil:other:9", want "org.example:pv:1.1"`). 거부된 업로드는 아무것도 남기지 않으므로 `allow_once` 저장소에서도 고쳐서 다시 올릴 수 있습니다. SNAPSHOT의 timestamp 파일은 디렉터리의 버전(`1.0-SNAPSHOT`)과 비교합니다. `groupId`와 `artifactId` 안의 `${이름}`은 그 POM의 `<properties>`로 바꾼 뒤 비교합니다. 거기에 없는 속성(부모 POM이 정하는 `${scala.binary.version}` 등)은 `/`와 `:`가 아닌 한 글자 이상과 맞는 것으로 보고, 나머지 부분은 그대로 비교합니다. 그래서 `<artifactId>hudi-spark_${scala.binary.version}</artifactId>`인 POM은 `hudi-spark_2.12` 경로에는 올라가지만 `other_2.12` 경로에는 400입니다. `version`에 `${…}`가 들어 있으면 비교하지 않습니다. 명령줄(`-Drevision=…`)이나 부모 POM이 정하는 CI-friendly 버전(`${revision}`)은 서버가 알 수 없기 때문이며, 그래서 flatten-maven-plugin 없이 `mvn -Drevision=1.2.3 deploy`로 배포해도 받습니다. `encoding="ISO-8859-1"`, `EUC-KR`처럼 다른 인코딩을 선언한 POM도 받습니다. 선언이 없거나 UTF-8로 선언한 POM에 UTF-8이 아닌 바이트가 있으면(예: MS949로 저장한 `<description>`) Maven처럼 그 바이트를 U+FFFD로 바꿔 읽으므로 받으며, 좌표는 그대로 비교합니다. classifier가 붙은 `.pom`과 이미 저장된 POM은 검사하지 않습니다.
- sbt 플러그인은 hosted 저장소에 `sbt publish`로 배포할 수 있습니다. sbt 1.9 이상은 기본 설정(`sbtPluginPublishLegacyMavenStyle := true`)에서 한 버전 디렉터리에 두 방식으로 올립니다. Maven 방식 파일은 이름에 디렉터리와 같은 `_<Scala 버전>_<sbt 버전>`이 붙고(`com/example/sbt-foo_2.12_1.0/1.0.0/sbt-foo_2.12_1.0-1.0.0.jar`), 옛 방식(legacy layout) 파일은 붙지 않습니다(`com/example/sbt-foo_2.12_1.0/1.0.0/sbt-foo-1.0.0.jar`). sbt 1.9 미만은 옛 방식으로만 올립니다. 서버는 두 방식을 모두 저장하고 hosted와 group에서 내줍니다. 옛 방식 파일은 디렉터리 이름이 `_<숫자>_<숫자>`(예: `_2.12_1.0`)로 끝나고 파일 이름이 그 접미사를 뺀 이름, `-`, 버전, classifier(있으면), 확장자로 되어 있을 때만 받고, 그 밖의 이름은 400입니다. 옛 방식 `.pom`은 접미사를 뺀 artifactId(`sbt-foo`)와 비교해 검사합니다(예: `400 .pom declares "com.example:sbt-foo_2.12_1.0:1.0.0", want "com.example:sbt-foo:1.0.0"`). 옛 방식 파일은 GA의 버전으로 치지 않으므로 `maven-metadata.xml`의 버전 목록에는 Maven 방식 파일이 있는 버전만 들어가고, sbt 1.9 미만으로 배포한 버전은 목록에 없습니다(버전을 정확히 적으면 받을 수 있습니다). 컴포넌트 검색, Cleanup, 컴포넌트 삭제(웹 UI의 "Delete component"와 API)에서는 두 방식의 파일(proxy에 캐시된 옛 방식 파일 포함)이 한 컴포넌트(`com.example:sbt-foo_2.12_1.0:1.0.0`)이므로 컴포넌트를 지우면 둘 다 지웁니다. Maven 방식 파일만 지우면 sbt가 옛 방식 파일로 그 버전을 계속 받기 때문입니다.
- proxy와 group은 Maven 파일만 조회하고, proxy는 그것만 원격에서 받아 캐시합니다: 아티팩트(서명 `.asc` 포함), `maven-metadata.xml`, `.meta/prefixes.txt`, `archetype-catalog.xml`, sbt의 옛 방식(legacy layout)으로 된 sbt 플러그인 파일과 그 체크섬입니다. 옛 방식 파일은 디렉터리 이름에만 `_<Scala 버전>_<sbt 버전>`이 붙고 파일 이름에는 붙지 않습니다(예: `com/eed3si9n/sbt-assembly_2.12_1.0/2.1.1/sbt-assembly-2.1.1.jar`). sbt와 coursier가 이 경로로 요청하므로 proxy와 group에서 받을 수 있고, hosted 저장소에도 올릴 수 있습니다(위 항목 참고). 디렉터리(`junit/junit/4.13.2`, `junit/junit/`)처럼 그 밖의 경로는 원격에 묻지 않고 404로 답하므로, proxy 주소를 브라우저로 열어 원격의 디렉터리 목록을 볼 수는 없습니다(캐시된 파일은 웹 UI의 Browse에서 봅니다). IDE가 받는 원격 인덱스(`.index/`)도 받지 않습니다. 이름이 `.html`로 끝나지 않는 파일에 원격이 `text/html`로 답하면(디렉터리 목록이나 오류 페이지) 저장하지 않습니다. 캐시에 없던 파일이면 404로 처리하며 negative cache에 기록합니다. 이때 서버 로그에 `remote answered with a web page, taken as not found`가 남으니, 원격 서버가 아티팩트를 `text/html`로 잘못 내주는지 이 줄로 확인하세요. 이미 캐시된 파일(만료된 `maven-metadata.xml` 등)을 다시 확인할 때 이런 답이 오면 원격 장애(점검 페이지나 로그인 페이지)로 보고, 원격에 접속할 수 없을 때처럼 캐시된 사본을 그대로 줍니다. 이때는 서버 로그에 `remote fetch failed`와 `remote answered with a web page`가 남습니다. 이전 버전이 캐시한 디렉터리 목록과 IDE 인덱스(`.index/`)는 이제 제공하지 않지만(404) Browse에는 남아 있습니다. 해는 없으며, 지우려면 그 proxy를 대상으로 `repository.rebuild-metadata` 작업을 한 번 실행하세요(업그레이드 후 proxy마다 한 번 돌리기를 권장합니다. "예약 작업" 참고). 이 작업은 proxy가 더는 받지 않는 경로(Maven 파일이 아닌 경로)의 캐시만 지우고, Maven 파일 경로의 캐시는 `text/html`이어도 남깁니다. 원격이 형식을 잘못 표시한 진짜 아티팩트일 수 있기 때문입니다. 작업 로그에 지운 경로가 처음 20개까지 남고, 지운 캐시의 blob은 다음 `blobstore.compact` 작업이 회수합니다.
- group은 `.meta/prefixes.txt`에 404로 답합니다(멤버의 접두사 목록을 그대로 내주면 다른 멤버의 아티팩트가 없다고 오해하기 때문입니다). 병합한 메타데이터의 `Last-Modified`는 멤버가 오프라인이 되어 내용이 바뀌어도 뒤로 가지 않고 앞으로만 움직입니다.
- 업로드 경로의 이름에 `?`, `#`, `;`, 제어 문자(NUL 포함)나 UTF-8이 아닌 바이트(직접 만든 요청의 `%E9` 같은)가 있으면 400으로 거부합니다(조회는 막지 않으므로 이미 있는 파일은 계속 받을 수 있습니다). release 정책 저장소는 timestamp가 붙은 SNAPSHOT 버전(`1.4-20260101.000000-1`)도 400으로 거부합니다.

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
`npm install`, `npm dist-tag`, `npm deprecate`, `npm unpublish`, `npm whoami`, `npm logout`(그 토큰을 폐기)을 지원하며, `npm search`(404)는 지원하지 않습니다(`npm audit`은 아래 참고).
`npm ping`과 `npm audit`도 저장소 읽기 권한이 필요합니다(익명 읽기가 꺼져 있으면 토큰 필요). `npm login`은 토큰 없이 됩니다. 헬스 체크에는 `npm ping` 대신 인증 없이 200을 주는 `GET /service/rest/v1/status`를 쓰세요.
새 버전은 SemVer 2.0 형식(`1.2.3`, `1.2.3-beta.1`)이어야 하고 빌드 메타데이터(`3.0.0+a`처럼 `+…`)는 붙일 수 없습니다(400). npm은 `+…`를 무시하므로 `3.0.0`과 같은 버전이 둘이 되어 `npm install X@3.0.0`이 실패하기 때문입니다. dist-tag 이름은 버전이나 범위처럼 보이면(`1.0.0`, `^1`, `1.x`) 거부됩니다.
publish는 저장한 tarball도 검사합니다. gzip으로 압축한 tar여야 하고, 맨 위 디렉터리 바로 아래에 일반 파일 `package.json`이 하나만 있어야 하며, 그 `name`과 `version`이 publish 문서와 같아야 합니다. npm은 압축을 풀 때 경로의 첫 요소를 떼므로 맨 위 디렉터리 이름은 `npm pack`이 쓰는 `package/`가 아니어도 됩니다. `version`은 npm이 publish할 때처럼 앞의 `v`·`=`와 빌드 메타데이터(`+…`)를 뗀 값으로도 비교하므로, `package.json`에 `1.2.3+ci.5`처럼 적은 패키지도 `npm publish`로 올라갑니다. publish 문서의 새 버전마다 `versions.<버전>.name`도 패키지 이름과 같아야 합니다. 빈 파일, gzip이나 tar가 아닌 파일, `package.json`이 없거나 둘 이상인 tarball, `package.json`이 1 MiB를 넘는 tarball은 400입니다. 압축을 푼 크기는 publish 문서 하나에 든 새 tarball을 모두 합쳐 1 GiB까지만 읽고, 넘으면 400(`more than 1073741824 bytes uncompressed in this publish`)입니다. `npm publish`는 문서 하나에 버전 하나를 보내므로 tarball 하나가 1 GiB까지입니다. 버전 형식, `versions.<버전>.name`, shasum·integrity는 tarball을 읽기 전에 검사합니다. 처음 publish하는 패키지 이름은 npm이 새 패키지에 요구하는 대로 소문자여야 하고 `node_modules`, `favicon.ico`는 쓸 수 없습니다(400). 점(`.`)이나 밑줄(`_`)로 시작하는 이름, 214자를 넘는 이름, URL에 그대로 쓸 수 없는 문자가 든 이름은 이전부터 400입니다. 다만 npm처럼 scope 이름은 scope나 패키지 부분이 밑줄로 시작해도 되므로, `@acme/_internal` 같은 패키지도 publish할 수 있고 proxy와 group으로 받을 수 있습니다. scope나 패키지 부분이 점으로 시작하는 이름(`@acme/.x`)은 계속 400입니다. 이미 저장된 패키지는 대문자 이름이어도(예전 빌드에서 올린 경우) 새 버전을 계속 publish할 수 있고, `JSONStream`처럼 예전 규칙으로 만든 공개 패키지도 proxy와 group으로 그대로 받을 수 있습니다.
그룹은 멤버 순서상 그 패키지를 처음 가진 멤버 하나만으로 답합니다(hosted를 proxy보다 앞에 두세요). packument, 버전 문서, dist-tag, tarball이 모두 그 멤버의 것이고 뒤 멤버의 버전·dist-tag는 섞이지 않으며, 뒤 멤버에만 있는 버전의 tarball URL을 직접 요청해도 404입니다. 예를 들어 hosted에 `foo@1.0.0`이 있으면 proxy에 `foo@9.9.9`와 `beta` 태그가 있어도 그룹은 `1.0.0`만 보여 주고 `npm install foo@beta`는 실패합니다. hosted에 없는 이름은 proxy에서 받습니다.
그 패키지를 처음 가진 멤버보다 앞선 멤버가 "없음"(404)이 아닌 오류를 내면 그룹은 5xx(502 또는 그 멤버의 5xx)로, Offline이면 503으로 답합니다. hosted의 일시 오류나 점검 때문에 proxy에 있는 같은 이름의 공개 패키지가 대신 나가지 않게 하기 위해서입니다. 패키지를 가진 멤버보다 뒤에 있는 멤버는 조회하지 않으므로, hosted 뒤의 proxy가 장애이거나 Offline이어도 hosted 패키지는 계속 받을 수 있습니다(반대로 proxy를 hosted보다 앞에 두면 proxy 장애 때 그룹이 5xx를 냅니다).
tarball 요청은 packument를 읽지 않아도 됩니다. 그룹은 멤버 순서대로 그 tarball을 찾아 처음 가진 멤버로 답하고(hosted는 DB, proxy는 캐시에 있으면 캐시, 없으면 원격에서 그 tarball만 받음), tarball이 없는 멤버에게만 그 패키지를 가졌는지 packument로 묻습니다. 가졌다면 그 멤버가 주인이므로 404이고 뒤 멤버의 tarball은 나가지 않습니다. 마지막 멤버에게는 묻지 않습니다. 그래서 `npm ci`처럼 lockfile로 tarball만 받는 설치에는 packument가 필요 없고, proxy에 캐시된 tarball은 원격 레지스트리가 장애이거나 원격에서 패키지가 삭제된 뒤에도 proxy URL로 받을 때처럼 그룹으로 받을 수 있습니다. 마지막이 아닌 proxy 멤버에 그 tarball이 없으면 그 멤버의 packument로 판단합니다. 캐시가 만료되었는데 원격이 장애이면 만료된 캐시를 쓰고(주인이면 404), 원격이 404·410이면 그 멤버에는 패키지가 없는 것으로 보고 다음 멤버로 넘어갑니다(그룹 packument와 같은 판단). tarball이 캐시에 없고 원격에도 닿지 않으면 packument 캐시가 있어도 그룹은 502로 실패합니다.
`npm unpublish <패키지>@<버전>`은 그 버전을 지우고, `npm unpublish <패키지> --force`는 모든 버전과 dist-tag를 지워 패키지가 404가 됩니다. 저장소(그룹이면 `writeMember`)에 `delete` 권한이 있어야 합니다(없으면 403). proxy나 `writeMember`가 없는 group에서는 `405`(빈 `Allow` 헤더, 메시지 `unpublish is not supported in <유형> repository "<이름>": unpublish from its hosted repository`)로 실패하므로, npm이 "삭제했다"고 잘못 보고하지 않습니다. 지운 버전을 가리키던 dist-tag는 지워지고, `latest`가 지운 버전을 가리켰다면 남은 버전 중 가장 높은 정식 버전(정식 버전이 없으면 가장 높은 프리릴리스)으로 옮겨집니다. npm이 보낸 dist-tag 값은 쓰지 않습니다. npm은 읽어 간 패키지 문서에서 그 버전을 뺀 문서를 보내는데, 그사이 다른 사람이 새 버전을 올렸다면 그 버전까지 지우지 않도록 `409`로 거부하니 다시 실행하세요. npm은 읽어 간 문서의 `_rev`도 요청 주소(`/-rev/<rev>`)에 넣어 보냅니다. hosted 패키지 문서의 `_rev`는 그 문서에서 가장 나중에 게시된 버전의 게시 시각(밀리초)입니다. 요청이 지울 버전 중 이보다 나중에 게시된 버전, 즉 npm이 읽은 뒤 게시되었거나 지워졌다가 다시 게시된 버전이 있으면 아무것도 지우지 않고 `409`로 거부합니다. 문서를 보내지 않는 `npm unpublish <패키지> --force`(버전이 하나뿐인 패키지의 `npm unpublish <패키지>@<버전>`도 같습니다)도 이 규칙을 따릅니다. 그사이 다른 사람이 다른 버전을 지웠거나 dist-tag를 바꿨거나 `npm deprecate`를 한 것은 거부 사유가 아니므로, 서로 다른 버전을 지우는 `npm unpublish`를 동시에 여러 개 실행해도 모두 성공합니다. 이전 버전이 저장한 문서에는 `_rev`가 없어 npm이 `undefined`를 보내며, 이처럼 숫자가 아닌 `_rev`는 비교하지 않습니다. 그 패키지가 한 번 바뀌면(publish, dist-tag 변경, deprecate, 버전 삭제) `_rev`가 생깁니다. 이 문서는 10 MiB까지 받으며(넘으면 413), 그보다 큰 패키지의 버전은 웹 UI나 API로 지우세요("컴포넌트 삭제" 참고). hosted 패키지 문서는 `Cache-Control: no-cache`로 보내므로 npm이 캐시에 남은 옛 문서로 지운 버전을 고르지 않습니다. 지우지 않고 쓰지 말라고만 알리려면 `npm deprecate`를 쓰세요(설치할 때 경고가 나옵니다).

**`npm audit`**: proxy 저장소는 감사 요청(`POST …/-/npm/v1/security/audits`, `…/audits/quick`, `…/advisories/bulk`)을 원격 레지스트리에 그대로 전달하고 원격의 상태 코드와 본문을 그대로 돌려줍니다. 그룹은 멤버 순서상 첫 proxy 멤버(그룹 안의 그룹 포함)의 원격으로 보냅니다. 결과는 캐시하지 않고, 요청과 응답 본문은 각각 16 MiB까지입니다(요청이 넘으면 413, 응답이 넘으면 502). hosted 저장소와 proxy 멤버가 없는 그룹은 취약점 0건인 빈 보고서(`advisories/bulk`는 `{}`)를 돌려주므로 `npm install`의 감사 단계가 실패하지 않습니다. 이것은 검사 결과가 아니라 검사하지 않았다는 뜻입니다. 저장소 읽기 권한이 필요하며, 원격이 차단(`blocked` 또는 자동 차단)되어 있으면 502입니다.

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

그룹은 멤버 순서상 그 프로젝트의 파일을 처음 가진 멤버 하나만으로 답합니다(hosted를 proxy보다 앞에 두세요). 프로젝트 페이지(HTML, PEP 691 JSON)와 파일 다운로드 모두 그 멤버의 것이고, 뒤 멤버의 파일은 섞이지 않으며 뒤 멤버에만 있는 파일의 URL을 직접 요청해도 404입니다. 그 멤버보다 앞선 멤버가 "없음"이 아닌 오류를 내면 5xx, Offline이면 503으로 답합니다. hosted에 없는 프로젝트는 proxy에서 받습니다. `/simple/` 프로젝트 목록은 Online 멤버의 이름을 합친 것입니다. 파일 다운로드는 그 파일을 처음 가진 멤버가 답하므로(proxy는 캐시에 있으면 인덱스를 읽지 않음), proxy에 캐시된 파일은 원격(PyPI)이 장애이거나 원격에서 프로젝트가 삭제된 뒤에도 그룹으로 받을 수 있습니다(`uv.lock`처럼 파일 URL로 설치할 때). 파일이 없는 멤버가 그 프로젝트를 가졌으면 404이고, 마지막 멤버에게는 묻지 않습니다. 캐시에 없는 파일은 proxy가 인덱스에서 URL과 sha256을 찾아야 하므로 인덱스가 필요합니다.

업로드는 twine으로 hosted 저장소(또는 writeMember가 있는 그룹)에 합니다. 업로드 URL은 반드시 `/`로 끝나야 합니다.

```bash
twine upload --repository-url http://localhost:8081/repository/pypi-hosted/ -u ci dist/*
```

사용자 이름 자리에 `__token__`을 쓰면 비밀번호 자리의 API 토큰이 누구 것이든 그 주인으로 인증됩니다(`twine upload -u __token__ -p cc_…`). 다른 사용자 이름은 토큰 주인과 같아야 합니다. 이 규칙은 모든 포맷의 Basic 인증에 같습니다.

업로드 검사: 파일은 `.whl`, `.tar.gz`, `.zip`만 받습니다(그 밖은 400). wheel 파일 이름은 `{이름}-{버전}(-{빌드 태그})-{python 태그}-{abi 태그}-{platform 태그}.whl` 형식이어야 하고, 빌드 태그는 숫자로 시작해야 합니다(아니면 400 `invalid wheel file name`). `version`은 PEP 440 버전이면 어떤 표기든 받습니다. `1.0-1`, `01.2`, `2.0a`, `1.0post2`, `1.0.dev`, `1.0RC2`는 정규형 `1.0.post1`, `1.2`, `2.0a0`, `1.0.post2`, `1.0.dev0`, `1.0rc2`로 저장하고 비교합니다. 앞에 붙은 `v`(`v1.3`)와 앞뒤 공백은 받지 않습니다(400 `invalid version "v1.3"`). 폼의 `version`과 파일 이름의 버전은 정규화해서 비교하므로 `1.0`과 `1.0.0`처럼 PEP 440에서 같은 버전이면 됩니다.

파일 내용도 검사합니다. wheel은 zip이어야 하고 `*.dist-info` 디렉터리가 하나뿐이어야 하며, 그 안의 `METADATA`의 `Name`과 `Version`이 폼의 이름·버전과 (정규화해서) 같아야 합니다. sdist(`.tar.gz`, `.zip`)는 맨 위 디렉터리의 첫 `PKG-INFO`가 이 릴리스의 `<이름>-<버전>/` 디렉터리(표기는 달라도 됩니다)에 있어야 하고, 그 `Name`과 `Version`이 같아야 합니다. 텍스트 파일, 확장자와 다른 종류의 아카이브, 다른 패키지나 다른 버전의 메타데이터가 든 파일은 400(`invalid distribution file: …` 또는 `the file's metadata is for …`)이고 아무것도 기록되지 않습니다. zip(wheel, zip sdist)은 항목 10,000개와 central directory 2 MiB까지만 검사할 수 있어 이를 넘으면 400입니다. 항목 수와 크기는 zip에 적힌 값만 믿지 않고 central directory를 직접 세어 확인합니다. `.tar.gz` sdist는 `PKG-INFO`가 압축을 푼 처음 512 MiB 안에 있어야 합니다(hatchling, flit, poetry 같은 빌드 도구는 `PKG-INFO`를 맨 끝에 넣습니다).

같은 파일을 다시 올리면 `File already exists.`(400)로 거부됩니다. 같은 파일인지는 이름이 아니라 정규화한 프로젝트 이름, 버전, wheel 태그로 판단하므로 `Foo_Bar-1.0.0`과 `foo-bar-1.0`처럼 표기만 다른 것도 중복입니다. wheel의 python·abi·platform 태그는 집합으로 비교하므로(`py3.py3`은 `py3`, `py3.py2`는 `py2.py3`과 같습니다) 압축 태그 표기만 다른 wheel과 숫자가 같은 빌드 태그(`01`과 `1`)의 wheel도 중복입니다. 한 릴리스에는 sdist를 하나만 올릴 수 있습니다(`.tar.gz`가 있으면 `.zip`은 400, PEP 527). 이름이 다른 기존 파일과 겹치면 `File already exists: this release already has "<기존 파일>" …`로 그 파일을 알려 줍니다. twine은 서버의 오류 메시지를 기본으로 숨기므로, 원인을 보려면 `twine upload --verbose ...`로 실행하세요.

twine이 보내는 `sha256_digest`, `md5_digest`, `blake2_256_digest` 중 받은 파일과 다른 것이 있으면 400입니다. `Requires-Python`은 폼 값보다 파일 안의 메타데이터(wheel `METADATA`, sdist `PKG-INFO`)를 우선하며, 폼 값이든 메타데이터 값이든 지정자로 올바르지 않으면(`banana`, `3.8` 등) 400입니다. 메타데이터는 첫 빈 줄까지의 헤더 부분만 읽으므로 긴 README(long_description)가 있어도 업로드는 거부되지 않습니다. 헤더 부분이 1 MiB를 넘으면 그 뒤의 헤더는 읽지 않습니다. 그 뒤에 있는 `Requires-Python`은 무시하고 폼 값을 쓰며, `Name`이나 `Version`이 그 뒤에 있으면 400입니다(빌드 도구는 둘을 맨 앞에 씁니다).

프로젝트 페이지(HTML, PEP 691 JSON)의 파일 링크는 호스트 없는 경로(`/repository/<저장소>/packages/…`, `baseURL`에 경로가 있으면 그 경로가 앞에 붙습니다)이므로, pip는 인덱스에 접속한 주소(스킴, 호스트, 포트, URL에 넣은 인증 정보)로 파일을 받습니다. 그래서 `localhost`와 `127.0.0.1`처럼 `baseURL`과 다른 이름으로 접속해도 파일 다운로드가 401이 되지 않습니다. 리버스 프록시가 경로 앞부분을 붙여 서비스한다면 `baseURL`에도 같은 경로를 두세요.

twine에는 삭제 명령이 없으므로 잘못 올린 버전은 웹 UI나 API로 지웁니다("컴포넌트 삭제" 참고). 그 버전의 파일(wheel, sdist)이 모두 지워지고, 프로젝트 페이지와 group에서 바로 사라집니다.

**JSON API와 PEP 658 메타데이터.** pypi.org의 JSON API와 같은 모양의 문서를 `GET /repository/<저장소>/pypi/<프로젝트>/json`(프로젝트: `info`는 최신 정식 릴리스, `releases`에 모든 릴리스의 파일, `urls`에 최신 릴리스의 파일)과 `/pypi/<프로젝트>/<버전>/json`(그 릴리스만, `releases` 없음)으로 답합니다. 파일 항목에는 `filename`, `url`(`baseURL`로 만든 절대 URL), `digests`(`sha256`, `md5`, `blake2b_256`), `size`, `upload_time`, `packagetype`, `python_version`, `requires_python`, `yanked`가 들어갑니다. hosted는 업로드 때 읽어 둔 wheel의 `METADATA`·sdist의 `PKG-INFO`(`Summary`, `Requires-Dist`, `Classifier`, `Project-URL`, 본문 설명 등)로 `info`를 만들고, proxy는 원격의 `/pypi/<프로젝트>/json`을 메타데이터(`metadataMaxAge`)로 캐시해 파일 URL만 이 서버의 것으로 바꿔 답합니다(원격 문서 최대 8 MiB, 넘으면 502). 메타데이터를 읽을 수 없는 파일만 있는 hosted 릴리스의 `info`에는 `name`과 `version`만 들어갑니다. group은 그 프로젝트를 처음 가진 멤버의 문서입니다(simple 인덱스와 같은 규칙). 없는 프로젝트·버전은 404입니다.

각 파일 옆에 PEP 658 메타데이터 `…/packages/<프로젝트>/<파일>.metadata`(wheel의 `METADATA` 또는 sdist의 `PKG-INFO` 원문)를 제공하고, simple 인덱스(HTML `data-core-metadata`·`data-dist-info-metadata`, JSON `core-metadata`·`dist-info-metadata`)에 그 sha256과 함께 알리므로 pip가 wheel을 내려받지 않고 의존성을 풀 수 있습니다. hosted는 업로드 때 메타데이터를 파일과 함께 저장합니다(16 MiB 초과면 400). 이전 버전이 저장한 파일은 simple 인덱스에 처음 나열될 때(또는 처음 요청될 때) 파일에서 꺼내 저장하고 그 sha256을 알립니다. 꺼낼 수 없는 파일(초기 버전이 검사 없이 저장한 비표준 아카이브)은 그 사실을 기억해 인덱스에 알리지 않으므로 pip가 404로 중단되지 않습니다. proxy는 캐시된 메타데이터, 캐시된 파일에서 꺼낸 메타데이터, 원격 인덱스가 알린 `<파일 URL>.metadata`(알린 sha256과 대조) 순으로 답하고, 원격이 알리지 않은 파일은 인덱스에도 알리지 않습니다. 메타데이터 파일은 Browse에서 그 버전의 파일로 보이며 컴포넌트 삭제·Cleanup 때 파일과 함께 지워집니다.

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

같은 버전 파일을 다시 올리면 409로 거부됩니다(`writePolicy: allow`이면 허용). `.mod`를 먼저 올렸다면 zip 안의 `go.mod`가 그 `.mod`와 바이트 단위로 같아야 하고, 다르면 409입니다. `go.mod`가 없는 zip에는 go 명령이 그런 zip에 쓰는 `module <모듈 경로>` 한 줄짜리 `.mod`만 맞습니다. `writePolicy: allow`이면 zip의 `go.mod`가 기존 `.mod`를 대신하는데, 이미 있는 파일을 바꾸는 것이므로 edit 권한이 필요합니다(없으면 403).

`@v/list`, `@latest`, `.info`는 `.zip`이 있는 버전만 보여 줍니다. `.mod`만 올라간 버전은 목록에 나오지 않고 `.info`도 404이지만(group은 다음 멤버에게 묻습니다), 이미 올라간 `.mod`는 직접 요청하면 받을 수 있습니다. zip은 Go 모듈 zip 규칙의 핵심을 검사합니다. 같은 이름의 항목이 둘 이상이거나 대소문자만 다른 이름이 겹치거나, 경로에 `\`·`:`나 빈 요소(`//`), `.`·`..` 요소, `con`·`nul`·`com1`처럼 Windows 예약 이름이 있으면 400입니다. `zip -r`이 만드는 디렉터리 항목(`모듈경로@버전/`, `sub/`)은 받지만, 같은 이름의 파일과 디렉터리가 함께 있거나 디렉터리 항목에 내용이 있으면 400입니다. 모든 항목을 끝까지 풀어 읽어서 CRC와 헤더에 적힌 크기가 실제와 맞는지 확인하고(다르면 400), 압축을 푼 크기 합계 500 MiB는 실제로 읽은 바이트로 지킵니다. 루트의 `LICENSE`와 `go.mod`는 각각 16 MiB까지입니다. 올린 파일(`.mod`·`.zip`·`.info`)의 검사는 서버 전체에서 동시에 2개까지만 하고, 나머지 업로드는 본문을 디스크에 받아 둔 채 차례를 기다립니다.

`.mod`와 zip 안의 `go.mod`는 go 명령이 읽지 못하는 형식이면 400입니다. 검사하는 것은 토큰(주석은 `//`만 쓰고, 따옴표 문자열은 그 줄에서 끝나야 함), 괄호 블록의 짝, `module` 지시어(정확히 하나, 경로 하나), `go` 지시어(많아야 하나, `1.21`·`1.21.0`·`1.22rc1` 같은 버전), 그리고 Go 1.26이 아는 지시어(`module`, `go`, `toolchain`, `godebug`, `require`, `exclude`, `replace`, `retract`, `tool`, `ignore`)만 쓰는지입니다. `go` 지시어가 1.26보다 새 버전이면 모르는 지시어도 받습니다(새 Go가 추가한 지시어일 수 있고, 이때는 go 명령도 새 Go를 요구합니다). 다른 지시어의 인자(`require`의 버전, `replace`의 형식 등)는 검사하지 않으며, 한 줄이 1 MiB를 넘으면 400입니다.

모듈 경로의 메이저 버전 접미사는 버전과 일치해야 합니다(`example.com/hello/v2`는 `v2.x.y`, `gopkg.in/yaml.v3`는 `v3.x.y`). 접미사는 `/v2`부터이며, `/v0`, `/v1`, `/v01`, `/v1.2`처럼 `v`와 숫자·점으로 끝나는 다른 경로는 go 명령처럼 400으로 거부합니다. 접미사가 없는 경로에 `v2` 이상을 올리려면 `+incompatible` 버전을 쓰며, 이 zip에는 루트 `go.mod`가 없어야 합니다.

잘못 올린 버전은 그 버전의 `.mod`·`.info`·`.zip` 중 하나에 `DELETE`를 보내 지웁니다. 세 파일이 함께 지워지며(`.mod`만 올라간 미완성 버전도 마찬가지), `delete` 권한이 필요합니다(`add`만으로는 403). 성공하면 204, 그런 버전이 없으면 404이고, group으로 보내면 `writeMember`에서 지웁니다. go 명령은 받아 둔 모듈을 캐시(`GOMODCACHE`)와 `go.sum`에 남기므로, 지운 버전 번호로 다른 내용을 올리지 마세요.

```bash
curl -u ci -X DELETE http://localhost:8081/repository/go-hosted/example.com/hello/@v/v1.0.0.zip
```

주의 사항:

- `anonymousRead: false`이면 go 명령이 인증 정보를 필요로 합니다. `~/.netrc`에 `machine <호스트> login <사용자> password <비밀번호>`를 설정하세요.
- 기본 정책(`allow_once`)에서 `.info`를 직접 올릴 때는 `.mod`/`.zip`보다 먼저 올려야 합니다. 나중에 올리면 자동 생성된 `.info`와 충돌해 409가 납니다.
- group 조회는 hosted 모듈 경로도 proxy.golang.org로 전달합니다(list, latest, 버전 쿼리). 따라서 사내 모듈 경로 이름이 외부 프록시에 노출됩니다.
- 폐쇄망에서는 공개 모듈에도 `GOSUMDB=off` 또는 `GONOSUMDB`를 설정하세요(sum.golang.org에 접속할 수 없습니다).
- 업로드 크기 제한: `.zip` 500 MiB, `.mod` 16 MiB, `.info` 1 MiB. 초과하면 413을 반환합니다.

### Docker 설정

Docker/OCI 레지스트리 API는 `/v2/`에서 제공합니다. 이미지 이름은 `<저장소 이름>/<이미지 경로>`입니다(호스트 하나, 경로 접두사. Nexus와 달리 저장소별 포트가 없습니다).

```bash
docker login <host>                              # 사용자 비밀번호 또는 API 토큰
docker push <host>/docker-hosted/team/app:1.0
docker pull <host>/docker-hosted/team/app:1.0
```

- hosted, proxy, group을 모두 지원합니다. proxy의 `remote`는 레지스트리 주소(`/v2/` 없이. Docker Hub는 `https://registry-1.docker.io`)이며, Docker Hub에서는 경로 없는 이미지 이름(`nginx`)을 `library/nginx`로 바꿔 요청합니다. 원격이 401과 Bearer 안내(`WWW-Authenticate`)를 돌려주면 그 realm에서 토큰을 받아(`remoteAuth`가 있으면 그 자격 증명으로, 없으면 익명으로) 다시 요청하고, 토큰은 realm·service·scope별로 만료까지 재사용합니다. realm이 자격 증명을 거부하면(401/403) 404로 답하고, realm에 장애가 있으면 502로 답하며 부정 캐시에 남기지 않습니다. 태그로 받은 매니페스트와 태그 목록은 `metadataMaxAge`마다 다시 확인하고, digest로 받은 매니페스트와 레이어는 digest로 검증해 영구 캐시합니다(`contentMaxAge`). 멀티 아키텍처 이미지는 인덱스(manifest list)째로 받습니다. group은 멤버 순서대로 답하되 그 이미지를 가진 hosted 멤버가 있으면(중첩된 group 안의 hosted 멤버 포함) 그 멤버의 답(없으면 404)으로 끝나고, Offline 멤버는 건너뛰며, "없음"이 아닌 오류를 낸 멤버가 있으면 5xx(원격의 요청 제한은 그대로 429)입니다. catalog는 멤버의 합집합입니다. proxy와 group에 push하면 405입니다.
- 인증은 Distribution 토큰 방식(`/v2/token`, 5분 유효)이며 Basic 인증도 바로 받습니다. `GET /v2/`는 자격 증명 없는 요청에 익명 읽기가 켜져 있어도 401과 토큰 안내를 돌려줍니다. docker, crane, skopeo가 토큰 주소를 그 응답에서 찾기 때문이며, 익명 읽기가 켜진 저장소는 그 뒤 익명 토큰으로 로그인 없이 pull 됩니다. push에는 add, 매니페스트 삭제(`DELETE /v2/<이름>/manifests/<digest>`)에는 delete 권한이 필요합니다.
- `writePolicy`: `allow`는 태그 덮어쓰기 허용, `allow_once`는 이미 있는 태그를 다른 이미지로 바꾸지 못합니다(409). `deny`는 읽기 전용입니다.
- 매니페스트는 받은 바이트 그대로 저장되며(최대 4 MiB. schema2, manifest list, OCI manifest, OCI index를 받고 schema1은 거부), 참조하는 blob이 모두 있어야 저장됩니다. 레이어는 내용 주소 저장소에서 중복 제거되고, 업로드 하나는 `maxUploadMiB`를 넘을 수 없습니다. 중단된 업로드 세션은 24시간 뒤 정리됩니다.
- 매니페스트를 digest로 삭제하면 태그와 더 이상 참조되지 않는 레이어 행이 함께 지워지고(최근 24시간 안에 올리거나 HEAD로 확인한 레이어는 진행 중인 push가 쓸 수 있으므로 남겨 두고 다음 삭제 때 지웁니다), blob은 compact 작업이 회수합니다. 태그로는 삭제할 수 없습니다(400). Cleanup 정책은 docker 저장소에 적용되지 않습니다.
- 리버스 프록시 뒤에서는 `baseURL`의 호스트가 토큰의 service 값이 되므로 클라이언트가 접속하는 주소와 같아야 합니다. HTTPS가 아니면 docker 데몬에 insecure-registries 설정이 필요합니다(localhost 제외).

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

같은 버전을 다시 publish하면 409로 거부됩니다. `cargo yank`/`cargo yank --undo`를 지원합니다. yank한 버전은 새로 의존성을 고를 때만 빠지고 `Cargo.lock`에 고정된 빌드는 계속 받습니다. 아예 지우려면(예: 잘못 올린 crate) 웹 UI나 API로 삭제하세요("컴포넌트 삭제" 참고). 삭제한 버전은 인덱스와 다운로드에서 바로 사라지므로, 그 버전을 고정한 빌드는 실패합니다.
publish는 `.crate`를 검사합니다. gzip으로 압축한 tar여야 하고, 모든 항목이 `<이름>-<버전>/` 아래에 있어야 하며(절대 경로와 `..` 불가), 링크나 sparse 항목은 받지 않습니다. `<이름>-<버전>/Cargo.toml`(1 MiB 이하)이 있어야 하고, 그 `[package]` 표의 `name`과 `version`이 publish 메타데이터와 대소문자까지 같아야 합니다. 두 값은 한 줄 문자열이어야 하므로 `version.workspace = true` 같은 workspace 상속이 남아 있으면 거부합니다(`cargo publish`는 실제 값으로 바꿔 써서 올리므로 해당하지 않습니다). 위반하면 모두 400입니다. 저장된 crate는 `crates/<이름>/<버전>.crate` 경로로도 받을 수 있습니다(UI와 API가 이 주소를 알려 줍니다).
publish 메타데이터 중 인덱스에 그대로 들어가는 의존성 항목도 검사해, cargo가 읽지 못하는 줄이 인덱스에 남지 않게 합니다. 의존성 이름과 `explicit_name_in_toml`(영문자나 `_`로 시작하고 영문자·숫자·`-`·`_`만, 64자 이하), `version_req`(cargo의 semver 요구 조건: `^1.2`, `~1`, `>=1, <2`, `=1.0.0-rc.1`, `1.*`, `*` 등), `kind`(`normal`, `dev`, `build`), `target`(`cfg(...)` 식이나 `x86_64-unknown-linux-gnu` 같은 타깃 이름), `registry`(절대 URL)가 맞지 않으면 400입니다. crates.io와 달리 `*` 요구 조건과 다른 레지스트리의 의존성은 허용합니다.
Windows 장치 이름(`con`, `prn`, `aux`, `nul`, `com1`–`com9`, `lpt1`–`lpt9`)과 표준 라이브러리 이름(`std`, `core`, `alloc`, `proc_macro`, `test`)으로는 새 crate를 만들 수 없습니다(400, 대소문자와 `-`/`_`는 구분하지 않음). 이미 그 이름으로 올라가 있는 crate는 crates.io처럼 계속 새 버전을 올릴 수 있습니다.
crate 이름은 올린 표기(대소문자 포함) 그대로 인덱스, 웹 UI(검색 결과와 컴포넌트·asset 화면의 `cargo add` 예시), 패키지 API(`/-/api/repositories/<저장소>/packages`)와 활동 이벤트에 나옵니다. cargo는 의존성 이름을 대소문자까지 맞춰 찾기 때문입니다. 저장 경로(`crates/<이름>/<버전>.crate`)는 소문자이고, 인덱스·다운로드·yank·패키지 API 조회는 대소문자를 가리지 않습니다. proxy에 캐시된 crate도 웹 UI에는 캐시해 둔 인덱스 파일에서 읽은 올린 표기로 나오고, 인덱스 파일이 캐시에 없으면 소문자 이름으로 나옵니다("컴포넌트는 파생 값" 참고). 패키지 API는 packages 테이블을 읽으므로 proxy의 crate는 돌려주지 않습니다.
그룹 인덱스는 멤버 순서상 그 crate를 처음 가진 멤버의 인덱스를 그대로 냅니다(멤버끼리 합치지 않으므로 hosted를 proxy보다 앞에 두세요). 멤버가 "없음"이 아닌 오류를 내면 다음 멤버로 넘어가지 않고 5xx로 답합니다. 건너뛰면 오래된 버전이 조용히 선택될 수 있기 때문입니다.
인덱스 응답에는 hosted, proxy, group 모두 `ETag`가 붙고, 맞는 `If-None-Match`에는 304로 답합니다. proxy의 ETag는 캐시한 원격 인덱스 파일의 내용을, group의 ETag는 응답한 멤버와 그 멤버의 ETag를 따르므로, 내용이 바뀌거나 다른 멤버가 응답하게 되면 달라집니다. `Last-Modified`는 보내지 않고 `If-Modified-Since`는 무시합니다. yank는 올린 시각을 바꾸지 않고 group은 응답하는 멤버가 바뀔 수 있어, 시각으로 비교하면 바뀐 인덱스에 304가 나갈 수 있기 때문입니다. cargo는 `ETag`가 있으면 그것만 씁니다.

- `anonymousRead: false`인 저장소는 config.json이 `auth-required: true`를 알립니다. cargo 1.74 이상은 읽기에도 토큰이 필요하므로, `cargo login --registry commandcenter` 실행 시 `token create`로 출력된 토큰을 입력하세요.
- 설정 파일의 `baseURL`은 `index = "sparse+http://…"`에 쓰는 주소와 같아야 합니다. config.json의 `dl`·`api`가 이 값으로 만들어집니다.
- proxy 저장소의 `remote`는 `sparse+` 접두사를 뗀 sparse index URL입니다. 예: `https://index.crates.io/`

### Raw(일반 파일) 설정

`raw` 포맷은 Nexus의 raw처럼 어떤 파일이든 경로 그대로 저장하고 내려주는 저장소입니다. 설치 파일, 문서, 스크립트, 정적 사이트처럼 패키지 형식이 없는 파일에 씁니다. 클라이언트 설정은 없고 HTTP만 쓰면 됩니다.

```bash
# 업로드(add 권한): 경로는 자유. Content-Type 헤더를 보내면 그대로 저장되고, 없으면 확장자로 정합니다.
curl -u ci:PASSWORD -T installer.sh -H 'Content-Type: text/x-shellscript' http://localhost:8081/repository/raw-hosted/tools/installer.sh
# 다운로드(read 권한): 저장된 Content-Type으로 응답합니다. HEAD, Range, If-None-Match(ETag)도 됩니다.
curl -O http://localhost:8081/repository/raw-hosted/tools/installer.sh
# 삭제(delete 권한)
curl -u ci:PASSWORD -X DELETE http://localhost:8081/repository/raw-hosted/tools/installer.sh
```

- hosted: `writePolicy`를 Maven처럼 따릅니다. `allow_once`(기본)는 같은 경로 재업로드를 400으로 거부하고, `allow`는 덮어쓰기를 허용하되 이미 있는 파일을 덮어쓰려면 edit 권한이 필요하며(없으면 403), `deny`는 업로드·삭제를 400으로 막습니다. 경로는 Maven과 같은 규칙으로 검사합니다: `..`나 `.` 세그먼트, `//`, 역슬래시, 제어 문자, `?`·`#`·`;`, UTF-8이 아닌 바이트는 400입니다. 공백과 유니코드는 됩니다(URL 인코딩해서 보내세요).
- 디렉터리 목록: hosted 저장소에서 `/`로 끝나는 경로(저장소 루트 `/repository/raw-hosted/` 포함)를 GET하면 웹 서버처럼 하위 항목을 HTML 목록으로 보여 줍니다. `Accept: application/json`이면 `{"path":"docs/","entries":[{"name":"a.txt","dir":false,"size":5,"lastModified":"…"},{"name":"sub","dir":true}],"truncated":false}` 형태의 JSON입니다. 한 번에 최대 1000개를 보여 주고 더 있으면 `truncated`가 true입니다. 파일이 하나도 없는 디렉터리는 404입니다(루트는 항상 200). proxy와 group은 디렉터리 목록을 제공하지 않습니다(404).
- proxy: `remote` 아래의 어떤 경로든 원격에서 받아 캐시합니다. 원격이 보낸 Content-Type을 저장하고, 없으면 확장자로 정합니다. 다른 포맷과 달리 `text/html` 응답도 그대로 파일로 캐시합니다(raw 원격은 웹 사이트이기 때문). `contentMaxAge`(기본 영구)마다 다시 확인하고, 원격이 404/410을 준 경로는 `negativeCacheTTL` 동안 다시 묻지 않습니다. `/`로 끝나는 경로는 원격에 묻지 않습니다(원격의 목록 페이지를 파일로 캐시하지 않기 위해).
- group: 멤버 순서대로 처음 그 경로를 가진 멤버가 답합니다(hosted를 proxy보다 앞에 두세요). `writeMember`가 있으면 PUT·DELETE는 그 hosted 멤버로 갑니다.
- 웹 UI: Browse는 Maven처럼 경로 트리입니다. 컴포넌트는 파일 하나하나이며 이름이 경로이고 버전은 없습니다(검색의 version 조건은 raw에 맞지 않습니다). 컴포넌트 삭제(API `DELETE /-/api/repositories/<저장소>/components?name=<경로>`)는 version 없이 호출합니다. 메타데이터 탭은 raw에 없습니다.
- Cleanup 정책: hosted와 proxy 모두 `componentAgeDays`·`lastDownloadedDays`로 파일 단위로 정리합니다(정책 포맷 `all` 또는 `raw`). `releaseType`은 raw에서 모든 파일을 release로 봅니다. `namePattern`은 경로 전체와 맞춰야 합니다(예: `tools/.*\.sh`).
- 권한: 역할의 `format:raw` 대상도 쓸 수 있습니다. 활동 이벤트는 `upload`/`delete`에 경로가 기록됩니다.

### Helm 설정

Helm 차트 저장소(Nexus의 helm 포맷)입니다. `index.yaml`과 `<이름>-<버전>.tgz`를 `/repository/<저장소>/` 아래에서 제공하며, 서버가 `index.yaml`을 직접 만들어 유지합니다(차트를 올리거나 지울 때마다 같은 트랜잭션에서 다시 생성. `digest`, `created`, 저장소 기준 상대 `urls` 포함).

```bash
helm repo add cc http://localhost:8081/repository/helm-all
helm repo update
helm install app cc/demo --version 1.0.0

# 올리기: 파일 PUT(curl)이나 helm cm-push(POST api/charts). add 권한이 필요합니다.
curl -u ci:<비밀번호> --upload-file demo-1.0.0.tgz http://localhost:8081/repository/helm-hosted/
helm cm-push demo-1.0.0.tgz cc-hosted      # helm-push 플러그인. 응답 201 {"saved":true}
```

- hosted는 올린 tgz에서 `<디렉터리>/Chart.yaml`(1 MiB 이하)을 읽어 `name`, `version`(SemVer 2), `description`, `appVersion`, `dependencies` 등 모든 필드를 인덱스 항목으로 저장합니다. 파일 이름은 `<name>-<version>.tgz`여야 하며(버전의 `+`는 helm package처럼 `_`로 써도 됨) 다르면 400입니다. gzip tar가 아니거나 Chart.yaml이 없으면 400입니다. 업로드 본문은 `maxUploadMiB`를 따릅니다.
- `writePolicy`: `allow_once`(기본)는 같은 버전 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요), `deny`는 읽기 전용(400)입니다. cm-push의 `--force`는 무시하고 저장소 정책을 따릅니다.
- proxy의 `remote`는 `index.yaml`이 있는 차트 저장소 URL입니다(예: `https://charts.bitnami.com/bitnami/`). 원격 `index.yaml`은 `metadataMaxAge`마다 다시 확인하고, 항목의 `urls`를 proxy 경로로 바꿔 제공합니다. 차트는 처음 받을 때 원격 인덱스의 URL에서 가져와 `digest`로 검증한 뒤 영구 캐시합니다(`contentMaxAge`). 원격이 응답하지 않으면 만료된 인덱스를 `Warning: 110`과 함께 제공합니다. 큰 인덱스(bitnami 약 30 MB)는 내용이 바뀔 때 한 번만, 차트 버전 하나씩 스트리밍으로 해석하므로 메모리는 인덱스 크기와 무관합니다(인덱스 최대 256 MiB, 버전 항목 하나 또는 `entries` 밖 부분이 2 MiB를 넘으면 502). 원격에 `index.yaml`이 없으면(잘못된 `remote`) 빈 인덱스 대신 404로 답합니다.
- group의 `index.yaml`은 멤버 순서상 그 차트 이름을 처음 가진 멤버의 버전만 냅니다(hosted를 proxy보다 앞에 두세요). 다운로드도 같은 규칙이라 소유 멤버에 없는 버전은 뒤 멤버에 있어도 404입니다. 멤버가 "없음"이 아닌 오류를 내면 5xx로 답합니다. `writeMember`가 있으면 group에 올린 차트는 그 hosted로 갑니다.
- 삭제는 웹 UI·API의 컴포넌트 삭제("컴포넌트 삭제" 참고)와 Cleanup 정책(helm hosted·proxy 모두 가능)으로 하며, 지우면 `index.yaml`이 바로 갱신됩니다. 역할 권한 대상으로 `format:helm`을 쓸 수 있고, 컴포넌트 메타데이터 탭은 description·home·dependencies를 보여 줍니다.
- `helm push chart.tgz oci://<host>/<docker 저장소>/<차트>`는 OCI 매니페스트 push이므로 docker 저장소를 쓰세요("Docker 설정" 참고). helm 저장소의 `index.yaml`에는 OCI 차트가 나오지 않습니다.

### NuGet 설정

NuGet V3 피드(Nexus의 nuget 포맷)입니다. `dotnet restore`, `nuget.exe`, Visual Studio가 쓰는 서비스 인덱스 `index.json`, flat container(`v3-flatcontainer/`), registration(`registration/`), 검색(`query`), push API(`api/v2/package`)를 `/repository/<저장소>/` 아래에서 제공합니다. 모든 문서는 `baseURL`로 만든 절대 URL을 담으므로 `baseURL`이 클라이언트가 여는 주소와 같아야 합니다.

```bash
dotnet nuget add source http://localhost:8081/repository/nuget-all/index.json -n cc \
  -u ci -p <비밀번호 또는 API 토큰> --store-password-in-clear-text   # 읽기에 인증이 필요할 때
dotnet restore

# 올리기: API 키 자리에 API 토큰을 씁니다("API 토큰 발급" 참고). add 권한이 필요합니다.
dotnet nuget push Demo.Pkg.1.0.0.nupkg -s http://localhost:8081/repository/nuget-hosted/index.json -k <API 토큰>
nuget push Demo.Pkg.1.0.0.nupkg -Source http://localhost:8081/repository/nuget-hosted/index.json -ApiKey <API 토큰>
curl -u ci:<비밀번호> -T Demo.Pkg.1.0.0.nupkg http://localhost:8081/repository/nuget-hosted/api/v2/package   # 파일을 본문으로 보내도 됩니다
dotnet nuget delete Demo.Pkg 1.0.0 -s http://localhost:8081/repository/nuget-hosted/index.json -k <API 토큰> --non-interactive   # delete 권한
```

- hosted는 올린 `.nupkg`(zip)의 루트에서 `.nuspec`(1 MiB 이하)을 읽어 `id`, `version`, `description`, `authors`, `license`/`licenseUrl`, `projectUrl`, `tags`, 대상 프레임워크별 `dependencies`를 저장하고, 파일은 `<id>/<버전>/<id>.<버전>.nupkg`와 `.nuspec`로 둡니다(id는 소문자, 버전은 NuGet 정규화: 앞의 0 제거, `1.0`→`1.0.0`, 네 번째 자리 0 제거, 빌드 메타데이터 제거). registration과 flat container는 이 행에서 바로 만들므로 따로 인덱스 파일이 없습니다. zip이 아니거나 `.nuspec`이 없으면 400, 심볼 패키지(`.snupkg`)는 400으로 거부하며(서비스 인덱스에 `SymbolPackagePublish`가 없어 `dotnet nuget push`는 `.snupkg`를 건너뜁니다) 업로드 본문은 `maxUploadMiB`를 따릅니다.
- push 인증은 `X-NuGet-ApiKey` 헤더의 API 토큰(`-k`/`-ApiKey`), 또는 Basic(비밀번호나 토큰)입니다. `dotnet nuget push`는 먼저 `index.json`을 읽으므로 익명 읽기가 꺼진 저장소에는 `dotnet nuget add source`에 사용자/비밀번호도 넣어 두세요.
- `writePolicy`: `allow_once`(기본)는 같은 버전 재push를 409로 거부(`--skip-duplicate`가 이를 인식), `allow`는 덮어쓰기 허용(edit 권한 필요), `deny`는 읽기 전용(400)입니다.
- proxy의 `remote`는 V3 서비스 인덱스 URL입니다(예: `https://api.nuget.org/v3/index.json`; 그 디렉터리 URL도 됩니다). 서비스 인덱스·flat container 목록·registration·검색 결과는 `metadataMaxAge`마다 다시 확인하고, 문서 안의 registration·flat container URL을 proxy 주소로 바꿔 제공합니다(여러 페이지로 나뉜 registration은 클라이언트가 따라올 때 그 페이지만 가져옵니다). `.nupkg`/`.nuspec`은 처음 받을 때 캐시합니다(`contentMaxAge`). 원격이 응답하지 않으면 캐시된 문서를 `Warning: 110`과 함께 제공하고, 캐시에 없는 것은 502입니다. `remote`에 서비스 인덱스가 없으면 502로 그 URL을 알려 줍니다.
- group은 멤버 순서상 그 패키지 id를 처음 가진 멤버만으로 답합니다(hosted를 proxy보다 앞에 두세요). 소유 멤버에 없는 버전은 뒤 멤버에 있어도 404입니다. 검색은 멤버 결과를 합치되 같은 id는 앞 멤버 것만 냅니다. `writeMember`가 있으면 group에 push한 패키지는 그 hosted로 갑니다.
- 삭제는 `dotnet nuget delete`(delete 권한), 웹 UI·API의 컴포넌트 삭제, Cleanup 정책(nuget hosted·proxy)으로 하며 row와 파일만 지우면 registration이 바로 갱신됩니다. 역할 권한 대상으로 `format:nuget`을 쓸 수 있고, 컴포넌트 메타데이터 탭은 description·license·projectUrl·repository와 대상 프레임워크별 dependencies를 보여 줍니다.

### RubyGems 설정

RubyGems 저장소(Nexus의 rubygems 포맷)입니다. Bundler와 RubyGems 3 이상의 `gem`이 쓰는 compact index(`versions`, `names`, `info/<gem>`), 옛 `gem`이 쓰는 Marshal 인덱스(`specs.4.8.gz`, `latest_specs.4.8.gz`, `prerelease_specs.4.8.gz`, `quick/Marshal.4.8/<gem>.gemspec.rz`, `api/v1/dependencies`), gem 파일(`gems/<이름>-<버전>[-<플랫폼>].gem`), push API(`api/v1/gems`, `api/v1/gems/yank`)를 `/repository/<저장소>/` 아래에서 제공합니다. compact index에는 URL이 들어가지 않으므로 `baseURL`과 무관합니다.

```bash
gem sources --add http://localhost:8081/repository/gems-all/ --remove https://rubygems.org/
bundle config set --global mirror.https://rubygems.org http://localhost:8081/repository/gems-all/   # Gemfile의 source는 그대로 두고 미러로 쓸 때
bundle config set --global http://localhost:8081/repository/gems-all/ ci:<비밀번호 또는 API 토큰>   # 읽기에 인증이 필요할 때
bundle install

# 올리기: API 키 자리에 API 토큰을 씁니다("API 토큰 발급" 참고). add 권한이 필요합니다.
gem push demo-1.0.0.gem --host http://localhost:8081/repository/gems-hosted --key cc   # ~/.gem/credentials 에 ":cc: <API 토큰>" 한 줄
GEM_HOST_API_KEY=<API 토큰> gem push demo-1.0.0.gem --host http://localhost:8081/repository/gems-hosted
curl -u ci:<비밀번호> -H 'Content-Type: application/octet-stream' --data-binary @demo-1.0.0.gem http://localhost:8081/repository/gems-hosted/api/v1/gems
gem yank demo -v 1.0.0 --host http://localhost:8081/repository/gems-hosted --key cc   # delete 권한. 플랫폼 gem은 -p java 를 붙입니다
```

- hosted는 올린 `.gem`(tar)의 `metadata.gz`(YAML Gem::Specification, 압축 해제 8 MiB 이하)를 읽어 `name`, `version`, `platform`, `authors`, `summary`, `description`, `homepage`, `licenses`, `required_ruby_version`, `required_rubygems_version`, `dependencies`(runtime/development), `metadata`, sha256를 저장하고, 파일은 `gems/<이름>-<버전>[-<플랫폼>].gem`에 둡니다. 버전은 Gem::Version 규칙대로 `-`를 `.pre.`로 바꾸고(`1.0.0-rc1` → `1.0.0.pre.rc1`), 컴포넌트 버전은 플랫폼을 포함합니다(`1.0.0-java`). `info/<gem>`, `names`, specs 인덱스, `gemspec.rz`, `api/v1/dependencies`는 행에서 바로 만들고, `versions`만 파일로 두어 push·삭제 때 같은 트랜잭션에서 줄을 덧붙입니다(append-only: Bundler는 `Range`로 늘어난 부분만 받고, `ETag`는 파일 전체의 md5, `Repr-Digest`는 sha-256으로 검증합니다). tar가 아니거나 `metadata.gz`가 없거나 이름·버전·플랫폼이 올바르지 않으면 400이며 업로드 본문은 `maxUploadMiB`를 따릅니다.
- push 인증은 `gem push`가 보내는 `Authorization: <API 키>`(API 토큰 그대로), 또는 Basic(비밀번호나 토큰)입니다. Bundler는 `bundle config set <저장소 URL> 사용자:비밀번호`로 인증 정보를 둡니다.
- `writePolicy`: `allow_once`(기본)는 같은 버전 재push를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요, `versions`에 새 md5 줄 추가), `deny`는 읽기 전용(400)입니다.
- `gem yank`(`DELETE api/v1/gems/yank`, delete 권한)는 그 버전을 삭제합니다(파일과 행을 지우고 `versions`에 `-<버전>` 줄을 덧붙임). 없는 버전은 404입니다.
- proxy의 `remote`는 `https://rubygems.org/`(또는 다른 RubyGems 서버의 루트 URL)입니다. `versions`(약 20 MiB)·`names`·`info/*`·specs 인덱스·`api/v1/dependencies` 응답은 `metadataMaxAge`마다 다시 확인하고, `.gem`과 `gemspec.rz`는 처음 받을 때 캐시합니다(`contentMaxAge`). 내용을 바꾸지 않고 그대로 전달하므로 Bundler의 증분 받기도 그대로 동작하며, 256 MiB를 넘는 인덱스 문서는 502입니다. 원격이 응답하지 않으면 캐시된 문서를 `Warning: 110`과 함께 제공하고, 캐시에 없는 것은 502입니다. rubygems.org는 `api/v1/dependencies`를 더 이상 제공하지 않으므로(404) 옛 `gem`은 다른 서버의 proxy에서만 이 API를 씁니다.
- group은 `versions`와 `names`를 멤버 순서대로 병합합니다. 앞 멤버가 가진 gem 이름은 뒤 멤버의 버전을 숨기므로(내부 gem이 공개 저장소의 같은 이름에 가려지지 않도록) hosted를 proxy보다 앞에 두세요. 병합 결과는 group마다 60초 동안 메모리에 캐시합니다. `info/<gem>`, `gems/`, `quick/`는 그 이름을 처음 가진 멤버만으로 답하고 소유 멤버에 없는 버전은 뒤 멤버에 있어도 404입니다. `api/v1/dependencies`는 앞 멤버가 답한 gem을 빼고 다음 멤버에 묻습니다. `specs.4.8.gz` 등 Marshal 인덱스는 병합하지 않고 그 파일을 처음 가진 멤버 것을 그대로 냅니다. `writeMember`가 있으면 group에 push한 gem은 그 hosted로 갑니다.
- 삭제는 `gem yank`, 웹 UI·API의 컴포넌트 삭제, Cleanup 정책(`format: rubygems`, hosted·proxy 모두)으로 하며 지우면 모든 인덱스가 바로 갱신됩니다. 컴포넌트는 gem 이름과 `<버전>[-<플랫폼>]`으로 나타나고(prerelease 판정은 버전에 글자가 있는지로 하므로 `1.0.0-java`는 release), 역할 권한 대상으로 `format:rubygems`를 쓸 수 있습니다. 컴포넌트 메타데이터 탭은 summary·description, licenses, homepage, `source_code_uri`와 runtime/development dependencies를 보여 줍니다.

### Composer 설정

Composer v2 저장소(Nexus의 composer 포맷은 proxy만 있지만 여기서는 hosted·group도 됩니다)입니다. Composer 2.x가 읽는 `packages.json`, 패키지별 메타데이터 `p2/<vendor>/<package>.json`(개발 버전은 `~dev.json`), 배포 파일 `dist/…zip`, 검색 `search.json`을 `/repository/<저장소>/` 아래에서 제공합니다. 문서는 `baseURL`로 만든 절대 URL을 담으므로 `baseURL`이 클라이언트가 여는 주소와 같아야 합니다.

```bash
# 프로젝트에 저장소 추가(또는 ~/.composer/config.json에 -g로 전역 설정)
composer config repositories.commandcenter composer http://localhost:8081/repository/composer-all/
composer config -g repo.packagist false            # Packagist 대신 proxy/group만 쓸 때
composer config -g http-basic.localhost:8081 ci <비밀번호 또는 API 토큰>   # 읽기에 인증이 필요할 때
composer require monolog/monolog

# 올리기: 패키지 루트(또는 첫 디렉터리)에 composer.json이 있는 zip을 PUT 합니다. add 권한이 필요합니다.
git archive --format=zip --prefix=demo-1.2.0/ -o demo-1.2.0.zip v1.2.0
curl -u ci:<비밀번호> -T demo-1.2.0.zip http://localhost:8081/repository/composer-hosted/packages/upload/acme/demo/1.2.0
curl -u ci:<비밀번호> -X DELETE http://localhost:8081/repository/composer-hosted/packages/upload/acme/demo/1.2.0   # delete 권한
```

- hosted는 올린 zip의 `composer.json`(1 MiB 이하)을 읽어 `name`이 URL의 `<vendor>/<package>`와 같은지 확인하고, 버전은 URL 것을 Composer 규칙으로 정규화합니다(`v1.2`→`1.2.0.0`, `1.0.0-beta1`→`1.0.0.0-beta1`, `dev-main`은 그대로, `1.x-dev`→`1.9999999.9999999.9999999-dev`; `composer.json`에 `version`이 있으면 같은 값이어야 합니다). 파일은 `dist/<vendor>/<package>/<정규화 버전>.zip`에 두고 메타데이터는 행에서 바로 만듭니다. `packages.json`의 `available-packages`에 저장소의 패키지 목록을 실으므로 Composer는 여기 있는 패키지만 이 저장소에 묻습니다. zip이 아니거나 `composer.json`이 없으면 400이며 업로드 본문은 `maxUploadMiB`를 따릅니다. `/`가 든 브랜치 버전(`dev-feature/x`)은 올릴 수 없습니다.
- `writePolicy`: `allow_once`(기본)는 같은 버전 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요), `deny`는 읽기 전용(400)입니다.
- proxy의 `remote`는 Composer 저장소 루트입니다(예: `https://repo.packagist.org/`, Satis·Private Packagist도 됩니다; `packages.json`까지 적어도 됩니다). `packages.json`·패키지 메타데이터·검색 결과는 `metadataMaxAge`마다 다시 확인하고, `metadata-url`·`search`를 proxy 주소로 바꾸며 v1 `providers-url`과 `notify-batch`·`security-advisories`는 뺍니다. 각 버전의 `dist.url`은 `dist/<vendor>/<package>/<버전>/<원본 URL의 sha1>.zip`으로 바꾸고 `source`(git)는 제거하므로 Composer는 proxy만 봅니다. zip은 처음 요청될 때 원본 URL(GitHub 등)에서 받아 영구 캐시하며 메타데이터의 `shasum`이 있으면 검증합니다(불일치는 502). `remote`에 `packages.json`이 없거나 `metadata-url`이 없는 v1 전용 저장소면 502로 알려 줍니다.
- group은 멤버 순서상 그 패키지를 처음 가진 멤버만으로 답합니다(hosted를 proxy보다 앞에 두세요). 검색은 멤버 결과를 합치되 같은 이름은 앞 멤버 것만 냅니다. `writeMember`가 있으면 group에 올린 패키지는 그 hosted로 갑니다.
- 삭제는 `DELETE packages/upload/<vendor>/<package>/<버전>`(delete 권한), 웹 UI·API의 컴포넌트 삭제, Cleanup 정책(composer hosted·proxy)으로 합니다. 역할 권한 대상으로 `format:composer`를 쓸 수 있고, 컴포넌트 메타데이터 탭은 description·license·homepage·`support.source`와 `require`(scope require)·`require-dev`(scope require-dev, optional)를 보여 줍니다.

### APT 설정

Debian·Ubuntu 패키지 저장소(Nexus의 apt 포맷)입니다. `.deb` 파일은 `pool/` 아래에, apt가 읽는 메타데이터는 `dists/<distribution>/` 아래에 두며, hosted는 `Release`, `<component>/binary-<arch>/Packages`(와 `.gz`)를 서버가 직접 만들어 유지합니다(패키지를 올리거나 지울 때마다 같은 트랜잭션에서 다시 생성). 서명 키가 있으면 `Release.gpg`(분리 서명)와 `InRelease`(클리어 서명)도 함께 만듭니다.

```bash
# 서명 키가 있는 저장소: 공개키를 받아 signed-by로 지정합니다.
curl -fsSL http://localhost:8081/repository/apt-hosted/public.key | sudo gpg --dearmor -o /usr/share/keyrings/commandcenter.gpg
echo "deb [signed-by=/usr/share/keyrings/commandcenter.gpg] http://localhost:8081/repository/apt-hosted stable main" | sudo tee /etc/apt/sources.list.d/commandcenter.list
# (옛 방식) sudo apt-key add 로 공개키를 등록해도 됩니다: curl -fsSL .../public.key | sudo apt-key add -

# 서명 키가 없는 저장소: trusted=yes 로 서명 검사를 건너뜁니다(내부망 전용으로만 권장).
echo "deb [trusted=yes] http://localhost:8081/repository/apt-hosted stable main" | sudo tee /etc/apt/sources.list.d/commandcenter.list
sudo apt update && sudo apt install hello

# 올리기: 파일 PUT(curl) 또는 multipart POST(필드 이름 file). add 권한이 필요합니다. 응답 201과 저장된 경로를 JSON으로 돌려줍니다.
curl -u ci:<비밀번호> -T hello_1.0_amd64.deb http://localhost:8081/repository/apt-hosted/pool/hello_1.0_amd64.deb
curl -u ci:<비밀번호> -F file=@hello_1.0_amd64.deb http://localhost:8081/repository/apt-hosted/
```

- hosted는 올린 `.deb`(ar 아카이브)의 `control` 파일을 읽어 `Package`, `Version`, `Architecture`, `Maintainer`, `Depends`, `Description` 등 모든 필드를 `Packages` 항목으로 저장하고, 파일은 PUT 경로와 무관하게 Debian pool 규칙대로 `pool/<component>/<첫 글자 또는 lib?>/<패키지>/<패키지>_<버전>_<아키텍처>.deb`에 둡니다(버전의 `:`는 dpkg처럼 `%3a`로 씀). `Packages`에는 `Filename`, `Size`, `MD5sum`, `SHA1`, `SHA256`가 붙습니다. `Architecture: all` 패키지는 모든 아키텍처의 `Packages`에 함께 나오고, `Release`의 `Architectures`는 저장된 패키지에서 도출합니다(아키텍처별 패키지가 하나도 없으면 `all`만 나오므로 클라이언트 아키텍처의 패키지를 하나는 올려 두세요). 업로드 본문은 `maxUploadMiB`를 따릅니다.
- **control 아카이브는 무압축 `control.tar`와 gzip·xz·zstd·bzip2·lzma로 압축한 `control.tar.gz`·`.xz`·`.zst`·`.bz2`·`.lzma`를 모두 읽습니다.** Debian `dpkg-deb`의 기본값(xz)과 Ubuntu 21.10 이후의 기본값(zstd)으로 만든 패키지도 그대로 올릴 수 있습니다. 압축을 푼 control 아카이브는 64 MiB, `control` 파일은 1 MiB까지만 읽습니다. 압축 해제 창(xz 사전·zstd window)을 8 MiB보다 크게 선언한 아카이브(`xz -7` 이상, `zstd --ultra`·`--long=24` 이상 등), xz BCJ 필터나 zstd 사전(dictionary)을 쓴 아카이브, 그 밖의 압축(`.lz4` 등)은 400입니다. 패키지 이름은 소문자·숫자·`+.-` 두 글자 이상, 버전은 숫자로 시작해야 합니다(Debian 정책).
- 저장소 설정 `distribution`(기본 `stable`)과 `component`(기본 `main`)가 `dists/<distribution>/<component>/…` 경로와 sources.list의 suite·component가 됩니다. `signingKey`에는 비밀번호 없는 RSA(2048비트 이상) OpenPGP 개인키를 ASCII armor로 넣습니다(`gpg --batch --passphrase '' --quick-gen-key 'CC <apt@example.com>' rsa4096 sign never` 뒤 `gpg --armor --export-secret-keys apt@example.com`). 키는 원격 인증 정보처럼 암호화되어 저장되고 API·UI에는 설정 여부(`signingKeySet`)만 나오며, 공개키는 `/repository/<저장소>/public.key`로 제공합니다. 키가 없으면 서명 파일 없이 제공하므로 클라이언트에 `[trusted=yes]`가 필요합니다. 비밀번호가 걸린 키, v5/v6 키, RSA가 아닌 주키는 거부합니다.
- `writePolicy`: `allow_once`(기본)는 같은 패키지·버전·아키텍처 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요), `deny`는 읽기 전용(400)입니다.
- proxy의 `remote`는 `dists/`와 `pool/`이 있는 저장소 URL입니다(예: `https://deb.debian.org/debian/`, `https://archive.ubuntu.com/ubuntu/`). 원격이 주는 파일을 경로 그대로 캐시하며 `dists/` 아래(`InRelease`, `Packages.xz`, `by-hash/…` 등)는 `metadataMaxAge`마다 다시 확인하고 `pool/`의 `.deb`는 영구 캐시합니다(`contentMaxAge`). 원격의 서명(`InRelease`, `Release.gpg`)은 손대지 않고 그대로 전달하므로 클라이언트는 배포판의 키링(`debian-archive-keyring`)으로 그대로 검증합니다: `deb http://localhost:8081/repository/apt-proxy bookworm main`. 원격이 응답하지 않으면 만료된 메타데이터를 `Warning: 110`과 함께 제공합니다.
- group은 경로마다 멤버 순서상 그 경로를 처음 가진 멤버가 답합니다(raw와 같은 규칙). hosted 멤버들의 `Packages`를 합치지는 않으므로(Nexus도 같음) 같은 distribution 이름을 쓰는 hosted 두 개를 한 group에 넣으면 앞 멤버의 `dists/`만 보입니다. hosted와 proxy를 섞을 때는 distribution 이름이 겹치지 않게 하고(예: hosted `stable`, proxy `bookworm`) sources.list에 group URL로 두 줄을 쓰면 됩니다. `public.key`는 키가 있는 첫 멤버의 것이고, `writeMember`가 있으면 group에 올린 패키지는 그 hosted로 갑니다.
- 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(apt hosted·proxy 모두 가능)으로 하며, 지우면 `dists/`가 바로 갱신됩니다. 컴포넌트는 `.deb` 파일 하나이며 그룹 자리에 아키텍처, 이름에 패키지, 버전에 Debian 버전이 들어갑니다(Cleanup `namePattern`은 `amd64:hello`처럼 `아키텍처:패키지`에 맞춥니다). 역할 권한 대상으로 `format:apt`를 쓸 수 있고, 컴포넌트 메타데이터 탭은 Description·Maintainer·Homepage와 Depends(`depends`), Pre-Depends, Recommends, Suggests를 보여 줍니다.

### Yum 설정

Yum/DNF(RPM) 저장소(Nexus의 yum 포맷)입니다. `.rpm` 파일과 그 목록인 `repodata/`(`repomd.xml`, `primary`/`filelists`/`other`의 sha256 이름 gz 파일)를 `/repository/<저장소>/` 아래에서 제공하며, hosted는 서버가 `repodata/`를 직접 만들어 유지합니다(패키지를 올리거나 지울 때마다 같은 트랜잭션에서 createrepo_c와 같은 모양으로 다시 생성).

```ini
# /etc/yum.repos.d/cc.repo
[cc-el9]
name=Command Center el9
baseurl=http://localhost:8081/repository/yum-hosted/el9/
enabled=1
gpgcheck=1            # 서명된 RPM은 그대로 검증됩니다
gpgkey=file:///etc/pki/rpm-gpg/RPM-GPG-KEY-mine
repo_gpgcheck=0       # repomd.xml은 서명하지 않으므로 0으로 둡니다
```

```bash
# 올리기: 파일 PUT(curl). add 권한이 필요합니다. 파일 이름은 패키지의 <이름>-<버전>-<릴리스>.<아키>.rpm 이어야 합니다.
curl -u ci:<비밀번호> --upload-file demo-1.0-1.el9.x86_64.rpm http://localhost:8081/repository/yum-hosted/el9/
dnf --repo cc-el9 makecache && dnf install demo
```

- hosted는 올린 RPM의 헤더를 읽어 name, epoch/version/release, arch, summary, description, license, url, vendor, group, buildhost, sourcerpm, provides/requires/conflicts/obsoletes, 파일 목록, changelog를 저장하고 `primary.xml.gz`(`checksum type="sha256" pkgid="YES"`, `header-range`), `filelists.xml.gz`, `other.xml.gz`, `repomd.xml`을 다시 만듭니다. 파일 이름이 헤더의 `<이름>-<버전>-<릴리스>.<아키>.rpm`과 다르거나, RPM이 아니거나, `repodata/` 아래이면 400입니다. 업로드 본문은 `maxUploadMiB`를 따릅니다. 첫 업로드 전에는 `repodata/repomd.xml`이 404입니다.
- `depth`(hosted 전용, 기본 0): `repodata/`를 두는 위치입니다. `0`이면 저장소 루트에 하나를 두고 모든 경로의 RPM을 나열하며, `1`이면 첫 디렉터리마다(`el8/`, `el9/` …) 각각 두고 그 아래 RPM만 나열합니다(Nexus의 repodata depth). `depth: 1`에서는 루트에 바로 올릴 수 없고(400) `baseurl`은 `…/yum-hosted/el9/`처럼 디렉터리까지 적습니다. `location href`는 그 디렉터리 기준 상대 경로입니다.
- `writePolicy`: `allow_once`(기본)는 같은 패키지(이름·버전·릴리스·아키 같음) 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요, repodata도 갱신), `deny`는 읽기 전용(400)입니다.
- `repomd.xml`은 서명하지 않으므로 클라이언트의 `.repo`에 `repo_gpgcheck=0`을 두세요. `gpgcheck=1`은 RPM 자체의 서명을 검사하므로 서명된 패키지를 올리면 그대로 동작합니다.
- proxy의 `remote`는 미러의 URL이며 요청 경로를 그대로 원격에 붙여 캐시합니다(예: `remote: https://dl.rockylinux.org/pub/`, `baseurl=http://localhost:8081/repository/yum-proxy/rocky/9/BaseOS/x86_64/os/`). `repodata/` 아래 파일은 `metadataMaxAge`마다 다시 확인하고, RPM은 `contentMaxAge`(기본 영구)로 캐시합니다. 원격의 디렉터리 목록(HTML)은 파일로 캐시하지 않고 404로 답합니다. 내용을 바꾸지 않으므로 미러의 `repomd.xml` 서명(`repomd.xml.asc`)도 그대로 전달되어 `repo_gpgcheck=1`을 쓸 수 있습니다.
- group은 멤버 순서상 그 경로를 처음 가진 멤버가 답합니다. Nexus와 같이 멤버의 `repodata/`를 병합하지 않으므로 hosted와 proxy의 패키지를 한 `baseurl`로 합치려 하지 말고, 디렉터리(`depth: 1`)나 저장소별로 `.repo` 항목을 나누세요. `writeMember`가 있으면 group에 올린 RPM은 그 hosted로 갑니다.
- 컴포넌트는 디렉터리를 포함한 이름(`el9/x/demo`)과 `<버전>-<릴리스>.<아키>`(`1.0-1.el9.x86_64`)로 나타나며, 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(`format: yum`, hosted·proxy 모두)으로 하고 지우면 `repodata/`가 바로 갱신됩니다. 역할 권한 대상으로 `format:yum`을 쓸 수 있고, 컴포넌트 메타데이터 탭은 summary·description, license, url, requires/provides를 보여 줍니다.

### Conda 설정

conda·mamba·micromamba가 쓰는 Conda 채널(Nexus의 conda 포맷)입니다. `/repository/<저장소>/`가 채널이고, 그 아래 `noarch`, `linux-64`, `osx-arm64`, `win-64` 같은 subdir 디렉터리에 패키지(`.tar.bz2`, `.conda`)와 목록인 `repodata.json`을 두며, 채널 루트의 `channeldata.json`이 패키지별 최신 버전을 요약합니다. hosted는 서버가 이 문서들을 직접 만들어 유지합니다(패키지를 올리거나 지울 때마다 같은 트랜잭션에서 다시 생성).

```bash
# 채널로 등록: .condarc 또는 conda config
conda config --add channels http://localhost:8081/repository/conda-hosted
# 또는 한 번만: -c <채널 URL>
conda install -c http://localhost:8081/repository/conda-hosted foo
micromamba install -c http://localhost:8081/repository/conda-all numpy

# 올리기: 파일 PUT(curl). add 권한이 필요합니다. 경로는 <subdir>/<이름>-<버전>-<빌드>.tar.bz2 또는 .conda 입니다.
curl -u ci:<비밀번호> --upload-file foo-1.0-py_0.tar.bz2 http://localhost:8081/repository/conda-hosted/noarch/
curl -u ci:<비밀번호> -T bar-2.1-h1234_0.conda http://localhost:8081/repository/conda-hosted/linux-64/bar-2.1-h1234_0.conda
```

`.condarc`에 직접 적을 때:

```yaml
channels:
  - http://localhost:8081/repository/conda-all
  - defaults
```

- hosted는 올린 `.tar.bz2`, 또는 `.conda` 안의 `info-<이름>-<버전>-<빌드>.tar.zst`에서 `info/index.json`과 `info/about.json`을 읽어 name, version, build, build_number, depends, constrains, license, noarch, timestamp 등 index.json의 모든 항목에 md5·sha256·size를 더해 `<subdir>/repodata.json`과 `current_repodata.json`(같은 내용)의 `packages`(`.conda`는 `packages.conda`)에 싣고, about.json의 summary·description·home·license·dev_url·doc_url로 `channeldata.json`을 만듭니다. 파일 이름이 index.json의 이름·버전·빌드와 다르거나 subdir이 index.json의 `subdir`과 다르면 400입니다. 업로드 본문은 `maxUploadMiB`를 따릅니다. 아직 패키지가 없는 subdir의 `repodata.json`은 빈 문서로 답하므로(conda는 subdir의 404를 오류로 봅니다) noarch 패키지만 올린 채널도 바로 쓸 수 있습니다.
- **`.conda` 파일**: zip 구조와 `metadata.json`(`conda_pkg_format_version`)을 확인하고, zstd로 압축된 `info-<이름>-<버전>-<빌드>.tar.zst`를 풀어 `.tar.bz2`와 똑같이 index.json과 about.json을 읽습니다. 따라서 `packages.conda` 항목에도 depends·constrains·license·timestamp 등 index.json의 모든 항목이 실리고, conda가 의존성을 풉니다. 파일 이름이 index.json의 이름·버전·빌드와 다르거나 subdir이 다르면 400입니다. info tarball은 압축을 푼 크기 64 MiB까지 읽고, zstd window가 8 MiB를 넘거나 사전을 쓴 tarball, 항목이 64개를 넘는 zip은 400입니다. 이전 버전에서 올린 `.conda`는 다시 올려야 의존성이 실립니다. 같은 빌드를 `.tar.bz2`와 `.conda` 둘 다 올리면 하나의 컴포넌트(파일 두 개)로 다룹니다.
- `repodata.json.bz2`·`.zst`·`.jlap`는 hosted가 만들지 않습니다(404). conda·mamba는 이때 `repodata.json`으로 넘어가므로 설정은 필요 없습니다.
- `writePolicy`: `allow_once`(기본)는 같은 경로 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요, repodata도 갱신), `deny`는 읽기 전용(400)입니다.
- proxy의 `remote`는 채널 URL입니다(예: `https://conda.anaconda.org/conda-forge/`, `https://repo.anaconda.com/pkgs/main/`). 요청 경로를 그대로 원격에 붙여 캐시하며 `repodata.json`(과 `.bz2`·`.zst`·`.jlap`), `current_repodata.json`, `channeldata.json`은 `metadataMaxAge`마다 다시 확인하고 패키지는 `contentMaxAge`(기본 영구)로 캐시합니다. conda-forge linux-64의 `repodata.json`은 수백 MB이지만 메모리에 담지 않고 그대로 저장소로 흘려보냅니다. 내용은 바꾸지 않습니다(repodata에는 URL이 없음). 원격이 응답하지 않으면 만료된 메타데이터를 `Warning: 110`과 함께 제공합니다.
- group은 패키지 파일은 멤버 순서상 그 파일을 처음 가진 멤버가 답하고, `<subdir>/repodata.json`·`current_repodata.json`·`channeldata.json`은 **멤버들의 문서를 합쳐서** 답합니다(hosted를 앞에 두면 hosted 패키지 우선, 같은 파일 이름은 앞 멤버 것). 합친 문서는 group 저장소에 저장해 두고 멤버 문서가 바뀔 때(hosted 업로드·삭제, proxy의 원격 갱신)만 다시 만듭니다. group은 `repodata.json.zst`·`.bz2`·`.jlap`를 404로 답해 클라이언트가 합친 `repodata.json`을 쓰게 합니다. `writeMember`가 있으면 group에 올린 패키지는 그 hosted로 갑니다.
- 컴포넌트는 subdir을 그룹으로, 이름과 `<버전>-<빌드>`(`1.0-py_0`)를 버전으로 나타나며(라벨 `noarch/foo@1.0-py_0`, Cleanup `namePattern`은 `noarch:foo`처럼 `subdir:이름`에 맞춥니다), 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(`format: conda`, hosted·proxy 모두)으로 하고 지우면 `repodata.json`·`channeldata.json`이 바로 갱신됩니다. 역할 권한 대상으로 `format:conda`를 쓸 수 있고, 컴포넌트 메타데이터 탭은 summary·description, license, home, dev_url과 depends(`depends`)·constrains(`constrains`)를 보여 줍니다.

### Conan 설정

Conan 원격 저장소(Nexus의 conan 포맷)입니다. Conan 2.x가 쓰는 v2 API(리비전)와, Conan 1.x가 리비전 없이 쓰는 v1 API의 읽기·업로드 부분을 `/repository/<저장소>/` 아래에서 제공합니다. v1 문서는 `baseURL`로 만든 절대 URL을 담으므로 `baseURL`이 클라이언트가 여는 주소와 같아야 합니다.

```bash
conan remote add cc http://localhost:8081/repository/conan-all
conan remote login cc ci -p <비밀번호 또는 API 토큰>     # 읽기에 인증이 필요하거나 올릴 때
conan install --requires=zlib/1.3 -r cc --build=missing
conan create . -r cc                                    # 의존성은 cc에서 받습니다
conan upload "mypkg/*" -r cc --confirm                   # add 권한. hosted 또는 writeMember가 있는 group
conan list "zlib/*" -r cc                                # 검색
conan remove "mypkg/1.0#*" -r cc --confirm               # delete 권한
```

- 로그인은 `v1/users/authenticate`에 Basic으로 비밀번호나 API 토큰을 보내 1시간짜리 토큰을 받고, 이후 요청은 `Authorization: Bearer <토큰>`으로 옵니다. 토큰이 만료되면 Conan이 저장된 자격으로 다시 로그인합니다(`conan remote login`, `CONAN_LOGIN_USERNAME_*`/`CONAN_PASSWORD_*`, `credentials.json`). `v1/ping`과 `v1/users/*`는 권한 없이 열리고, 그 외 읽기는 read, 업로드는 add, 삭제는 delete 권한입니다. 익명 읽기가 꺼진 저장소는 `conan remote login`을 먼저 하세요.
- hosted는 파일을 v2 URL 경로 그대로 저장합니다: 레시피 `v2/conans/<name>/<version>/<user>/<channel>/revisions/<rrev>/files/<파일>`(user/channel이 없으면 `_`), 바이너리 `.../revisions/<rrev>/packages/<package_id>/revisions/<prev>/files/<파일>`. 레시피 리비전마다 패키지 행을 하나 두어 검색과 `latest`/`revisions`(최신 먼저)에 씁니다. 웹 UI의 컴포넌트는 레시피 리비전입니다: 이름은 레시피 이름, 그룹은 `user/channel`, 버전은 `<version>#<rrev>`이고 그 리비전의 바이너리 파일이 모두 애셋으로 보입니다.
- `writePolicy`: 새 리비전은 언제나 새 컴포넌트입니다. `allow_once`(기본)는 같은 리비전의 같은 파일을 다른 내용으로 다시 올리면 409(같은 내용은 200), `allow`는 덮어쓰기 허용(edit 권한), `deny`는 읽기 전용(400)입니다. `checksum_deploy`(`X-Checksum-Deploy`)는 같은 경로에 같은 sha1 파일이 있을 때만 201이고 아니면 404라 클라이언트가 내용을 올립니다. 업로드 본문은 `maxUploadMiB`를 따릅니다.
- proxy의 `remote`는 Conan 서버 루트(예: `https://center.conan.io/`, Artifactory는 `https://<host>/artifactory/api/conan/<repo>/`)이고 v2 API만 씁니다. `latest`·`revisions`·`files` 목록·패키지 검색은 `metadataMaxAge`마다 다시 확인하고, 파일은 처음 받을 때 캐시합니다(`contentMaxAge`). 원격이 응답하지 않으면 캐시된 문서를 `Warning: 110`과 함께 주고, 없는 레시피는 `negativeCacheTTL` 동안 404입니다. `remoteAuth`에 사용자/비밀번호를 주면 원격의 `v1/users/authenticate`에서 토큰을 받아 Bearer로 보내고, 거부되면 한 번 다시 로그인합니다(Conan Center는 익명).
- group은 멤버 순서상 그 레시피를 처음 가진 멤버(`latest`가 200인 멤버)만으로 답하므로 hosted를 proxy보다 앞에 두세요. 검색은 멤버 결과를 합칩니다. `writeMember`가 있으면 group에 올린 것은 그 hosted로 갑니다.
- Conan 1.x(리비전 없음)는 v1 API로 최신 리비전을 읽습니다: 스냅샷(`{파일: md5}`, 저장된 `conanmanifest.txt`에서), `digest`, `download_urls`, 패키지 목록(`search`), `upload_urls`(새 레시피는 리비전 `0`, 패키지는 패키지 리비전 `0`에 올라갑니다). v1의 `remove_files`와 `packages/delete`는 없으므로 Conan 1의 `conan remove -p`와, 원격에만 있는 파일을 지우는 재업로드는 실패합니다. 리비전을 켠 Conan 1(`CONAN_REVISIONS_ENABLED=1`)은 Conan 2처럼 v2 API를 씁니다.
- 삭제는 `conan remove -r`(레시피, 리비전, 패키지, 패키지 리비전 모두 delete 권한), 웹 UI·API의 컴포넌트 삭제(레시피 리비전과 그 바이너리 전부), Cleanup 정책(conan hosted·proxy, 레시피 리비전 단위; `releaseType`은 `#rrev`를 뗀 버전으로 판단)으로 합니다. 역할 권한 대상으로 `format:conan`을 쓸 수 있고, 컴포넌트 메타데이터 탭은 `conanfile.py`를 훑어 `description`·`license`·`homepage`·`url`과 `requires`/`tool_requires`/`build_requires`/`test_requires`(클래스 속성과 `self.requires("…")` 호출)를 보여 줍니다. 조건문은 평가하지 않습니다.

### R 설정

R(CRAN 형식) 저장소입니다. `install.packages()`, remotes, pak, renv가 읽는 구조 그대로 `/repository/<저장소>/` 아래에 소스 패키지는 `src/contrib/<패키지>_<버전>.tar.gz`, 바이너리는 `bin/windows/contrib/<R.x>/<패키지>_<버전>.zip`, `bin/macosx/contrib/<R.x>/…tgz`(`bin/macosx/big-sur-arm64/contrib/<R.x>/` 등 빌드 하위 디렉터리 포함)로 두고, 각 contrib 디렉터리에 `PACKAGES`, `PACKAGES.gz`, `PACKAGES.rds`를 제공합니다. hosted는 서버가 세 파일을 직접 만들어 유지합니다(패키지를 올리거나 지울 때마다 같은 트랜잭션에서 `tools::write_PACKAGES`와 같은 필드 순서로 다시 생성).

```r
# ~/.Rprofile 또는 세션에서
options(repos = c(CC = "http://localhost:8081/repository/r-all"))   # CRAN 대신 그룹(또는 hosted/proxy) 하나만
install.packages("foo")                                              # 소스와 바이너리를 OS·R 버전에 맞게 찾습니다
# 인증이 필요한 저장소: options(repos = c(CC = "http://user:password@localhost:8081/repository/r-all"))
# renv: renv.lock의 Repositories 항목이나 options(repos=) 를 같은 URL로 바꿉니다
```

```bash
# 올리기: R CMD build 로 만든 tar.gz 를 contrib 디렉터리에 PUT(curl). add 권한이 필요합니다.
curl -u ci:<비밀번호> --upload-file foo_1.0.0.tar.gz http://localhost:8081/repository/r-hosted/src/contrib/
# Windows 바이너리(R CMD INSTALL --build 로 만든 zip)와 macOS 바이너리(tgz)는 R 버전 디렉터리에 올립니다
curl -u ci:<비밀번호> --upload-file foo_1.0.0.zip http://localhost:8081/repository/r-hosted/bin/windows/contrib/4.4/
curl -u ci:<비밀번호> --upload-file foo_1.0.0.tgz http://localhost:8081/repository/r-hosted/bin/macosx/big-sur-arm64/contrib/4.4/
```

- hosted는 올린 파일 안의 `<패키지>/DESCRIPTION`을 읽어(tar.gz·tgz는 tar+gzip, zip은 zip) 필드 전체와 md5/sha256/크기를 저장하고, `Package`·`Version`이 파일 이름과 다르거나 DESCRIPTION이 없거나 디렉터리와 확장자가 맞지 않으면(`src/contrib`에 zip 등) 400입니다. 파일 이름은 `<패키지>_<버전>.<확장자>`이며 패키지 이름은 R 규칙(영문자로 시작, 2자 이상, 영숫자와 `.`)을 따릅니다. 업로드 본문은 `maxUploadMiB`를 따릅니다. 첫 업로드 전에도 contrib 디렉터리의 `PACKAGES`(빈 파일)·`PACKAGES.rds`(0행 행렬)를 200으로 답하므로 다른 저장소의 패키지를 설치할 때 경고가 나지 않습니다.
- `PACKAGES`에는 Package, Version, Priority, Depends, Imports, LinkingTo, Suggests, Enhances, License, License_is_FOSS, License_restricts_use, OS_type, Archs, MD5sum(파일의 md5), NeedsCompilation, 바이너리면 Built가 들어가며 디렉터리마다 패키지·버전 순으로 정렬됩니다. 같은 패키지의 여러 버전을 함께 두면 R이 최신 버전을 고릅니다.
- `PACKAGES.rds`: R 직렬화 형식(XDR, 버전 2, gzip)으로 쓴 문자 행렬이며 R 2.3 이상 어느 버전이든 읽습니다. R은 `.rds`를 먼저 읽고 실패하면 `PACKAGES.gz`, `PACKAGES` 순으로 내려가므로 세 파일의 내용은 항상 같습니다. 컴포넌트는 contrib 디렉터리를 그룹으로 하여 `src/contrib/foo@1.0.0`, `bin/windows/contrib/4.4/foo@1.0.0`처럼 나타나며, 같은 패키지·버전이라도 디렉터리가 다르면 별개 컴포넌트입니다.
- `writePolicy`: `allow_once`(기본)는 같은 디렉터리의 같은 패키지·버전 재업로드를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요, 인덱스도 갱신), `deny`는 읽기 전용(400)입니다.
- proxy의 `remote`는 CRAN 미러 또는 Posit Package Manager의 루트이며(예: `remote: https://cloud.r-project.org/`, `remote: https://packagemanager.posit.co/cran/latest/`) 요청 경로를 그대로 원격에 붙여 캐시합니다. 내용은 바꾸지 않습니다. `PACKAGES*`와 `Meta/` 아래 파일은 `metadataMaxAge`마다 다시 확인하고, 패키지 파일(`src/contrib/Archive/<패키지>/`의 옛 버전 포함)은 `contentMaxAge`(기본 영구)로 캐시합니다. 원격의 디렉터리 목록(HTML)은 404로 답합니다. `options(repos = c(CRAN = "http://localhost:8081/repository/r-proxy"))`로 씁니다.
- group은 패키지 파일을 멤버 순서상 처음 가진 멤버에서 답하고, contrib 디렉터리의 `PACKAGES`는 멤버들의 것을 병합합니다(hosted를 앞에 두면 사내 패키지가 먼저 나오고, 같은 Package·Version은 앞선 멤버 것만 남습니다). 병합 결과는 멤버의 문서가 바뀔 때까지 저장해 두고, 그룹의 `PACKAGES.gz`·`PACKAGES.rds`는 병합한 `PACKAGES`에서 만들어(미러의 .rds를 읽지 않음) 같은 방식으로 캐시합니다. 어느 멤버에도 없는 디렉터리는 빈 인덱스로 답합니다. `writeMember`가 있으면 group에 올린 파일은 그 hosted로 갑니다.
- 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(`format: r`, hosted·proxy 모두)으로 하고, 지우면 그 디렉터리의 인덱스 세 파일이 바로 갱신됩니다. 역할 권한 대상으로 `format:r`을 쓸 수 있고, 컴포넌트 메타데이터 탭은 Title·Description, License, URL, BugReports와 Depends/Imports/LinkingTo/Suggests(Suggests는 선택)를 보여 줍니다.

### Git LFS 설정

`gitlfs` 포맷은 Git LFS 서버(Batch API, basic 전송)입니다. Git 저장소 자체는 다른 곳(Gitea, GitHub 등)에 두고 큰 파일(LFS 오브젝트)만 Command Center에 저장합니다. Nexus의 Git LFS와 같이 hosted만 지원하며 proxy·group은 만들 수 없습니다(설정 검증에서 400).

```bash
# 저장소마다 LFS 서버를 지정합니다(.lfsconfig를 커밋하면 모든 클론에 적용됩니다).
git config -f .lfsconfig lfs.url http://localhost:8081/repository/lfs-hosted/info/lfs
# 또는 로컬 설정만: git config lfs.url http://localhost:8081/repository/lfs-hosted/info/lfs
# (…/repository/lfs-hosted/ 까지만 써도 됩니다.)

# 인증: git credential helper에 API 토큰을 저장합니다(비밀번호도 되지만 토큰을 권장합니다).
git config credential.helper store      # 또는 osxkeychain, manager 등
printf 'protocol=http\nhost=localhost:8081\nusername=ci\npassword=<API 토큰>\n' | git credential approve

git lfs track '*.bin'
git add .gitattributes file.bin && git commit -m 'lfs'
git lfs push origin main          # 오브젝트는 lfs.url로, 커밋은 origin으로
git push origin main
git lfs pull                      # 다운로드(read 권한)
```

- 권한: 다운로드(batch `download`, GET)는 read, 업로드(batch `upload`, PUT)는 add, 오브젝트 DELETE는 delete 권한입니다. 인증이 없으면 batch 응답이 401과 `LFS-Authenticate: Basic realm="commandcenter"`로 자격 증명을 요구하며, git-lfs는 credential helper의 값을 Basic으로 보냅니다. `Authorization: Bearer <API 토큰>`도 됩니다. 역할 대상으로 `format:gitlfs`를 쓸 수 있습니다.
- 저장: 오브젝트는 sha256(oid)으로 이름 붙은 파일 하나이며 `objects/<aa>/<bb>/<oid>` 경로에 저장됩니다. 업로드 내용의 sha256이 oid와 다르면 422로 거부합니다. 같은 oid는 같은 내용이므로 재업로드는 `allow_once`에서도 200으로 받습니다(멱등). `writePolicy: deny`는 업로드·삭제를 400으로 막습니다. `POST …/objects/verify`는 저장된 크기가 맞으면 200, 아니면 422입니다.
- 크기 제한: 오브젝트 하나는 `maxUploadMiB`(기본 1024)를 넘을 수 없습니다(413). LFS 오브젝트는 크므로 LFS를 쓰는 인스턴스는 이 값을 넉넉히 올리고 리버스 프록시의 본문 제한("Ingress와 큰 업로드" 참고)도 맞추세요.
- 잠금(File Locking)은 지원하지 않습니다. `locks` API는 501로 답하고 git-lfs는 그 뒤로 잠금 확인을 건너뜁니다(`lfs.<url>.locksverify=false`). `git lfs lock`은 쓸 수 없습니다.
- 웹 UI: 컴포넌트는 오브젝트 하나하나이며 이름이 oid이고 버전은 없습니다. 컴포넌트 삭제(API `DELETE /-/api/repositories/<저장소>/components?name=<oid>`)는 version 없이 호출합니다. 메타데이터 탭은 크기와 sha256만 보여 줍니다. Cleanup 정책(포맷 `all` 또는 `gitlfs`)은 `componentAgeDays`·`lastDownloadedDays`로 오브젝트 단위로 정리합니다. 활동 이벤트는 `upload`/`delete`에 oid가 기록됩니다.

### p2 설정

Eclipse p2 업데이트 사이트(Nexus의 p2 포맷)입니다. Eclipse의 Install New Software, Tycho/Maven의 p2 리졸버가 요청하는 파일(`p2.index`, `content.xml`/`content.jar`/`content.xml.xz`, `artifacts.xml`/`artifacts.jar`/`artifacts.xml.xz`, `compositeContent.*`, `compositeArtifacts.*`, `site.xml`, `features/`, `plugins/`, `binary/`)을 `/repository/<저장소>/` 아래에서 제공합니다. Nexus와 같이 **proxy와 group만** 지원하며 hosted는 설정 검증에서 거부합니다(`p2 supports proxy and group repositories only`).

```
Eclipse: Help > Install New Software… > Add… > Location: http://localhost:8081/repository/p2-all/
```

```xml
<!-- Tycho (pom.xml): layout p2 -->
<repository>
  <id>cc-p2</id>
  <url>http://localhost:8081/repository/p2-all/</url>
  <layout>p2</layout>
</repository>
```

- proxy의 `remote`는 p2 사이트 URL입니다(예: `https://download.eclipse.org/releases/2024-09/`, `https://download.eclipse.org/eclipse/updates/4.33/`). 요청 경로를 그대로 원격에 붙여 캐시하며 `p2.index`·`content.*`·`artifacts.*`·`composite*`·`site.xml`은 `metadataMaxAge`마다 다시 확인하고, `features/`·`plugins/`·`binary/`(와 구형 `.pack200.gz`)는 `contentMaxAge`(기본 영구)로 캐시합니다. 원격이 보낸 Content-Type을 그대로 저장합니다. 원격이 응답하지 않으면 만료된 메타데이터를 `Warning: 110`과 함께 제공합니다. `artifacts.xml`의 매핑은 `${repoUrl}` 기준이라 바꾸지 않습니다.
- 복합(composite) 사이트: `compositeContent.xml`/`compositeArtifacts.xml`(과 `.jar`)의 `<child location>`을 proxy를 통해 풀리도록 바꿔서 내보냅니다. 상대 경로 자식은 그대로 두고, 원격 `remote` 아래의 절대 URL 자식은 proxy 안의 상대 경로로, 다른 호스트·다른 경로의 자식은 `.remote/<자식 URL의 sha1>/`로 바꾸며 그 아래 요청은 해당 자식 URL에서 받아 캐시합니다(sha1→URL 표는 `.remote/index.json` 자산에 보관). `.jar`는 풀어서 바꾼 뒤 다시 압축합니다. 캐시된 자산은 원격 원본 그대로이고 응답할 때만 바꿉니다. `.xml.xz`는 xz를 풀지 않으므로 그대로 전달합니다(복합 문서를 xz로 내는 사이트는 없습니다). 원격 인증 정보는 `remote` 호스트에만 보냅니다.
- group은 멤버를 자식으로 하는 복합 저장소입니다: `p2.index`와 `compositeContent.xml`·`compositeArtifacts.xml`을 서버가 만들어(`<child location='<baseURL>/repository/<멤버>/'/>`, offline 멤버 제외, `p2.atomic.composite.loading=false`) 답하고, 그 밖의 파일(`plugins/…` 등)은 멤버 순서상 처음 가진 멤버가 답합니다. group 루트의 `content.*`·`artifacts.*`·`site.xml`·`composite*.jar`는 404입니다(클라이언트는 `p2.index`를 따라 xml을 읽습니다). `baseURL` 설정이 필요합니다.
- 컴포넌트는 디렉터리를 그룹으로 하여 `plugins/<id>@<버전>`, `features/<id>@<버전>`, `binary/<id>@<버전>`으로 나타나며(`.remote/<sha1>/` 아래 캐시도 같은 컴포넌트), `.pack200.gz`와 문서 파일은 컴포넌트가 아닙니다. 삭제는 Cleanup 정책(`format: p2`, proxy)으로 하며 `namePattern`은 `plugins:org.example.a`처럼 `디렉터리:id`에 맞춥니다. OSGi 버전은 모두 release로 봅니다. 역할 권한 대상으로 `format:p2`를 쓸 수 있고, 컴포넌트 메타데이터 탭은 캐시된 plugin의 MANIFEST.MF에서 Bundle-Name·Bundle-Vendor·Bundle-License·Require-Bundle을 보여 줍니다(feature·binary는 없음).

### Swift 설정

Swift 패키지 레지스트리(SE-0292, SwiftPM 5.7 이상의 `swift package-registry`)입니다. 릴리스 목록(`<scope>/<name>`), 릴리스 메타데이터(`<scope>/<name>/<버전>`), 매니페스트(`<scope>/<name>/<버전>/Package.swift`, `?swift-version=`), 소스 아카이브(`<scope>/<name>/<버전>.zip`), 저장소 URL 조회(`identifiers?url=`), 게시(`PUT <scope>/<name>/<버전>`), 로그인 확인(`POST login`)을 `/repository/<저장소>/` 아래에서 제공합니다. 릴리스 URL은 `baseURL`로 만든 절대 URL이므로 `baseURL`이 클라이언트가 여는 주소와 같아야 합니다.

```bash
swift package-registry set https://cc.example.com/repository/swift-all            # 기본 레지스트리(--global 이면 사용자 설정)
swift package-registry set --scope mona https://cc.example.com/repository/swift-all   # 특정 scope만 이 레지스트리로

# 로그인: 레지스트리 URL 뒤에 /login 을 붙여 줍니다. 인증 정보는 호스트 단위로 저장됩니다.
swift package-registry login https://cc.example.com/repository/swift-all/login --username ci --password <비밀번호>
swift package-registry login https://cc.example.com/repository/swift-all/login --token <API 토큰>

# 올리기: add 권한이 필요합니다. 패키지 루트의 package-metadata.json 이 있으면 metadata로 함께 보냅니다.
swift package-registry publish mona.LinkedList 1.1.1 --url https://cc.example.com/repository/swift-hosted
curl -u ci:<비밀번호> -H 'Content-Type: application/zip' -T LinkedList-1.1.1.zip https://cc.example.com/repository/swift-hosted/mona/LinkedList/1.1.1   # swift package archive-source 로 만든 zip
```

```swift
dependencies: [
    .package(id: "mona.LinkedList", from: "1.1.1"),
],
targets: [
    .target(name: "App", dependencies: [.product(name: "LinkedList", package: "mona.LinkedList")]),
]
```

- 위 `swift package-registry` 명령은 레지스트리 명세를 따라 적었습니다. 실제 SwiftPM 클라이언트로는 아직 검증하지 않았습니다.
- 패키지는 identifier `scope.name`으로 가리킵니다. scope는 영숫자와 하이픈(처음·끝·연속 금지, 39자 이하), name은 영숫자와 하이픈·밑줄(처음·끝·연속 금지, 100자 이하)이며 대소문자를 구분하지 않습니다. 버전은 SemVer 2.0.0(`v` 접두사 없음)이어야 하고 어긋나면 404입니다.
- SwiftPM은 레지스트리를 HTTPS로 엽니다. 운영에서는 `baseURL`을 `https://`로 두고 TLS를 끝내는 리버스 프록시 뒤에서 운영하세요. 시험용 http 저장소는 `swift package-registry set --allow-insecure-http`(SwiftPM 5.9 이상)로만 쓸 수 있습니다.
- hosted는 올린 zip에서 `Package.swift`와 `Package@swift-X.Y.swift`를 찾아 `.package(url:/id:/path: …)` 의존성(`from:`, `exact:`, `.upToNextMajor(…)` 등 요구 조건은 쓴 그대로)과 metadata(description, licenseURL, readmeURL, repositoryURLs)를 저장하고, 파일은 `<scope>/<name>/<버전>/source-archive.zip`에 둡니다. SwiftPM은 받은 아카이브의 최상위 디렉터리 한 단계를 벗겨 내므로 zip은 `swift package archive-source`처럼 하나의 최상위 디렉터리 아래(`<이름>/Package.swift`)에 패키지를 담아야 하며, 그렇지 않거나 zip이 아니면 400입니다. 업로드 본문은 `maxUploadMiB`를 따릅니다.
- 아카이브 응답에는 `Digest: sha-256=…`, 릴리스 메타데이터에는 checksum과 latest/successor/predecessor `Link`가 붙고, 서명(`source-archive-signature`)을 함께 올리면 `signing`으로 제공합니다. 다른 API 버전(`application/vnd.swift.registry.v2…`)을 요구하면 406, 오류는 모두 `application/problem+json`입니다.
- `writePolicy`: `allow_once`(기본)는 같은 버전 재게시를 409로 거부, `allow`는 덮어쓰기 허용(edit 권한 필요), `deny`는 읽기 전용(400)입니다.
- `identifiers?url=`은 metadata의 `repositoryURLs`로 패키지를 찾습니다(대소문자·`.git`·끝의 `/` 무시). 게시할 때 `repositoryURLs`를 넣어 두면 `swift package resolve --replace-scm-with-registry`가 URL 의존성을 이 레지스트리의 패키지로 바꿔 받습니다.
- proxy의 `remote`는 다른 Swift 레지스트리의 루트 URL입니다. 릴리스 목록·메타데이터·identifiers는 `metadataMaxAge`마다 다시 확인하고 목록의 릴리스 URL을 proxy 주소로 바꿔 제공합니다. 아카이브는 처음 받을 때 원격 릴리스 메타데이터의 checksum과 비교해 맞을 때만 캐시하고(다르면 502), 매니페스트는 캐시한 아카이브에서 읽습니다.
- group은 멤버 순서상 그 패키지를 처음 가진 멤버만으로 답합니다(hosted를 proxy보다 앞에 두세요). identifiers는 멤버 결과를 합칩니다. `writeMember`가 있으면 group에 게시한 패키지는 그 hosted로 갑니다.
- 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(`format: swift`, hosted·proxy)으로 합니다. 컴포넌트는 `scope.name@버전`(소문자)으로 나타나고, 역할 권한 대상으로 `format:swift`를 쓸 수 있습니다. 컴포넌트 메타데이터 탭은 description, licenseURL, readmeURL, 첫 repositoryURL과 `.package(...)` 의존성을 보여 줍니다.

### CocoaPods 설정

CocoaPods 트렁크 CDN(`https://cdn.cocoapods.org/`)의 proxy입니다(Nexus와 같이 proxy만 있고, hosted·group은 저장소 설정 검사에서 400으로 거부합니다). `pod install`이 CDN 소스에서 읽는 루트의 인덱스 파일(`CocoaPods-version.yml`, `all_pods.txt`, `all_pods_versions_<a>_<b>_<c>.txt`, `deprecated_podspecs.txt`)과 `Specs/<a>/<b>/<c>/<Pod>/<버전>/<Pod>.podspec.json`을 `/repository/<저장소>/` 아래에서 제공합니다.

```ruby
# Podfile
source 'http://localhost:8081/repository/pods-proxy/'
target 'App' do
  pod 'Alamofire', '~> 5.0'
end
```

```bash
pod install --repo-update
# 읽기에 인증이 필요하면 ~/.netrc 에: machine localhost login ci password <비밀번호 또는 API 토큰>
```

- proxy의 `remote`는 트렁크 CDN `https://cdn.cocoapods.org/`입니다. **CDN 소스만 지원하며** 옛 Specs git 저장소(`https://github.com/CocoaPods/Specs.git`)는 remote로 쓸 수 없습니다. 인덱스 파일은 `metadataMaxAge`마다 다시 확인하고, podspec은 버전마다 바뀌지 않으므로 처음 받을 때 캐시합니다(`contentMaxAge`, 기본 영구). 샤드 디렉터리(Pod 이름 md5의 앞 세 글자)가 맞지 않는 경로는 404입니다. 원격이 응답하지 않으면 캐시된 인덱스를 `Warning: 110`과 함께 제공합니다.
- podspec의 `source.http`는 `<baseURL>/repository/<저장소>/pods/<Pod>/<버전>/<원본 URL의 sha1>.<확장자>`로 바꿔 내보냅니다. 그 주소를 처음 요청하면 원본 URL(GitHub 릴리스 등)에서 받아 podspec 옆에 영구 캐시하고, podspec에 `sha256`이 있으면 검증합니다(불일치는 502이며 캐시하지 않습니다). 확장자는 원본 URL의 것(`.zip`, `.tar.gz`, `.tgz` …)을 유지하므로 CocoaPods가 압축 형식을 그대로 알아봅니다. 바꾼 URL은 절대 주소이므로 `baseURL`이 클라이언트가 여는 주소와 같아야 합니다.
- **git 소스는 proxy하지 않습니다.** `source.git`(과 svn·hg)은 손대지 않고 그대로 두므로, 그런 Pod는 클라이언트가 원본 git 서버(GitHub 등)에 직접 접속할 수 있어야 합니다. 폐쇄망에서는 http 소스 Pod만 받을 수 있으니 git 소스 Pod는 내부 git 미러를 따로 두세요.
- 컴포넌트는 Pod 이름과 버전(`Alamofire@5.0.0`)이며 podspec과 그 아카이브가 한 컴포넌트입니다. 삭제는 Cleanup 정책(`format: cocoapods`)으로 하고, 인덱스 파일은 컴포넌트가 아니라 지우지 않습니다. 지운 Pod는 다시 요청하면 새로 받습니다. 역할 권한 대상으로 `format:cocoapods`를 쓸 수 있고, 컴포넌트 메타데이터 탭은 summary(없으면 description), license, homepage, 원본 소스 URL, dependencies를 보여 줍니다.

### HuggingFace 설정

Hugging Face Hub(`https://huggingface.co/`)의 proxy입니다(Nexus와 같이 proxy만 있고, hosted·group은 400으로 거부합니다). `huggingface_hub`, `transformers`, `datasets`, `hf download`가 `HF_ENDPOINT`로 이 저장소를 Hub처럼 씁니다.

```bash
export HF_ENDPOINT=http://localhost:8081/repository/hf-proxy   # 끝에 / 없이
export HF_HUB_DOWNLOAD_TIMEOUT=600   # 큰 파일: proxy가 원본에서 다 받을 때까지 기다립니다(아래 참고)
export HF_HUB_ETAG_TIMEOUT=60
hf download bert-base-uncased        # 또는 huggingface-cli download
python -c "from transformers import AutoModel; AutoModel.from_pretrained('bert-base-uncased')"
# 읽기에 인증이 필요하면 HF_TOKEN에 Command Center API 토큰을 넣습니다(Hub 토큰이 아닙니다)
export HF_TOKEN=<Command Center API 토큰>
```

- `hf auth login`으로 저장해 둔 Hub 토큰이 있으면 클라이언트가 그 토큰을 proxy로 보내고 proxy는 401로 답합니다. `HF_TOKEN`을 Command Center 토큰으로 두거나 `HF_HUB_DISABLE_IMPLICIT_TOKEN=1`을 설정하세요.
- `api/models|datasets|spaces/<repo id>`(`/revision/<리비전>`, `/tree/…` 포함) 문서는 그대로 전달하며 `metadataMaxAge` 동안 캐시합니다(짧게, 예: `10m`). 쿼리(`expand[]`, `blobs=true`, `recursive`)가 다르면 따로 캐시합니다. 그 밖의 API(`api/whoami-v2`, 목록·검색)는 404입니다.
- 파일(`<repo id>/resolve/<리비전>/<경로>`, `datasets/<repo id>/resolve/…`, `spaces/…`)은 Hub가 주는 CDN 리다이렉트를 따라가 받아 **커밋 sha 아래 영구 캐시**합니다(`contentMaxAge`). 브랜치·태그는 revision 문서로 커밋을 찾고 그 문서는 `metadataMaxAge`마다 다시 확인하므로 브랜치가 움직이면 새 커밋의 파일을 받으며, 이전 커밋 파일은 그대로 남습니다. 응답에는 `X-Repo-Commit`, `ETag`·`X-Linked-Etag`(파일 내용의 sha256, Hub의 LFS 값과 같음), `X-Linked-Size`, `Content-Length`가 붙고 Range(이어받기)를 지원합니다. 캐시에 없는 LFS 파일의 HEAD는 Hub의 헤더로 답하고 내려받지 않습니다.
- **토큰**: gated·비공개 모델은 저장소의 `remoteAuth.bearerToken`에 Hub 읽기 토큰을 넣습니다. gated 모델은 그 토큰 계정이 Hub에서 접근 승인을 받아 두어야 합니다. 토큰은 `remote` 호스트에만 보내고 리다이렉트된 CDN에는 보내지 않으며, 클라이언트의 Authorization은 Hub로 전달하지 않습니다. 토큰 계정이 볼 수 있는 모델은 이 저장소를 읽을 수 있는 모든 사용자에게 보이므로 읽기 권한을 제한하세요.
- **큰 파일**: proxy는 파일을 원본에서 끝까지 받아 캐시한 뒤 보냅니다. 받는 시간이 클라이언트의 대기 시간(기본 `HF_HUB_DOWNLOAD_TIMEOUT` 10초, 재시도 5회)보다 길면 클라이언트는 실패하지만 proxy의 다운로드는 계속되므로 다시 실행하면 캐시에서 받습니다. 수 GB 모델은 `HF_HUB_DOWNLOAD_TIMEOUT`을 크게 잡거나 미리 한 번 받아 두세요. 한 파일은 30분 안에 원본에서 받아져야 하며, 디스크는 모델 크기만큼 필요합니다.
- 지원하지 않는 것: `/`가 든 리비전(`refs/pr/1`), `POST …/paths-info`(405), Xet 전송(클라이언트가 HTTP로 받습니다), 업로드.
- **정리**: 컴포넌트는 `<repo id>@<커밋 sha>`이고 데이터셋·스페이스는 그룹이 `datasets`·`spaces`입니다(라벨 `datasets/org/ds@<sha>`, Cleanup `namePattern`은 `datasets:org/ds`). 한 커밋의 파일(하위 디렉터리 포함)이 한 컴포넌트입니다. Cleanup 정책(`format: huggingface`, 예: `lastDownloadedDays`)으로 오래 쓰지 않은 모델을 지우며 api 문서는 남습니다. 역할 권한 대상으로 `format:huggingface`를 쓸 수 있고, 메타데이터 탭은 `pipeline_tag (library_name)`, license, Hub 페이지 주소를 보여 줍니다.

### Terraform 설정

Command Center는 Terraform/OpenTofu 레지스트리 프로토콜(modules.v1, providers.v1)을 `terraform` 형식 저장소로 제공합니다. hosted는 모듈 아카이브와 프로바이더 릴리스를 업로드받고, proxy는 registry.terraform.io를 캐시하며, group은 모듈·프로바이더마다 그것을 가진 첫 멤버가 답합니다. 아래 명령은 레지스트리 프로토콜을 재현한 테스트로만 확인했고, 실제 `terraform`·`tofu` 클라이언트로는 아직 검증하지 않았습니다.

**HTTPS가 필요합니다.** Terraform은 레지스트리를 호스트 이름으로만 찾으며 항상 `https://<호스트>/.well-known/terraform.json`을 읽습니다. 따라서 Command Center를 클라이언트가 신뢰하는 인증서로 HTTPS(기본 443 포트, 다른 포트라면 소스 주소에 `호스트:포트`)로 노출해야 하고, 일반 설정 `baseURL`도 그 https 주소로 맞춰야 합니다. HTTP로는 동작하지 않습니다.

**루트 디스커버리는 기본 저장소를 가리킵니다.** 호스트 루트의 `/.well-known/terraform.json`은 terraform 저장소 하나만 가리킵니다. 일반 설정 `terraformDefaultRepository`에 지정한 저장소이며, 지정하지 않았으면 이름순으로 첫 terraform 저장소입니다. 한 호스트에는 저장소 하나만 노출되므로 hosted와 proxy를 묶은 group을 기본 저장소로 두는 것을 권장합니다. 저장소마다 `/repository/<repo>/.well-known/terraform.json`도 응답하지만 Terraform은 루트만 읽습니다. 디스커버리 문서는 인증 없이 공개됩니다.

**인증(~/.terraformrc).** API 토큰을 Bearer로 보냅니다.

```hcl
credentials "cc.example.com" {
  token = "<API 토큰>"
}
```

환경 변수 `TF_TOKEN_cc_example_com`(점은 `_`)도 같습니다. `terraform login`은 지원하지 않습니다. 저장소가 익명 읽기(anonymousRead)를 허용하면 토큰 없이 읽을 수 있습니다.

주의: Terraform은 모듈 아카이브와 프로바이더 zip을 받을 때 위 토큰을 보내지 않고 `~/.netrc`(`machine cc.example.com login <사용자> password <API 토큰>`)만 사용하며, 프로바이더의 SHA256SUMS와 서명 파일은 자격 증명 없이 받습니다. 그래서 익명 읽기를 끈 저장소에서 모듈을 쓰려면 `~/.netrc`도 설정해야 하고, 프로바이더를 쓰려면 그 저장소(또는 group)에 `anonymousRead: true`가 필요합니다.

**사용 예.**

```hcl
module "vpc" {
  source  = "cc.example.com/acme/vpc/aws"   # 호스트/네임스페이스/이름/프로바이더
  version = "1.2.0"
}

terraform {
  required_providers {
    demo = {
      source  = "cc.example.com/acme/demo"  # 호스트/네임스페이스/타입
      version = "2.0.0"
    }
  }
}
```

**모듈 업로드.** *.tf 파일이 루트(또는 최상위 디렉터리 하나)에 있는 tar.gz 또는 zip을 올리며, add 권한이 필요합니다.

```sh
tar -czf vpc-1.2.0.tgz -C vpc .
curl -fsS -X PUT -H "Authorization: Bearer $TOKEN" --data-binary @vpc-1.2.0.tgz \
  https://cc.example.com/repository/terraform-hosted/v1/modules/acme/vpc/aws/1.2.0
# 경로 형식(확장자가 형식을 정합니다): …/repository/terraform-hosted/modules/acme/vpc/aws/1.2.0.tgz
```

삭제는 같은 v1 경로에 `DELETE`를 보냅니다(delete 권한).

**프로바이더 업로드.** 플랫폼별 zip, SHA256SUMS, 그 서명, (선택) 매니페스트를 버전 경로 아래로 올립니다. 저장 이름이 `terraform-provider-<type>_<version>_<os>_<arch>.zip`이므로 SHA256SUMS에 적힌 파일 이름도 이와 같아야 합니다(goreleaser 기본 출력 그대로입니다).

```sh
B=https://cc.example.com/repository/terraform-hosted/v1/providers/acme/demo/2.0.0
H="Authorization: Bearer $TOKEN"
curl -fsS -X PUT -H "$H" --data-binary @terraform-provider-demo_2.0.0_linux_amd64.zip  $B/linux/amd64
curl -fsS -X PUT -H "$H" --data-binary @terraform-provider-demo_2.0.0_darwin_arm64.zip $B/darwin/arm64
curl -fsS -X PUT -H "$H" --data-binary @terraform-provider-demo_2.0.0_SHA256SUMS      $B/shasums
curl -fsS -X PUT -H "$H" --data-binary @terraform-provider-demo_2.0.0_SHA256SUMS.sig  $B/shasums.sig
curl -fsS -X PUT -H "$H" --data-binary @terraform-registry-manifest.json            $B/manifest
```

서명은 `gpg --detach-sign terraform-provider-demo_2.0.0_SHA256SUMS`로 만들고, 공개 키(`gpg --armor --export <키>`)를 저장소 설정 `gpgPublicKey`에 넣습니다. 다운로드 문서가 이 키(key_id, ascii_armor)를 알려 주며, 키가 없으면 Terraform이 프로바이더를 거부합니다. 매니페스트가 없으면 프로토콜은 5.0입니다. 삭제는 `curl -X DELETE -H "$H" $B`입니다.

**writePolicy.** allow_once는 같은 파일의 재업로드는 그대로 받고 내용이 다르면 409로 거부합니다. allow는 덮어쓰기에 edit 권한이 필요하고, deny는 읽기 전용입니다.

**registry.terraform.io 프록시.** remote를 `https://registry.terraform.io/`로 둔 proxy는 원격 디스커버리를 읽고 버전 목록을 metadataMaxAge 동안 캐시합니다. 모듈의 http(s) 아카이브 주소는 프록시 경로로 바꾸어 요청이 올 때 받아 캐시합니다. 프로바이더의 download_url, shasums_url, shasums_signature_url도 프록시 경로로 바꾸어 요청이 올 때 받으며, zip은 shasum으로 검증합니다. 원격 자격 증명은 레지스트리 호스트에만 보내고 releases.hashicorp.com이나 GitHub에는 보내지 않습니다. 원격이 내려가도 이미 캐시된 모듈과 프로바이더는 설치됩니다.

**git:: 소스의 한계.** 업스트림이 `git::`처럼 http(s) 아카이브가 아닌 소스를 주면(registry.terraform.io 공개 모듈 대부분이 GitHub `git::`입니다) 그대로 전달하므로 캐시되지 않으며, 클라이언트가 그 Git 호스트에 직접 접근할 수 있어야 합니다. 폐쇄망에서는 그런 모듈을 아카이브로 받아 hosted에 올려 사용합니다.

**정리.** 모듈 컴포넌트는 `<네임스페이스>/<이름>/<프로바이더>@<버전>`, 프로바이더는 `<네임스페이스>/<타입>@<버전>`입니다. 삭제는 웹 UI·API의 컴포넌트 삭제와 Cleanup 정책(`format: terraform`)으로 하고, 역할 권한 대상으로 `format:terraform`을 쓸 수 있습니다. 컴포넌트 메타데이터 탭은 모듈 README의 첫 문단, 프로바이더의 프로토콜과 플랫폼을 보여 줍니다(hosted만).

## 운영

| 작업 | 명령 |
|---|---|
| 로그 | `docker compose logs -f` |
| 메모리 확인 | `docker stats commandcenter` |
| 사용하지 않는 blob 정리 | `docker compose exec commandcenter commandcenter gc` |
| API 토큰 발급 | `docker compose exec commandcenter commandcenter token create -user ci` |
| 비밀번호 해시(첫 가져오기용) | `docker run --rm -i ghcr.io/sungjinseo/commandcenter:1.2.3 hash-password` |
| 관리자 비상 복구 | `docker compose exec -T commandcenter commandcenter user reset-password -user admin -admin`(서버가 멈춰 있으면 `docker compose run --rm -T commandcenter user reset-password -user admin -admin`) |
| 비밀값 키 교체 | 서버를 멈추고 `docker compose run --rm -v "${PWD}/new.key:/run/new.key:ro" commandcenter secret-key rotate -new-key-file /run/new.key`, 그다음 키 설정을 새 키로 바꿔 `docker compose up -d`("저장된 비밀값 암호화"의 키 교체 참고) |
| 버전 확인 | `docker compose exec commandcenter commandcenter version` (릴리스 이미지·바이너리는 태그 이름(예: `v1.2.3`), 직접 빌드한 이미지는 빌드 인자 `VERSION`의 값, 지정하지 않았으면 `dev`) |

Kubernetes에서는 `docker compose exec commandcenter` 대신 `kubectl -n <네임스페이스> exec commandcenter-0 --`를 씁니다("Kubernetes"의 "동작 방식" 참고).

### 로그

표준 출력에 JSON 한 줄씩 기록합니다. 요청마다 `id`, `method`, `path`, `status`, `bytes`, `ms`, `user`, `remote` 필드가 있습니다.
클라이언트가 보낸 `X-Request-Id`는 1–64자의 `[A-Za-z0-9._-]`일 때만 쓰고, 아니면 서버가 새로 만듭니다. 500 오류 메시지에 나오는 request id로 로그를 검색할 수 있습니다.

### 로그 레벨

로그 레벨은 실행 중에 바꿀 수 있습니다. `GET /-/api/logging`(관리자)은 현재 레벨을 `{"level":"info","persistent":false}`로 알려 주고, `PUT /-/api/logging`에 `{"level":"debug"}`(`debug`, `info`, `warn`, `error`)를 보내면(관리자, 비밀번호·세션 로그인, `X-Command-Center-UI: 1` 헤더) 바로 적용됩니다. 노드가 여럿이면 모든 노드에 함께 적용됩니다. 관리자 화면의 로그 페이지에서도 고를 수 있습니다. 바뀐 레벨은 저장되지 않아 서버를 다시 시작하면 `info`로 돌아오고, 활동 이벤트 `logging.level`이 남습니다. `debug`는 로그 양이 크게 늘어나므로 원인을 찾은 뒤 되돌리는 것이 좋습니다.

### 메트릭

`GET /-/metrics`는 `admin` 사용자만 호출할 수 있습니다(JSON).

```bash
curl -u admin http://localhost:8081/-/metrics
```

필드: `heapAllocBytes`, `heapSysBytes`, `sysBytes`, `numGC`, `goroutines`, `rssBytes`(Linux에서만 값이 있음).

`GET /-/metrics/prometheus`는 같은 정보를 Prometheus 텍스트 형식(0.0.4)으로 내보냅니다. 기본으로 `admin`만 호출할 수 있고, 일반 설정의 `metricsAnonymous`를 `true`로 두면 인증 없이도 읽을 수 있습니다(수집기가 자격 증명을 보낼 수 없을 때만 켜고, 리버스 프록시에서 경로를 막아 두는 편이 안전합니다).

| 메트릭 | 설명 |
|---|---|
| `commandcenter_build_info{version,goversion}` | 항상 1 |
| `commandcenter_uptime_seconds`, `go_goroutines`, `go_memstats_heap_alloc_bytes`, `go_memstats_sys_bytes`, `process_resident_memory_bytes`, `go_gc_cycles_total` | 프로세스·Go 런타임 |
| `commandcenter_requests_total{repository}`, `commandcenter_request_errors_total`(5xx·429), `commandcenter_request_bytes_total{repository,direction="in\|out"}`, `commandcenter_request_duration_ms_sum/_count` | 저장소별 요청 카운터. 프로세스가 시작한 뒤의 누적값이며(재시작하면 0부터) `rate()`로 쓰면 됩니다 |
| `commandcenter_upstream_requests_total`, `commandcenter_upstream_failures_total`, `commandcenter_cache_hits_total`, `commandcenter_cache_misses_total` | proxy 저장소의 원격 가져오기와 캐시 조회 |
| `commandcenter_repository_assets{repository}`, `commandcenter_repository_bytes` | 저장소의 asset 수와 크기(30초 캐시) |
| `commandcenter_blobstore_blobs{store}`, `commandcenter_blobstore_bytes` | blob store별 blob 수와 크기(30초 캐시) |
| `commandcenter_tasks_last_result{task,type}` | 마지막 실행이 성공이면 1, 실패면 0(한 번도 안 돌았으면 없음) |
| `commandcenter_health_check{name}` | 상태 점검(Database, Blob store, Memory, Remote fetch slots, Repositories) 통과 1, 실패 0 |

Prometheus 수집 설정 예:

```yaml
scrape_configs:
  - job_name: commandcenter
    metrics_path: /-/metrics/prometheus
    scheme: https
    basic_auth:
      username: metrics-admin     # admin 역할의 계정(API 토큰도 비밀번호 자리에 넣을 수 있음)
      password_file: /etc/prometheus/commandcenter.pw
    static_configs:
      - targets: ["commandcenter.example.com:443"]
```

### 지원 번들과 스레드 덤프

문제를 보고할 때 필요한 정보를 한 번에 모읍니다. 관리자 화면의 지원(Support) 페이지에서 내려받거나 API로 호출합니다.

- `GET /-/api/support/zip`(관리자, 비밀번호·세션 로그인): `commandcenter-support-<호스트>-<시각>.zip`을 내려줍니다. 안에는 `info.json`(버전·Go 버전·OS·가동 시간·PID), `config.json`(적용 중인 일반·SMTP·Realm·LDAP·OIDC·SSL 설정), `repositories.json`, `blobstores.json`, `status.json`(상태 점검), `sysinfo.json`, `tasks.json`, `webhooks.json`, `request-stats.json`(최근 24시간 저장소별 통계), `events.json`(최근 활동 500건), `logs.txt`(메모리에 남아 있는 로그), `threads.txt`(스레드 덤프), `metrics.txt`(Prometheus 텍스트)가 들어 있습니다.
- 비밀값은 들어가지 않습니다. 비밀번호·클라이언트 시크릿·S3 secret key·웹훅 시크릿은 API 화면과 같이 "설정됨" 여부(`passwordSet` 등)로만 나타나고, 원격 URL의 자격 증명도 지워집니다. 그래도 외부에 보내기 전에 `config.json`의 호스트 이름 등 공개하고 싶지 않은 값이 있는지 한 번 확인하는 것이 좋습니다.
- 크기는 제한되어 있습니다(활동 500건, 로그 링 1 MiB, 스레드 덤프 8 MiB). 내려받으면 활동 이벤트 `support.zip`이 남습니다.
- `GET /-/api/support/threads`(관리자): 모든 goroutine의 스택을 텍스트로 돌려줍니다(최대 8 MiB). 요청이 멈춘 것 같을 때 어디서 기다리는지 보는 용도입니다.

```bash
curl -u admin -o support.zip https://commandcenter.example.com/-/api/support/zip
curl -u admin https://commandcenter.example.com/-/api/support/threads | head
```

### OpenAPI 문서

`GET /-/api/openapi.json`은 Command Center API(`/-/api/*`)와 Nexus REST API 호환 계층(`/service/rest/v1/*`)의 모든 경로를 OpenAPI 3.0 문서로 돌려줍니다. 인증 없이 받을 수 있으며, Nexus가 Swagger 2.0 문서를 두는 `GET /service/rest/swagger.json`도 같은 문서를 돌려줍니다. 경로마다 메서드, 경로·쿼리 파라미터, 필요한 권한, 응답 코드를 적었지만 본문 스키마는 이름만 있는 자유 형식 객체이므로 필드는 이 가이드를 보세요. Swagger UI, Postman, `openapi-generator` 같은 도구에 이 URL을 넣으면 경로 목록을 볼 수 있습니다.

```bash
curl -s http://localhost:8081/-/api/openapi.json | jq '.paths | keys | length'
```

### Cleanup 정책

오래되었거나 더 이상 내려받지 않는 컴포넌트를 자동으로 지우는 규칙입니다. 메뉴는 OPERATIONS → Cleanup 정책입니다. 정책은 이름, 대상 포맷과 아래 기준으로 이루어지며(정책의 `format`은 `all`, `maven`, `npm`, `go`, `pypi`, `cargo`, `raw`, `helm`, `yum`, `nuget`, `apt`, `rubygems`, `composer`, `conda`, `conan`, `r`, `gitlfs`, `p2`, `swift`, `cocoapods`, `huggingface`, `terraform` 중 하나. docker 저장소에는 Cleanup 정책을 쓸 수 없습니다), 저장소 폼의 **Cleanup policy** 선택(저장소 정의의 `cleanupPolicy`)으로 hosted·proxy 저장소에 연결합니다. 정책의 포맷은 `all`이거나 저장소의 포맷과 같아야 하고, 저장소가 쓰는 정책은 지울 수 없습니다(409, 저장소 이름을 알려 줌).

| 기준 | 의미 |
|---|---|
| `componentAgeDays` | 컴포넌트의 **가장 새로운 에셋**이 N일보다 오래전에 만들어졌다. 0이면 사용 안 함. |
| `lastDownloadedDays` | 컴포넌트의 가장 최근 다운로드가 N일보다 오래되었다. 한 번도 받지 않았으면 생성 시각을 씁니다. 0이면 사용 안 함. |
| `releaseType` | `any`, `release`, `prerelease`(SNAPSHOT 또는 semver 프리릴리스). |
| `namePattern` | `group:name`에 맞추는 RE2 정규식(선택, 200자 이하). |

- 두 기간과 `retain`(아래) 중 **하나 이상**은 정해야 하며, 정한 기준은 모두 **AND**로 결합합니다. 모든 기준을 만족하는 컴포넌트만 후보입니다.
- **미리보기**(정책 화면의 "후보 목록 보기")는 후보 개수·용량과 최대 500개의 목록을 보여 줍니다. 저장소마다 에셋을 20 000개까지만 훑으므로 목록이 잘렸거나 저장소가 그보다 크면 `truncated`가 켜지고, 이때 개수와 용량은 **하한**입니다.
- **Cargo hosted 저장소는 제외**됩니다. `Cargo.lock`에 고정된 crate를 자동으로 지우지 않기 위해서입니다. yank로 숨기거나, 지울 버전은 웹 UI·API로 하나씩 지우세요("컴포넌트 삭제" 참고). 정책은 Cargo hosted를 건너뛰고 UI에도 안내가 나옵니다. Cargo proxy는 대상입니다.
- hosted의 `writePolicy`는 정리에서 **무시**됩니다. `deny`인 저장소에서도 정리는 지웁니다.
- 정책은 저장만으로 아무것도 지우지 않습니다. **`repository.cleanup` 예약 작업**이 실행할 때만 지우며(다음 절), 한 번에 컴포넌트 5 000개까지, 개수와 용량을 작업 로그와 활동에 기록합니다. proxy는 캐시된 행만 지우고 다음 요청 때 다시 받아 옵니다.
- 작업 실행도 저장소마다 **한 번에 에셋 20 000개**까지만 훑습니다. 큰 저장소는 작업·저장소별 **스캔 위치**(settings 테이블)를 기억해 다음 실행이 마지막으로 훑은 컴포넌트 다음부터 이어 가고, 끝에 닿으면 처음으로 돌아갑니다. 그래서 여러 번 실행하면 저장소 전체를 차례로 평가합니다. 작업 로그에 어디서부터 몇 개를 훑었고 다음 실행이 어디서 시작하는지가 나옵니다. 작업을 지우면 그 위치도 지웁니다.
- 평가 직전에 메모리에 모인 다운로드 수를 먼저 DB에 반영하므로 방금 내려받은 컴포넌트도 보호됩니다.
- **평가한 것만 지웁니다.** 삭제는 평가 때 본 에셋 목록, 가장 새로운 생성·수정 시각, 마지막 다운로드 시각을 삭제 트랜잭션 안에서 다시 확인하고, 그 뒤로 에셋이 더해졌거나(예: 새 SNAPSHOT 빌드) 다시 올라왔거나 내려받아졌으면 그 컴포넌트는 지우지 않고 "평가 후 변경"으로 로그에 남깁니다(실패로 치지 않음).
- 실행 중에도 편집을 따릅니다. 컴포넌트마다 저장소 정의를 다시 보고(메모리), 정책은 50개마다 또는 정책이 수정될 때 다시 읽어, 저장소가 바뀌었거나 정책이 해제·변경되었으면 그 저장소의 삭제를 멈춥니다.
- 지운 에셋의 **blob은 바로 반환되지 않습니다.** 다음 `blobstore.compact` 작업이 회수합니다.
- 설정 PUT에서 `cleanupPolicy`를 빼면 기존 값이 유지됩니다.

#### 버전 유지(retain), 제외, 미리보기

- **`retain`**(1–1000, 0이면 사용 안 함)은 컴포넌트(같은 저장소의 같은 group과 name)마다 가장 새로운 N개 버전을 다른 기준과 상관없이 남깁니다. 그보다 오래된 버전만 나머지 기준으로 평가합니다. `retain`만 정한 정책도 만들 수 있습니다(가장 새로운 N개를 빼고 모두 지움).
- **`retainSortBy`**는 `version`(기본값)이나 `date`입니다. `version`은 포맷의 버전 규칙으로 정렬합니다. Maven은 ComparableVersion 규칙(1.0-rc1 < 1.0-SNAPSHOT < 1.0 < 1.0-sp1), PyPI는 PEP 440(1.0.dev1 < 1.0a1 < 1.0 < 1.0.post1), yum은 rpm 비교(epoch:version-release, 아키텍처 무시), apt는 Debian 규칙(1.0~rc1 < 1.0), RubyGems는 Gem::Version, R은 숫자 비교, Composer는 dev < alpha < beta < RC < 안정판 < patch, conda는 VersionOrder와 빌드 번호, npm·Cargo·Helm·Go·NuGet·Conan은 SemVer입니다. `date`는 컴포넌트의 가장 새로운 에셋이 만들어진 시각입니다. docker·raw·gitlfs는 버전 순서가 없어 날짜로 정렬하며, raw와 gitlfs는 컴포넌트마다 버전이 하나라 retain이 있으면 아무것도 지우지 않습니다.
- 순위는 제외된 버전과 다른 기준에 맞지 않는 버전까지 모든 버전으로 매깁니다. 같다고 비교되는 버전(같은 EVR의 여러 아키텍처, 같은 gem의 플랫폼별 파일)은 같은 순위로 함께 남습니다.
- 한 컴포넌트의 버전은 한 번의 스캔에서 모두 보아야 순위를 매깁니다. 스캔 예산에 잘린 컴포넌트는 그 실행에서 건너뛰고, 다음 실행이 그 컴포넌트 처음부터 다시 훑습니다. 한 컴포넌트의 에셋이 스캔 예산(20 000개)보다 많으면 retain으로는 정리되지 않습니다. apt는 패키지가 pool의 여러 곳에 있을 수 있어 pool 전체가 한 번의 스캔에 들어올 때만 retain을 적용합니다.
- **`exclude`**의 `namePattern`(`group:name`)과 `versionPattern`(버전)은 문자열 전체에 맞추는 RE2 정규식(각 200자 이하)이며, 맞는 컴포넌트는 절대 지우지 않습니다.
- **`pathPattern`**은 컴포넌트의 에셋 경로 중 하나라도 전체가 맞아야 하는 정규식입니다(Nexus의 `criteriaAssetRegex`, 앞의 `/`는 있어도 됩니다). **`notes`**(1000자 이하)는 정책 설명입니다.
- 정책 PUT은 모든 필드를 바꾸므로 빠진 필드는 지워집니다.
- **저장 전 미리보기**: `POST /-/api/cleanup/preview`(관리자, 본문은 정책과 `"repository"`)는 저장하지 않은 정책을 한 저장소에 적용해 봅니다. 정리 작업과 같은 평가 코드를 쓰고 저장소 끝까지 훑으므로, 바로 작업을 실행하면 미리보기 목록이 그대로 지워집니다(그 사이 바뀐 컴포넌트는 남김). 1000개까지의 목록과 전체 개수·용량, 컴포넌트마다 선택된 이유를 돌려주고, `?format=csv`는 모든 후보를 CSV로 내려받습니다. 미리보기는 아무것도 지우지 않습니다.

### 예약 작업

OPERATIONS → 예약 작업(Tasks)에서 만들고 편집하고 지웁니다. 작업은 한 번에 하나씩 실행하며, 실행 중에 시간이 된 다른 작업은 다음 확인(30초마다) 때 실행됩니다. 노드가 여럿이면 모든 노드를 통틀어 하나씩 실행하고, 예정된 실행은 리더 노드만 합니다("노드와 준비 상태" 참고).

| 종류 | 설명 | 파라미터 |
|---|---|---|
| `blobstore.compact` | 쓰이지 않는 blob과 1시간 넘은 임시 파일 정리(`commandcenter gc`와 같음) | `blobStore`(선택, 한 store만. 비우면 모든 store. 없는 이름은 만들 때 400) |
| `repository.cleanup` | 정책이 연결된 저장소를 정리 | `repos`(비우면 정책이 있는 모든 저장소) |
| `repository.rebuild-metadata` | Maven hosted: 모든 GA와 SNAPSHOT 버전의 `maven-metadata.xml`을 에셋에서 다시 만들고 아티팩트가 없는 GA의 문서에서 버전을 지움(`<plugins>`는 남김). Maven proxy: 이전 버전이 캐시한, Maven 파일이 아닌 경로의 캐시(디렉터리 목록, `.index/`)를 지움. apt·yum·helm·conda hosted: 패키지 행에서 `dists/`, `repodata/`(depth 1은 디렉터리마다), `index.yaml`, `repodata.json`·`channeldata.json`을 업로드 때와 같은 방식으로 다시 씀 | `repo`(비우면 다시 만들 수 있는 모든 저장소), `format`(비운 `repo`와 함께 한 형식만, 예: `helm`) |
| `repository.verify-blobs` | 저장소의 모든 에셋에 대해 blob이 blob store에 있는지(크기 포함) 확인하고, `checksum`이면 blob을 다시 읽어 SHA-256을 비교. 빠지거나 다른 blob이 하나라도 있으면 결과가 **Failed**이며 로그에 경로를 100개까지 남김. 아무것도 지우지 않음 | `repository`(비우면 모든 저장소), `checksum`(기본 꺼짐), `maxBytes`(checksum 때 한 번에 읽는 바이트, 기본 10 GiB. 넘치면 멈추고 다음 실행이 저장소별 커서에서 이어 감) |
| `maven.purge-snapshots` | Maven hosted 저장소의 SNAPSHOT 버전을 GA마다 최신 `minimumRetained`개만 남기고, 나머지 중 `snapshotRetentionDays`보다 오래된(0이면 나이 무관) 버전을 디렉터리째 지움. `lastDownloadedDays`를 주면 그 기간 동안 내려받지 않은(한 번도 없으면 저장된 지 그만큼 지난) 버전만 지움. `removeIfReleased`면 같은 GA에 정식 `X`가 있는 `X-SNAPSHOT`은 남길 개수와 무관하게, 정식 버전이 `gracePeriodDays`보다 오래됐을 때 지움. 삭제는 컴포넌트 삭제 경로를 지나므로 `maven-metadata.xml`이 다시 만들어지고 `delete` 활동이 남으며, 평가 뒤 바뀐 버전은 건너뜀. `writePolicy: deny` 저장소는 만들 때 400, 실행 때 Failed | `repository`(Maven hosted, 필수), `minimumRetained`(기본 1), `snapshotRetentionDays`(기본 30), `lastDownloadedDays`(기본 0), `removeIfReleased`, `gracePeriodDays`, `preview`(지울 것을 로그에만 남김) |
| `blobstore.recalculate-size` | blob store의 blob 수를 세어 에셋이 참조하는 수·바이트와 비교해 로그에 남김. 화면의 용량은 에셋 행에서 실시간으로 계산하므로 저장할 값은 없고, 다음 compaction이 지울 blob 수를 미리 봄 | `blobStore`(선택) |
| `blobstore.delete-temp-files` | 각 store `tmp/`의 1시간 넘은 임시 파일(`up-*`)과 24시간 넘게 쓰지 않은 Docker 업로드 세션(`ses-*`)을 지움(compact가 시작할 때 하는 정리와 같음). S3 store의 multipart 잔여물은 다루지 않음 | `blobStore`(선택) |
| `blobstore.group-remove-member` | 그룹 멤버를 읽기 전용으로 바꾸고 그 멤버에만 있는 blob을 다른 쓰기 가능한 멤버로 복사(SHA-256 확인)한 뒤 그룹에서 뺌. 그동안 읽기는 계속되고, 실패하면 멤버는 읽기 전용으로 남아 다시 실행하면 이어 감("Blob store 그룹과 S3 장애 조치" 참고) | `group`, `member`(필수. 다른 쓰기 가능한 멤버가 있어야 함) |
| `repository.change-blobstore` | 저장소의 blob을 다른 blob store로 옮기고 설정을 바꿈("저장소의 blob store 바꾸기" 참고). 보통 `POST /-/api/repositories/<이름>/blobstore`가 만들고 실행함 | `repo`, `target`(필수) |
| `repository.export` | 저장소를 `exportRoot` 아래로 내보냄("저장소 내보내기와 가져오기" 참고) | `repositories`(목록 또는 `*`), `destination`(필수), `includeProxies`(기본 꺼짐), `since`(RFC 3339, 증분), `hardLinks`(기본 켬) |
| `repository.import` | 내보낸 디렉터리를 가져옴 | `source`(필수, `<destination>/<내보내기 ID>`), `repositories`(내보낸 이름 → 대상 이름, 비우면 같은 이름), `createMissing`, `overwrite` |
| `vulnerability.sync` | OSV 덤프(`<dumpURL>/<생태계>/all.zip`)를 내려받아 로컬 취약점 DB에 저장("취약점 데이터와 방화벽(OSV)" 참고) | 없음 |
| `vulnerability.scan` | hosted·proxy 저장소의 컴포넌트를 검사해 결과를 저장 | `repository`(선택. 비우면 대상 포맷의 모든 hosted·proxy 저장소) |
| `security.purge-expired-user-tokens` | 만료된 사용자 토큰을 지움. `expirationDays`가 0이면 아무것도 지우지 않음("사용자 토큰" 참고) | 없음 |

작업 목록 API(`GET /-/api/tasks`)의 `types[]`에는 종류마다 `params`(이름·형식·기본값)가 함께 옵니다. Nexus 호환 API(`GET /service/rest/v1/tasks`)는 `type`을 Nexus 이름으로 바꿔 줍니다(`repository.maven.rebuild-metadata`, `repository.maven.remove-snapshots`, `blobstore.delete-temp-files`). 같은 API로 작업을 만들고 고치고 실행·중지할 수도 있습니다("Nexus REST API 호환" 참고).

첫 시작 때 작업이 하나도 없으면 `compact-blobstore`(`blobstore.compact`, `daily:03:00`, 사용)를 만듭니다. 이 기본 작업은 지울 수 없습니다(`DELETE`는 409). 지우면 재시작해도 다시 생기지 않아 compaction이 조용히 멈추기 때문이며, 끄고 싶으면 "사용"을 끄세요.

**실패 알림**: 작업의 `notifyEmail`에 주소를 넣으면 실행이 Failed로 끝날 때 메일을 보냅니다("이메일 알림" 참고). 실행 결과는 성공·실패 모두 `task.finished` 이벤트로 webhook에도 나갑니다("Webhook" 참고). 편집 화면의 "실패 알림 이메일" 입력란에 쉼표나 줄바꿈으로 구분해 적습니다.
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
- 기존 `POST /-/api/gc`는 compact 실행의 별칭으로 남아 있습니다. "지금 실행"과 달리 실행이 끝난 뒤 200으로 응답하며(실패하면 500), `removed`와 `bytes`는 지운 blob의 수와 바이트 수입니다(작업의 `lastRemoved`·`lastBytes`와 같음).

### 저장소 내보내기와 가져오기

저장소 내보내기(`repository.export`)와 가져오기(`repository.import`)는 예약 작업으로 실행하며, 라이선스 없이 쓸 수 있습니다. 먼저 설정 파일의 `exportRoot` 또는 환경 변수 `COMMANDCENTER_EXPORT_ROOT`에 절대 경로를 지정합니다(둘 다 주면 시작하지 않습니다). 내보내기와 가져오기는 이 디렉터리 아래에서만 읽고 쓰며, `../`나 바깥을 가리키는 심볼릭 링크는 거부합니다.

내보내기는 `<destination>/<작업 ID>/`에 `manifest.json`, 저장소별 `assets.jsonl`(파일 메타데이터, 패키지 행, 태그), `blobs/`를 씁니다. 같은 파일 시스템의 파일 blob store라면 blob을 하드 링크로 만들고(`hardLinks`, 기본 켬), 그렇지 않으면 복사합니다. `manifest.json`은 마지막에 쓰므로, 이 파일이 없는 디렉터리는 끝나지 않은 내보내기입니다. 같은 작업을 다시 실행하면 같은 디렉터리를 다시 쓰고 이미 있는 blob은 건너뜁니다. `since`를 지정하면 그 시각 이후 바뀐 파일만 내보냅니다(증분). 프록시는 `includeProxies`를 켠 경우에만 포함하고, group은 내보내지 않습니다(멤버를 내보내세요).

가져오기는 `source`에 `<destination>/<내보내기 ID>`를 지정합니다. 모든 blob의 SHA-256을 확인하며, 손상되거나 없는 blob이 있는 컴포넌트는 건너뛰고 작업 결과를 실패로 남깁니다. 다른 내용의 파일이 이미 있으면 `overwrite`를 켜지 않는 한 그대로 두고 로그에 남깁니다. 시각과 다운로드 수는 내보낸 값 그대로 가져옵니다. `createMissing`을 켜면 없는 저장소를 내보낸 설정으로 만듭니다(없는 blob store는 default, 없는 Cleanup 정책·라우팅 규칙은 빼고 경고를 남깁니다). 비밀값(원격 비밀번호·토큰, 원격 URL의 사용자 정보와 쿼리, apt `signingKey`)은 내보내지 않으므로 가져온 뒤 다시 입력하며, apt 저장소는 대상의 키로 다시 서명합니다. 비어 있는 저장소로 전체 내보내기를 가져오면 인덱스 문서(maven-metadata.xml, index.yaml, repodata 등)를 그대로 복사하고, 그 밖의 경우에는 가져온 뒤 다시 생성합니다.

진행 상황과 결과는 작업 로그(저장소마다 파일·바이트·행 수, 1만 파일마다 한 줄)에서 확인합니다. API로는 `POST /-/api/export`, `POST /-/api/import`(관리자, 비밀번호·세션 로그인과 `X-Command-Center-UI: 1` 헤더)에 파라미터를 보내면 수동 작업이 만들어져 바로 실행되고 202와 작업 ID가 돌아옵니다. 이렇게 만든 작업도 작업 개수 한도(100개)에 들어가므로 다 쓴 작업은 지우세요. 중지한 내보내기는 `manifest.json`을 남기지 않고, 중지한 가져오기는 그때까지 가져온 컴포넌트를 남깁니다.

### 로그·활동

- **로그**: SYSTEM → 시스템 상태·로그에서 서버 로그를 봅니다. 최근 **2 000건 또는 1 MiB** 중 먼저 닿는 한도까지 메모리에 보관하며, 재시작하면 사라집니다(표준 출력의 JSON 로그는 그대로 `docker compose logs`로 남습니다). 수준(INFO/WARN/ERROR)과 텍스트로 거르고, "다운로드"는 `commandcenter.log` 텍스트 파일을 내려줍니다(`GET /-/api/logs`, `/-/api/logs/download`, 관리자 전용).
- 로그에는 비밀 값이 들어가지 않습니다. 요청 경로의 토큰은 가려지고 NUL 같은 제어 문자는 `\x00` 꼴로 바뀌어 기록되며, proxy 원격 URL의 **자격 증명(userinfo)과 쿼리 값은 제거**하며, 인증 헤더는 기록하지 않습니다. 작업 로그에 나오는 URL도 같은 처리를 합니다.
- **활동(Events)**: 대시보드의 "최근 운영 활동"과 `GET /-/api/events`(관리자, 최대 200건씩, `before`·`kind`로 페이징·필터). DB에 최근 10 000건만 남기며, 기록은 비동기라 큐(1 000건)가 가득 차면 요청을 막지 않고 그 이벤트를 버립니다. 모든 활동 이벤트는 등록한 webhook에도 HTTP POST로 전달됩니다("Webhook" 참고). 종류:

  | kind | 시점 |
  |---|---|
  | `upload` | hosted 쓰기 성공(Maven 주 아티팩트, npm publish, PyPI 업로드, Go zip, Cargo publish) |
  | `delete` | 에셋·컴포넌트 삭제(Maven `DELETE`, `npm unpublish`, Go `DELETE`, 웹 UI·API, Cleanup). npm·PyPI·Go·Cargo는 버전마다 한 건이며 지운 사람과 파일 수(`assets=N`)가 남습니다 |
  | `repo.create` / `repo.update` / `repo.delete` | 저장소 변경 |
  | `role.create` / `role.update` / `role.delete` | 역할 변경. 저장소를 지워 그 저장소 권한이 빠진 역할도 `role.update`(저장소 이름, `removed repo:<이름>`)로 남습니다 |
  | `token.create` / `token.revoke` | API 토큰 발급·폐기 |
  | `task.run` | 작업 실행(결과와 요약) |
  | `cleanup` | 정리 결과(개수, 용량) |
  | `policy.create` / `policy.update` / `policy.delete` | 정책 변경 |
  | `routingrule.*` / `selector.*` / `webhook.*` | 라우팅 규칙, 컨텐츠 셀렉터, webhook 변경 |
  | `usertoken.create` / `usertoken.reset` / `usertoken.delete` / `settings.userTokens` / `settings.defaultRoles` | 사용자 토큰 발급·재설정·삭제와 설정, 기본 역할 변경 |
  | `realms.ldap.update` / `realms.oidc.update` / `realms.crowd.update` / `realms.rutauth.update` / `realms.saml.update` / `realms.crowd.test` | Realm 설정 변경(바뀐 키 이름만), Crowd 연결 테스트 |
  | `tag.create` / `tag.update` / `tag.delete` / `tag.associate` / `tag.dissociate` / `staging.move` / `staging.delete` | 태그 변경과 연결, 스테이징 이동·삭제 |
  | `blobstore.update` / `blobstore.convert` / `blobstore.quota` / `repo.blobstore` / `repo.import` | blob store 쿼터·그룹 변경, 그룹으로 바꾸기, 소프트 쿼터 위반·해소, 저장소의 blob store 이동 시작·끝, 저장소 가져오기 |
  | `firewall.blocked` / `firewall.release` / `vulnerability.settings` / `vulnerability.import` | 방화벽 격리·해제, 취약점 데이터 설정·가져오기 |
  | `settings.http` / `logging.level` / `support.zip` / `ssl.add` / `ssl.remove` | 외부 HTTP 설정 변경, 로그 레벨 변경, 지원 번들 내려받기, 신뢰 저장소 변경 |

### 다운로드 수·요청 추이

- 에셋 본문을 GET으로 성공적으로 받을 때마다(HEAD는 제외) 그 에셋의 다운로드가 1 늘고 마지막 다운로드 시각이 갱신됩니다. proxy가 캐시를 채우는 요청도 셉니다. 컴포넌트 상세의 기본 정보와 Assets 탭, Browse 파일 목록에 표시되며 Cleanup의 `lastDownloadedDays`가 이 값을 씁니다.
- 요청마다 DB에 쓰지 않습니다. 카운터는 메모리에 모았다가 **30초마다**(또는 항목이 10 000개에 닿으면, 종료할 때) 한 트랜잭션으로 반영합니다. 그래서 화면의 값은 최대 30초 늦습니다. 주기는 숨은 설정 `metricsFlushSeconds`(1..3600, 기본 30)로 바꿉니다.
- 요청 추이는 `/repository/<이름>/…` 요청 수와 응답 바이트를 시간 단위(UTC)로 집계해 대시보드의 7일 차트와 시스템 상태의 24시간 차트에 그립니다(`GET /-/api/stats/requests?range=24h|7d|30d&repo=`). 24h는 시간별, 7d·30d는 일별이며 **90일**이 지난 집계는 하루 한 번 지웁니다.
- 저장소별 상태(`GET /-/api/stats/repositories?range=1h|24h|7d`, admin)는 같은 시간 단위 집계에서 저장소마다 요청 수, 오류 수(5xx·429)와 오류율, 수신·송신 바이트, 응답 시간의 평균·최대를, proxy는 원격 요청 수·실패 수·원격 응답 시간의 평균·최대와 캐시 적중·미스도 냅니다. 대시보드의 "저장소별 상태" 표가 이것이며, 숫자를 누르면 그 저장소로 거른 로그 화면으로 갑니다. `1h`는 현재 시간대와 직전 시간대이고(응답의 `since`), 분위수(p95)는 시간 단위 집계에서 낼 수 없어 제공하지 않습니다.

### blob 정리 (`commandcenter gc`)

어떤 자산에도 참조되지 않는 blob 파일(업로드 실패나 덮어쓰기로 남은 것)을 지웁니다. 수정된 지 1시간이 안 된 blob은 진행 중인 업로드를 보호하기 위해 지우지 않습니다. 결과는 `removed N unreferenced blobs`로 출력됩니다. `gc`는 예약 작업과 같은 실행 잠금을 잡습니다. 어느 노드에서든 작업이 실행 중이면 `task "<ID>" is running` 오류로 끝나므로 잠시 뒤 다시 실행하세요. `gc`가 도는 동안에는 예약 작업이 시작되지 않고, 지금 실행은 409를 받습니다.

쓰기 도중 프로세스가 강제 종료되면(SIGKILL, OOM, 중지 대기 시간 초과) store의 `tmp/`에 미완성 임시 파일(`up-*`)이 남습니다. 1시간 넘게 수정되지 않은 임시 파일은 GC 때와 서버 시작 때 지웁니다. `gc`, `token create` 같은 CLI 명령도 시작할 때 같은 정리를 합니다. 진행 중인 쓰기는 늦어도 몇 분마다 파일을 갱신하므로, 서버가 돌고 있을 때 CLI를 실행해도 쓰는 중인 파일은 지워지지 않습니다. 지운 임시 파일 수는 위 결과에 들어가지 않습니다. compact 작업이 지운 경우에는 서버 로그에 `removed stale temp files`로 남습니다.

### 백업과 복원

Kubernetes의 명령은 "Kubernetes에서 백업"에 있습니다. 원칙은 이 절과 같습니다.

- **백업 대상**: 메타데이터 DB(저장소 정의, 사용자, 역할, 토큰 해시, 설정, 파일 목록)와 모든 blob store입니다. compose에서는 DB가 볼륨 `commandcenter-postgres`에, default store가 볼륨 `commandcenter-data`의 `blobs/`에, 경로를 비워 만든 이름 붙은 store가 같은 볼륨의 `blobstores/<이름>`에 있습니다. 절대 경로로 만든 store와 S3 store는 따로 챙기세요. `config.yaml`과 `.env`도 함께 보관하세요.
- **백업 파일 보호**: DB에는 proxy 원격 인증, LDAP bind, SMTP 비밀번호가 들어 있고, 비밀값 키를 정하지 않았다면 평문입니다("저장된 비밀값 암호화" 참고). 덤프 파일도 서버와 같은 수준으로 접근을 제한하세요. 키를 정했다면 키도 백업하되 DB 백업과 다른 곳에 두세요. 키가 없으면 복원한 서버가 시작하지 않습니다.
- **순서: DB 먼저, blob 나중.** `pg_dump`는 서버를 멈추지 않고 한 시점의 일관된 덤프를 받습니다. 그 **뒤에** blob 디렉터리를 복사하면 덤프가 가리키는 blob은 모두 들어갑니다. blob은 내용 주소 방식이라 한 번 쓰면 바뀌지 않으므로 덤프 뒤에 생긴 blob이 섞여도 무해합니다(다음 compact 작업이 정리합니다). 순서를 바꾸면 덤프가 복사되지 않은 blob을 가리켜 복원한 뒤 404가 됩니다.
- 백업하는 동안에는 compact 작업(`commandcenter gc` 포함)이 돌지 않게 하세요. 덤프 뒤에 지워진 파일의 blob을 compact가 회수하면 백업에서 빠집니다. 기본 compact 작업은 매일 03:00(서버 시간대)에 돕니다.

**백업 (compose, Linux/macOS/Git Bash)**

```bash
docker compose exec -T postgres pg_dump -U commandcenter -Fc commandcenter > commandcenter.dump
docker run --rm -v commandcenter-data:/data -v "$PWD":/backup busybox tar czf /backup/data.tgz /data
```

(Git Bash는 두 번째 줄 앞에 `MSYS_NO_PATHCONV=1`을 붙이고 `"$(pwd -W)"`를, PowerShell은 `"${PWD}:/backup"`을 씁니다. PowerShell의 `>`는 바이너리 덤프를 깨뜨리므로 `pg_dump … -f /tmp/cc.dump` 후 `docker compose cp postgres:/tmp/cc.dump .`로 꺼내세요.)

**복원** — 서버를 멈추고 빈 데이터베이스와 빈 blob 볼륨에 되돌립니다. `docker compose down -v`는 두 볼륨을 모두 삭제하므로 백업 파일을 먼저 확인하세요.

```bash
docker compose down -v
docker compose up -d --wait postgres
docker compose exec -T postgres pg_restore -U commandcenter -d commandcenter --no-owner < commandcenter.dump
docker compose create commandcenter
docker run --rm -v commandcenter-data:/data -v "$PWD":/backup busybox tar xzf /backup/data.tgz -C /
docker compose up -d
```

외부 PostgreSQL이라면 그 서버에서 `pg_dump`/`pg_restore`(또는 관리형 서비스의 스냅샷)를 쓰고, 위의 blob 복사만 compose 볼륨에 합니다.

**저장소 점검(verify-blobs)**: `repository.verify-blobs` 작업은 DB의 에셋이 가리키는 blob이 blob store에 실제로 있는지 점검합니다("예약 작업" 참고). 백업을 복원한 뒤나 blob 디렉터리를 옮긴 뒤 한 번 돌려 두 쪽이 같은 시점의 것인지 확인하세요. 기본은 존재와 크기만 보므로 빠르고, `checksum`을 켜면 blob을 다시 읽어 SHA-256까지 비교합니다. 큰 store는 `maxBytes`(기본 10 GiB)만큼 읽고 멈춘 뒤 다음 실행이 저장소별 커서에서 이어 가므로 매일 밤 일정으로 두면 전체를 돌아가며 점검합니다. 결과가 Failed면 로그에 빠진 blob(`missing:`)과 내용이 다른 blob(`mismatch:`)의 경로가 100개까지 남습니다. 작업은 아무것도 지우지 않습니다. 빠진 blob은 백업에서 해당 파일(`blobs/sha256/xx/yy/<sha>`)을 되돌리거나 에셋을 지우고 다시 올리고, 내용이 다른 blob은 디스크 손상을 의심하고 백업에서 되돌리세요.

### 릴리스 이미지와 바이너리

`v`로 시작하는 태그를 푸시하면 GitHub Actions(`.github/workflows/release.yml`)가 CI를 다시 통과시킨 뒤 다음을 만듭니다.

- **이미지** `ghcr.io/sungjinseo/commandcenter`(linux/amd64, linux/arm64). `v1.2.3`이면 `1.2.3` 태그가 붙고, 움직이는 태그 `1.2`, `1`, `latest`는 이 버전이 각 범위(1.2.x, 1.x, 전체)의 정식 릴리스 중 가장 높을 때만 이 버전으로 옮겨집니다. 예를 들어 `v1.3.0` 다음에 유지보수 릴리스 `v1.2.5`를 내면 `1.2.5`와 `1.2`만 바뀌고 `1`과 `latest`는 1.3.0에 남습니다. `v0.x`에는 `0` 태그가 없고, `v1.3.0-rc.1`처럼 `-`가 들어간 프리릴리스에는 자기 태그(`1.3.0-rc.1`)만 붙으며 가장 높은 버전을 따질 때도 세지 않습니다. GitHub Release의 "Latest" 표시도 `latest` 태그와 같은 버전에 붙습니다.
- **운영에서는 `1.2.3`처럼 정확한 버전을 고정하세요.** 새 버전이 DB를 바꾼 뒤에는 이전 버전으로 되돌리기 어려우므로 `latest`나 `1` 같은 움직이는 태그로 의도치 않게 올라가면 안 됩니다.
- **Helm 차트** `oci://ghcr.io/sungjinseo/charts/commandcenter`: 차트 버전과 appVersion은 릴리스 버전(`1.2.3`)이고, 기본 이미지 태그도 그 버전입니다(`helm show chart oci://ghcr.io/sungjinseo/charts/commandcenter --version 1.2.3`으로 확인). `--version`으로 고정해서 씁니다("Kubernetes" 참고). 차트도 이미지와 같은 방법으로 서명하므로, 아래 `cosign verify`의 이미지 이름을 `ghcr.io/sungjinseo/charts/commandcenter:1.2.3`으로 바꿔 확인합니다.
- compose 파일은 이 이미지를 씁니다. `.env`의 `COMMANDCENTER_VERSION`에 버전(`1.2.3`, `v` 없이)을 적고, 사내 레지스트리로 옮긴 사본을 쓰려면 `COMMANDCENTER_IMAGE`에 그 이름(태그 없이)을 적습니다("빠른 시작" 참고). 직접 빌드했거나 사본을 쓴다면 이 가이드의 `docker run` 명령에 나오는 `ghcr.io/sungjinseo/commandcenter:1.2.3`도 그 이름으로 바꿔 쓰세요.
- **SBOM과 provenance**: 이미지에 SPDX SBOM과 빌드 provenance 증명이 함께 올라갑니다. `docker buildx imagetools inspect ghcr.io/sungjinseo/commandcenter:1.2.3 --format '{{ json .SBOM }}'`(provenance는 `.Provenance`)로 확인합니다.
- **이미지 서명**: 이미지는 cosign으로 키 없이(keyless) 서명합니다. 서명 인증서에는 서명한 워크플로와 태그(`https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/v1.2.3`)가 적히고, 서명은 Sigstore의 공개 투명성 로그에 남습니다. 서명은 태그가 가리키는 이미지(여러 플랫폼을 묶은 인덱스)의 digest에 붙고, 플랫폼별 이미지의 digest에는 붙지 않습니다. cosign 3 이상으로 다음처럼 확인합니다.

  ```bash
  cosign verify ghcr.io/sungjinseo/commandcenter:1.2.3 \
    --certificate-identity https://github.com/sungjinseo/commandcenter/.github/workflows/release.yml@refs/tags/v1.2.3 \
    --certificate-oidc-issuer https://token.actions.githubusercontent.com
  ```

- **바이너리**: GitHub Release에 `commandcenter-v1.2.3-linux-amd64.tar.gz`, `commandcenter-v1.2.3-linux-arm64.tar.gz`(이미지 안의 바이너리와 같은 파일, 라이선스·고지 파일 포함)와 `SHA256SUMS`가 올라갑니다. 받은 뒤 `sha256sum -c --ignore-missing SHA256SUMS`로 확인하세요.

### 업그레이드

1. 올릴 버전의 [CHANGELOG](../CHANGELOG.md)를 읽습니다. 설정이나 운영 절차가 바뀌면 거기에 적습니다.
2. 백업합니다("백업과 복원" 참고). 업그레이드한 뒤 되돌릴 수 있는 방법은 이 백업뿐입니다.
3. 새 버전으로 바꿉니다. `.env`의 `COMMANDCENTER_VERSION`을 새 버전으로 고치고 다시 올리면, compose가 새 이미지를 받은 뒤 컨테이너를 바꿉니다("릴리스 이미지와 바이너리" 참고).

   ```bash
   docker compose up -d
   ```

   소스에서 빌드한다면 새 버전의 checkout에서 이미지를 빌드하고(`docker build --build-arg VERSION=v1.2.4 -t commandcenter:1.2.4 .`) `.env`의 `COMMANDCENTER_VERSION`을 그 태그(`1.2.4`)로 바꿔 올립니다. 이전 컨테이너는 진행 중인 요청을 마친 뒤 끝납니다("정상 종료" 참고). Helm 차트로 설치했다면 "차트 업그레이드"를 따릅니다.

   데이터 볼륨과 DB는 그대로 유지되고, 새 버전은 처음 시작할 때 DB 스키마를 필요한 만큼 새 구조로 바꿉니다. 구조 변경과 스키마 버전 기록은 한 트랜잭션으로 하므로, 도중에 중단되어도 DB는 이전 상태로 남고 다음 시작 때 다시 바꿉니다.
4. 시작 로그에 ERROR가 없는지 보고, 로그인·다운로드·업로드를 한 번씩 확인합니다. UI 세션은 재시작으로 끊기고 API 토큰은 유지됩니다.

**다운그레이드는 지원하지 않습니다.** DB에 스키마 버전(`schema_version` 테이블, 지금은 6)을 기록하고, 이 버전이 돌 수 없는 더 새 스키마의 DB로는 시작하지 않습니다. 이때 시작 오류는 `PostgreSQL database "commandcenter" on db.example was written by a newer version of Command Center (database schema 7, for builds of schema 7 and newer; this version supports up to 6) …`처럼 두 버전을 알려 주며, DB는 건드리지 않습니다. 되돌려야 하면 업그레이드 전에 받은 백업을 이전 버전의 이미지와 함께 복원하세요("백업과 복원" 참고). 백업 뒤에 올라온 패키지와 바뀐 설정은 사라집니다.

이 검사가 없는 이전 버전(구조 버전을 기록하지 않음)은 새 버전이 쓴 데이터로도 경고 없이 시작해 잘못 동작합니다. 사용자를 `config.yaml`에서 읽던 버전으로 내리면 인증이 `config.yaml`의 해시로 돌아가 UI에서 바꾼 비밀번호가 통하지 않습니다. 이름 붙은 blob store를 모르는 버전으로 내리면 기본이 아닌 store에 둔 저장소는 기존 파일을 찾지 못하고, 새로 올린 파일은 default store(`<dataDir>/blobs`)에 씁니다. 비밀값 키를 정해 두었다면 암호화된 값을 그대로 비밀번호로 보내 원격·SMTP·LDAP 인증이 실패합니다. 그렇게 돌린 뒤 이 버전으로 다시 올렸다면, default store의 GC는 어느 저장소든 참조하는 blob을 지우지 않으므로 그동안 쓴 파일은 남아 있습니다("Blob Store" 참고). 서버를 멈추고 default store의 blob을 그 저장소의 store로 복사하면 다시 받을 수 있습니다. 내용 주소 방식이라 이름이 같으면 내용도 같으므로 덮어써도 무해하고, 그 store의 저장소가 참조하지 않는 사본은 다음 compact 작업이 지웁니다. 경로를 비워 만든 store라면 아래와 같이 합니다(절대 경로로 만든 store는 그 경로를 마운트해 바꿔 쓰세요).

```bash
docker compose stop
docker run --rm -u 65532:65532 -v commandcenter-data:/data busybox cp -a /data/blobs/sha256/. /data/blobstores/<store 이름>/blobs/sha256/
docker compose start
```

### 메모리 설정

- `memoryLimitMiB`(기본 80)는 Go 메모리 한도를 정합니다. 단, `GOMEMLIMIT` 환경변수가 있으면 그 값이 우선하며 compose 파일은 `GOMEMLIMIT: 80MiB`를 설정합니다. 한도를 바꾸려면 둘 중 실제로 쓰이는 쪽(compose 사용 시 `GOMEMLIMIT`)을 고치세요.
- compose의 `mem_limit: 128m`은 컨테이너 하드 한도입니다. 위 두 값보다 충분히 커야 합니다.
- 원격 다운로드는 동시에 32개까지만 진행합니다.
- 동시 연결은 **1024개**까지 받습니다. 자리가 차면 새 연결을 받기 위해, 요청 사이에 쉬고 있는(keep-alive) 연결이나 첫 요청의 헤더를 아직 다 보내지 않은 연결 가운데 그 상태로 가장 오래(1초 이상) 있던 것을 닫습니다. 요청을 처리 중인 연결은 닫지 않으며, 닫을 수 있는 연결이 없으면 새 연결은 커널 대기열(listen backlog)에서 기다립니다. keep-alive 유휴 연결은 자리가 남아 있어도 60초 뒤에 닫힙니다. 측정해 보면 연결 하나는 유휴, 헤더 수신 중, 본문 대기 중일 때 14~25 KiB에 받은 헤더 크기의 약 1.6배를 더 씁니다. 요청 헤더는 16 KiB까지만 받으므로(넘으면 431) 연결 하나는 많아야 약 40 KiB, 1024개면 약 40 MiB로 `GOMEMLIMIT`(80 MiB) 안에 머뭅니다. 고정값이며 설정으로 바꿀 수 없습니다. 훨씬 많은 클라이언트를 받아야 하면 앞단 리버스 프록시에서 연결을 모아 주세요.

### 업로드 크기 제한

| 대상 | 제한 | 초과 시 |
|---|---|---|
| 모든 포맷의 업로드(GET/HEAD 외) 본문 | `maxUploadMiB`(기본 1024 MiB) | 413 |
| Maven `maven-metadata.xml` 업로드 | 1 MiB | 413 |
| Maven `.pom` 업로드(classifier 없는 것) | 1 MiB | 413 |
| npm publish 문서(tarball 첨부 제외), unpublish 문서 | 10 MiB | 413 |
| npm tarball 검사: 압축을 푼 크기(publish 문서 하나의 새 tarball 합계) / 안의 `package.json` | 1 GiB / 1 MiB | 400 |
| PyPI 업로드 폼 | 사용하는 필드 각 4 KiB, 그 밖의 필드 합계 16 MiB, 필드 최대 256개 | 413 |
| PyPI 파일 검사 | zip(wheel, zip sdist) 항목 10,000개, central directory 2 MiB 이하(서버가 직접 센 값). `.tar.gz` sdist는 `PKG-INFO`가 압축을 푼 처음 512 MiB 안 | 400 |
| Go `.zip` / `.mod` / `.info` | 500 MiB / 16 MiB / 1 MiB | 413 |
| Go zip 구조 | 항목 50000개, 중앙 디렉터리 4 MiB 이하(서버가 직접 센 값) | 400 |
| Go zip 내용 | 압축 해제 합계 500 MiB(실제로 읽은 바이트), 루트 `LICENSE`·`go.mod` 각 16 MiB | 400 |
| Cargo crate / publish 메타데이터 | 50 MiB / 1 MiB | 413 |
| Cargo crate 안의 `Cargo.toml` | 1 MiB | 400 |

## 제한 사항

- 웹 UI로 저장소 만들기·편집·삭제, 사용자·역할·Realm(LDAP, OIDC)·Blob Store·일반 설정·이메일 관리, Maven, npm, PyPI 업로드를 할 수 있습니다. Go·Cargo 업로드 화면은 없습니다(클라이언트로 올립니다). 저장소 정의는 DB에 있고 `config.yaml`의 `repositories:`는 첫 시작에만 쓰입니다. 포맷 고유의 검색(`npm search` 포함)도 없습니다.
- **Terraform의 알려진 제한**: Terraform은 레지스트리를 `https://<호스트>`로만 찾으므로 HTTPS가 필요하고, 한 호스트에는 기본 저장소(`terraformDefaultRepository`) 하나만 보입니다. Terraform은 모듈 아카이브·프로바이더 zip을 받을 때 `~/.terraformrc` 토큰을 보내지 않으므로(모듈은 `~/.netrc`만, 프로바이더의 `SHA256SUMS`·서명은 자격 증명 없이) 비공개 프로바이더는 그 저장소(또는 group)에 `anonymousRead: true`가 필요하고, 비공개 모듈은 `~/.netrc`가 필요합니다. 업스트림의 `git::` 모듈 소스는 캐시하지 않습니다("Terraform 설정" 참고).
- **SAML의 알려진 제한**: Single Logout이 없어 로그아웃은 Command Center 세션만 끝냅니다. IdP-initiated 로그인은 `allowIdpInitiated`를 켰을 때만 받습니다("SAML 설정" 참고).
- **방화벽(OSV)의 알려진 제한**: 격리는 파일을 받아 캐시한 뒤 내줄 때 거부하는 방식이라 메타데이터(npm packument, PyPI 인덱스, `maven-metadata.xml`)에는 격리된 버전이 그대로 나오고, 클라이언트가 그 버전을 고르면 403(일반 텍스트 본문)을 받습니다. 온라인 모드에서 각 버전의 첫 다운로드는 OSV API를 기다립니다(최대 15초, 그 뒤 fail open). conda, Docker, apt, yum, Helm, raw, Git LFS는 대상이 아닙니다("취약점 데이터와 방화벽(OSV)" 참고).
- **스테이징의 알려진 제한**: 스테이징 이동은 maven, npm, raw, pypi, nuget, helm, yum만 되고 docker를 포함한 나머지 포맷은 거부합니다. npm은 대상의 `latest`만 갱신하고 다른 dist-tag는 옮기지 않습니다. 프로토콜 클라이언트로 지운 컴포넌트는 태그 연결이 남습니다("태그와 스테이징" 참고).
- **내보내기·가져오기의 알려진 제한**: 저장소 설정의 비밀값(원격 비밀번호·토큰, 원격 URL의 사용자 정보와 쿼리, apt `signingKey`)은 내보내지 않으므로 가져온 뒤 다시 넣어야 합니다. 증분 내보내기(`since`)는 바뀐 파일만 담으므로 행만 바뀐 변경(npm dist-tag, 더 오래된 매니페스트로 옮긴 docker 태그, yank)은 전체 내보내기가 필요합니다("저장소 내보내기와 가져오기" 참고).
- **Conan·R·Git LFS의 알려진 제한**: Conan v1 API에는 `remove_files`와 `packages/delete`가 없어 리비전 없는 Conan 1.x의 `conan remove -p`와 원격에만 있는 파일을 지우는 재업로드가 실패합니다(Conan 2와 리비전을 켠 Conan 1은 해당 없음). R group은 미러의 `PACKAGES.rds`를 읽지 않고 병합한 `PACKAGES`에서 `.gz`·`.rds`를 만듭니다. Git LFS는 hosted만 되고 잠금(`locks`) API는 501입니다("Conan 설정", "R 설정", "Git LFS 설정" 참고).
- **p2·Swift·CocoaPods·HuggingFace의 알려진 제한**: p2는 Nexus와 같이 proxy·group만 되고(hosted 없음), 복합 사이트의 `.xml.xz` 문서는 풀지 않고 그대로 전달하므로 그 안의 자식 위치는 바뀌지 않습니다. Swift의 `swift package-registry` 명령은 레지스트리 명세를 따라 적은 것으로 실제 SwiftPM 클라이언트로는 아직 검증하지 않았고, SwiftPM은 레지스트리를 HTTPS로 열므로 운영에서는 `baseURL`을 `https://`로 두어야 합니다. CocoaPods는 트렁크 CDN 소스만 proxy하며 `source.git`(svn·hg 포함) Pod는 proxy하지 않으므로 클라이언트가 원본 git 서버에 직접 접속해야 합니다. HuggingFace는 한 파일을 원본에서 30분 안에 받아야 하고, 큰 파일의 첫 요청은 클라이언트가 먼저 시간 초과할 수 있으므로 `HF_HUB_DOWNLOAD_TIMEOUT`을 늘리세요. `/`가 든 리비전(`refs/pr/*`), `POST …/paths-info`, Xet 전송, 업로드는 지원하지 않습니다("p2 설정", "Swift 설정", "CocoaPods 설정", "HuggingFace 설정" 참고).
- **NuGet·APT·Yum의 알려진 제한**: APT hosted는 control 아카이브의 압축 해제 창이 8 MiB를 넘는 `.deb`(`xz -7` 이상 등)를 400으로 거부합니다. Yum hosted는 `repomd.xml`을 서명하지 않으므로 클라이언트 `.repo`에 `repo_gpgcheck=0`이 필요합니다. NuGet은 심볼 패키지(`.snupkg`)를 400으로 거부합니다. apt·yum group은 멤버의 `Packages`·`repodata/`를 병합하지 않고 그 경로를 처음 가진 멤버가 답합니다("NuGet 설정", "APT 설정", "Yum 설정" 참고).
- **RubyGems·Composer·Conda의 알려진 제한**: RubyGems group은 `versions`·`names`만 병합하고 Marshal 인덱스(`specs.4.8.gz` 등)는 그 파일을 처음 가진 멤버 것을 그대로 냅니다. Conda hosted는 `repodata.json.bz2`·`.zst`·`.jlap`를 만들지 않습니다(클라이언트는 `repodata.json`으로 넘어갑니다). Composer는 v1 `providers-url`과 `notify-batch`·`security-advisories`를 제공하지 않으며(Composer 2 전용), `/`가 든 브랜치 버전은 올릴 수 없습니다("RubyGems 설정", "Composer 설정", "Conda 설정" 참고).
- **여러 대의 쓰기 처리량**: 같은 PostgreSQL DB와 공유 스토리지로 여러 대를 함께 띄울 수 있지만(active-active), 쓰기는 DB에서 하나씩 처리되므로 노드를 늘려도 쓰기 처리량은 늘지 않습니다. 차트와 compose가 띄우는 PostgreSQL은 Pod(컨테이너) 하나라 고가용성이 아니므로, 고가용성에는 복제 구성된 PostgreSQL이나 관리형 서비스를 씁니다("고가용성(여러 노드)" 참고).
- HTTPS를 직접 받지 않습니다. 리버스 프록시에서 TLS를 끝내세요("TLS와 리버스 프록시" 참고). 다운그레이드는 지원하지 않습니다("업그레이드" 참고).
- **의도적으로 넣지 않은 것**: 저장소·사용자 **할당량(quota)**(blob store의 소프트 쿼터는 있습니다), 에디션(Pro/OSS) 구분, **SMTP의 PLAIN 외 인증**(LOGIN, CRAM-MD5, OAuth), TLS 검증 생략 옵션, **AD 확장 매칭 필터**(`:1.2.840…:=`), LDAP 부분 일치·비교 필터, LDAP 중첩 그룹 해석, LDAP 사용자 자동 삭제·토큰 자동 폐기.
- Go 체크섬 DB(`sumdb/`)는 프록시하지 않습니다. 사내 모듈에는 `GONOSUMDB`를 쓰세요.
- Go zip 검사는 go 명령의 규칙을 전부 옮기지는 않았습니다. 하위 디렉터리의 `go.mod`, 루트의 `GO.MOD`처럼 대소문자가 다른 `go.mod`, 파일 이름의 `*`·`?`·`"`·`'`·`<`·`>`·`|`·백틱과 제어 문자, `.`으로 끝나는 이름, 다른 항목의 상위 디렉터리와 이름이 같은 파일(`a`와 `a/b.go`)은 받아들이지만, go 명령은 이런 zip을 내려받을 때 거부합니다. 이런 zip은 올리지 마세요.
- 삭제는 hosted 저장소에서만 됩니다(`npm unpublish`와 Go `DELETE`는 group으로 보내면 `writeMember`에서 지웁니다). proxy의 캐시는 Cleanup 정책으로만 지웁니다. twine과 cargo에는 삭제 명령이 없으므로 PyPI와 Cargo는 웹 UI나 API로 지우고, 패키지 문서가 10 MiB를 넘는 npm 패키지는 버전 하나를 지우는 `npm unpublish`가 413이므로 역시 웹 UI나 API로 지웁니다("컴포넌트 삭제" 참고).
- 같은 파일·버전을 다시 올릴 때의 응답:

  | 포맷 | 응답 |
  |---|---|
  | Maven (`allow_once`) | 400 |
  | npm | 403 |
  | PyPI | 400 `File already exists.` |
  | Go | 409 (`writePolicy: allow`이면 허용) |
  | Cargo | 409 |
  | NuGet, APT, Yum (`allow_once`) | 409 (`writePolicy: allow`이면 허용) |
  | RubyGems, Composer, Conda (`allow_once`) | 409 (`writePolicy: allow`이면 허용) |
  | Conan (`allow_once`) | 같은 리비전의 같은 파일을 다른 내용으로 올리면 409, 같은 내용이면 200 (`writePolicy: allow`이면 허용) |
  | R (`allow_once`) | 409 (`writePolicy: allow`이면 허용) |
  | Git LFS | 200 (같은 oid는 같은 내용이므로 멱등) |
  | Swift (`allow_once`) | 409 (`writePolicy: allow`이면 허용) |
  | Terraform (`allow_once`) | 같은 내용이면 201, 다른 내용이면 409 (`writePolicy: allow`이면 허용) |

  Cargo는 `-`/`_`와 대소문자만 다른 이름, 빌드 메타데이터(`+…`)만 다른 버전도 같은 것으로 보고 409로 거부합니다.
- **group과 의존성 혼동**: group은 멤버를 순서대로 조회합니다. Maven과 Go는 `latest`를 멤버 전체 중 가장 새로운 버전으로 고르므로, hosted 멤버를 앞에 두어도 proxy 쪽에 더 높은 버전이 있으면 그것이 `latest`가 됩니다. npm·PyPI·Cargo group은 그 패키지(프로젝트, crate)를 처음 가진 멤버 하나만으로 답하므로, hosted를 proxy보다 앞에 두면 같은 이름의 공개 버전이 섞이지 않습니다. npm과 PyPI는 앞선 멤버가 Offline이거나 오류를 내면 503·5xx로 실패하고, tarball·파일 URL을 직접 요청해도 그 멤버의 것만 줍니다. Cargo group은 Offline 멤버를 건너뛰고, crate 파일 다운로드 URL은 멤버 순서대로 찾습니다. Maven·Go·Cargo에서 사내 패키지 이름을 공개 저장소에서 선점당하지 않으려면 사내 전용 group을 따로 두거나 proxy 멤버를 넣지 마세요.
- Maven 메타데이터를 가진 멤버가 하나뿐이면 병합하지 않고 그 문서를 그대로 제공합니다. 다만 Maven 그룹에 이미 병합해 저장한 문서가 있으면 내용이 달라졌을 때 새로 저장하여 `Last-Modified`가 앞으로 움직입니다. npm 그룹은 packument를 병합하지 않고 처음 가진 멤버의 문서로 답하므로, 그 멤버가 바뀌면(예: hosted에서 마지막 버전을 지워 proxy가 답하게 되면) `Last-Modified`가 뒤로 갈 수 있습니다. 이전 버전이 그룹에 저장해 둔 병합 packument는 읽지 않습니다.
- 405 응답은 `Allow` 헤더로 그 경로가 실제로 받는 메서드를 알려 줍니다(예: npm dist-tag 경로 `/-/package/{pkg}/dist-tags/{tag}`는 `PUT, DELETE`, proxy 저장소의 쓰기 경로는 `GET, HEAD`). 받는 메서드가 없는 경로(proxy와 `writeMember`가 없는 group의 npm `-rev`, proxy의 Cargo publish·PyPI 업로드 경로)는 빈 `Allow`를 보냅니다. hosted(와 `writeMember`가 있는 group)의 npm `{pkg}/-rev/{rev}`는 `PUT, DELETE`, tarball의 `-rev`는 `DELETE`, Go `.mod`·`.info`·`.zip`은 `GET, HEAD, PUT, DELETE`를 알립니다.
- proxy가 받은 파일의 체크섬이 원격 인덱스와 다르면 첫 요청은 502이고, 이후 `negativeCacheTTL` 동안은 404입니다. 이 404는 그 사이 원격 인덱스가 바뀌어도 만료될 때까지 유지됩니다.

## 문제 해결

- **`baseURL` 불일치**: npm/cargo가 "다운로드 실패", 엉뚱한 호스트로 접속, `ECONNREFUSED`를 내면 응답 안의 URL이 클라이언트가 접속하는 주소와 다른 것입니다. `baseURL`을 클라이언트가 쓰는 주소(스킴, 호스트, 포트)와 똑같이 맞추고 `docker compose restart` 하세요. pip는 PyPI 파일 링크가 호스트 없는 경로라서 해당하지 않습니다(`baseURL`의 경로 부분만 맞으면 됩니다).
- **npm 401**: `~/.npmrc`의 토큰이 서버에 없으면(예: 볼륨을 다시 만든 뒤) `anonymousRead`가 켜져 있어도 모든 요청이 401입니다. `npm login`을 다시 하거나 `_authToken` 줄을 지우세요.
- **`… was written by a newer version of Command Center`**: 더 새 버전이 업그레이드한 DB로 이전 버전을 띄웠습니다. 새 버전으로 다시 시작하거나, 업그레이드 전에 받은 백업을 지금 버전과 함께 복원하세요("업그레이드" 참고).
- **`no database configured: set database.url (or COMMANDCENTER_DATABASE_URL) to a PostgreSQL URL`**: 설정 파일의 `database.url`이나 환경 변수 `COMMANDCENTER_DATABASE_URL`에 PostgreSQL URL을 넣으세요("메타데이터 데이터베이스" 참고).
- **`it is encrypted, and no secret key is set` / `it was encrypted with another key`**: 비밀값 키가 없거나 다릅니다. 암호화할 때 쓴 키를 `COMMANDCENTER_SECRET_KEY`나 `secretKeyFile`로 주세요. `secret-key rotate`로 키를 바꿨다면 새 키이고, 교체 전에 받은 백업을 복원했다면 이전 키입니다("저장된 비밀값 암호화" 참고).
- **`508 proxy loop detected`**: proxy 저장소의 `remote`가 결국 같은 Command Center 인스턴스를 가리키면(예: 자기 자신의 그룹) 요청이 돌고 돌지 않도록 서버가 508로 거절합니다. 요청마다 `X-Command-Center-Hop` 헤더에 인스턴스 ID를 덧붙여 알아내므로, 서로 다른 두 인스턴스가 이어지는 것(A가 B를 proxy)은 막지 않습니다. `remote`를 고치세요.
- **`431 Request Header Fields Too Large`**: 요청 헤더가 16 KiB를 넘었습니다. 브라우저라면 같은 사이트(상위 도메인 포함)에 쌓인 쿠키가 큰 경우가 많으니 쿠키를 지워 보세요.
- **웹 UI의 저장·업로드가 403 `cross-origin request detected…`**: 브라우저의 `Origin`이 서버가 받은 `Host`와도, `baseURL`의 출처(스킴, 호스트, 포트)와도 다릅니다. 보통 `Host`를 바꾸는 프록시 뒤에서 평문 HTTP로 접속할 때이니, `baseURL`을 브라우저 주소창의 주소와 똑같이 맞추세요("TLS와 리버스 프록시" 참고). `baseURL`에 기본 포트(`:80`, `:443`)를 적으면 브라우저의 `Origin`과 맞지 않으니 빼세요.
- **npm·PyPI 그룹이 `503 group member "…" is offline`**: 그 멤버가 Offline입니다. 요청한 패키지를 그 멤버보다 앞의 멤버가 갖고 있지 않으면 그룹은 다음 멤버로 넘어가지 않습니다(사내 패키지 대신 같은 이름의 공개 패키지가 설치되는 것을 막기 위해서입니다). 멤버를 Online으로 돌리거나 group의 멤버 목록에서 빼세요.
- **proxy가 502이고 로그에 `address refused by the remote egress policy`**: 원격, 원격의 리다이렉트, 또는 원격 인덱스가 준 파일 URL이 허용되지 않은 내부 주소로 향했거나, 호스트가 `10.1`, `167772161`처럼 네 부분의 십진수가 아닌 숫자 표기이거나 ASCII가 아닌 이름입니다("저장소 키"의 "원격 접속 제한" 참고). 사내 미러를 쓰려면 `remote`를 그 미러의 사내 주소(사설 IP로 해석되는 이름)로 지정하고, IP 주소는 `10.0.0.1`처럼 네 부분으로, 국제화 도메인은 퓨니코드(`xn--…`)로 적으세요.
- **로그에 `stopped after 5 redirects`**: 원격이 리다이렉트를 5번 넘게 이어 갔습니다. `remote`를 최종 주소로 바꾸세요.
- **`remoteAuth`를 쓰는 proxy의 로그에 `remote status 401`(또는 403)**: 클라이언트는 502(만료된 캐시가 있으면 그 캐시)를 받습니다. 자격 증명이 맞다면, 원격이 다른 포트, `http`, 하위 도메인으로 리다이렉트해서 그 요청에 자격 증명을 보내지 않은 것일 수 있습니다(`remoteAuth` 참고. 같은 호스트의 `http`에서 `https`로 옮기는 리다이렉트는 자격 증명을 유지합니다). TLS를 끝내는 앞단 뒤의 원격이 `http://…:8081/` 같은 주소로 리다이렉트하는 경우가 흔하니, 원격이 자기 `https` 주소로 리다이렉트하게 고치세요(앞단이 `X-Forwarded-Proto`를 넘기게 하는 등). 최종 주소가 `https`라면 `remote`를 그 주소로 바꿔도 됩니다.
- **Kubernetes에서 Pod가 `CreateContainerConfigError`**: `secretKey.existingSecret`이 가리키는 Secret이나 그 안의 키(`secretKey.key`)가 없습니다. Secret을 만들면 Pod가 곧 시작합니다("Helm 차트로 설치" 참고).
- **`config.yaml`이 디렉터리**: `config.yaml` 없이 `docker compose up`을 하면 Docker가 같은 이름의 디렉터리를 만듭니다. `docker compose down`, 디렉터리 삭제, 파일 복사 후 다시 실행하세요.
- **Windows 경로**: Git Bash에서 `docker run -v`에 `/c/...` 같은 경로를 쓰면 경로가 변환되어 마운트가 어긋날 수 있습니다. `MSYS_NO_PATHCONV=1`을 설정하고 `$(pwd -W)`(또는 PowerShell의 `${PWD}`)를 쓰세요. 데이터는 바인드 마운트보다 named volume(compose 기본값)이 파일 권한과 I/O에 안정적입니다.
- **시작 실패 / 동작 이상**: 먼저 `docker logs commandcenter`(또는 `docker compose logs`)를 확인하세요. 설정 오류는 시작할 때 항목별로 출력됩니다.
- **로그인이 `429 too many failed sign-ins`**: 그 이름으로 실패가 반복되어 잠시 거절된 것입니다("보호"의 로그인 시도 제한). `Retry-After`만큼 기다리거나 관리자가 비밀번호를 재설정하면 풀립니다. 예전 비밀번호를 저장한 빌드나 CI가 계속 재시도하고 있지 않은지 서버 로그의 `sign-in throttled`와 요청 로그로 확인하세요.
- **proxy 저장소가 비어 있음**: 컨테이너에서 원격 레지스트리로 HTTPS 접속이 되어야 합니다. 사내 프록시가 있으면 compose의 `HTTPS_PROXY` 환경변수를 설정하고, 프록시를 거치지 않을 사내 호스트는 `NO_PROXY`에 넣으세요.

## 개발

로컬에 Go를 설치하지 않고 컨테이너(golang:1.26)에서 테스트합니다.

테스트는 PostgreSQL이 필요합니다. `docker run -d --name cc-test-pg -p 5432:5432 -e POSTGRES_PASSWORD=pw postgres:17-alpine`로 띄우고 `COMMANDCENTER_TEST_POSTGRES_URL='postgres://postgres:pw@localhost:5432/postgres?sslmode=disable' go test ./...`로 실행합니다. 테스트마다 데이터베이스를 따로 만들고 지우므로 패키지가 병렬로 돌아도 겹치지 않습니다. 변수가 없으면 DB가 필요한 테스트는 그 이유를 밝히며 실패합니다. 스모크·부하 테스트(`test/smoke`, `test/load`)는 PostgreSQL 컨테이너(`postgres:17-alpine`, `SMOKE_PG_IMAGE`로 바꿈)를 함께 띄웁니다.

main에 push하거나 PR을 열면 GitHub Actions(`.github/workflows/ci.yml`)가 gofmt, `go vet`, `go test`(배포 바이너리처럼 CGO 끔, PostgreSQL 서비스 컨테이너), `go test -race`, govulncheck, UI 스크립트 구문 검사, 이미지 빌드(linux/amd64·arm64), Helm 차트와 참조 매니페스트 검사(`helm lint`, `helm template`, kubeconform)를 실행합니다. `ci.yml`의 `go-version`이 Dockerfile의 `golang` 태그(현재 1.26.10)와 다르거나, `THIRD_PARTY_LICENSES.txt`가 바이너리에 링크되는 모듈·Go 버전과 맞지 않으면(`scripts/third-party-licenses.sh --check`) CI가 실패합니다. Dependabot이 Go 모듈, 베이스 이미지, Actions 업데이트를 매주 PR로 올리는데, 링크되는 모듈이나 `golang` 태그를 바꾸는 PR은 이 검사에서 실패합니다. 그 PR 브랜치에서 `bash scripts/third-party-licenses.sh`로 `THIRD_PARTY_LICENSES.txt`를 다시 만들어 커밋하고(스크립트는 Dockerfile의 `golang` 이미지에서 돌므로 Docker만 있으면 됩니다), `golang` 태그가 바뀌었으면 `ci.yml`의 `go-version`도 같은 버전으로 올리세요.

릴리스는 태그를 푸시하면 됩니다(`git tag v1.2.3 && git push origin v1.2.3`). 결과물은 "릴리스 이미지와 바이너리"를 보세요. 여러 버전을 함께 낼 때(예: `v1.3.0`과 유지보수 릴리스 `v1.2.5`)는 태그를 한 번에 푸시하세요(`git push origin v1.2.5 v1.3.0`). 각 실행은 시작할 때 본 태그로 움직이는 태그를 정하므로, 낮은 버전의 실행이 높은 버전의 태그를 보지 못하면 `latest`가 낮은 버전으로 돌아갈 수 있습니다.

```bash
scripts/go.sh test ./...      # 단위·통합 테스트(PostgreSQL 컨테이너 commandcenter-test-pg를 함께 띄웁니다)
scripts/go.sh vet ./...

test/e2e.sh                   # 스모크 테스트(포맷 5개, UI, LDAP, 메일)와 부하 테스트를 모두 실행하고 요약 출력

test/smoke/maven-smoke.sh     # 실제 클라이언트로 포맷별 E2E
test/smoke/npm-smoke.sh
test/smoke/pypi-smoke.sh
test/smoke/go-smoke.sh
test/smoke/cargo-smoke.sh

test/load/load.sh             # 128 MiB 컨테이너 메모리 부하 테스트
```

기여 절차와 코드 규칙은 [CONTRIBUTING.md](../CONTRIBUTING.md)에 있습니다.
